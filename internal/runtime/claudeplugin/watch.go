package claudeplugin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/jhh3/pyre/internal/contracts"
)

// Message is one inbox entry the agent should be told about. It never
// carries the body.
type Message struct {
	ID     string
	Sender string
	Cursor int64
}

// Source is where the watcher learns about mail.
type Source interface {
	// Poll returns messages that need the agent after cursor (long-polling
	// up to wait) and the new cursor.
	Poll(ctx context.Context, after int64, wait time.Duration) ([]Message, int64, error)
	// Pending returns the subset of ids that still need the agent
	// (unacknowledged and not delivered some other way).
	Pending(ctx context.Context, ids []string) ([]string, error)
}

// WatchOptions configures Watch.
type WatchOptions struct {
	Out io.Writer // stdout: every line is one notification to Claude
	Log io.Writer // diagnostics (stderr); never notifications
	// StateFile persists the cursor and notified ids so a restarted watcher
	// does not ring again for the same mail ("" keeps state in memory).
	StateFile string
	// RunDir is the Claude adapter's run state dir: messages with a paste
	// attempt in its deliveries.jsonl are left to that path.
	RunDir string
	// MinInterval is the minimum gap between notification lines; mail
	// arriving meanwhile is coalesced into the next line (default 15s).
	MinInterval time.Duration
	// Grace delays a notification after a message is first seen, letting
	// the paste path (idle, unattached) claim it first (default 3s).
	Grace time.Duration
	// Remind re-notifies mail that is still unread this long after its
	// notification (default 10m; negative disables).
	Remind time.Duration
	// PollWait is the long-poll duration (default 50s).
	PollWait time.Duration
	// Alive, if set, is checked every AliveEvery (default 10s); when it
	// returns false the watcher exits (its Claude session is gone). An idle
	// watcher writes nothing to stdout, so a closed pipe alone would not
	// end it.
	Alive      func() bool
	AliveEvery time.Duration
	Now        func() time.Time
}

func (o *WatchOptions) defaults() {
	if o.Out == nil {
		o.Out = os.Stdout
	}
	if o.Log == nil {
		o.Log = io.Discard
	}
	if o.MinInterval <= 0 {
		o.MinInterval = 15 * time.Second
	}
	if o.Grace <= 0 {
		o.Grace = 3 * time.Second
	}
	if o.Remind == 0 {
		o.Remind = 10 * time.Minute
	}
	if o.PollWait <= 0 {
		o.PollWait = 50 * time.Second
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.AliveEvery <= 0 {
		o.AliveEvery = 10 * time.Second
	}
}

// ErrSessionGone is returned by Watch when WatchOptions.Alive reports that
// the Claude session that started the watcher has exited.
var ErrSessionGone = errors.New("inbox-watch: session process gone")

type watchState struct {
	Cursor   int64            `json:"cursor"`
	Notified map[string]int64 `json:"notified"` // message id -> unix time of last notification
}

type pendingMsg struct {
	msg       Message
	firstSeen time.Time
}

type watcher struct {
	o        WatchOptions
	src      Source
	mu       sync.Mutex
	saveMu   sync.Mutex // serializes state-file writes
	st       watchState
	pending  map[string]*pendingMsg
	lastLine time.Time
}

// Watch runs until ctx ends or stdout breaks.
func Watch(ctx context.Context, src Source, o WatchOptions) error {
	o.defaults()
	w := &watcher{o: o, src: src, pending: map[string]*pendingMsg{}}
	w.load()
	results := make(chan []Message, 4)
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); w.pollLoop(ctx, results) }()
	defer wg.Wait()
	defer cancel()
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	lastAlive := time.Now()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msgs := <-results:
			now := w.o.Now()
			w.mu.Lock()
			for _, m := range msgs {
				if _, seen := w.pending[m.ID]; seen {
					continue
				}
				if t, done := w.st.Notified[m.ID]; done && (w.o.Remind < 0 || now.Sub(time.Unix(t, 0)) < w.o.Remind) {
					continue
				}
				w.pending[m.ID] = &pendingMsg{msg: m, firstSeen: now}
			}
			w.mu.Unlock()
		case <-tick.C:
			if w.o.Alive != nil && time.Since(lastAlive) >= w.o.AliveEvery {
				lastAlive = time.Now()
				if !w.o.Alive() {
					fmt.Fprintln(w.o.Log, "inbox-watch: session process gone; exiting")
					return ErrSessionGone
				}
			}
		}
		if err := w.maybeRing(ctx); err != nil {
			return err
		}
	}
}

