package claude

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Decision I67: a proactive account move must not stop a Claude session
// with live background tasks (iss_…).

// The incident's hook sequence: Stop lists Pyre's monitor and the agent's
// shells, then idle_prompt turns waiting_on_background into waiting_input a
// minute later. The tasks are still known (the rebalancer moves only
// waiting_input runs, so the state alone let the move through).
func TestBackgroundTasksSurviveIdlePrompt(t *testing.T) {
	rs := newRunState()
	t0 := time.Now()
	stop := `{"background_tasks":[
		{"type":"shell","id":"b1","status":"running","description":"New Pyre messages for this session","command":"/synthetic/pyred inbox-watch --run-dir /tmp/r"},
		{"type":"shell","id":"b2","status":"running","description":"until grep -q synced /tmp/synthetic.log","command":"until grep -q synced /tmp/synthetic.log"},
		{"type":"subagent","id":"a1","status":"running","description":"Survey synthetic crates","agent_type":"general-purpose"},
		{"type":"shell","id":"b3","status":"completed","description":"done already"}]}`
	rs.applyHook(hookRecord{Event: "SessionStart", At: t0, SessionID: "abc"})
	rs.applyHook(hookRecord{Event: "UserPromptSubmit", At: t0.Add(1)})
	rs.applyHook(hookRecord{Event: "Stop", At: t0.Add(2), Payload: json.RawMessage(stop)})
	if s, _ := rs.derive(); s != contracts.RunWaitingBackground {
		t.Fatalf("after Stop: %s", s)
	}
	rs.applyHook(hookRecord{Event: "Notification", At: t0.Add(3), Payload: json.RawMessage(`{"notification_type":"idle_prompt"}`)})
	if s, _ := rs.derive(); s != contracts.RunWaitingInput {
		t.Fatalf("after idle_prompt: %s", s)
	}
	want := []string{"shell: until grep -q synced /tmp/synthetic.log", "subagent: Survey synthetic crates"}
	if !slices.Equal(rs.bgTasks, want) {
		t.Fatalf("tasks %q", rs.bgTasks)
	}
	// The next turn ends with none: the list clears.
	rs.applyHook(hookRecord{Event: "UserPromptSubmit", At: t0.Add(4)})
	rs.applyHook(hookRecord{Event: "Stop", At: t0.Add(5), Payload: json.RawMessage(`{"background_tasks":[{"type":"shell","status":"running","description":"New Pyre messages for this session"}]}`)})
	if len(rs.bgTasks) != 0 {
		t.Fatalf("tasks after a clean turn %q", rs.bgTasks)
	}
	// A Stop without the field (older Claude Code) leaves what is known.
	rs.bgTasks = []string{"shell: x"}
	rs.applyHook(hookRecord{Event: "Stop", At: t0.Add(6), Payload: json.RawMessage(`{}`)})
	if len(rs.bgTasks) != 1 {
		t.Fatal("a Stop without background_tasks cleared the list")
	}
}

func TestExitDialogTasks(t *testing.T) {
	head := []string{"─────", " Background work is running", " The following will stop when you exit:", ""}
	opts := []string{"", " ❯ 1. Exit and stop tasks", "   2. Move to background and exit", "   3. Stay", "", " Enter to confirm · Esc to cancel"}
	monitorOnly := append(append(slices.Clone(head), " monitor · New Pyre messages for this session"), opts...)
	if got := exitDialogTasks(monitorOnly); len(got) != 0 {
		t.Fatalf("monitor only: %q", got)
	}
	if !exitStopsTasksSelected(monitorOnly) || !exitDialogShown(monitorOnly) {
		t.Fatal("dialog not recognised")
	}
	withShell := append(append(slices.Clone(head), " monitor · New Pyre messages for this session", " shell · until grep -q synced /tmp/synthetic.log", "│ monitor · ssh synthetic-host tail -f job.log │"), opts...)
	got := exitDialogTasks(withShell)
	if !slices.Equal(got, []string{"shell · until grep -q synced /tmp/synthetic.log", "monitor · ssh synthetic-host tail -f job.log"}) {
		t.Fatalf("tasks %q", got)
	}
	// Transcript text above the dialog is not a task.
	above := append([]string{" ⏺ Ran 1. step", " 1. something"}, withShell...)
	if got := exitDialogTasks(above); len(got) != 2 {
		t.Fatalf("tasks %q", got)
	}
}

