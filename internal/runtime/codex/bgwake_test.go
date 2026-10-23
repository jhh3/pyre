package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// automations records OnAutomation calls.
type automations struct {
	mu  sync.Mutex
	got []contracts.NodeAutomation
}

func (r *automations) add(a contracts.NodeAutomation) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, a)
}

func (r *automations) wakes() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, a := range r.got {
		if a.Action == contracts.AutomationBackgroundWake {
			n++
		}
	}
	return n
}

func (r *automations) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return fmt.Sprintf("%+v", r.got)
}

// waitWakes waits until n wake automation reports arrived (they are sent
// asynchronously after the turn is visible).
func (r *automations) waitWakes(t *testing.T, n int) {
	t.Helper()
	end := time.Now().Add(3 * time.Second)
	for r.wakes() < n && time.Now().Before(end) {
		time.Sleep(20 * time.Millisecond)
	}
	if r.wakes() != n {
		t.Fatalf("want %d wake automation report(s): %s", n, r)
	}
}

func newWakeEnv(t *testing.T, mutate func(*Options)) (*env, *automations) {
	rec := &automations{}
	e := newEnv(t, func(o *Options) {
		o.OnAutomation = rec.add
		if mutate != nil {
			mutate(o)
		}
	})
	e.a.wakeSettle = 150 * time.Millisecond
	return e, rec
}