func (w *watcher) pollLoop(ctx context.Context, out chan<- []Message) {
	backoff := time.Second
	for ctx.Err() == nil {
		w.mu.Lock()
		cur := w.st.Cursor
		w.mu.Unlock()
		msgs, next, err := w.src.Poll(ctx, cur, w.o.PollWait)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			fmt.Fprintf(w.o.Log, "inbox-watch: poll failed (retry in %s): %v\n", backoff, err)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		w.mu.Lock()
		if next > w.st.Cursor {
			w.st.Cursor = next
		}
		w.mu.Unlock()
		w.save()
		if len(msgs) > 0 {
			select {
			case out <- msgs:
			case <-ctx.Done():
				return
			}
		}
		// Remind: re-offer notified-but-unread mail periodically.
		if w.o.Remind > 0 {
			if again := w.dueReminders(); len(again) > 0 {
				select {
				case out <- again:
				case <-ctx.Done():
					return
				}
			}
		}
	}
}

func (w *watcher) dueReminders() []Message {
	now := w.o.Now()
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []Message
	for id, t := range w.st.Notified {
		if now.Sub(time.Unix(t, 0)) >= w.o.Remind {
			out = append(out, Message{ID: id})
		}
	}
	return out
}

func (w *watcher) maybeRing(ctx context.Context) error {
	now := w.o.Now()
	w.mu.Lock()
	if !w.lastLine.IsZero() && now.Sub(w.lastLine) < w.o.MinInterval {
		w.mu.Unlock()
		return nil
	}
	var ready []Message
	for _, p := range w.pending {
		if now.Sub(p.firstSeen) >= w.o.Grace {
			ready = append(ready, p.msg)
		}
	}
	w.mu.Unlock()
	if len(ready) == 0 {
		return nil
	}
	// Leave messages to the paste path when it already claimed them.
	attempted := pasteAttempts(w.o.RunDir)
	var ids []string
	for _, m := range ready {
		if !attempted[m.ID] {
			ids = append(ids, m.ID)
		}
	}
	still := map[string]bool{}
	if len(ids) > 0 {
		pend, err := w.src.Pending(ctx, ids)
		if err != nil {
			fmt.Fprintf(w.o.Log, "inbox-watch: status check failed: %v\n", err)
			return nil // retry next tick
		}
		for _, id := range pend {
			still[id] = true
		}
	}
	var ring []Message
	w.mu.Lock()
	for _, m := range ready {
		delete(w.pending, m.ID)
		if still[m.ID] {
			ring = append(ring, m)
		} else {
			delete(w.st.Notified, m.ID) // read/acked or handled elsewhere: stop reminding
		}
	}
	w.mu.Unlock()
	if len(ring) == 0 {
		return nil
	}
	sort.Slice(ring, func(i, j int) bool { return ring[i].Cursor < ring[j].Cursor })
	line := FormatLine(ring)
	if _, err := io.WriteString(w.o.Out, line+"\n"); err != nil {
		return fmt.Errorf("stdout closed: %w", err)
	}
	if f, ok := w.o.Out.(interface{ Sync() error }); ok {
		_ = f.Sync()
	}
	w.mu.Lock()
	w.lastLine = now
	if w.st.Notified == nil {
		w.st.Notified = map[string]int64{}
	}
	for _, m := range ring {
		w.st.Notified[m.ID] = now.Unix()
	}
	w.mu.Unlock()
	w.save()
	fmt.Fprintf(w.o.Log, "inbox-watch: notified %d message(s)\n", len(ring))
	return nil
}

// FormatLine renders one notification: count, ids and senders, and the
// instruction. Never the body.
func FormatLine(ms []Message) string {
	var parts []string
	for i, m := range ms {
		if i == 5 {
			parts = append(parts, fmt.Sprintf("and %d more", len(ms)-5))
			break
		}
		p := clean(m.ID, 80)
		if s := clean(m.Sender, 40); s != "" {
			p += " from " + s
		}
		parts = append(parts, p)
	}
	noun, it, each := "message", "it", "it"
	if len(ms) != 1 {
		noun, it, each = "messages", "them", "each"
	}
	// Name the MCP tools exactly: they are pre-approved for the run, while
	// the equivalent `pyre inbox` shell command opens a permission prompt
	// (and leaves no acceptance evidence). Observed on Opus 5.5 with a
	// vaguer "pyre inbox tool" wording.
	// The reply tool is named for the same reason: `pyre send` in Bash
	// opens a permission dialog that may sit unanswered.
	return fmt.Sprintf("Pyre: %d new %s for this session (%s). Read %s with the MCP tool mcp__pyre__inbox (unacked: true; pre-approved, not the pyre shell command), then reply with mcp__pyre__send_message (reply_to: the message id) or call mcp__pyre__ack_message for %s.",
		len(ms), noun, strings.Join(parts, ", "), it, each)
}

