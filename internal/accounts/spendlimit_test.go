package accounts

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// The 2026-10-24 spend-limit incident (iss_…):
// sessions moved off an account at its "org's monthly spend limit" onto one
// whose extra usage was already spent, hit that account's "individual spend
// limit" 15 minutes later, and the switch breaker paused them all while
// other accounts had room.

func (e *testEnv) observeWindows(id string, ws ...contracts.UsageWindow) {
	e.t.Helper()
	o := contracts.AccountObservation{AccountID: id, Provider: contracts.ProviderAnthropic, ReportingNode: "node_synth", ObservedAt: e.clock.Now(),
		Source: "mock", Confidence: "high", Eligibility: contracts.EligibleYes, Windows: ws}
	if err := e.svc.IngestObservation(context.Background(), o); err != nil {
		e.t.Fatal(err)
	}
}

func uwin(name string, used float64) contracts.UsageWindow {
	return contracts.UsageWindow{Name: name, UsedPercent: used}
}

// TestFallbackSkipsSpentExtraUsage: an account whose extra usage is spent
// ranks after every account without a spent window, even with more plan
// headroom (mobot: 5h 0%, weekly 77%, extra usage 100% beat "me": 5h 6%,
// weekly 82%). When it is the only one with room it is still chosen, and
// the reason says why.
func TestFallbackSkipsSpentExtraUsage(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addAccount("acct_synth-a", contracts.HarnessClaude, "A")
	e.addAccount("acct_synth-spent", contracts.HarnessClaude, "Spent")
	e.addAccount("acct_synth-room", contracts.HarnessClaude, "Room")
	e.observeWindows("acct_synth-a", uwin("five_hour", 95), uwin("seven_day", 70))
	e.observeWindows("acct_synth-spent", uwin("five_hour", 0), uwin("seven_day", 77), uwin("extra_usage", 100))
	e.observeWindows("acct_synth-room", uwin("five_hour", 6), uwin("seven_day", 82))
	e.startRun("ses_1", "run_1", contracts.HarnessClaude, explicit("acct_synth-a"), claudeCaps)

	reset := e.clock.Now().Add(3 * time.Hour)
	plan, err := e.svc.HandleLimitEvent(ctx, "ses_1", "run_1", contracts.LimitEvent{Kind: LimitUsage, ResetAt: &reset})
	if err != nil || plan.Outcome != contracts.FallbackNativeResume || plan.ToAccountID != "acct_synth-room" {
		t.Fatalf("plan %+v %v", plan, err)
	}

	// Only the spent account has room: it is used, and said so.
	e.addAccount("acct_synth-a2", contracts.HarnessClaude, "A2")
	e.observeWindows("acct_synth-a2", uwin("five_hour", 50), uwin("seven_day", 70))
	e.startRun("ses_2", "run_2", contracts.HarnessClaude, explicit("acct_synth-a2"), claudeCaps)
	e.observeWindows("acct_synth-room", uwin("five_hour", 100), uwin("seven_day", 82))
	plan, err = e.svc.HandleLimitEvent(ctx, "ses_2", "run_2", contracts.LimitEvent{Kind: LimitUsage, ResetAt: &reset})
	if err != nil || plan.ToAccountID != "acct_synth-spent" || !strings.Contains(plan.Reason, "extra usage at 100%") {
		t.Fatalf("plan %+v %v", plan, err)
	}
}

