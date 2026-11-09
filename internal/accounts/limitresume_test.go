package accounts

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Decision I70: session.limit_resume on the node. A session whose limit
// left no account free is continued on the best eligible account once one
// has room (the limited one included), with the standard continue prompt;
// before that the answer is no_room with the earliest reset.
func TestResumeAfterLimit(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addAccount("acct_synth-a", contracts.HarnessClaude, "A")
	e.addAccount("acct_synth-b", contracts.HarnessClaude, "B")
	e.observe("acct_synth-b", contracts.ProviderAnthropic, 100, time.Minute)
	e.startRun("ses_1", "run_1", contracts.HarnessClaude, explicit("acct_synth-a"), claudeCaps)
	e.ctrl.bg = map[string][]string{"ses_1": {"shell: until gh run watch 1234"}}

	reset := e.clock.Now().Add(time.Hour)
	plan, err := e.svc.HandleLimitEvent(ctx, "ses_1", "run_1", contracts.LimitEvent{Kind: LimitUsage, ResetAt: &reset,
		Detail: "You've hit your individual spend limit · resets 1am (UTC)"})
	if err != nil || plan.Outcome != contracts.FallbackWaiting {
		t.Fatalf("plan %+v %v", plan, err)
	}
	info := e.ctrl.runs["ses_1"]
	info.Live = false // the fallback stopped it
	e.ctrl.runs["ses_1"] = info

	res, err := e.svc.ResumeAfterLimit(ctx, "ses_1", "run_1")
	if err != nil || res.Outcome != contracts.LimitResumeNoRoom || res.ResetAt == nil {
		t.Fatalf("before any room: %+v %v", res, err)
	}
	if n := len(e.ctrl.continuations); n != 0 {
		t.Fatalf("%d continuations without room", n)
	}

	// B regains room: the session continues there.
	e.clock.Advance(10 * time.Minute)
	e.observe("acct_synth-b", contracts.ProviderAnthropic, 5, time.Second)
	res, err = e.svc.ResumeAfterLimit(ctx, "ses_1", "run_1")
	if err != nil || res.Outcome != contracts.LimitResumed || res.AccountID != "acct_synth-b" || res.NewRunID == "" || res.Kind != string(contracts.ContinuationNativeResume) {
		t.Fatalf("resume %+v %v", res, err)
	}
	k := e.ctrl.continuations[0]
	if k.PreviousRunID != "run_1" || k.Resume == nil {
		t.Fatalf("continuation %+v", k)
	}
	for _, want := range []string{`[pyre] Continue where you left off (switched from account "A" to account "B" because of the usage limit).`,
		"Background tasks were stopped by the account switch (shell: until gh run watch 1234)"} {
		if !strings.Contains(k.Prompt, want) {
			t.Fatalf("prompt %q lacks %q", k.Prompt, want)
		}
	}

	// Asked again (a lost answer): the session's current run moved on.
	res, err = e.svc.ResumeAfterLimit(ctx, "ses_1", "run_1")
	if err != nil || res.Outcome != contracts.LimitResumeRefused || len(e.ctrl.continuations) != 1 {
		t.Fatalf("second ask %+v %v", res, err)
	}
}

// The account that ran out is back after its reset and nobody else has
// room: the session continues on it, told that it has room again (not
// that it switched).
func TestResumeAfterLimitSameAccount(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addAccount("acct_synth-a", contracts.HarnessClaude, "A")
	e.addAccount("acct_synth-b", contracts.HarnessClaude, "B")
	e.observe("acct_synth-b", contracts.ProviderAnthropic, 100, time.Minute)
	e.startRun("ses_1", "run_1", contracts.HarnessClaude, explicit("acct_synth-a"), claudeCaps)
	reset := e.clock.Now().Add(time.Hour)
	if plan, err := e.svc.HandleLimitEvent(ctx, "ses_1", "run_1", contracts.LimitEvent{Kind: LimitUsage, ResetAt: &reset}); err != nil || plan.Outcome != contracts.FallbackWaiting {
		t.Fatalf("plan %+v %v", plan, err)
	}
	info := e.ctrl.runs["ses_1"]
	info.Live = false
	e.ctrl.runs["ses_1"] = info
	e.clock.Advance(time.Hour + time.Minute)
	e.observe("acct_synth-a", contracts.ProviderAnthropic, 0, time.Second)
	e.observe("acct_synth-b", contracts.ProviderAnthropic, 100, time.Second)
	res, err := e.svc.ResumeAfterLimit(ctx, "ses_1", "run_1")
	if err != nil || res.Outcome != contracts.LimitResumed || res.AccountID != "acct_synth-a" {
		t.Fatalf("resume %+v %v", res, err)
	}
	if p := e.ctrl.continuations[0].Prompt; p != `[pyre] Continue where you left off (account "A" has room again after the usage limit).` {
		t.Fatalf("prompt %q", p)
	}
}