func clean(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		s = s[:n]
	}
	return s
}

// pasteAttempts reads the Claude adapter journal: ids with an attempting,
// submitted or accepted record belong to the paste path.
func pasteAttempts(runDir string) map[string]bool {
	out := map[string]bool{}
	if runDir == "" {
		return out
	}
	f, err := os.Open(filepath.Join(runDir, "deliveries.jsonl"))
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var r struct {
			MessageID string `json:"message_id"`
			State     string `json:"state"`
		}
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			continue
		}
		switch r.State {
		case "attempting", "submitted", "accepted", "indeterminate":
			out[r.MessageID] = true
		case "rejected", "cleared":
			delete(out, r.MessageID)
		}
	}
	return out
}

func (w *watcher) load() {
	w.st = watchState{Notified: map[string]int64{}}
	if w.o.StateFile == "" {
		return
	}
	b, err := os.ReadFile(w.o.StateFile)
	if err != nil {
		return
	}
	_ = json.Unmarshal(b, &w.st)
	if w.st.Notified == nil {
		w.st.Notified = map[string]int64{}
	}
}

func (w *watcher) save() {
	if w.o.StateFile == "" {
		return
	}
	w.saveMu.Lock()
	defer w.saveMu.Unlock()
	w.mu.Lock()
	// Bound the notified set.
	if len(w.st.Notified) > 1000 {
		type kv struct {
			id string
			t  int64
		}
		var all []kv
		for id, t := range w.st.Notified {
			all = append(all, kv{id, t})
		}
		sort.Slice(all, func(i, j int) bool { return all[i].t > all[j].t })
		w.st.Notified = map[string]int64{}
		for _, e := range all[:1000] {
			w.st.Notified[e.id] = e.t
		}
	}
	b, _ := json.Marshal(w.st)
	w.mu.Unlock()
	tmp := w.o.StateFile + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, w.o.StateFile)
	}
}

// APISource reads a session's inbox through the Pyre API (local pyred
// socket or hub) with the session's own credential.
type APISource struct {
	API     contracts.API
	Session string
}

var _ Source = APISource{}

// skipStates are messages the runtime path already handled or that need
// reconciliation, never a doorbell.
var skipStates = map[contracts.RuntimeDeliveryState]bool{
	contracts.DeliveryAttempting: true, contracts.DeliveryAccepted: true, contracts.DeliveryIndeterminate: true,
	contracts.DeliveryExpired: true, contracts.DeliveryCancelled: true,
}

func needsAgent(v contracts.MessageView) bool {
	return v.Envelope.Mode != contracts.ModeInfo && v.Status.Ack == contracts.AckNone && !skipStates[v.Status.Runtime]
}

func (s APISource) Poll(ctx context.Context, after int64, wait time.Duration) ([]Message, int64, error) {
	pg, err := s.API.ListMessages(ctx, contracts.ListMessagesRequest{
		PageRequest: contracts.PageRequest{Limit: 200},
		Box:         "inbox", Address: &contracts.Address{Kind: contracts.AddrSession, ID: s.Session},
		Unacked: true, AfterCursor: after, WaitSeconds: int(wait / time.Second),
	})
	if err != nil {
		return nil, after, err
	}
	next := after
	var out []Message
	for _, v := range pg.Items {
		if v.Cursor > next {
			next = v.Cursor
		}
		if !needsAgent(v) {
			continue
		}
		sender := v.Envelope.Sender.Name
		if sender == "" {
			sender = v.Envelope.Sender.ID
		}
		out = append(out, Message{ID: v.Envelope.MessageID, Sender: sender, Cursor: v.Cursor})
	}
	return out, next, nil
}

func (s APISource) Pending(ctx context.Context, ids []string) ([]string, error) {
	var out []string
	for _, id := range ids {
		v, err := s.API.GetMessage(ctx, contracts.GetRequest{ID: id})
		if err != nil {
			if contracts.IsCode(err, contracts.ErrNotFound) {
				continue
			}
			return nil, err
		}
		if needsAgent(v) {
			out = append(out, id)
		}
	}
	return out, nil
}
