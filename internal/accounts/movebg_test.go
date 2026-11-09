package accounts

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Decision I67: account switches and live harness background tasks.

func (e *testEnv) setBackground(sessionID string, tasks ...string) {
	e.ctrl.mu.Lock()
	defer e.ctrl.mu.Unlock()
	if e.ctrl.bg == nil {
		e.ctrl.bg = map[string][]string{}
	}
	e.ctrl.bg[sessionID] = tasks
}

// heldEvents are the node.automation move_held events reported.
func heldEvents(e *testEnv) []contracts.NodeAutomation {
	var out []contracts.NodeAutomation
	for _, b := range e.rep.of(contracts.ReportEvent) {
		var ev contracts.Event
		json.Unmarshal(b, &ev)
		if ev.Kind != contracts.EvNodeAutomation {
			continue
		}
		var a contracts.NodeAutomation
		json.Unmarshal(ev.Payload, &a)
		if a.Action == contracts.AutomationMoveHeld {
			out = append(out, a)
		}
	}
	return out
}

// A session whose last turn ended with a background shell running is not
// moved (the stop would kill it, and nothing would wake the session when
// its job finishes); the hold is audited once, and the move happens on a
// later sweep once the tasks are gone (iss_…).
func TestRebalanceHoldsSessionWithBackgroundTasks(t *testing.T) {
	e := rebalanceEnv(t)
	ctx := context.Background()
	e.startRun("ses_1", "run_1", contracts.HarnessClaude, softPin, claudeCaps)
	e.setState("ses_1", contracts.RunWaitingInput)
	e.setBackground("ses_1", "shell: until grep -q synced /tmp/synthetic-job.log; do sleep 30; done")

	if plans := e.sweep(); len(plans) != 0 || len(e.ctrl.order) != 0 {
		t.Fatalf("moved a session with a live background task: %+v %v", plans, e.ctrl.order)
	}
	if p, _ := getFallback(ctx, e.svc.db, "run_1"); p != nil {
		t.Fatalf("claim left behind: %+v", p)
	}
	held := heldEvents(e)
	if len(held) != 1 || held[0].RunID != "run_1" || held[0].SessionID != "ses_1" ||
		!strings.Contains(held[0].Detail, `stays on account "opus" instead of moving to account "B": 1 live background task(s)`) ||
		!strings.Contains(held[0].Detail, "until grep -q synced") {
		t.Fatalf("held events %+v", held)
	}
	// Held again on the next sweep, audited once.
	e.clock.Advance(rebalanceEvery)
	if plans := e.svc.Rebalance(ctx); len(plans) != 0 || len(e.ctrl.order) != 0 {
		t.Fatalf("moved: %+v %v", plans, e.ctrl.order)
	}
	if n := len(heldEvents(e)); n != 1 {
		t.Fatalf("held audited %d times", n)
	}
	// The task finished: the move goes ahead (no cooldown from the hold).
	e.setBackground("ses_1")
	e.clock.Advance(rebalanceEvery)
	plans := e.svc.Rebalance(ctx)
	if len(plans) != 1 || plans[0].ToAccountID != "acct_synth-b" || plans[0].Outcome != contracts.FallbackNativeResume || len(plans[0].StoppedTasks) != 0 {
		t.Fatalf("plans after the task finished %+v", plans)
	}
	if len(e.ctrl.quiesceOpts) != 1 || !e.ctrl.quiesceOpts[0].Proactive {
		t.Fatalf("quiesce options %+v", e.ctrl.quiesceOpts)
	}
	if k := e.ctrl.continuations[0]; k.Prompt != "" {
		t.Fatalf("a clean move prompts the session: %q", k.Prompt)
	}
}

