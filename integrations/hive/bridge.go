package hive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// NoteStore is the slice of contracts.NoteAPI the bridge needs. The hub (or
// the daemon's local proxy) implements it.
type NoteStore interface {
	ListNotes(ctx context.Context, req contracts.ListNotesRequest) (contracts.Page[contracts.Note], error)
	UpdateNoteSync(ctx context.Context, req contracts.UpdateNoteSyncRequest) (contracts.Note, error)
}

// noteGetter is optionally implemented by a NoteStore (contracts.NoteAPI
// does) so a note seen without its body can be fetched in full.
type noteGetter interface {
	GetNote(ctx context.Context, req contracts.GetRequest) (contracts.Note, error)
}

// Publisher writes notes into Hive. *Client implements it.
type Publisher interface {
	Available(ctx context.Context) (bool, string)
	FindNote(ctx context.Context, noteID string) (ref string, found bool, err error)
	PublishNote(ctx context.Context, n contracts.Note) (ref string, err error)
}

// Sync states recorded on hub notes (contracts.Note.SyncState).
const (
	SyncPending = "pending"
	SyncIndexed = "indexed"
	SyncFailed  = "failed"
)

// BridgeConfig configures a Bridge.
type BridgeConfig struct {
	Store NoteStore
	Hive  Publisher
	Clock contracts.Clock
	// StateFile persists the ledger (writes whose hub sync record is not yet
	// confirmed, retry counters, scan cursor). Empty = in memory only.
	StateFile string

	BatchSize    int           // notes written per SyncOnce (default 20)
	PageSize     int           // ListNotes page size (default 100)
	MaxScanPages int           // pages scanned per SyncOnce (default 10)
	Interval     time.Duration // Run loop period (default 30s)
	// MinPassGap is the minimum time between the starts of two Run passes
	// (default 2s): a burst of Notify calls (one per note.created event)
	// coalesces into one pass instead of back-to-back hub scans. Negative
	// disables it.
	MinPassGap     time.Duration
	InitialBackoff time.Duration // after Hive is unavailable/failing (default 5s)
	MaxBackoff     time.Duration // backoff ceiling (default 5m)
	MaxAttempts    int           // per-note failures before it is marked failed (default 5)
	MaxLedger      int           // ledger entry cap (default 2000)
	Logf           func(format string, args ...any)
}

// Bridge publishes explicit hub notes/handoffs/decisions into Hive so they
// become searchable, and records hive_ref/sync_state on the hub note.
//
// Guarantees:
//   - Idempotent: the deterministic marker "[pyre:<note_id> …]" is looked up
//     before any write, and the ledger records completed writes, so a crash
//     at any point never produces a second Hive entry for one note.
//   - Bounded: one write at a time (concurrency 1), BatchSize per pass,
//     MaxScanPages per pass, passes at least MinPassGap apart, exponential
//     backoff while Hive is failing. Run marks its Hive calls as background
//     work (see Background): they share the client's background slots with
//     transcript capture and run at lowered CPU priority.
//   - Isolated: it runs in its own goroutine; nothing in messaging or launch
//     waits for it, and Hive outages leave notes "pending", not lost.
type Bridge struct {
	cfg BridgeConfig

	runMu  sync.Mutex // serialises SyncOnce (concurrency 1)
	mu     sync.Mutex // guards ledger + backoff
	led    ledger
	wake   chan struct{}
	nextAt time.Time
	delay  time.Duration
	// unsynced holds hub notes seen without a confirmed Hive index, even
	// ones never attempted (e.g. written while Hive was down), so Pending
	// reports them. Not persisted: rebuilt by the next scan.
	unsynced map[string]bool
	passes   int
}

type ledgerEntry struct {
	Ref       string    `json:"ref,omitempty"`     // Hive uid once known
	Written   bool      `json:"written,omitempty"` // `hive note` succeeded (never write again)
	Attempts  int       `json:"attempts,omitempty"`
	NextTry   time.Time `json:"next_try,omitempty"`
	LastError string    `json:"last_error,omitempty"`
	Updated   time.Time `json:"updated"`
}