// TestSwitchBreakerConfirmedSecondLimit: the account a session was moved
// to reports its own limit (another reset) inside the breaker window: the
// session switches again, the account is cooled down until its reset, and
// the plan records the limit. A replayed message (the reset the session
// already left) and a loop back to an account it left still pause.
func TestSwitchBreakerConfirmedSecondLimit(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, a := range []string{"a", "b", "c", "d"} {
		e.addAccount("acct_synth-"+a, contracts.HarnessClaude, strings.ToUpper(a))
	}
	e.observe("acct_synth-b", contracts.ProviderAnthropic, 10, time.Minute)
	e.observe("acct_synth-c", contracts.ProviderAnthropic, 20, time.Minute)
	e.observe("acct_synth-d", contracts.ProviderAnthropic, 30, time.Minute)
	e.startRun("ses_1", "run_1", contracts.HarnessClaude, explicit("acct_synth-a"), claudeCaps)

	resetA := e.clock.Now().Add(3 * time.Hour)
	first, err := e.svc.HandleLimitEvent(ctx, "ses_1", "run_1", contracts.LimitEvent{Kind: LimitRate, ResetAt: &resetA,
		Detail: "rate_limit: You've hit your org's monthly spend limit · your session limit resets 2:20am (UTC)"})
	if err != nil || first.ToAccountID != "acct_synth-b" {
		t.Fatalf("first %+v %v", first, err)
	}
	if first.Limit == nil || first.Limit.ResetAt == nil || !first.Limit.ResetAt.Equal(resetA) || !strings.Contains(first.Limit.Detail, "spend limit") {
		t.Fatalf("first plan limit %+v", first.Limit)
	}

	// A replay of A's message on B's run (same reset): paused, no cooldown on B.
	e.clock.Advance(5 * time.Minute)
	replay, err := e.svc.HandleLimitEvent(ctx, "ses_1", first.NewRunID, contracts.LimitEvent{Kind: LimitRate, ResetAt: &resetA})
	if err != nil || replay.Outcome != contracts.FallbackIntervention || !strings.HasPrefix(replay.Reason, SwitchPausedPrefix) {
		t.Fatalf("replay %+v %v", replay, err)
	}
	if cd, _ := getCooldown(ctx, e.svc.db, "acct_synth-b"); cd != nil {
		t.Fatalf("replay cooled down B: %+v", cd)
	}

	// B's own limit with its own reset on B's next run: switches to C.
	e.clock.Advance(10 * time.Minute)
	e.observe("acct_synth-c", contracts.ProviderAnthropic, 20, time.Minute)
	e.observe("acct_synth-d", contracts.ProviderAnthropic, 30, time.Minute)
	e.ctrl.runs["ses_1"] = RunInfo{SessionID: "ses_1", RunID: "run_b2", Harness: contracts.HarnessClaude, Capabilities: claudeCaps,
		NativeRef: &contracts.NativeRef{Kind: "claude_session", ID: "00000000-0000-4000-8000-0000000000fc"}}
	if _, err := e.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, RunID: "run_b2", SessionID: "ses_1", Policy: explicit("acct_synth-b")}); err != nil {
		t.Fatal(err)
	}
	resetB := e.clock.Now().Add(90 * time.Minute)
	second, err := e.svc.HandleLimitEvent(ctx, "ses_1", "run_b2", contracts.LimitEvent{Kind: LimitUsage, ResetAt: &resetB,
		Detail: "rate_limit: You've hit your individual spend limit · your session limit resets 1am (UTC)"})
	if err != nil || second.Outcome != contracts.FallbackNativeResume || second.FromAccountID != "acct_synth-b" || second.ToAccountID != "acct_synth-c" {
		t.Fatalf("second %+v %v", second, err)
	}
	if cd, _ := getCooldown(ctx, e.svc.db, "acct_synth-b"); cd == nil || !cd.Until.Equal(resetB.UTC()) {
		t.Fatalf("B cooldown %+v, want until %s", cd, resetB)
	}

	// A limit on C without a reset hint: nothing tells it from a replay; paused.
	e.clock.Advance(5 * time.Minute)
	third, err := e.svc.HandleLimitEvent(ctx, "ses_1", second.NewRunID, contracts.LimitEvent{Kind: LimitUsage})
	if err != nil || third.Outcome != contracts.FallbackIntervention || !strings.HasPrefix(third.Reason, SwitchPausedPrefix) {
		t.Fatalf("third %+v %v", third, err)
	}
}

// TestSwitchBreakerConfirmedRepeatCap: confirmed repeats stop at
// SessionSwitchMax switches in the window.
func TestSwitchBreakerConfirmedRepeatCap(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ids := []string{"a", "b", "c", "d", "e"}
	for i, a := range ids {
		e.addAccount("acct_synth-"+a, contracts.HarnessClaude, strings.ToUpper(a))
		if i > 0 {
			e.observe("acct_synth-"+a, contracts.ProviderAnthropic, float64(10*i), time.Minute)
		}
	}
	e.startRun("ses_1", "run_0", contracts.HarnessClaude, explicit("acct_synth-a"), claudeCaps)
	run := "run_0"
	for i := 0; i <= SessionSwitchMax; i++ {
		reset := e.clock.Now().Add(time.Duration(i+1) * time.Hour)
		plan, err := e.svc.HandleLimitEvent(ctx, "ses_1", run, contracts.LimitEvent{Kind: LimitUsage, ResetAt: &reset})
		if err != nil {
			t.Fatal(err)
		}
		if i < SessionSwitchMax {
			if plan.Outcome != contracts.FallbackNativeResume && plan.Outcome != contracts.FallbackHandoff {
				t.Fatalf("switch %d: %+v", i, plan)
			}
			run = plan.NewRunID
			e.clock.Advance(2 * time.Minute)
			continue
		}
		if plan.Outcome != contracts.FallbackIntervention || !strings.HasPrefix(plan.Reason, SwitchPausedPrefix) {
			t.Fatalf("switch %d past the cap: %+v", i, plan)
		}
	}
}