// Claude's exit dialog is the last check: when it lists tasks the hooks
// had not, the guarded stop cancels it, nothing is stopped or started, the
// hold is audited and the session is not tried again for a while.
func TestRebalanceExitDialogWithTasksCancelsMove(t *testing.T) {
	e := rebalanceEnv(t)
	ctx := context.Background()
	e.startRun("ses_1", "run_1", contracts.HarnessClaude, softPin, claudeCaps)
	e.setState("ses_1", contracts.RunWaitingInput)
	e.ctrl.dialogTasks = []string{"shell · ssh synthetic-host 'until [ -f /tmp/done ]; do sleep 60; done'"}

	if plans := e.sweep(); len(plans) != 0 {
		t.Fatalf("plans %+v", plans)
	}
	if len(e.ctrl.order) != 1 || e.ctrl.order[0] != "refused:run_1" || len(e.ctrl.continuations) != 0 {
		t.Fatalf("order %v continuations %+v", e.ctrl.order, e.ctrl.continuations)
	}
	if p, _ := getFallback(ctx, e.svc.db, "run_1"); p != nil {
		t.Fatalf("claim left behind: %+v", p)
	}
	if a, _ := e.svc.assignmentForRun(ctx, e.svc.db, "run_1"); a.ReleasedAt != nil {
		t.Fatal("assignment released for a run that was not stopped")
	}
	held := heldEvents(e)
	if len(held) != 1 || !strings.Contains(held[0].Detail, "the exit dialog listed them; it was cancelled") {
		t.Fatalf("held events %+v", held)
	}
	if len(e.rep.of(contracts.ReportFallback)) != 0 {
		t.Fatal("a held move reported a fallback plan")
	}
	// Not retried right away: each try types /exit into the session.
	e.clock.Advance(rebalanceEvery)
	e.svc.Rebalance(ctx)
	if len(e.ctrl.order) != 1 {
		t.Fatalf("retried at once: %v", e.ctrl.order)
	}
}

// A limit leaves no choice: the forced switch stops the run with its
// background tasks, and both the plan (audit, report) and the successor's
// prompt say so.
func TestLimitSwitchStopsBackgroundTasksAndSaysSo(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addAccount("acct_synth-a", contracts.HarnessClaude, "A")
	e.addAccount("acct_synth-b", contracts.HarnessClaude, "B")
	e.observe("acct_synth-a", contracts.ProviderAnthropic, 10, time.Minute)
	e.observe("acct_synth-b", contracts.ProviderAnthropic, 30, time.Minute)
	e.startRun("ses_1", "run_1", contracts.HarnessClaude, explicit("acct_synth-a"), claudeCaps)
	e.setBackground("ses_1", "shell: watch synthetic build", "subagent: Survey synthetic crates")

	plan, err := e.svc.HandleLimitEvent(ctx, "ses_1", "run_1", contracts.LimitEvent{Kind: LimitUsage})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Outcome != contracts.FallbackNativeResume || plan.ToAccountID != "acct_synth-b" {
		t.Fatalf("plan %+v", plan)
	}
	if len(e.ctrl.quiesceOpts) != 1 || e.ctrl.quiesceOpts[0].Proactive {
		t.Fatalf("a limit switch used a guarded stop: %+v", e.ctrl.quiesceOpts)
	}
	if len(plan.StoppedTasks) != 2 || !strings.Contains(plan.Reason, "background tasks were stopped by the account switch: shell: watch synthetic build; subagent: Survey synthetic crates") {
		t.Fatalf("plan %+v", plan)
	}
	want := "[pyre] Continue where you left off (switched from account \"A\" to account \"B\" because of the usage limit). " +
		"Background tasks were stopped by the account switch (shell: watch synthetic build; subagent: Survey synthetic crates); re-check your jobs and restart any watcher you still need."
	if k := e.ctrl.continuations[0]; k.Prompt != want {
		t.Fatalf("prompt %q", k.Prompt)
	}
	var reported contracts.FallbackPlan
	if b := e.rep.of(contracts.ReportFallback); len(b) != 1 || json.Unmarshal(b[0], &reported) != nil || len(reported.StoppedTasks) != 2 {
		t.Fatalf("reported plan %+v", reported)
	}
}

// A limit switch without background tasks reads as before.
func TestLimitSwitchWithoutBackgroundTasks(t *testing.T) {
	e := newEnv(t)
	e.addAccount("acct_synth-a", contracts.HarnessClaude, "A")
	e.addAccount("acct_synth-b", contracts.HarnessClaude, "B")
	e.observe("acct_synth-a", contracts.ProviderAnthropic, 10, time.Minute)
	e.observe("acct_synth-b", contracts.ProviderAnthropic, 30, time.Minute)
	e.startRun("ses_1", "run_1", contracts.HarnessClaude, explicit("acct_synth-a"), claudeCaps)
	plan, err := e.svc.HandleLimitEvent(context.Background(), "ses_1", "run_1", contracts.LimitEvent{Kind: LimitUsage})
	if err != nil || len(plan.StoppedTasks) != 0 || strings.Contains(plan.Reason, "background") {
		t.Fatalf("plan %+v %v", plan, err)
	}
	if k := e.ctrl.continuations[0]; strings.Contains(k.Prompt, "Background") {
		t.Fatalf("prompt %q", k.Prompt)
	}
}