type ledger struct {
	Version int                     `json:"version"`
	Cursor  string                  `json:"cursor,omitempty"`
	Notes   map[string]*ledgerEntry `json:"notes"`
}

// NewBridge validates cfg and loads the ledger.
func NewBridge(cfg BridgeConfig) (*Bridge, error) {
	if cfg.Store == nil || cfg.Hive == nil {
		return nil, errors.New("hive bridge: Store and Hive are required")
	}
	if cfg.Clock == nil {
		cfg.Clock = contracts.SystemClock{}
	}
	def := func(v *int, d int) {
		if *v <= 0 {
			*v = d
		}
	}
	def(&cfg.BatchSize, 20)
	def(&cfg.PageSize, 100)
	def(&cfg.MaxScanPages, 10)
	def(&cfg.MaxAttempts, 5)
	def(&cfg.MaxLedger, 2000)
	if cfg.Interval <= 0 {
		cfg.Interval = 30 * time.Second
	}
	switch {
	case cfg.MinPassGap == 0:
		cfg.MinPassGap = 2 * time.Second
	case cfg.MinPassGap < 0:
		cfg.MinPassGap = 0
	}
	if cfg.InitialBackoff <= 0 {
		cfg.InitialBackoff = 5 * time.Second
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = 5 * time.Minute
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	b := &Bridge{cfg: cfg, wake: make(chan struct{}, 1), led: ledger{Version: 1, Notes: map[string]*ledgerEntry{}}, unsynced: map[string]bool{}}
	if cfg.StateFile != "" {
		if data, err := os.ReadFile(cfg.StateFile); err == nil {
			var l ledger
			if err := json.Unmarshal(data, &l); err != nil {
				return nil, fmt.Errorf("hive bridge: corrupt state file %s: %w", cfg.StateFile, err)
			}
			if l.Notes == nil {
				l.Notes = map[string]*ledgerEntry{}
			}
			b.led = l
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	return b, nil
}

// BridgeReport summarises one SyncOnce pass.
type BridgeReport struct {
	Skipped         string   `json:"skipped,omitempty"` // "backoff" | "hive_unavailable"
	Scanned         int      `json:"scanned"`
	Candidates      int      `json:"candidates"`
	Published       int      `json:"published"`        // new Hive entries written
	AlreadyInHive   int      `json:"already_in_hive"`  // found by marker; no write
	Recorded        int      `json:"recorded"`         // hub sync records updated to indexed
	Failed          int      `json:"failed"`           // write failures this pass
	HubErrors       int      `json:"hub_errors"`       // UpdateNoteSync/ListNotes failures
	Deferred        int      `json:"deferred"`         // candidates left for later passes
	Errors          []string `json:"errors,omitempty"` // bounded
	NextAttemptAt   *time.Time
	HiveUnavailable string `json:"hive_unavailable,omitempty"`
}

func (r *BridgeReport) addErr(err error) {
	if len(r.Errors) < 10 {
		r.Errors = append(r.Errors, err.Error())
	}
}

// Notify requests a prompt pass (e.g. on a hub note.created event). It never
// blocks.
func (b *Bridge) Notify() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

// Pending is the number of hub notes the bridge knows are not yet confirmed
// indexed (ledgered writes plus notes seen pending by the last scans), for
// MemoryStatus.PendingItems.
func (b *Bridge) Pending() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(b.led.Notes)
	for id := range b.unsynced {
		if _, ok := b.led.Notes[id]; !ok {
			n++
		}
	}
	return n
}

// observe records whether a scanned hub note still needs Hive indexing.
func (b *Bridge) observe(n contracts.Note) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n.SyncState == SyncIndexed && n.HiveRef != "" {
		delete(b.unsynced, n.NoteID)
		return
	}
	if len(b.unsynced) < b.cfg.MaxLedger {
		b.unsynced[n.NoteID] = true
	}
}

// countPending pages hub notes from the start (bounded, without moving the
// scan cursor) to refresh the pending set while Hive cannot be written.
func (b *Bridge) countPending(ctx context.Context) {
	cursor := ""
	for page := 0; page < b.cfg.MaxScanPages; page++ {
		pg, err := b.cfg.Store.ListNotes(ctx, contracts.ListNotesRequest{PageRequest: contracts.PageRequest{Cursor: cursor, Limit: b.cfg.PageSize}})
		if err != nil {
			return
		}
		for _, n := range pg.Items {
			b.observe(n)
		}
		if cursor = pg.NextCursor; cursor == "" {
			return
		}
	}
}

// Run loops until ctx is cancelled: a pass every Interval or on Notify,
// never two passes closer than MinPassGap.
func (b *Bridge) Run(ctx context.Context) error {
	ctx = Background(ctx)
	t := time.NewTicker(b.cfg.Interval)
	defer t.Stop()
	for {
		started := time.Now()
		if _, err := b.SyncOnce(ctx); err != nil && ctx.Err() == nil {
			b.cfg.Logf("hive bridge: %v", err)
		}
		b.mu.Lock()
		b.passes++
		b.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		case <-b.wake:
		}
		if wait := b.cfg.MinPassGap - time.Since(started); wait > 0 {
			gap := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				gap.Stop()
				return ctx.Err()
			case <-gap.C:
			}
		}
	}
}

