package hive

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// A20 throttling: Pyre's own background limits are shrunk (one background
// slot, a capture queue of 8) and a sleeping in-process Hive stands in for
// the real one, so the test costs milliseconds of CPU and kilobytes of
// memory. It never loads the machine.

// slowHive is an in-process Runner: every background subprocess sleeps
// bgDelay; interactive searches answer after 1 ms. It records how many
// background subprocesses ran at once and whether each interactive call
// overlapped background work.
type slowHive struct {
	bgDelay atomic.Int64 // nanoseconds

	mu          sync.Mutex
	entries     []entry
	bgRunning   int
	bgMax       int
	captures    int
	notes       int
	interactive int
	overlapped  int // interactive calls made while background work ran
}

func (h *slowHive) Run(ctx context.Context, c Cmd) (Result, error) {
	if len(c.Args) >= 2 && c.Args[0] == "mem" && c.Args[1] == "version" {
		return Result{Stdout: []byte("hive-mem 0.2.3 (synthetic)\n")}, nil
	}
	if c.Background {
		h.mu.Lock()
		h.bgRunning++
		h.bgMax = max(h.bgMax, h.bgRunning)
		h.mu.Unlock()
		defer func() {
			h.mu.Lock()
			h.bgRunning--
			h.mu.Unlock()
		}()
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-time.After(time.Duration(h.bgDelay.Load())):
		}
	} else {
		h.mu.Lock()
		h.interactive++
		if h.bgRunning > 0 {
			h.overlapped++
		}
		h.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case len(c.Args) >= 2 && c.Args[0] == "mem" && c.Args[1] == "capture":
		h.captures++
		return Result{}, nil
	case len(c.Args) >= 1 && c.Args[0] == "note":
		text := c.Args[len(c.Args)-1]
		title, _, _ := strings.Cut(text, "\n")
		h.notes++
		h.entries = append(h.entries, entry{ID: int64(len(h.entries) + 1), UID: fmt.Sprintf("note:synth%04d", len(h.entries)+1),
			TS: time.Now().Unix(), Kind: "note", Title: title})
		return Result{}, nil
	case len(c.Args) >= 1 && c.Args[0] == "search":
		q := c.Args[len(c.Args)-1]
		var out []byte
		for _, e := range h.entries {
			if strings.Contains(e.Title, q) || q == "synthetic" {
				b, _ := json.Marshal(e)
				out = append(append(out, b...), '\n')
			}
		}
		if len(out) == 0 {
			return Result{ExitCode: 1, Stderr: []byte("no results\n")}, nil
		}
		return Result{Stdout: out}, nil
	}
	return Result{ExitCode: 1}, nil
}

func (h *slowHive) snapshot() (bgMax, captures, notes, interactive, overlapped int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.bgMax, h.captures, h.notes, h.interactive, h.overlapped
}

func p95(d []time.Duration) time.Duration {
	if len(d) == 0 {
		return 0
	}
	s := slices.Clone(d)
	slices.Sort(s)
	return s[(len(s)*95+99)/100-1]
}

