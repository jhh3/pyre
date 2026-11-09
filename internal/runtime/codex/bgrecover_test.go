package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// restartRun drops adapter e.a's connections (a pyred restart) and
// recovers h on a new adapter.
func restartRun(t *testing.T, e *env, h contracts.RunHandle) *Adapter {
	t.Helper()
	rr, _ := e.a.run(h)
	dropConns(t, e.a, rr.meta.ThreadID)
	a2 := e.restart()
	a2.wakeSettle = 150 * time.Millisecond
	if _, err := a2.Recover(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	return a2
}

// waitRollout waits until the run's rollout contains want.
func waitRollout(t *testing.T, a *Adapter, h contracts.RunHandle, want string) {
	t.Helper()
	rr, _ := a.run(h)
	end := time.Now().Add(5 * time.Second)
	for time.Now().Before(end) {
		if p := rolloutPath(rr.stateDir, rr.meta); p != "" {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), want) {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("rollout never got %q", want)
}

// iss_…: a yielded command exits unseen inside
// a long turn, pyred restarts (an upgrade) before that turn ends, and the
// turn then ends. The exit the previous pyred held for judgement is read
// back from the rollout: the recovered run is woken with it.
func TestCodexWakeAfterRecoverMidTurn(t *testing.T) {
	t.Parallel()
	e, _ := newWakeEnv(t, nil)
	h := e.launch(e.a, "run_r1", e.home, nil, runEnv("run_r1"))
	e.wait(e.a, h, 5*time.Second, idle)
	dlv(t, e.a, h, "msg_r1", "render FAKE_BG_MS=100:r FAKE_SLEEP_MS=3000 then pyre waiting-on --quiet", contracts.ModeQueue)
	rr, _ := e.a.run(h)
	end := time.Now().Add(3 * time.Second)
	for held := 0; held == 0; {
		rr.ts.mu.Lock()
		held = len(rr.ts.inTurn)
		rr.ts.mu.Unlock()
		if time.Now().After(end) {
			t.Fatal("the in-turn exit was never held")
		}
		time.Sleep(20 * time.Millisecond)
	}
	a2 := restartRun(t, e, h)
	if obs, _ := a2.Observe(context.Background(), h, ""); obs.State != contracts.RunRunning {
		t.Fatalf("recovered mid-turn: %s %s", obs.State, obs.StateReason)
	}
	got := waitNotices(t, a2, h, 1, 8*time.Second)
	for _, want := range []string{"[pyre] background command finished: `bg-r` (exit 0, cwd /fake/work, ran 1s)", "bg r done"} {
		if !strings.Contains(got[0].Text, want) {
			t.Errorf("notice lacks %q:\n%s", want, got[0].Text)
		}
	}
	e.wait(a2, h, 5*time.Second, idle)
	time.Sleep(500 * time.Millisecond)
	if n, _ := wakeEntries(t, a2, h); len(n) != 1 {
		t.Fatalf("want exactly one wake turn, got %d", len(n))
	}
}

// iss_…: a long turn (37 exec_command calls
// before the restart, most returning their exit) yields a command that
// exits unseen, pyred restarts, and a second yielded command (the build)
// exits while the turn still runs, received live by the recovered pyred.
// The rollout backfill at the turn's end queued every one of the 37 before
// judging them, and the 32-entry queue dropped its oldest: the build. One
// wake must report both.
func TestCodexWakeAfterRecoverMidTurnLiveExit(t *testing.T) {
	t.Parallel()
	e, _ := newWakeEnv(t, nil)
	h := e.launch(e.a, "run_r5", e.home, nil, runEnv("run_r5"))
	e.wait(e.a, h, 5*time.Second, idle)
	dlv(t, e.a, h, "msg_r5", "render FAKE_EXECS=40 FAKE_BG_MS=100:frames FAKE_BG_MS=1500:build FAKE_SLEEP_MS=3000 then pyre waiting-on --quiet", contracts.ModeQueue)
	waitRollout(t, e.a, h, `"id":"exec-bg-frames"`+`,"process_id"`) // exited before the restart
	a2 := restartRun(t, e, h)
	got := waitNotices(t, a2, h, 1, 8*time.Second)
	for _, want := range []string{"2 background commands finished", "`bg-frames` (exit 0", "`bg-build` (exit 0"} {
		if !strings.Contains(got[0].Text, want) {
			t.Errorf("notice lacks %q:\n%s", want, got[0].Text)
		}
	}
	e.wait(a2, h, 5*time.Second, idle)
	time.Sleep(500 * time.Millisecond)
	if n, _ := wakeEntries(t, a2, h); len(n) != 1 {
		t.Fatalf("want exactly one wake turn, got %d", len(n))
	}
}

// A command that exits while pyred is down (thread idle) wakes the
// recovered run.
func TestCodexWakeAfterRecoverIdle(t *testing.T) {
	t.Parallel()
	e, rec := newWakeEnv(t, nil)
	h := e.launch(e.a, "run_r2", e.home, nil, runEnv("run_r2"))
	e.wait(e.a, h, 5*time.Second, idle)
	dlv(t, e.a, h, "msg_r2", "start FAKE_BG_MS=900:s", contracts.ModeQueue)
	e.wait(e.a, h, 5*time.Second, idle)
	rr, _ := e.a.run(h)
	dropConns(t, e.a, rr.meta.ThreadID)
	waitRollout(t, e.a, h, `"id":"exec-bg-s"`+`,"process_id"`) // exited while pyred was down
	a2 := e.restart()
	a2.wakeSettle = 150 * time.Millisecond
	if _, err := a2.Recover(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	got := waitNotices(t, a2, h, 1, 5*time.Second)
	if !strings.Contains(got[0].Text, "`bg-s` (exit 0") {
		t.Fatalf("%s", got[0].Text)
	}
	time.Sleep(500 * time.Millisecond)
	if n, _ := wakeEntries(t, a2, h); len(n) != 1 {
		t.Fatalf("want one notice, got %d", len(n))
	}
	rec.waitWakes(t, 1)
}

// Recovered idle, completions older than backfillIdleWindow are the
// previous pyred's business: no wake.
func TestCodexRecoverIdleWindow(t *testing.T) {
	t.Parallel()
	e, rec := newWakeEnv(t, nil)
	h := e.launch(e.a, "run_r4", e.home, nil, runEnv("run_r4"))
	e.wait(e.a, h, 5*time.Second, idle)
	dlv(t, e.a, h, "msg_r4", "start FAKE_BG_MS=900:o", contracts.ModeQueue)
	e.wait(e.a, h, 5*time.Second, idle)
	rr, _ := e.a.run(h)
	dropConns(t, e.a, rr.meta.ThreadID)
	waitRollout(t, e.a, h, `"id":"exec-bg-o"`+`,"process_id"`)
	o := e.o
	o.Now = func() time.Time { return time.Now().Add(backfillIdleWindow + time.Minute) }
	a2, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	a2.wakeSettle = 150 * time.Millisecond
	if _, err := a2.Recover(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1 * time.Second)
	if n, _ := wakeEntries(t, a2, h); len(n) != 0 || rec.wakes() != 0 {
		t.Fatalf("an exit older than the window woke the recovered run: %+v", n)
	}
}

// Restarts never repeat a wake the previous pyred delivered, an in-turn
// exit the model got, or the exits of commands Pyre itself killed.
func TestCodexRecoverNoDuplicateWakes(t *testing.T) {
	t.Parallel()
	e, rec := newWakeEnv(t, nil)
	h := e.launch(e.a, "run_r3", e.home, nil, runEnv("run_r3"))
	e.wait(e.a, h, 5*time.Second, idle)
	// Woken before the restart.
	dlv(t, e.a, h, "msg_r3a", "start FAKE_BG_MS=200:v", contracts.ModeQueue)
	waitNotices(t, e.a, h, 1, 5*time.Second)
	rec.waitWakes(t, 1)
	e.wait(e.a, h, 5*time.Second, idle)
	a2 := restartRun(t, e, h)
	time.Sleep(800 * time.Millisecond)
	if n, _ := wakeEntries(t, a2, h); len(n) != 1 {
		t.Fatalf("a delivered wake was repeated after the restart: %d notices", len(n))
	}
	// Polled in its own turn (seen), then a restart while idle.
	dlv(t, a2, h, "msg_r3b", "run FAKE_BG_MS=100:f FAKE_SLEEP_MS=600 FAKE_POLL=f", contracts.ModeQueue)
	e.wait(a2, h, 5*time.Second, idle)
	time.Sleep(500 * time.Millisecond) // its in-turn judgement ran (a wake would reconnect a2)
	e.a = a2
	a3 := restartRun(t, e, h)
	time.Sleep(800 * time.Millisecond)
	if n, _ := wakeEntries(t, a3, h); len(n) != 1 {
		t.Fatalf("a seen in-turn exit woke the recovered run: %d notices", len(n))
	}
	// Pyre killed the command (idle interrupt), then a restart.
	dlv(t, a3, h, "msg_r3c", "wait FAKE_BG_MS=5000:w", contracts.ModeQueue)
	e.wait(a3, h, 5*time.Second, idle)
	if rc, _ := a3.Interrupt(context.Background(), h); rc.Outcome != contracts.OutcomeSucceeded {
		t.Fatalf("%+v", rc)
	}
	waitRollout(t, a3, h, `"id":"exec-bg-w"`+`,"process_id"`)
	time.Sleep(300 * time.Millisecond)
	e.a = a3
	a4 := restartRun(t, e, h)
	time.Sleep(800 * time.Millisecond)
	if n, _ := wakeEntries(t, a4, h); len(n) != 1 {
		t.Fatalf("a command Pyre killed woke the recovered run: %d notices", len(n))
	}
}

func rEvent(kind, turn string) map[string]any {
	return map[string]any{"type": "event_msg", "payload": map[string]any{"type": kind, "turn_id": turn}}
}

func rExit(id, pid, turn string, at time.Time) map[string]any {
	return map[string]any{"type": "event_msg", "timestamp": at.UTC().Format(time.RFC3339Nano), "payload": map[string]any{"type": "item_completed", "turn_id": turn,
		"completed_at_ms": at.UnixMilli(), "started_at_ms": at.Add(-time.Minute).UnixMilli(),
		"item": map[string]any{"type": "CommandExecution", "id": id, "process_id": pid, "source": "unified_exec_startup", "status": "completed", "exit_code": 0,
			"command": []string{"/bin/bash", "-lc", "make " + id}, "cwd": "file:///w", "aggregated_output": "ok\n", "duration": map[string]any{"secs": 60}}}}
}

// Unit: which rollout completions a recovered run reads back, and how.
func TestCodexMissedExits(t *testing.T) {
	t0 := time.Date(2026, 10, 24, 4, 50, 0, 0, time.UTC)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	p := writeRollout(t,
		rEvent("task_started", "t1"),
		rExit("old", "11", "t1", at(1)),
		rEvent("task_complete", "t1"),
		rEvent("task_started", "t2"),
		rExit("in2", "22", "t2", at(10)),   // its own turn: in-turn candidate
		rExit("late1", "33", "t1", at(11)), // an ended turn's: after-turn
		rEvent("task_complete", "t2"),
		rExit("after2", "44", "t2", at(20)), // after t2 ended
	)
	got := func(turn string, since time.Time) string {
		var s []string
		for _, m := range missedExits(p, turn, since) {
			k := m.done.ItemID
			if m.done.InTurn {
				k += "*"
			}
			s = append(s, k)
		}
		return strings.Join(s, ",")
	}
	if g := got("t2", time.Time{}); g != "in2*,late1,after2" {
		t.Errorf("t2: %s", g)
	}
	if g := got("", time.Time{}); g != "in2*,late1,after2" {
		t.Errorf("last turn: %s", g)
	}
	if g := got("t1", time.Time{}); g != "old*,in2,late1,after2" {
		t.Errorf("t1: %s", g)
	}
	if g := got("t2", at(15)); g != "after2" {
		t.Errorf("since launch: %s", g)
	}
	if g := got("t9", time.Time{}); g != "" {
		t.Errorf("unknown turn: %s", g)
	}
	m := missedExits(p, "t2", time.Time{})[0].done
	if m.Command != "make in2" || m.Cwd != "/w" || m.ProcessID != "22" || m.DurationMs == nil || *m.DurationMs != 60000 || m.Output != "ok\n" || m.TurnID != "t2" {
		t.Errorf("read back %+v", m)
	}
	// An aborted turn's own exits are not candidates.
	ab := writeRollout(t, rEvent("task_started", "t1"), rExit("x", "11", "t1", at(1)), rEvent("turn_aborted", "t1"), rExit("y", "12", "t1", at(2)))
	if g := missedExits(ab, "", time.Time{}); len(g) != 1 || g[0].done.ItemID != "y" || g[0].done.InTurn {
		t.Errorf("aborted: %+v", g)
	}
}

// Unit: a rollout larger than the scan window whose cut falls between a
// command's yield and its exit (iss_…: inline
// images) leaves the exit unjudged, never "returned by exec_command";
// missedExits still finds the turn's exits after the cut.
func TestCodexExitsSeenCutRollout(t *testing.T) {
	t0 := time.Now().Add(-time.Hour)
	big := strings.Repeat("A", rolloutScanBytes)
	p := writeRollout(t,
		rEvent("task_started", "t1"),
		rCall("custom_tool_call", "c1", `text(await tools.exec_command({cmd:"python3 review-batch.py",yield_time_ms:1000}));`),
		rOut("custom_tool_call", "c1", codeOut(`{"chunk_id":"a","session_id":3327,"output":""}`)),
		map[string]any{"type": "response_item", "timestamp": t0.Add(time.Minute).UTC().Format(time.RFC3339Nano), "payload": map[string]any{"type": "custom_tool_call_output", "call_id": "img", "output": big}},
		map[string]any{"type": "event_msg", "timestamp": t0.Add(2 * time.Minute).UTC().Format(time.RFC3339Nano), "payload": map[string]any{"type": "token_count"}},
		rExit("exec-3327", "3327", "t1", t0.Add(150*time.Second)), // started at t0+90s
		rExit("exec-5", "5", "t1", t0.Add(5*time.Minute)),         // started at t0+4m
		rEvent("task_complete", "t1"),
	)
	// The scanned part starts at the token_count line (t0+2m). exec-3327
	// started before it and its yield is not in it: unjudged. exec-5
	// started inside it and never yielded: exec_command returned its exit.
	lines := missedExits(p, "t1", time.Time{})
	if len(lines) != 2 || !lines[0].done.InTurn || !lines[1].done.InTurn {
		t.Fatalf("missed after the cut: %+v", lines)
	}
	got := exitsSeen(p, []execRef{{"exec-3327", "3327"}, {"exec-5", "5"}})
	if _, ok := got["exec-3327"]; ok || !got["exec-5"] {
		t.Fatalf("cut rollout: %v (want exec-3327 unjudged, exec-5 seen)", got)
	}
	small := writeRollout(t,
		rCall("custom_tool_call", "c1", `text(await tools.exec_command({cmd:"true"}));`),
		rExit("exec-1", "11", "t1", time.Now()),
	)
	if got := exitsSeen(small, []execRef{{"exec-1", "11"}}); !got["exec-1"] {
		t.Fatalf("uncut rollout without a yield: %v (want seen)", got)
	}
}

// cleanedAt round-trips.
func TestCodexCleanedAt(t *testing.T) {
	dir := t.TempDir()
	if !readCleanedAt(dir).IsZero() {
		t.Fatal("no file: zero")
	}
	now := time.Now()
	writeCleanedAt(dir, now)
	if got := readCleanedAt(dir); !got.Equal(now) {
		t.Fatalf("%v != %v", got, now)
	}
	if _, err := os.Stat(filepath.Join(dir, bgCleanedFile)); err != nil {
		t.Fatal(err)
	}
}