// Passes is the number of Run passes so far (status and tests).
func (b *Bridge) Passes() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.passes
}

// SyncOnce performs one bounded pass.
func (b *Bridge) SyncOnce(ctx context.Context) (BridgeReport, error) {
	b.runMu.Lock()
	defer b.runMu.Unlock()
	var rep BridgeReport
	now := b.cfg.Clock.Now()

	b.mu.Lock()
	if now.Before(b.nextAt) {
		next := b.nextAt
		b.mu.Unlock()
		rep.Skipped, rep.NextAttemptAt = "backoff", &next
		return rep, nil
	}
	b.mu.Unlock()

	if ok, detail := b.cfg.Hive.Available(ctx); !ok {
		rep.Skipped, rep.HiveUnavailable = "hive_unavailable", detail
		// Hive installed but not writable (degraded): keep the pending
		// count honest. A node without Hive never touches the hub here.
		if p, ok := b.cfg.Hive.(interface{ Present(context.Context) bool }); ok && p.Present(ctx) {
			b.countPending(ctx)
		}
		next := b.backoff(now)
		rep.NextAttemptAt = &next
		return rep, nil // notes stay pending; this is not an error for callers
	}

	cands, err := b.scan(ctx, now, &rep)
	if err != nil {
		rep.HubErrors++
		rep.addErr(err)
		return rep, err
	}
	rep.Candidates = len(cands)
	for i, n := range cands {
		if i >= b.cfg.BatchSize {
			rep.Deferred = len(cands) - i
			break
		}
		if ctx.Err() != nil {
			rep.Deferred = len(cands) - i
			break
		}
		if transient := b.publishOne(ctx, n, &rep); transient {
			// Hive itself is failing: stop the batch and back off globally.
			rep.Deferred = len(cands) - i - 1
			next := b.backoff(b.cfg.Clock.Now())
			rep.NextAttemptAt = &next
			break
		}
	}
	if rep.Failed == 0 && rep.NextAttemptAt == nil {
		b.mu.Lock()
		b.delay = 0
		b.mu.Unlock()
	}
	return rep, b.save()
}

func (b *Bridge) backoff(now time.Time) time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.delay == 0 {
		b.delay = b.cfg.InitialBackoff
	} else {
		b.delay *= 2
		if b.delay > b.cfg.MaxBackoff {
			b.delay = b.cfg.MaxBackoff
		}
	}
	b.nextAt = now.Add(b.delay)
	return b.nextAt
}

