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

// A command that finishes inside the turn that started it, which then
// polled it (write_stdin after the exit), is no wake.
func TestCodexWakeNotForInTurnCompletion(t *testing.T) {
	t.Parallel()
	e, rec := newWakeEnv(t, nil)
	h := e.launch(e.a, "run_w4", e.home, nil, nil)
	e.wait(e.a, h, 5*time.Second, idle)
	dlv(t, e.a, h, "msg_w4", "run FAKE_BG_MS=100:f FAKE_SLEEP_MS=600 FAKE_POLL=f", contracts.ModeQueue)
	e.wait(e.a, h, 5*time.Second, idle)
	time.Sleep(800 * time.Millisecond)
	if n, _ := wakeEntries(t, e.a, h); len(n) != 0 || rec.wakes() != 0 {
		t.Fatalf("in-turn completion woke the thread: %+v", n)
	}
}

// iss_…: a yielded command exits while its own
// turn still runs, the model never polls it again and ends the turn (on
// `pyre waiting-on --quiet`): the idle run is woken with the result.
func TestCodexWakeForUnseenInTurnCompletion(t *testing.T) {
	t.Parallel()
	e, rec := newWakeEnv(t, nil)
	h := e.launch(e.a, "run_w4b", e.home, nil, nil)
	e.wait(e.a, h, 5*time.Second, idle)
	dlv(t, e.a, h, "msg_w4b", "run FAKE_BG_MS=100:u FAKE_SLEEP_MS=600 then pyre waiting-on --quiet", contracts.ModeQueue)
	got := waitNotices(t, e.a, h, 1, 5*time.Second)
	for _, want := range []string{"[pyre] background command finished: `bg-u` (exit 0", "bg u done", "no turn of yours saw the command finish"} {
		if !strings.Contains(got[0].Text, want) {
			t.Errorf("notice lacks %q:\n%s", want, got[0].Text)
		}
	}
	e.wait(e.a, h, 5*time.Second, idle)
	time.Sleep(600 * time.Millisecond)
	if n, _ := wakeEntries(t, e.a, h); len(n) != 1 {
		t.Fatalf("want exactly one wake turn, got %d", len(n))
	}
	rec.waitWakes(t, 1)
}

// iss_…: in a long turn the in-turn holds fill
// with exec_command calls that returned their exit (40 here, 50 in the
// incident) before the yielded command exits: it is still held, judged
// and woken.
func TestCodexWakeForUnseenInTurnCompletionAfterManyExecs(t *testing.T) {
	t.Parallel()
	e, rec := newWakeEnv(t, nil)
	h := e.launch(e.a, "run_w4c", e.home, nil, nil)
	e.wait(e.a, h, 5*time.Second, idle)
	dlv(t, e.a, h, "msg_w4c", "run FAKE_EXECS=40 FAKE_BG_MS=300:late FAKE_SLEEP_MS=900 then pyre waiting-on --quiet", contracts.ModeQueue)
	got := waitNotices(t, e.a, h, 1, 5*time.Second)
	if !strings.Contains(got[0].Text, "[pyre] background command finished: `bg-late` (exit 0") {
		t.Errorf("notice:\n%s", got[0].Text)
	}
	e.wait(e.a, h, 5*time.Second, idle)
	time.Sleep(600 * time.Millisecond)
	if n, _ := wakeEntries(t, e.a, h); len(n) != 1 {
		t.Fatalf("want exactly one wake turn, got %d", len(n))
	}
	rec.waitWakes(t, 1)
}