// The guarded stop refuses at once while the last Stop listed a live shell:
// /exit is never typed and the session keeps running.
func TestFakeGuardedStopRefusesWithBackgroundShell(t *testing.T) {
	t.Parallel()
	f := newFakeEnv(t, nil)
	h := f.launch("run_bgguard1", map[string]string{"FAKE_BG_ON_EXIT": "1"}, nil)
	f.waitState(f.a, h, contracts.RunWaitingInput, 15*time.Second)
	deliver(t, f.a, h, "msg_bgg1", "start a watcher FAKE_BG_SHELL", contracts.ModeQueue)
	f.waitState(f.a, h, contracts.RunWaitingBackground, 15*time.Second)
	if got := f.a.BackgroundWork(context.Background(), h); len(got) != 1 || !strings.Contains(got[0], "until grep -q synced") {
		t.Fatalf("background work %q", got)
	}
	rc, err := f.a.StopUnlessBackground(context.Background(), h)
	tasks := contracts.BackgroundRefusal(err)
	if len(tasks) != 1 || rc.Outcome != contracts.OutcomeFailed || !strings.Contains(rc.Detail, "its last turn ended with them running") {
		t.Fatalf("guarded stop %+v %v", rc, err)
	}
	if p, err := f.a.srv.Pane(context.Background(), h.TmuxTarget); err != nil || p.Dead {
		t.Fatal("the session was stopped")
	}
	if si, err := f.a.screen(context.Background(), h.TmuxTarget); err != nil || !si.InputEmpty() || exitDialogShown(si.Lines) {
		t.Fatalf("screen after a refused stop: %v %q", err, si.Lines)
	}
}

// Claude's exit dialog is the backstop: when it lists a task besides
// Pyre's monitor, the guarded stop answers Escape and the session stays at
// an empty prompt. A plain stop afterwards confirms the dialog and reports
// what it ended.
func TestFakeGuardedStopCancelsExitDialogWithTasks(t *testing.T) {
	t.Parallel()
	f := newFakeEnv(t, nil)
	h := f.launch("run_bgguard2", map[string]string{"FAKE_BG_ON_EXIT": "1", "FAKE_BG_ON_EXIT_TASK": "shell · until [ -f /tmp/synthetic-done ]; do sleep 60; done"}, nil)
	f.waitState(f.a, h, contracts.RunWaitingInput, 15*time.Second)
	deliver(t, f.a, h, "msg_bgg2", "hello", contracts.ModeQueue)
	f.waitState(f.a, h, contracts.RunWaitingInput, 15*time.Second)
	if got := f.a.BackgroundWork(context.Background(), h); len(got) != 0 {
		t.Fatalf("hooks listed %q", got)
	}
	rc, err := f.a.StopUnlessBackground(context.Background(), h)
	tasks := contracts.BackgroundRefusal(err)
	if len(tasks) != 1 || tasks[0] != "shell · until [ -f /tmp/synthetic-done ]; do sleep 60; done" || !strings.Contains(rc.Detail, "the dialog was cancelled") {
		t.Fatalf("guarded stop %+v %v", rc, err)
	}
	if p, err := f.a.srv.Pane(context.Background(), h.TmuxTarget); err != nil || p.Dead {
		t.Fatal("the session was stopped")
	}
	si, err := f.a.screen(context.Background(), h.TmuxTarget)
	if err != nil || exitDialogShown(si.Lines) || !si.InputEmpty() {
		t.Fatalf("dialog left up or prompt not empty: %v %q", err, si.Lines)
	}
	// It still takes input.
	deliver(t, f.a, h, "msg_bgg3", "still here", contracts.ModeQueue)
	f.waitState(f.a, h, contracts.RunWaitingInput, 15*time.Second)

	rc, err = f.a.Stop(context.Background(), h)
	if err != nil || rc.Outcome != contracts.OutcomeSucceeded || !strings.Contains(rc.Detail, "/exit") {
		t.Fatalf("stop %+v %v", rc, err)
	}
	if got := f.a.BackgroundWork(context.Background(), h); len(got) != 1 || !strings.Contains(got[0], "synthetic-done") {
		t.Fatalf("stopped tasks %q", got)
	}
}

// With only Pyre's monitor running, the guarded stop confirms the dialog
// and stops the session as before.
func TestFakeGuardedStopProceedsWithMonitorOnly(t *testing.T) {
	t.Parallel()
	f := newFakeEnv(t, nil)
	h := f.launch("run_bgguard3", map[string]string{"FAKE_BG_ON_EXIT": "1"}, nil)
	f.waitState(f.a, h, contracts.RunWaitingInput, 15*time.Second)
	deliver(t, f.a, h, "msg_bgg4", "hello FAKE_BG_MONITOR_ONLY", contracts.ModeQueue)
	f.waitState(f.a, h, contracts.RunWaitingInput, 15*time.Second)
	rc, err := f.a.StopUnlessBackground(context.Background(), h)
	if err != nil || rc.Outcome != contracts.OutcomeSucceeded || !strings.Contains(rc.Detail, "/exit") {
		t.Fatalf("guarded stop %+v %v", rc, err)
	}
	if p, err := f.a.srv.Pane(context.Background(), h.TmuxTarget); err == nil && !p.Dead {
		t.Fatal("still running")
	}
}