// wakeEntries are the run's wake notices in transcript order, plus the
// index of each in all entries.
func wakeEntries(t *testing.T, a *Adapter, h contracts.RunHandle) (notices []contracts.TranscriptEntry, all []contracts.TranscriptEntry) {
	t.Helper()
	resp, err := a.ReadTranscript(context.Background(), h, contracts.ReadSessionRequest{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	for _, en := range resp.Entries {
		if en.Kind == "system" && strings.HasPrefix(en.Text, "[pyre] ") {
			notices = append(notices, en)
		}
	}
	return notices, resp.Entries
}

func waitNotices(t *testing.T, a *Adapter, h contracts.RunHandle, n int, d time.Duration) []contracts.TranscriptEntry {
	t.Helper()
	end := time.Now().Add(d)
	var got []contracts.TranscriptEntry
	for time.Now().Before(end) {
		if got, _ = wakeEntries(t, a, h); len(got) >= n {
			return got
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("wanted %d wake notice(s), got %d", n, len(got))
	return nil
}

// A background command that finishes after its turn ended starts exactly
// one new turn with its command, exit code, cwd and output.
func TestCodexWakeAfterTurnEnds(t *testing.T) {
	t.Parallel()
	e, rec := newWakeEnv(t, nil)
	h := e.launch(e.a, "run_w1", e.home, nil, nil)
	e.wait(e.a, h, 5*time.Second, idle)
	if !contracts.HasCap(e.a.Capabilities(), contracts.CapWakeBackground) {
		t.Fatal("capability not advertised")
	}
	if r := dlv(t, e.a, h, "msg_w1", "start it FAKE_BG_MS=400:a", contracts.ModeQueue); r.State != contracts.DeliveryAccepted {
		t.Fatalf("%+v", r)
	}
	e.wait(e.a, h, 5*time.Second, idle)
	if n, _ := wakeEntries(t, e.a, h); len(n) != 0 {
		t.Fatalf("woken before the command finished: %+v", n)
	}
	got := waitNotices(t, e.a, h, 1, 5*time.Second)
	txt := got[0].Text
	for _, want := range []string{"[pyre] background command finished: `bg-a` (exit 0, cwd /fake/work", "Last output:", "bg a done"} {
		if !strings.Contains(txt, want) {
			t.Errorf("notice lacks %q:\n%s", want, txt)
		}
	}
	if strings.Contains(txt, "zsh -lc") {
		t.Errorf("notice shows the shell wrapper:\n%s", txt)
	}
	e.wait(e.a, h, 5*time.Second, idle)
	time.Sleep(600 * time.Millisecond)
	if n, _ := wakeEntries(t, e.a, h); len(n) != 1 {
		t.Fatalf("want exactly one wake turn, got %d", len(n))
	}
	rec.waitWakes(t, 1)
	// A wake is no Pyre message: it yields no delivery receipt.
	if obs, _ := e.a.Observe(context.Background(), h, ""); len(obs.Receipts) != 1 || obs.Receipts[0].MessageID != "msg_w1" {
		t.Fatalf("receipts %+v", obs.Receipts)
	}
}

// A completion during another turn waits for that turn to end and is then
// delivered as its own turn (never steered into the busy one).
func TestCodexWakeQueuedDuringTurn(t *testing.T) {
	t.Parallel()
	e, _ := newWakeEnv(t, nil)
	h := e.launch(e.a, "run_w2", e.home, nil, nil)
	e.wait(e.a, h, 5*time.Second, idle)
	dlv(t, e.a, h, "msg_w2a", "start it FAKE_BG_MS=700:b", contracts.ModeQueue)
	e.wait(e.a, h, 5*time.Second, idle)
	dlv(t, e.a, h, "msg_w2b", "keep busy FAKE_SLEEP_MS=1800", contracts.ModeQueue)
	time.Sleep(1200 * time.Millisecond) // the command has finished; the second turn still runs
	if obs, _ := e.a.Observe(context.Background(), h, ""); obs.State != contracts.RunRunning {
		t.Fatalf("second turn should still run: %s %s", obs.State, obs.StateReason)
	}
	if n, _ := wakeEntries(t, e.a, h); len(n) != 0 {
		t.Fatalf("woken during a busy turn: %+v", n)
	}
	got := waitNotices(t, e.a, h, 1, 6*time.Second)
	_, all := wakeEntries(t, e.a, h)
	busy, reply, notice := -1, -1, -1
	for i, en := range all {
		switch {
		case en.Kind == "pyre_message" && en.MessageID == "msg_w2b":
			busy = i
		case busy >= 0 && reply < 0 && en.Kind == "assistant":
			reply = i // the busy turn's reply
		case en.Kind == "system" && en.Text == got[0].Text:
			notice = i
		}
	}
	if busy < 0 || reply < 0 || notice <= reply {
		t.Fatalf("notice (entry %d) not after the busy turn's reply (entry %d): %+v", notice, reply, all)
	}
	if !strings.Contains(got[0].Text, "`bg-b`") {
		t.Fatalf("%s", got[0].Text)
	}
}

// Several completions close together share one notice.
func TestCodexWakeCoalesces(t *testing.T) {
	t.Parallel()
	e, rec := newWakeEnv(t, nil)
	e.a.wakeSettle = 300 * time.Millisecond
	h := e.launch(e.a, "run_w3", e.home, nil, nil)
	e.wait(e.a, h, 5*time.Second, idle)
	dlv(t, e.a, h, "msg_w3", "start FAKE_BG_MS=300:c FAKE_BG_MS=380:d FAKE_BG_MS=450:e", contracts.ModeQueue)
	got := waitNotices(t, e.a, h, 1, 5*time.Second)
	e.wait(e.a, h, 5*time.Second, idle)
	time.Sleep(700 * time.Millisecond)
	if n, _ := wakeEntries(t, e.a, h); len(n) != 1 {
		t.Fatalf("want one coalesced notice, got %d: %+v", len(n), n)
	}
	txt := got[0].Text
	for _, want := range []string{"[pyre] 3 background commands finished:", "1. `bg-c`", "2. `bg-d`", "3. `bg-e`", "bg e done"} {
		if !strings.Contains(txt, want) {
			t.Errorf("notice lacks %q:\n%s", want, txt)
		}
	}
	rec.waitWakes(t, 1)
}

// A command that finishes inside the turn that started it is no wake.
func TestCodexWakeNotForInTurnCompletion(t *testing.T) {
	t.Parallel()
	e, rec := newWakeEnv(t, nil)
	h := e.launch(e.a, "run_w4", e.home, nil, nil)
	e.wait(e.a, h, 5*time.Second, idle)
	dlv(t, e.a, h, "msg_w4", "run FAKE_BG_MS=100:f FAKE_SLEEP_MS=600", contracts.ModeQueue)
	e.wait(e.a, h, 5*time.Second, idle)
	time.Sleep(800 * time.Millisecond)
	if n, _ := wakeEntries(t, e.a, h); len(n) != 0 || rec.wakes() != 0 {
		t.Fatalf("in-turn completion woke the thread: %+v", n)
	}
}

// Interrupt cleans the thread's background terminals: the processes it
// kills report an exit, which is not a wake.
func TestCodexWakeNotAfterInterrupt(t *testing.T) {
	t.Parallel()
	e, rec := newWakeEnv(t, nil)
	h := e.launch(e.a, "run_w5", e.home, nil, nil)
	e.wait(e.a, h, 5*time.Second, idle)
	dlv(t, e.a, h, "msg_w5", "long FAKE_BG_MS=8000:g FAKE_SLEEP_MS=5000", contracts.ModeQueue)
	e.wait(e.a, h, 5*time.Second, func(o contracts.Observation) bool { return o.State == contracts.RunRunning })
	if rc, _ := e.a.Interrupt(context.Background(), h); rc.Outcome != contracts.OutcomeSucceeded {
		t.Fatalf("%+v", rc)
	}
	e.wait(e.a, h, 5*time.Second, idle)
	time.Sleep(800 * time.Millisecond)
	if n, _ := wakeEntries(t, e.a, h); len(n) != 0 || rec.wakes() != 0 {
		t.Fatalf("interrupt's killed command woke the thread: %+v", n)
	}
}

// A session the daemon says is archived (or archiving) is not woken.
func TestCodexWakeNotWhenArchived(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	asked := []string{}
	e, rec := newWakeEnv(t, func(o *Options) {
		o.WakeAllowed = func(_ context.Context, sid string) bool {
			mu.Lock()
			defer mu.Unlock()
			asked = append(asked, sid)
			return false
		}
	})
	h := e.launch(e.a, "run_w6", e.home, nil, nil)
	e.wait(e.a, h, 5*time.Second, idle)
	dlv(t, e.a, h, "msg_w6", "start FAKE_BG_MS=300:h", contracts.ModeQueue)
	e.wait(e.a, h, 5*time.Second, idle)
	time.Sleep(1200 * time.Millisecond)
	e.wait(e.a, h, time.Second, idle) // Observe does not re-arm a dropped notice
	if n, _ := wakeEntries(t, e.a, h); len(n) != 0 || rec.wakes() != 0 {
		t.Fatalf("archived session woken: %+v", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(asked) != 1 || asked[0] != "ses_x" {
		t.Fatalf("WakeAllowed calls: %v", asked)
	}
}

// A stopped run is never woken, even while another run keeps the shared
// app-server (and the thread's other subscribers) alive.
func TestCodexWakeNotAfterStop(t *testing.T) {
	t.Parallel()
	e, rec := newWakeEnv(t, nil)
	e.a.wakeSettle = time.Second // stop while the notice waits for quiet
	keep := e.launch(e.a, "run_w7k", e.home, nil, nil)
	h := e.launch(e.a, "run_w7", e.home, nil, nil)
	e.wait(e.a, h, 5*time.Second, idle)
	dlv(t, e.a, h, "msg_w7", "start FAKE_BG_MS=300:i", contracts.ModeQueue)
	rr, _ := e.a.run(h)
	end := time.Now().Add(5 * time.Second)
	for queued := false; !queued; {
		rr.ts.mu.Lock()
		queued = len(rr.ts.wakes) == 1 && rr.ts.wakeArmed
		rr.ts.mu.Unlock()
		if time.Now().After(end) {
			t.Fatal("completion never queued")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if rc, _ := e.a.Stop(context.Background(), h); rc.Outcome != contracts.OutcomeSucceeded {
		t.Fatalf("%+v", rc)
	}
	time.Sleep(1500 * time.Millisecond)
	hs, _ := e.a.homeServer(e.home)
	c := hs.connected()
	if c == nil {
		t.Fatal("shared app-server gone")
	}
	var resp turnsListResponse
	if err := c.call(context.Background(), "thread/turns/list", turnsListParams{ThreadID: h.NativeRef.ID, ItemsView: "full"}, &resp); err != nil {
		t.Fatal(err)
	}
	for _, tr := range resp.Data {
		for _, raw := range tr.Items {
			var it item
			if json.Unmarshal(raw, &it) == nil && strings.HasPrefix(it.ClientID, wakeIDPrefix) {
				t.Fatalf("stopped run woken: %s", raw)
			}
		}
	}
	if rec.wakes() != 0 {
		t.Fatalf("automation reports: %s", rec)
	}
	_, _ = e.a.Stop(context.Background(), keep)
}

// Unit: which completions queue a wake.
func TestCodexNoteCommandDone(t *testing.T) {
	now := time.Now()
	exit := 2
	cmd := func(id string) item {
		return item{Type: "commandExecution", ID: id, Command: "/bin/zsh -lc 'make test'", Status: "completed", ExitCode: &exit,
			CommandActions: []struct {
				Command string `json:"command"`
			}{{Command: "make test"}}}
	}
	ts := newThreadState("run_u", "ses_u", "thr_u")
	ts.setStatus(threadStatus{Type: "active"}, now)
	ts.activeTurn = "turn-2"
	if ts.noteCommandDone(cmd("x1"), "turn-2", now) {
		t.Error("completion inside its own turn queued")
	}
	if !ts.noteCommandDone(cmd("x2"), "turn-1", now) {
		t.Error("completion of an ended turn during another turn not queued")
	}
	if ts.noteCommandDone(cmd("x2"), "turn-1", now) {
		t.Error("duplicate item queued twice")
	}
	ts.activeTurn = "" // active, turn id unknown (after a pyred restart)
	if !ts.noteCommandDone(cmd("x3"), "turn-3", now) {
		t.Error("ambiguous completion not queued tentatively")
	}
	ts.dropTurnWakes("turn-3") // turn-3 was the active one after all
	if len(ts.wakes) != 1 || ts.wakes[0].ItemID != "x2" || ts.wakes[0].Command != "make test" {
		t.Fatalf("queue %+v", ts.wakes)
	}
	ts.setStatus(threadStatus{Type: "idle"}, now)
	declined := cmd("x4")
	declined.Status = "declined"
	if ts.noteCommandDone(declined, "turn-1", now) || ts.noteCommandDone(item{Type: "agentMessage", ID: "m"}, "turn-1", now) {
		t.Error("declined command or non-command queued")
	}
	ts.clearWakes(now.Add(time.Second))
	if ts.noteCommandDone(cmd("x5"), "turn-1", now) {
		t.Error("completion inside the mute window queued")
	}
	if !ts.noteCommandDone(cmd("x6"), "turn-1", now.Add(2*time.Second)) {
		t.Error("completion after the mute window not queued")
	}
	ts.closing = true
	if ts.noteCommandDone(cmd("x7"), "turn-1", now.Add(3*time.Second)) || ts.wakeable() {
		t.Error("closing run queued or wakeable")
	}
}

func TestCodexWakeNotice(t *testing.T) {
	zero, one := 0, 1
	d := int64(65_400)
	single := wakeNotice([]bgDone{{Command: "gh pr checks 12 --watch", Cwd: "/w", ExitCode: &one, DurationMs: &d, Output: "line\n" + strings.Repeat("x", 5000) + "\nlast line\n"}})
	if !strings.HasPrefix(single, "[pyre] background command finished: `gh pr checks 12 --watch` (exit 1, cwd /w, ran 1m5s). Last output:\n…\n") ||
		!strings.Contains(single, "last line") || strings.Contains(single, "line\nx") || len(single) > wakeTailBytes+600 {
		t.Fatalf("single notice (%d bytes):\n%.400s", len(single), single)
	}
	ts := newThreadState("run_u", "ses_u", "thr_u")
	ts.setStatus(threadStatus{Type: "idle"}, time.Now())
	ts.noteCommandDone(item{Type: "commandExecution", ID: "big", Command: "make", Status: "completed", ExitCode: &zero,
		AggregatedOutput: strings.Repeat("y", 20000) + "\nthe end\n"}, "turn-1", time.Now())
	if n := wakeNotice(ts.wakes); strings.Count(n, "…") != 1 || !strings.Contains(n, "the end") || len(n) > wakeTailBytes+600 {
		t.Fatalf("queued output not cut once (%d bytes):\n%.200s", len(n), n)
	}
	if n := wakeNotice([]bgDone{{Command: "true", Status: "failed"}}); !strings.Contains(n, "`true` (failed, exit code unknown). No output.") {
		t.Fatalf("%s", n)
	}
	var many []bgDone
	for i := 0; i < 10; i++ {
		many = append(many, bgDone{Command: "job", ExitCode: &zero, Output: strings.Repeat("ü", 3000)})
	}
	m := wakeNotice(many)
	if !strings.HasPrefix(m, "[pyre] 10 background commands finished:\n1. `job` (exit 0)") || !strings.Contains(m, "\n8. ") || strings.Contains(m, "\n9. ") ||
		!strings.Contains(m, "(and 2 more)") || len(m) > wakeTotalBytes+2000 {
		t.Fatalf("many notice (%d bytes):\n%.300s", len(m), m)
	}
	if !strings.Contains(m, "ü") || strings.ContainsRune(m, '\uFFFD') {
		t.Fatal("tail cut inside a rune")
	}
	if got := wakeDetail([]bgDone{{Command: "deploy --token=sk-synthetic12345678"}, {Command: "echo hi"}}); strings.Contains(got, "sk-synthetic") || !strings.Contains(got, "echo hi") {
		t.Fatalf("audit detail %q", got)
	}
}

// Wake turns read as Pyre notices in the transcript, not typed input.
func TestCodexWakeTranscriptKind(t *testing.T) {
	a := &Adapter{o: Options{Now: time.Now}}
	raw, _ := json.Marshal(map[string]any{"type": "userMessage", "id": "u1", "clientId": wakeIDPrefix + "1",
		"content": []any{map[string]any{"type": "text", "text": "[pyre] background command finished: `x` (exit 0). No output."}}})
	var seq int64
	es := a.turnEntries("run_t", []turn{{ID: "t", Status: "completed", Items: []json.RawMessage{raw}}}, &seq)
	if len(es) != 1 || es[0].Kind != "system" {
		t.Fatalf("%+v", es)
	}
}

func turnStartLog(t *testing.T, home string) []string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(home, "fake-turnstart.log"))
	var wakes []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, wakeIDPrefix) {
			wakes = append(wakes, l)
		}
	}
	return wakes
}

// A turn/start Codex refused never started a turn: it is retried.
func TestCodexWakeRetriesRefusal(t *testing.T) {
	t.Parallel()
	e, rec := newWakeEnv(t, nil)
	h := e.launch(e.a, "run_w8", e.home, nil, nil)
	e.wait(e.a, h, 5*time.Second, idle)
	dlv(t, e.a, h, "msg_w8", "start FAKE_BG_MS=500:r", contracts.ModeQueue)
	if err := os.WriteFile(filepath.Join(e.home, "fake-turnstart-faults"), []byte("rpc\nrpc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitNotices(t, e.a, h, 1, 6*time.Second)
	if got := turnStartLog(t, e.home); len(got) != 3 {
		t.Fatalf("want 2 refused + 1 accepted wake attempts, got %v", got)
	}
	rec.waitWakes(t, 1)
}

// A turn/start whose outcome is unknown (connection lost before the
// answer) may have started the turn: it is never sent again.
func TestCodexWakeDropsUnknownOutcome(t *testing.T) {
	t.Parallel()
	e, rec := newWakeEnv(t, nil)
	h := e.launch(e.a, "run_w9", e.home, nil, nil)
	e.wait(e.a, h, 5*time.Second, idle)
	dlv(t, e.a, h, "msg_w9", "start FAKE_BG_MS=400:u", contracts.ModeQueue)
	if err := os.WriteFile(filepath.Join(e.home, "fake-turnstart-faults"), []byte("drop\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	end := time.Now().Add(5 * time.Second)
	for len(turnStartLog(t, e.home)) == 0 && time.Now().Before(end) {
		time.Sleep(50 * time.Millisecond)
	}
	// Observe reconnects; nothing may resend the notice.
	for i := 0; i < 30; i++ {
		_, _ = e.a.Observe(context.Background(), h, "")
		time.Sleep(50 * time.Millisecond)
	}
	if got := turnStartLog(t, e.home); len(got) != 1 {
		t.Fatalf("want exactly one wake attempt, got %v", got)
	}
	if n, _ := wakeEntries(t, e.a, h); len(n) != 0 || rec.wakes() != 0 {
		t.Fatalf("dropped notice delivered: %+v", n)
	}
	if r := dlv(t, e.a, h, "msg_w9b", "still there", contracts.ModeQueue); r.State != contracts.DeliveryAccepted {
		t.Fatalf("run unusable after the lost connection: %+v", r)
	}
}

// Interrupting an idle agent stops the command it is waiting on; the
// killed command's exit is no wake.
func TestCodexWakeIdleInterruptStopsWait(t *testing.T) {
	t.Parallel()
	e, rec := newWakeEnv(t, nil)
	h := e.launch(e.a, "run_w10", e.home, nil, nil)
	e.wait(e.a, h, 5*time.Second, idle)
	dlv(t, e.a, h, "msg_w10", "wait on it FAKE_BG_MS=1500:w", contracts.ModeQueue)
	e.wait(e.a, h, 5*time.Second, idle)
	rc, _ := e.a.Interrupt(context.Background(), h)
	if rc.Outcome != contracts.OutcomeSucceeded || !strings.Contains(rc.Detail, "nothing to interrupt (no active turn); background commands stopped (backgroundTerminals/clean err=<nil>)") {
		t.Fatalf("%+v", rc)
	}
	time.Sleep(2500 * time.Millisecond) // past the command's own end and the mute window's start
	if n, _ := wakeEntries(t, e.a, h); len(n) != 0 || rec.wakes() != 0 {
		t.Fatalf("interrupted wait woke the agent: %+v", n)
	}
}

// Unit: a due capacity continue or a pending approval holds the queue;
// an interrupt ignores exactly the commands running at the time.
func TestCodexWakeHoldsAndMutes(t *testing.T) {
	now := time.Now()
	ts := newThreadState("run_u", "ses_u", "thr_u")
	ts.setStatus(threadStatus{Type: "systemError"}, now)
	if !ts.wakeable() {
		t.Fatal("an idle failed thread takes a wake")
	}
	ts.retryAt = now.Add(time.Minute)
	if ts.wakeable() {
		t.Fatal("wake would cancel the due capacity continue")
	}
	ts.retryAt = time.Time{}
	ts.approvals["r"] = &pendingApproval{approval: contracts.Approval{State: contracts.ApprovalPending}}
	if ts.wakeable() {
		t.Fatal("wake while an approval is pending")
	}
	delete(ts.approvals, "r")
	run := item{Type: "commandExecution", ID: "k1", Command: "watch", Status: "completed"}
	ts.noteCommandStart(run)
	ts.muteRunning()
	later := now.Add(time.Hour) // a clean that failed: the command exits much later
	if ts.noteCommandDone(run, "turn-1", later) {
		t.Fatal("exit of a command running at the interrupt queued")
	}
	if !ts.noteCommandDone(item{Type: "commandExecution", ID: "k2", Command: "new", Status: "completed"}, "turn-1", later) {
		t.Fatal("a command started after the interrupt is muted")
	}
}

// A capacity continue that is starting goes first: the wake waits and is
// delivered after it.
func TestCodexWakeWaitsForCapacityContinue(t *testing.T) {
	t.Parallel()
	e, _ := newWakeEnv(t, nil)
	h := e.launch(e.a, "run_w11", e.home, nil, nil)
	e.wait(e.a, h, 5*time.Second, idle)
	rr, _ := e.a.run(h)
	rr.retrying.Store(true)
	dlv(t, e.a, h, "msg_w11", "start FAKE_BG_MS=300:p", contracts.ModeQueue)
	time.Sleep(1200 * time.Millisecond)
	if n, _ := wakeEntries(t, e.a, h); len(n) != 0 {
		t.Fatalf("woken while a capacity continue was starting: %+v", n)
	}
	rr.retrying.Store(false)
	waitNotices(t, e.a, h, 1, 5*time.Second)
}