// scan pages through hub notes from the persisted cursor and returns notes
// that still need Hive indexing (plus ledgered notes whose hub record is
// unconfirmed). ListNotes has no sync-state filter in v1, so the scan is
// bounded by MaxScanPages per pass and resumes where it stopped.
func (b *Bridge) scan(ctx context.Context, now time.Time, rep *BridgeReport) ([]contracts.Note, error) {
	b.mu.Lock()
	cursor := b.led.Cursor
	b.mu.Unlock()
	var out []contracts.Note
	seen := map[string]bool{}
	for page := 0; page < b.cfg.MaxScanPages; page++ {
		pg, err := b.cfg.Store.ListNotes(ctx, contracts.ListNotesRequest{PageRequest: contracts.PageRequest{Cursor: cursor, Limit: b.cfg.PageSize}})
		if err != nil {
			return out, fmt.Errorf("list notes: %w", err)
		}
		for _, n := range pg.Items {
			rep.Scanned++
			if seen[n.NoteID] {
				continue
			}
			seen[n.NoteID] = true
			b.observe(n)
			if b.needsWork(n, now) {
				out = append(out, n)
			}
		}
		cursor = pg.NextCursor
		if cursor == "" || len(out) >= b.cfg.BatchSize {
			break
		}
	}
	b.mu.Lock()
	b.led.Cursor = cursor
	b.mu.Unlock()
	return out, nil
}

func (b *Bridge) needsWork(n contracts.Note, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	le := b.led.Notes[n.NoteID]
	if n.SyncState == SyncIndexed && n.HiveRef != "" {
		if le != nil {
			delete(b.led.Notes, n.NoteID) // hub already confirmed
		}
		return false
	}
	if le != nil && now.Before(le.NextTry) {
		return false
	}
	return true
}

// publishOne returns true when the failure looks like Hive itself is down
// (so the pass should stop and back off), false otherwise.
func (b *Bridge) publishOne(ctx context.Context, n contracts.Note, rep *BridgeReport) (transient bool) {
	le := b.entry(n.NoteID)
	ref := le.Ref
	if ref == "" {
		found := false
		var err error
		ref, found, err = b.cfg.Hive.FindNote(ctx, n.NoteID)
		if err != nil {
			b.fail(ctx, n, le, err, rep)
			return true
		}
		switch {
		case found:
			rep.AlreadyInHive++
		case le.Written:
			// Written earlier but not yet visible by marker: never rewrite.
			b.fail(ctx, n, le, ErrWrittenUnverified, rep)
			return false
		default:
			// Event payloads (note.created) omit Body. Never publish a note
			// without its body: fetch the full record when one looks partial.
			if strings.TrimSpace(n.Body) == "" {
				if g, ok := b.cfg.Store.(noteGetter); ok {
					full, gerr := g.GetNote(ctx, contracts.GetRequest{ID: n.NoteID})
					if gerr != nil {
						b.fail(ctx, n, le, fmt.Errorf("fetch full note: %w", gerr), rep)
						return false
					}
					n = full
				}
			}
			ref, err = b.cfg.Hive.PublishNote(ctx, n)
			if errors.Is(err, ErrWrittenUnverified) {
				b.mu.Lock()
				le.Written = true
				b.mu.Unlock()
				_ = b.save()
				b.fail(ctx, n, le, err, rep)
				return false
			}
			if err != nil {
				b.fail(ctx, n, le, err, rep)
				return isTransient(err)
			}
			rep.Published++
		}
		b.mu.Lock()
		le.Ref, le.Written, le.LastError, le.Updated = ref, true, "", b.cfg.Clock.Now()
		b.mu.Unlock()
		if err := b.save(); err != nil {
			rep.addErr(err)
		}
	}
	if _, err := b.cfg.Store.UpdateNoteSync(ctx, contracts.UpdateNoteSyncRequest{
		Mutation:  contracts.Mutation{OperationID: syncOpID(n.NoteID, SyncIndexed, ref)},
		NoteID:    n.NoteID,
		HiveRef:   ref,
		SyncState: SyncIndexed,
	}); err != nil {
		rep.HubErrors++
		rep.addErr(fmt.Errorf("record %s indexed: %w", n.NoteID, err))
		return false // ledger keeps the ref; next pass only retries the hub update
	}
	rep.Recorded++
	b.mu.Lock()
	delete(b.led.Notes, n.NoteID)
	delete(b.unsynced, n.NoteID)
	b.mu.Unlock()
	return false
}