// Background capture and the note bridge share one slot, run as lowered-
// priority background work, keep their queues within their caps under a
// burst, and never delay interactive memory search; every hub note is
// eventually indexed.
func TestBackgroundHiveWorkIsThrottledAndInteractiveSearchIsNot(t *testing.T) {
	f := newFakeHome(t, true, true)
	hv := &slowHive{}
	hv.bgDelay.Store(int64(100 * time.Millisecond))
	env := f.env()
	env.Runner = hv
	c := New(env, Options{StatusTTL: time.Hour, StagingRoot: filepath.Join(f.Home, "staging"), BackgroundSlots: 1})

	const queueCap, nCaptures, nNotes = 8, 200, 300
	k := NewCapturerWith(c, CapturerOptions{MaxQueue: queueCap, MinGap: 5 * time.Millisecond, Logf: func(string, ...any) {}})
	store := &memNotes{}
	for i := 1; i <= nNotes; i++ {
		store.notes = append(store.notes, contracts.Note{NoteID: fmt.Sprintf("note_SYNTHT%03d", i), Kind: contracts.NoteNote,
			Title: fmt.Sprintf("Synthetic throttle note %03d", i), Body: "synthetic body", CreatedAt: time.Now(), SyncState: SyncPending})
	}
	b, err := NewBridge(BridgeConfig{Store: store, Hive: c, BatchSize: 20, PageSize: 20, Interval: 20 * time.Millisecond,
		MinPassGap: 30 * time.Millisecond, InitialBackoff: 10 * time.Millisecond, MaxBackoff: 50 * time.Millisecond})
	must(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); k.Run(ctx) }()
	go func() { defer wg.Done(); _ = b.Run(ctx) }()

	// Burst: 200 distinct transcripts ending at once, 1000 note.created
	// notifications.
	codexHome := t.TempDir()
	var done atomic.Int64
	for i := 0; i < nCaptures; i++ {
		th := fmt.Sprintf("019a0000-aaaa-7bbb-8ccc-%012d", i)
		p := writeRollout(t, codexHome, "2026/10/05", th, `{"type":"session_meta","payload":{"id":"`+th+`"}}`+"\n")
		k.Enqueue(CaptureRequest{Transcript: p, SessionID: th, Reason: "pyre-run-end", Done: func() { done.Add(1) }})
	}
	burstStart := time.Now()
	for i := 0; i < 1000; i++ {
		b.Notify()
	}

	// Interactive searches during the background burst.
	var lat []time.Duration
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		s := time.Now()
		res, err := c.Search(context.Background(), contracts.MemorySearchParams{Query: "synthetic", Limit: 5})
		lat = append(lat, time.Since(s))
		if err != nil {
			t.Fatalf("interactive search: %v", err)
		}
		_ = res
		time.Sleep(5 * time.Millisecond)
	}
	elapsed := time.Since(burstStart)
	bgMax, captures, notes, interactive, overlapped := hv.snapshot()
	st := k.Stats()
	t.Logf("interactive searches=%d p95=%s max=%s overlapped-background=%d; background max concurrency=%d captures=%d notes=%d; capture queue max=%d dropped=%d; in %s",
		len(lat), p95(lat), slices.Max(lat), overlapped, bgMax, captures, notes, st.MaxPending, st.Dropped, elapsed.Round(time.Millisecond))

	if got := p95(lat); got > 50*time.Millisecond {
		t.Errorf("interactive search p95 %s exceeds the 50 ms budget while background work runs", got)
	}
	if overlapped < interactive/2 {
		t.Errorf("only %d of %d interactive calls overlapped background work; the test did not exercise contention", overlapped, interactive)
	}
	if bgMax != 1 {
		t.Errorf("background Hive subprocesses ran %d at once; the limit is 1 slot", bgMax)
	}
	if st.MaxPending > queueCap {
		t.Errorf("capture queue reached %d, cap %d", st.MaxPending, queueCap)
	}
	if st.Dropped == 0 {
		t.Errorf("a burst of %d captures into a queue of %d dropped nothing", nCaptures, queueCap)
	}
	if bs := c.BackgroundStats(); bs.Slots != 1 || bs.MaxRunning != 1 {
		t.Errorf("background stats %+v", bs)
	}

	// Durable data is never dropped: with Hive fast again, every hub note
	// ends up indexed exactly once; every capture hold was released.
	hv.bgDelay.Store(int64(time.Millisecond))
	waitUntil(t, 20*time.Second, "all notes indexed", func() bool {
		for i := 1; i <= nNotes; i++ {
			if n := store.get(fmt.Sprintf("note_SYNTHT%03d", i)); n.SyncState != SyncIndexed || n.HiveRef == "" {
				return false
			}
		}
		return true
	})
	waitUntil(t, 10*time.Second, "capture queue drained", func() bool { return k.Pending() == 0 && done.Load() == nCaptures })
	if _, _, notes, _, _ := hv.snapshot(); notes != nNotes {
		t.Errorf("hive note written %d times for %d hub notes", notes, nNotes)
	}

	// Rate limit: a storm of note.created notifications (one every ~0.5 ms
	// for 600 ms) coalesces into passes at least MinPassGap (30 ms) apart.
	before, start := b.Passes(), time.Now()
	for time.Since(start) < 600*time.Millisecond {
		b.Notify()
		time.Sleep(500 * time.Microsecond)
	}
	window := time.Since(start)
	ran := b.Passes() - before
	t.Logf("notification storm: %d bridge passes in %s", ran, window.Round(time.Millisecond))
	if maxPasses := int(window/(30*time.Millisecond)) + 2; ran > maxPasses || ran < 2 {
		t.Errorf("bridge ran %d passes in %s; MinPassGap allows 2..%d", ran, window, maxPasses)
	}
	cancel()
	wg.Wait()
}

func waitUntil(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	end := time.Now().Add(d)
	for !cond() {
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A background context waits for a slot (or its cancellation); an
// interactive one never does.
func TestBackgroundSlotWaitsAndCancels(t *testing.T) {
	f := newFakeHome(t, true, true)
	hv := &slowHive{}
	hv.bgDelay.Store(int64(300 * time.Millisecond))
	env := f.env()
	env.Runner = hv
	c := New(env, Options{StatusTTL: time.Hour, StagingRoot: filepath.Join(f.Home, "staging")})
	if c.BackgroundStats().Slots != 1 {
		t.Fatalf("default slots %d", c.BackgroundStats().Slots)
	}
	bg := Background(context.Background())
	go func() { _, _, _ = c.FindNote(bg, "note_SYNTHHOLD") }()
	waitUntil(t, 5*time.Second, "slot taken", func() bool { return c.BackgroundStats().Running == 1 })
	wctx, cancel := context.WithTimeout(bg, 50*time.Millisecond)
	defer cancel()
	if _, _, err := c.FindNote(wctx, "note_SYNTHWAIT"); err == nil {
		t.Fatal("second background call did not wait for the busy slot")
	}
	s := time.Now()
	if _, _, err := c.FindNote(context.Background(), "note_SYNTHFG"); err != nil || time.Since(s) > 100*time.Millisecond {
		t.Fatalf("interactive call waited for the background slot: %v after %s", err, time.Since(s))
	}
}