// Only a limit stop with no account free is resumed: a switch that
// happened, a login failure or a run still up is refused.
func TestResumeAfterLimitRefuses(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addAccount("acct_synth-a", contracts.HarnessClaude, "A")
	e.addAccount("acct_synth-b", contracts.HarnessClaude, "B")
	e.observe("acct_synth-b", contracts.ProviderAnthropic, 10, time.Minute)
	e.startRun("ses_1", "run_1", contracts.HarnessClaude, explicit("acct_synth-a"), claudeCaps)
	reset := e.clock.Now().Add(time.Hour)
	if plan, err := e.svc.HandleLimitEvent(ctx, "ses_1", "run_1", contracts.LimitEvent{Kind: LimitUsage, ResetAt: &reset}); err != nil || plan.Outcome != contracts.FallbackNativeResume {
		t.Fatalf("plan %+v %v", plan, err)
	}
	if res, err := e.svc.ResumeAfterLimit(ctx, "ses_1", "run_1"); err != nil || res.Outcome != contracts.LimitResumeRefused {
		t.Fatalf("switched run: %+v %v", res, err)
	}
	e.startRun("ses_2", "run_2", contracts.HarnessClaude, explicit("acct_synth-b"), claudeCaps)
	if res, err := e.svc.ResumeAfterLimit(ctx, "ses_2", "run_2"); err != nil || res.Outcome != contracts.LimitResumeRefused {
		t.Fatalf("no plan: %+v %v", res, err)
	}
	if n := len(e.ctrl.continuations); n != 1 {
		t.Fatalf("%d continuations (the switch's only)", n)
	}
}

// A run whose limit paused switching may be told to continue in place once
// its account is not known to be at a limit; never for another plan.
func TestLimitRoomFor(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addAccount("acct_synth-a", contracts.HarnessClaude, "A")
	e.startRun("ses_1", "run_1", contracts.HarnessClaude, explicit("acct_synth-a"), claudeCaps)
	reset := e.clock.Now().Add(time.Hour)
	if err := putFallback(ctx, e.svc.db, contracts.FallbackPlan{SessionID: "ses_1", FromRunID: "run_1", FromAccountID: "acct_synth-a", PlannedAt: e.clock.Now(),
		Outcome: contracts.FallbackIntervention, Reason: SwitchPausedPrefix + ": repeated limits within 30m (last automatic switch 23:14Z)",
		Limit: &contracts.LimitEvent{Kind: LimitRate, ResetAt: &reset}}); err != nil {
		t.Fatal(err)
	}
	e.observe("acct_synth-a", contracts.ProviderAnthropic, 100, time.Second)
	if r, err := e.svc.LimitRoomFor(ctx, "run_1"); err != nil || r.OK || r.Refused {
		t.Fatalf("at the limit: %+v %v", r, err)
	}
	e.observe("acct_synth-a", contracts.ProviderAnthropic, 3, time.Second)
	r, err := e.svc.LimitRoomFor(ctx, "run_1")
	if err != nil || !r.OK || r.AccountID != "acct_synth-a" || r.Prompt != `[pyre] Continue where you left off (account "A" has room again after a rate limit).` {
		t.Fatalf("room: %+v %v", r, err)
	}
	if r, err := e.svc.LimitRoomFor(ctx, "run_nonesuch"); err != nil || !r.Refused {
		t.Fatalf("no plan: %+v %v", r, err)
	}
}