func (b *Bridge) entry(id string) *ledgerEntry {
	b.mu.Lock()
	defer b.mu.Unlock()
	le := b.led.Notes[id]
	if le == nil {
		le = &ledgerEntry{Updated: b.cfg.Clock.Now()}
		b.led.Notes[id] = le
		b.evictLocked()
	}
	return le
}

// evictLocked bounds the ledger, dropping the oldest entries that hold no
// completed write (a written entry is what prevents duplicates; it is only
// dropped once the hub confirms or as a last resort).
func (b *Bridge) evictLocked() {
	if len(b.led.Notes) <= b.cfg.MaxLedger {
		return
	}
	type kv struct {
		id string
		e  *ledgerEntry
	}
	all := make([]kv, 0, len(b.led.Notes))
	for id, e := range b.led.Notes {
		all = append(all, kv{id, e})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].e.Written != all[j].e.Written {
			return !all[i].e.Written
		}
		return all[i].e.Updated.Before(all[j].e.Updated)
	})
	for _, x := range all[:len(all)-b.cfg.MaxLedger] {
		delete(b.led.Notes, x.id)
	}
}

func (b *Bridge) fail(ctx context.Context, n contracts.Note, le *ledgerEntry, err error, rep *BridgeReport) {
	rep.Failed++
	rep.addErr(fmt.Errorf("%s: %w", n.NoteID, err))
	now := b.cfg.Clock.Now()
	b.mu.Lock()
	le.Attempts++
	le.LastError = truncBytes(err.Error(), 300)
	le.Updated = now
	d := b.cfg.InitialBackoff << min(le.Attempts-1, 16)
	if d <= 0 || d > b.cfg.MaxBackoff {
		d = b.cfg.MaxBackoff
	}
	le.NextTry = now.Add(d)
	attempts := le.Attempts
	b.mu.Unlock()
	// Tell the hub once the note has failed repeatedly; until then it stays
	// "pending" (the default), which is the honest state during a blip.
	if attempts == b.cfg.MaxAttempts && n.SyncState != SyncFailed {
		if _, uerr := b.cfg.Store.UpdateNoteSync(ctx, contracts.UpdateNoteSyncRequest{
			Mutation:  contracts.Mutation{OperationID: syncOpID(n.NoteID, SyncFailed, "")},
			NoteID:    n.NoteID,
			SyncState: SyncFailed,
		}); uerr != nil {
			rep.HubErrors++
			rep.addErr(fmt.Errorf("record %s failed: %w", n.NoteID, uerr))
		}
	}
}

func isTransient(err error) bool {
	if errors.Is(err, ErrTimeout) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if contracts.IsCode(err, contracts.ErrUnavailable) {
		return true
	}
	return strings.Contains(err.Error(), "hive CLI not found")
}

// syncOpID is deterministic so a retried hub update is an idempotent replay;
// the ref hash keeps a different ref from colliding with an earlier attempt.
func syncOpID(noteID, state, ref string) string {
	sum := sha256.Sum256([]byte(ref))
	return "op_hivesync-" + strings.TrimPrefix(noteID, contracts.PrefixNote+"_") + "-" + state + "-" + hex.EncodeToString(sum[:4])
}

func (b *Bridge) save() error {
	if b.cfg.StateFile == "" {
		return nil
	}
	b.mu.Lock()
	data, err := json.MarshalIndent(b.led, "", " ")
	b.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(b.cfg.StateFile), 0o700); err != nil {
		return err
	}
	tmp := b.cfg.StateFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, b.cfg.StateFile)
}