// iss_…: a wake turn never stops the run's other
// background commands. The first finishes and wakes the agent; the second
// keeps running through that wake turn, finishes normally and wakes it again.
func TestCodexWakeKeepsOtherBackgroundCommands(t *testing.T) {
	t.Parallel()
	e, rec := newWakeEnv(t, nil)
	h := e.launch(e.a, "run_w4c", e.home, nil, nil)
	e.wait(e.a, h, 5*time.Second, idle)
	dlv(t, e.a, h, "msg_w4c", "start FAKE_BG_MS=300:p FAKE_BG_MS=2500:q", contracts.ModeQueue)
	first := waitNotices(t, e.a, h, 1, 5*time.Second)
	if !strings.Contains(first[0].Text, "`bg-p` (exit 0") || strings.Contains(first[0].Text, "bg-q") {
		t.Fatalf("first notice:\n%s", first[0].Text)
	}
	e.wait(e.a, h, 5*time.Second, idle) // the wake turn ended; q still runs
	rr, _ := e.a.run(h)
	rr.ts.mu.Lock()
	_, running := rr.ts.runningCmds["exec-bg-q"]
	rr.ts.mu.Unlock()
	if !running {
		t.Fatal("bg-q no longer running after the first wake turn")
	}
	got := waitNotices(t, e.a, h, 2, 6*time.Second)
	if txt := got[1].Text; !strings.Contains(txt, "`bg-q` (exit 0") || !strings.Contains(txt, "bg q done") {
		t.Fatalf("second notice (bg-q killed or missing):\n%s", txt)
	}
	rec.waitWakes(t, 2)
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
	ts.settleTurnWakes("turn-3", "completed") // turn-3 was the active one after all; x3 never yielded
	if len(ts.wakes) != 1 || ts.wakes[0].ItemID != "x2" || ts.wakes[0].Command != "make test" || ts.wakes[0].InTurn {
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

// Unit: an exec_command completion inside its own turn is held and becomes
// an InTurn candidate when the turn completes (not when it is interrupted).
func TestCodexSettleTurnWakes(t *testing.T) {
	now := time.Now()
	exec := func(id, pid string) item {
		return item{Type: "commandExecution", ID: id, Command: "sleep 30", Status: "completed", Source: execStartup, ProcessID: pid}
	}
	for _, tc := range []struct {
		status string
		want   []string
	}{{"completed", []string{"e3", "e1"}}, {"failed", []string{"e3", "e1"}}, {"interrupted", nil}} {
		ts := newThreadState("run_u", "ses_u", "thr_u")
		ts.setStatus(threadStatus{Type: "active"}, now)
		ts.activeTurn = "turn-1"
		if ts.noteCommandDone(exec("e1", "61711"), "turn-1", now) {
			t.Fatal("in-turn completion queued at once")
		}
		plain := exec("e2", "")
		plain.Source = "agent"
		ts.noteCommandDone(plain, "turn-1", now) // not exec_command's: never held
		ts.activeTurn = ""                       // after a pyred restart: queued tentatively
		ts.noteCommandDone(exec("e3", "4242"), "turn-1", now)
		ts.settleTurnWakes("turn-1", tc.status)
		var got []string
		for _, w := range ts.wakes {
			if !w.InTurn {
				t.Errorf("%s: %s not marked InTurn", tc.status, w.ItemID)
			}
			got = append(got, w.ItemID)
		}
		if strings.Join(got, ",") != strings.Join(tc.want, ",") || len(ts.inTurn) != 0 {
			t.Errorf("%s: queue %v, held %d", tc.status, got, len(ts.inTurn))
		}
		ts.setStatus(threadStatus{Type: "idle"}, now)
		if ts.noteCommandDone(exec("e1", "61711"), "turn-1", now) && tc.status != "interrupted" {
			t.Errorf("%s: e1 queued twice", tc.status)
		}
	}
}

// writeRollout writes a synthetic rollout (one JSON object per line).
func writeRollout(t *testing.T, lines ...map[string]any) string {
	t.Helper()
	var b strings.Builder
	for _, l := range lines {
		j, _ := json.Marshal(l)
		b.Write(j)
		b.WriteByte('\n')
	}
	p := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func rCall(kind, id, input string) map[string]any {
	k := "input"
	if kind == "function_call" {
		k = "arguments"
	}
	return map[string]any{"type": "response_item", "payload": map[string]any{"type": kind, "call_id": id, "name": "exec", k: input}}
}

func rOut(kind, id string, out any) map[string]any {
	return map[string]any{"type": "response_item", "payload": map[string]any{"type": kind + "_output", "call_id": id, "output": out}}
}

func rDone(id, pid string) map[string]any {
	return map[string]any{"type": "event_msg", "payload": map[string]any{"type": "item_completed",
		"item": map[string]any{"type": "CommandExecution", "id": id, "process_id": pid, "status": "failed", "exit_code": 1}}}
}

// codeOut is a code-mode tool output: the script's text parts, the tool
// result as escaped JSON inside one of them (as codex 0.163 writes it).
func codeOut(result string) []any {
	return []any{map[string]any{"type": "input_text", "text": "Script completed\nOutput:\n"}, map[string]any{"type": "input_text", "text": result}}
}

// Unit: the rollout tells whether the model got an exit. Shapes follow
// real codex 0.163 rollouts (iss_…, synthetic text).
func TestCodexExitsSeen(t *testing.T) {
	// The incident: exec_command yielded session 61711; the exit came while
	// the turn ran other commands; nothing polled 61711 again.
	incident := writeRollout(t,
		rCall("custom_tool_call", "c1", `text(await tools.exec_command({cmd:"python3 inspect.py","yield_time_ms":1000}));`),
		rOut("custom_tool_call", "c1", codeOut(`{"chunk_id":"a","wall_time_seconds":1.0,"session_id":61711,"output":""}`)),
		rCall("custom_tool_call", "c2", `text(await tools.exec_command({cmd:"ls review"}));`),
		rDone("exec-61711", "61711"),
		rOut("custom_tool_call", "c2", codeOut(`{"chunk_id":"b","exit_code":0,"output":"x"}`)),
		rCall("custom_tool_call", "c3", `text(await tools.exec_command({cmd:"pyre waiting-on --quiet 'frames'"}));`),
		rOut("custom_tool_call", "c3", codeOut(`{"chunk_id":"c","exit_code":0,"output":"recorded"}`)),
	)
	if got := exitsSeen(incident, []execRef{{"exec-61711", "61711"}}); len(got) != 1 || got["exec-61711"] {
		t.Fatalf("incident: %v (want unseen)", got)
	}
	// Polled after the exit (code mode): seen.
	polled := writeRollout(t,
		rCall("custom_tool_call", "c1", `text(await tools.exec_command({cmd:"cargo test"}));`),
		rOut("custom_tool_call", "c1", codeOut(`{"chunk_id":"a","session_id":37867,"output":"$ tsgo"}`)),
		rDone("exec-37867", "37867"),
		rCall("custom_tool_call", "c2", `text(await tools.write_stdin({session_id:37867,chars:"","yield_time_ms":1000}));`),
		rOut("custom_tool_call", "c2", codeOut(`{"chunk_id":"b","exit_code":0,"output":""}`)),
	)
	if got := exitsSeen(polled, []execRef{{"exec-37867", "37867"}}); !got["exec-37867"] {
		t.Fatalf("polled: %v (want seen)", got)
	}
	// A poll before the exit only: unseen. Classic function calls.
	early := writeRollout(t,
		rCall("function_call", "f1", `{"cmd":"make","yield_time_ms":1000}`),
		rOut("function_call", "f1", "Chunk ID: a\nProcess running with session ID 4242\nOutput:\n"),
		rCall("function_call", "f2", `{"session_id":4242,"chars":""}`),
		rOut("function_call", "f2", "Process running with session ID 4242\n"),
		rDone("exec-4242", "4242"),
		rCall("function_call", "f3", `{"session_id":42420,"chars":""}`), // another process
		rOut("function_call", "f3", "Process exited with code 0\n"),
	)
	if got := exitsSeen(early, []execRef{{"exec-4242", "4242"}}); len(got) != 1 || got["exec-4242"] {
		t.Fatalf("early poll: %v (want unseen)", got)
	}
	// exec_command returned the exit itself (never yielded): seen. A
	// script that kept polling the process and returned after the exit:
	// seen. A command whose completion is not in the rollout: absent.
	direct := writeRollout(t,
		rCall("custom_tool_call", "c1", `text(await tools.exec_command({cmd:"true"}));`),
		rDone("exec-1", "11"),
		rOut("custom_tool_call", "c1", codeOut(`{"chunk_id":"a","exit_code":0,"output":""}`)),
		rCall("custom_tool_call", "c2", `let r=await tools.exec_command({cmd:"make"}); while(r.session_id) r=await tools.write_stdin({session_id:r.session_id});`),
		rDone("exec-2", "22"),
		rOut("custom_tool_call", "c2", codeOut(`{"session_id":22} {"exit_code":0}`)),
	)
	got := exitsSeen(direct, []execRef{{"exec-1", "11"}, {"exec-2", "22"}, {"exec-3", "33"}})
	if !got["exec-1"] || !got["exec-2"] {
		t.Fatalf("direct/script: %v (want both seen)", got)
	}
	if _, ok := got["exec-3"]; ok {
		t.Fatalf("missing completion judged: %v", got)
	}
	if got := exitsSeen(filepath.Join(t.TempDir(), "none.jsonl"), []execRef{{"exec-1", "11"}}); len(got) != 0 {
		t.Fatalf("missing rollout: %v", got)
	}
}

// Unit: a script that yields a session and keeps running past its exit
// writes the yield after the exit; that is not the model getting the exit.
// Shape of codex 0.162.0 on devbox (iss_…,
// synthetic text): one script ran exec_command (yield_time_ms 1000,
// session 57675), then a second exec_command; 57675 exited 2.4 s before the
// script's output, which named it only as running.
func TestCodexExitsSeenYieldAfterExit(t *testing.T) {
	incident := writeRollout(t,
		rCall("custom_tool_call", "c1", `text(await tools.exec_command({cmd:"git checkout --detach abc; npm run build",yield_time_ms:1000}));
text(await tools.exec_command({cmd:"git push origin abc:refs/heads/ci/x"}));`),
		rDone("exec-57675", "57675"),
		rDone("exec-73729", "73729"),
		rOut("custom_tool_call", "c1", codeOut(`{"chunk_id":"a","wall_time_seconds":1.0,"session_id":57675,"output":"> build\n"}`+"\n"+`{"chunk_id":"b","exit_code":0,"output":"pushed"}`)),
		rCall("custom_tool_call", "c2", `text(await tools.mcp__pyre__waiting_on({what:"build (command session57675)",quiet:true}));`),
		rOut("custom_tool_call", "c2", codeOut(`{"type":"text","text":"recorded"}`)),
	)
	if got := exitsSeen(incident, []execRef{{"exec-57675", "57675"}}); len(got) != 1 || got["exec-57675"] {
		t.Fatalf("yield after exit: %v (want unseen)", got)
	}
	// Classic: parallel calls whose outputs are written after the exit; the
	// yield names the session, the other call is another process.
	classic := writeRollout(t,
		rCall("function_call", "f1", `{"cmd":"make","yield_time_ms":1000}`),
		rCall("function_call", "f2", `{"cmd":"sleep 3"}`),
		rDone("exec-4242", "4242"),
		rOut("function_call", "f1", "Chunk ID: a\nProcess running with session ID 4242\nOutput:\n"),
		rOut("function_call", "f2", "Chunk ID: b\nProcess exited with code 0\nOutput:\n"),
	)
	if got := exitsSeen(classic, []execRef{{"exec-4242", "4242"}}); len(got) != 1 || got["exec-4242"] {
		t.Fatalf("classic yield after exit: %v (want unseen)", got)
	}
	// A literal poll after the exit whose output still reports the session
	// running, then another command's exit: unseen.
	pollRunning := writeRollout(t,
		rCall("custom_tool_call", "c1", `text(await tools.exec_command({cmd:"make",yield_time_ms:1000}));`),
		rOut("custom_tool_call", "c1", codeOut(`{"chunk_id":"a","session_id":515,"output":""}`)),
		rCall("custom_tool_call", "c2", `text(await tools.write_stdin({session_id:515,chars:""})); text(await tools.exec_command({cmd:"sleep 2"}));`),
		rDone("exec-515", "515"),
		rOut("custom_tool_call", "c2", codeOut(`{"chunk_id":"b","session_id":515,"output":""} {"chunk_id":"c","exit_code":0,"output":""}`)),
	)
	if got := exitsSeen(pollRunning, []execRef{{"exec-515", "515"}}); len(got) != 1 || got["exec-515"] {
		t.Fatalf("poll that reported running: %v (want unseen)", got)
	}
	// One script yielded the session and polled it by variable until the
	// exit: seen.
	varPoll := writeRollout(t,
		rCall("custom_tool_call", "c1", `const r=await tools.exec_command({cmd:"make",yield_time_ms:1000}); text(r); text(await tools.write_stdin({session_id:r.session_id,yield_time_ms:30000}));`),
		rDone("exec-616", "616"),
		rOut("custom_tool_call", "c1", codeOut(`{"chunk_id":"a","session_id":616,"output":""} {"chunk_id":"b","exit_code":2,"output":"error"}`)),
	)
	if got := exitsSeen(varPoll, []execRef{{"exec-616", "616"}}); !got["exec-616"] {
		t.Fatalf("variable poll: %v (want seen)", got)
	}
}

// Unit: a full queue drops the in-turn candidates that ran shortest, then
// the oldest; never a long-running one for a burst of quick ones.
func TestCodexCapWakes(t *testing.T) {
	ms := func(n int64) *int64 { return &n }
	var ws []bgDone
	ws = append(ws, bgDone{ItemID: "after-old"}, bgDone{ItemID: "build", InTurn: true, DurationMs: ms(27000)})
	for i := 0; i < wakeQueueMax; i++ {
		ws = append(ws, bgDone{ItemID: fmt.Sprintf("q%d", i), InTurn: true, DurationMs: ms(int64(i))})
	}
	ws = capWakes(ws)
	if len(ws) != wakeQueueMax || ws[0].ItemID != "after-old" || ws[1].ItemID != "build" || ws[2].ItemID != "q2" {
		t.Fatalf("kept %v…", ws[:3])
	}
	var after []bgDone
	for i := 0; i < wakeQueueMax+3; i++ {
		after = append(after, bgDone{ItemID: fmt.Sprintf("a%d", i)})
	}
	if after = capWakes(after); len(after) != wakeQueueMax || after[0].ItemID != "a3" {
		t.Fatalf("after-turn completions: oldest dropped, got %v", after[0])
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
	// I61: an exit 143 is Codex's parent-death SIGTERM, not a failed job.
	term := 143
	for _, linux := range []bool{true, false} {
		prev := parentDeathSIGTERM
		parentDeathSIGTERM = linux
		n := wakeNotice([]bgDone{{Command: "bash render.sh", ExitCode: &term, Output: "frame 1754"}})
		parentDeathSIGTERM = prev
		if !strings.Contains(n, "`bash render.sh` (exit 143). Exit 143 is SIGTERM, not sent by Pyre") ||
			!strings.Contains(n, "children may still be running") || !strings.Contains(n, "Last output:\nframe 1754") ||
			strings.Contains(n, "app-server thread") != linux {
			t.Fatalf("exit 143 notice (linux %v):\n%s", linux, n)
		}
	}
	if strings.Contains(single, "SIGTERM") {
		t.Fatalf("exit 1 notice names SIGTERM:\n%s", single)
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
	if got := wakeDetail(nil, []bgDone{{Command: "deploy --token=sk-synthetic12345678"}, {Command: "echo hi"}}, nil); strings.Contains(got, "sk-synthetic") || !strings.Contains(got, "echo hi") {
		t.Fatalf("audit detail %q", got)
	}
}

// Wake turns read as Pyre notices in the transcript, not typed input.
func TestCodexWakeTranscriptKind(t *testing.T) {
	a := &Adapter{o: Options{Now: time.Now}}
	raw, _ := json.Marshal(map[string]any{"type": "userMessage", "id": "u1", "clientId": wakeIDPrefix + "1",
		"content": []any{map[string]any{"type": "text", "text": "[pyre] background command finished: `x` (exit 0). No output."}}})
	var seq int64
	es := a.turnEntries("run_t", []turn{{ID: "t", Status: "completed", Items: []json.RawMessage{raw}}}, &seq, false)
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

// The app-server dies (the kernel OOM-killed it) while a turn runs that
// yielded a watcher command: Pyre restarts it and starts one turn saying
// the turn was cut off and the command lost
// (iss_…).
func TestCodexWakeAfterAppServerDies(t *testing.T) {
	t.Parallel()
	e, rec := newWakeEnv(t, nil)
	h := e.launch(e.a, "run_lost", e.home, nil, nil)
	e.wait(e.a, h, 5*time.Second, idle)
	if r := dlv(t, e.a, h, "msg_l1", "watch it FAKE_BG_MS=60000:watcher FAKE_SLEEP_MS=60000", contracts.ModeQueue); r.State != contracts.DeliveryAccepted {
		t.Fatalf("%+v", r)
	}
	e.wait(e.a, h, 5*time.Second, running)
	rr, _ := e.a.run(h)
	end := time.Now().Add(3 * time.Second)
	for {
		rr.ts.mu.Lock()
		n := len(rr.ts.runningCmds)
		rr.ts.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(end) {
			t.Fatal("the watcher never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	b, _ := os.ReadFile(filepath.Join(e.o.StateRoot, "codex", homeKey(e.home), "fake-starts.log"))
	var pid int
	if _, err := fmt.Sscan(string(b), &pid); err != nil || pid <= 0 {
		t.Fatalf("fake-starts.log %q", b)
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	_ = p.Kill()
	// The daemon observes every run every 2 s; one observation finds the
	// server gone with the run's work in it.
	end = time.Now().Add(10 * time.Second)
	for starts(t, e, e.home) < 2 {
		if time.Now().After(end) {
			obs, _ := e.a.Observe(context.Background(), h, "")
			t.Fatalf("app-server not restarted: %+v", obs)
		}
		_, _ = e.a.Observe(context.Background(), h, "")
		time.Sleep(50 * time.Millisecond)
	}
	got := waitNotices(t, e.a, h, 1, 10*time.Second)
	txt := got[0].Text
	for _, want := range []string{"[pyre] The Codex app-server running this session exited at", "Pyre restarted it", "Your turn was cut off", "`bg-watcher`", "redo what is still needed"} {
		if !strings.Contains(txt, want) {
			t.Errorf("notice lacks %q:\n%s", want, txt)
		}
	}
	if n := starts(t, e, e.home); n != 2 {
		t.Fatalf("app-server started %d times, want 2", n)
	}
	rec.waitWakes(t, 1)
	e.wait(e.a, h, 5*time.Second, idle)
	time.Sleep(600 * time.Millisecond)
	if n, _ := wakeEntries(t, e.a, h); len(n) != 1 {
		t.Fatalf("want exactly one notice, got %d", len(n))
	}
}

func TestCodexLostNotice(t *testing.T) {
	at := time.Date(2026, 10, 27, 5, 51, 11, 0, time.UTC)
	one := lostNotice(&serverLoss{At: at, Turn: true, Cmds: []string{"while systemctl --user is-active --quiet u; do sleep 10; done"}})
	for _, want := range []string{"exited at " + at.Local().Format("15:04:05 MST"), "Your turn was cut off", "This command was still running and Codex lost it: `while systemctl"} {
		if !strings.Contains(one, want) {
			t.Errorf("notice lacks %q:\n%s", want, one)
		}
	}
	idleOnly := lostNotice(&serverLoss{At: at, Cmds: []string{"a", "b"}})
	if strings.Contains(idleOnly, "turn was cut off") || !strings.Contains(idleOnly, "These 2 commands") || !strings.Contains(idleOnly, "\n2. `b`") {
		t.Errorf("idle notice:\n%s", idleOnly)
	}
	both := wakeText(&serverLoss{At: at, Turn: true}, []bgDone{{Command: "make", Status: "completed"}}, nil)
	if !strings.HasPrefix(both, "[pyre] The Codex app-server") || !strings.Contains(both, "[pyre] background command finished: `make`") {
		t.Errorf("combined notice:\n%s", both)
	}
	var ts threadState
	ts.status = threadStatus{Type: "idle"}
	if ts.noteServerLost(at); ts.lost != nil {
		t.Fatal("an idle thread without commands lost nothing")
	}
	ts.closing, ts.runningCmds = true, map[string]string{"x": "y"}
	if ts.noteServerLost(at); ts.lost != nil {
		t.Fatal("a closing thread is not reported")
	}
}
