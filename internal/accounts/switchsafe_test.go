package accounts

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// TestSwitchBreakerOnePerSession: one automatic switch per session per
// 30m. A second limit inside the window (a real one, or a replayed one that
// slipped past detection) pauses switching: no quiesce, no successor, no
// cooldown on the new account, an intervention plan with the breaker
// reason. The window survives a daemon restart (persisted plans).
func TestSwitchBreakerOnePerSession(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addAccount("acct_synth-a", contracts.HarnessClaude, "A")
	e.addAccount("acct_synth-b", contracts.HarnessClaude, "B")
	e.addAccount("acct_synth-c", contracts.HarnessClaude, "C")
	e.observe("acct_synth-a", contracts.ProviderAnthropic, 10, time.Minute)
	e.observe("acct_synth-b", contracts.ProviderAnthropic, 20, time.Minute)
	e.observe("acct_synth-c", contracts.ProviderAnthropic, 30, time.Minute)
	e.startRun("ses_1", "run_1", contracts.HarnessClaude, explicit("acct_synth-a"), claudeCaps)

	reset := e.clock.Now().Add(3 * time.Hour)
	first, err := e.svc.HandleLimitEvent(ctx, "ses_1", "run_1", contracts.LimitEvent{Kind: LimitUsage, ResetAt: &reset})
	if err != nil || first.Outcome != contracts.FallbackNativeResume || first.ToAccountID != "acct_synth-b" {
		t.Fatalf("first plan %+v %v", first, err)
	}

	// Restart: a fresh service over the same node database.
	e.clock.Advance(5 * time.Minute)
	svc, err := New(ctx, e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.svc, e.ctrl.svc = svc, svc
	e.ctrl.order = nil

	second, err := e.svc.HandleLimitEvent(ctx, "ses_1", first.NewRunID, contracts.LimitEvent{Kind: LimitUsage, ResetAt: &reset})
	if err != nil {
		t.Fatal(err)
	}
	if second.Outcome != contracts.FallbackIntervention || !strings.HasPrefix(second.Reason, SwitchPausedPrefix+": repeated limits within 30m") ||
		second.ToAccountID != "" || second.NewRunID != "" || second.FromAccountID != "acct_synth-b" {
		t.Fatalf("second plan %+v", second)
	}
	if len(e.ctrl.order) != 0 || len(e.ctrl.continuations) != 1 {
		t.Fatalf("breaker must not stop or start runs: order %v, continuations %d", e.ctrl.order, len(e.ctrl.continuations))
	}
	if cd, _ := getCooldown(ctx, e.svc.db, "acct_synth-b"); cd != nil {
		t.Fatalf("paused limit recorded a cooldown on B: %+v", cd)
	}
	// Repeats for the same run return the paused plan (one flag, not many).
	if again, _ := e.svc.HandleLimitEvent(ctx, "ses_1", first.NewRunID, contracts.LimitEvent{Kind: LimitUsage}); again.Reason != second.Reason {
		t.Fatalf("repeat %+v", again)
	}
	if n := len(e.rep.of(contracts.ReportFallback)); n != 2 {
		t.Fatalf("fallback reports %d, want 2", n)
	}

	// After the window a limit on a later run switches again.
	e.clock.Advance(26 * time.Minute)
	e.observe("acct_synth-c", contracts.ProviderAnthropic, 30, time.Minute) // keep its usage fresh
	e.ctrl.runs["ses_1"] = RunInfo{SessionID: "ses_1", RunID: "run_3", Harness: contracts.HarnessClaude, Capabilities: claudeCaps,
		NativeRef: &contracts.NativeRef{Kind: "claude_session", ID: "00000000-0000-4000-8000-0000000000fb"}}
	if _, err := e.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, RunID: "run_3", SessionID: "ses_1", Policy: explicit("acct_synth-b")}); err != nil {
		t.Fatal(err)
	}
	third, err := e.svc.HandleLimitEvent(ctx, "ses_1", "run_3", contracts.LimitEvent{Kind: LimitUsage, ResetAt: &reset})
	if err != nil || third.Outcome != contracts.FallbackNativeResume || third.ToAccountID != "acct_synth-c" {
		t.Fatalf("third plan %+v %v", third, err)
	}
}

// TestSwitchBreakerNodeWide: automatic switching pauses once NodeSwitchMax
// different accounts ran out on a node within NodeSwitchWindow. One account
// running out moves every session on it without tripping the breaker.
func TestSwitchBreakerNodeWide(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.NodeSwitchMax = 2 })
	ctx := context.Background()
	for _, a := range []string{"a", "b", "c", "d"} {
		e.addAccount("acct_synth-"+a, contracts.HarnessClaude, strings.ToUpper(a))
	}
	e.observe("acct_synth-d", contracts.ProviderAnthropic, 5, time.Minute)
	limit := func(ses, run string) contracts.FallbackPlan {
		t.Helper()
		plan, err := e.svc.HandleLimitEvent(ctx, ses, run, contracts.LimitEvent{Kind: LimitUsage})
		if err != nil {
			t.Fatal(err)
		}
		e.clock.Advance(10 * time.Second)
		return plan
	}
	for i := 1; i <= 5; i++ {
		e.startRun(fmt.Sprintf("ses_a%d", i), fmt.Sprintf("run_a%d", i), contracts.HarnessClaude, explicit("acct_synth-a"), claudeCaps)
	}
	e.startRun("ses_b", "run_b", contracts.HarnessClaude, explicit("acct_synth-b"), claudeCaps)
	e.startRun("ses_c", "run_c", contracts.HarnessClaude, explicit("acct_synth-c"), claudeCaps)
	e.startRun("ses_c2", "run_c2", contracts.HarnessClaude, explicit("acct_synth-c"), claudeCaps)
	// A runs out: all five of its sessions move, well past NodeSwitchMax.
	for i := 1; i <= 5; i++ {
		if plan := limit(fmt.Sprintf("ses_a%d", i), fmt.Sprintf("run_a%d", i)); plan.Outcome != contracts.FallbackNativeResume {
			t.Fatalf("session %d on A: %+v", i, plan)
		}
	}
	// B is the second account out: still switches.
	if plan := limit("ses_b", "run_b"); plan.Outcome != contracts.FallbackNativeResume {
		t.Fatalf("session on B: %+v", plan)
	}
	// C would be a third account within the window: paused.
	plan := limit("ses_c", "run_c")
	if plan.Outcome != contracts.FallbackIntervention || !strings.Contains(plan.Reason, "2 accounts ran out on this node within 10m") {
		t.Fatalf("session on C not paused: %+v", plan)
	}
	// The window passes: switching resumes.
	e.clock.Advance(10 * time.Minute)
	e.observe("acct_synth-d", contracts.ProviderAnthropic, 5, time.Minute)
	if plan := limit("ses_c2", "run_c2"); plan.Outcome != contracts.FallbackNativeResume {
		t.Fatalf("after window %+v", plan)
	}
}
