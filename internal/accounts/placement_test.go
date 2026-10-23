package accounts

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/usage"
)

// An explicit account at its limit falls back to the best eligible one with
// a note (agents pinned 4 children to "opus" in cooldown and all 4 failed);
// strict pinning keeps today's failure.
func TestExplicitAtLimitFallsBackUnlessStrict(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addAccount("acct_synth-o", contracts.HarnessClaude, "opus")
	e.addAccount("acct_synth-b", contracts.HarnessClaude, "B")
	e.observe("acct_synth-b", contracts.ProviderAnthropic, 30, time.Minute)
	limited(t, e, "acct_synth-o")

	pin := contracts.AccountPolicy{Mode: contracts.AccountExplicit, AccountID: "acct_synth-o"}
	r, err := e.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: pin, RunID: "run_1", SessionID: "ses_1"})
	if err != nil || r.Assignment.AccountID != "acct_synth-b" {
		t.Fatalf("want fallback to B, got %+v %v", r.Assignment, err)
	}
	if !strings.HasPrefix(r.Assignment.Reason, `account "opus" is unavailable (cooldown until `) || !strings.Contains(r.Assignment.Reason, "using the best eligible anthropic account instead") {
		t.Fatalf("reason %q", r.Assignment.Reason)
	}

	sel, err := e.svc.Select(ctx, contracts.AccountSelectParams{Provider: contracts.ProviderAnthropic, Policy: pin})
	if err != nil || sel.AccountID != "acct_synth-b" || !strings.HasPrefix(sel.Reason, `account "opus" is unavailable`) {
		t.Fatalf("dry run %+v %v", sel, err)
	}

	pin.Strict = true
	_, err = e.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: pin, RunID: "run_2", SessionID: "ses_2"})
	ce := mustCode(t, err, contracts.ErrCapacityExhausted)
	if !strings.Contains(ce.Message, `account "opus" (acct_synth-o): cooldown until`) {
		t.Fatalf("strict error must name the account unambiguously: %s", ce.Message)
	}
}

// A home account past HomeWeeklyPercent weekly is no longer sticky, even
// with an empty 5-hour window.
func TestSpreadWeeklyHot(t *testing.T) {
	e := newEnv(t)
	cs := []*candidate{
		{acct: contracts.Account{AccountID: "home"}, home: true, room: usage.Headroom{FiveHour: 0, FiveOK: true, Weekly: 75, WeekOK: true}},
		{acct: contracts.Account{AccountID: "other"}, room: usage.Headroom{FiveHour: 20, FiveOK: true, Weekly: 30, WeekOK: true}},
	}
	for _, c := range cs {
		c.near = nearLimit(c.room)
	}
	e.svc.markCrowd(cs)
	if cs[0].crowd != crowdHot || e.svc.crowdNote(cs[0]) != "weekly at 75% (new runs go elsewhere from 70%)" {
		t.Fatalf("crowd %d note %q", cs[0].crowd, e.svc.crowdNote(cs[0]))
	}
	rank(cs)
	if cs[0].acct.AccountID != "other" {
		t.Fatalf("home at 75%% weekly still chosen")
	}
}

// limited puts an account in a usage-limit cooldown (observation passes do
// not end it).
func limited(t *testing.T, e *testEnv, id string) {
	t.Helper()
	if err := putCooldown(context.Background(), e.svc.db, id, e.clock.Now().Add(2*time.Hour), LimitUsage); err != nil {
		t.Fatal(err)
	}
}

func rebalanceEnv(t *testing.T) *testEnv {
	e := newEnv(t)
	e.addAccount("acct_synth-o", contracts.HarnessClaude, "opus")
	e.addAccount("acct_synth-b", contracts.HarnessClaude, "B")
	e.obsWin("acct_synth-o", 5, 88)
	e.obsWin("acct_synth-b", 10, 20)
	return e
}

var softPin = contracts.AccountPolicy{Mode: contracts.AccountExplicit, AccountID: "acct_synth-o"}

func (e *testEnv) setState(sessionID string, st contracts.RunState) {
	e.ctrl.mu.Lock()
	defer e.ctrl.mu.Unlock()
	r := e.ctrl.runs[sessionID]
	r.State, r.TurnInFlight = st, st == contracts.RunRunning
	e.ctrl.runs[sessionID] = r
}

// sweep runs the rebalancer twice, IdleBeforeMove apart (a run must read
// idle on two sweeps before it moves).
func (e *testEnv) sweep() []contracts.FallbackPlan {
	e.t.Helper()
	if first := e.svc.Rebalance(context.Background()); len(first) != 0 {
		e.t.Fatalf("moved on the first idle sighting: %+v", first)
	}
	e.clock.Advance(IdleBeforeMove + time.Second)
	return e.svc.Rebalance(context.Background())
}

// Between turns, an idle session leaves an account past MoveWeeklyPercent
// by native resume, with no prompt (it stays idle); a strict pin stays.
func TestRebalanceMovesIdleSession(t *testing.T) {
	e := rebalanceEnv(t)
	ctx := context.Background()
	e.startRun("ses_1", "run_1", contracts.HarnessClaude, softPin, claudeCaps)
	e.setState("ses_1", contracts.RunWaitingInput)
	e.startRun("ses_2", "run_2", contracts.HarnessClaude, explicit("acct_synth-o"), claudeCaps) // strict
	e.setState("ses_2", contracts.RunWaitingInput)

	plans := e.sweep()
	if len(plans) != 1 {
		t.Fatalf("plans %+v", plans)
	}
	p := plans[0]
	if p.SessionID != "ses_1" || p.FromAccountID != "acct_synth-o" || p.ToAccountID != "acct_synth-b" || p.Outcome != contracts.FallbackNativeResume || p.UncertainWork {
		t.Fatalf("plan %+v", p)
	}
	if !strings.HasPrefix(p.Reason, ProactivePrefix+`: account "opus" at 5h 5% used, weekly 88%; account "B" `) {
		t.Fatalf("reason %q", p.Reason)
	}
	if len(e.ctrl.order) != 2 || e.ctrl.order[0] != "quiesce:run_1" || e.ctrl.order[1] != "start:acct_synth-b" {
		t.Fatalf("order %v", e.ctrl.order)
	}
	k := e.ctrl.continuations[0]
	if k.Kind != contracts.ContinuationNativeResume || k.Resume == nil || k.Prompt != "" || !k.NativeOnly {
		t.Fatalf("continuation %+v", k)
	}
	if old, _ := e.svc.assignmentForRun(ctx, e.svc.db, "run_1"); old.ReleasedAt == nil {
		t.Fatal("old assignment still active")
	}
	if got, _ := getFallback(ctx, e.svc.db, "run_1"); got == nil || got.Outcome != contracts.FallbackNativeResume {
		t.Fatalf("stored plan %+v", got)
	}
	if len(e.rep.of(contracts.ReportFallback)) != 1 {
		t.Fatal("move not reported")
	}
	// The strict pin stays, and nothing moves again.
	e.clock.Advance(IdleBeforeMove + time.Second)
	if again := e.svc.Rebalance(ctx); len(again) != 0 {
		t.Fatalf("moved again: %+v", again)
	}
	if a, _ := e.svc.assignmentForRun(ctx, e.svc.db, "run_2"); a.ReleasedAt != nil {
		t.Fatal("strictly pinned session moved")
	}
}

// A proactive move is not a limit switch: a real limit right after it still
// switches (no circuit-breaker pause).
func TestRebalanceDoesNotTripTheBreaker(t *testing.T) {
	e := rebalanceEnv(t)
	ctx := context.Background()
	e.addAccount("acct_synth-c", contracts.HarnessClaude, "C")
	e.obsWin("acct_synth-c", 30, 30)
	e.startRun("ses_1", "run_1", contracts.HarnessClaude, softPin, claudeCaps)
	e.setState("ses_1", contracts.RunWaitingInput)
	plans := e.sweep()
	if len(plans) != 1 {
		t.Fatalf("plans %+v", plans)
	}
	e.clock.Advance(time.Minute)
	plan, err := e.svc.HandleLimitEvent(ctx, "ses_1", plans[0].NewRunID, contracts.LimitEvent{Kind: LimitUsage})
	if err != nil || plan.Outcome == contracts.FallbackIntervention || plan.ToAccountID != "acct_synth-c" {
		t.Fatalf("limit after a proactive move: %+v %v", plan, err)
	}
}

func TestRebalanceLeavesBusyOrStuckSessions(t *testing.T) {
	cases := []struct {
		name   string
		state  contracts.RunState
		recent bool
		caps   []string
		b      [2]float64 // B's 5h, weekly
	}{
		{"turn running", contracts.RunRunning, false, claudeCaps, [2]float64{10, 20}},
		{"waiting for an approval", contracts.RunWaitingApproval, false, claudeCaps, [2]float64{10, 20}},
		{"background work", contracts.RunWaitingBackground, false, claudeCaps, [2]float64{10, 20}},
		{"message queued or just delivered", contracts.RunWaitingInput, true, claudeCaps, [2]float64{10, 20}},
		{"no cross-account resume", contracts.RunWaitingInput, false, []string{contracts.CapResumeNative}, [2]float64{10, 20}},
		{"no account with room", contracts.RunWaitingInput, false, claudeCaps, [2]float64{10, 75}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := rebalanceEnv(t)
			e.obsWin("acct_synth-b", tc.b[0], tc.b[1])
			e.startRun("ses_1", "run_1", contracts.HarnessClaude, softPin, tc.caps)
			e.setState("ses_1", tc.state)
			r := e.ctrl.runs["ses_1"]
			r.RecentInput = tc.recent
			e.ctrl.runs["ses_1"] = r
			if plans := e.sweep(); len(plans) != 0 || len(e.ctrl.order) != 0 {
				t.Fatalf("moved: %+v %v", plans, e.ctrl.order)
			}
			if p, _ := getFallback(context.Background(), e.svc.db, "run_1"); p != nil {
				t.Fatalf("claim left behind: %+v", p)
			}
		})
	}
}

// A stop that hangs neither blocks limit fallbacks (fbmu is not held across
// it) nor runs forever (the move's deadline ends it); a run that may still
// stop is reported, not dropped.
func TestRebalanceStuckStopDoesNotBlockFallback(t *testing.T) {
	livePoll = time.Millisecond
	t.Cleanup(func() { livePoll = time.Second })
	e := rebalanceEnv(t)
	ctx := context.Background()
	e.startRun("ses_1", "run_1", contracts.HarnessClaude, softPin, claudeCaps)
	e.setState("ses_1", contracts.RunWaitingInput)
	e.startRun("ses_2", "run_2", contracts.HarnessClaude, explicit("acct_synth-b"), claudeCaps)
	e.svc.Rebalance(ctx) // first idle sighting
	e.clock.Advance(IdleBeforeMove + time.Second)

	block := make(chan struct{})
	e.ctrl.mu.Lock()
	e.ctrl.quiesceBlock, e.ctrl.quiesceBlockRun = block, "run_1"
	e.ctrl.mu.Unlock()
	mctx, cancel := context.WithCancel(ctx)
	done := make(chan []contracts.FallbackPlan)
	go func() { done <- e.svc.Rebalance(mctx) }()
	time.Sleep(100 * time.Millisecond) // the move is now stuck in Quiesce

	got := make(chan error)
	go func() {
		_, err := e.svc.HandleLimitEvent(ctx, "ses_2", "run_2", contracts.LimitEvent{Kind: LimitUsage})
		got <- err
	}()
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("limit fallback blocked behind a stuck proactive stop")
	}
	cancel() // the move's deadline
	plans := <-done
	if len(plans) != 1 || plans[0].Outcome != contracts.FallbackIntervention || !strings.Contains(plans[0].Reason, "was not confirmed") {
		t.Fatalf("stuck move: %+v", plans)
	}
	if p, _ := getFallback(ctx, e.svc.db, "run_1"); p == nil || p.Outcome != contracts.FallbackIntervention {
		t.Fatalf("stored %+v", p)
	}
}

// A stop that timed out but did end: the session resumes on its account.
func TestRebalanceUnconfirmedStopThatEndedResumes(t *testing.T) {
	livePoll = time.Millisecond
	t.Cleanup(func() { livePoll = time.Second })
	e := rebalanceEnv(t)
	e.startRun("ses_1", "run_1", contracts.HarnessClaude, softPin, claudeCaps)
	e.setState("ses_1", contracts.RunWaitingInput)
	e.svc.Rebalance(context.Background())
	e.clock.Advance(IdleBeforeMove + time.Second)
	e.ctrl.notExited = true
	e.ctrl.onQuiesce = func(c *fakeController) { r := c.runs["ses_1"]; r.Live = false; c.runs["ses_1"] = r }
	plans := e.svc.Rebalance(context.Background())
	if len(plans) != 1 || plans[0].ToAccountID != "acct_synth-o" || plans[0].Outcome != contracts.FallbackNativeResume || !strings.Contains(plans[0].Reason, "stop not confirmed") {
		t.Fatalf("plans %+v", plans)
	}
}

// A limit that arrives while a move holds the run is handled when it lets go.
func TestRebalanceDeferredLimit(t *testing.T) {
	e := rebalanceEnv(t)
	ctx := context.Background()
	e.startRun("ses_1", "run_1", contracts.HarnessClaude, softPin, claudeCaps)
	claim := contracts.FallbackPlan{SessionID: "ses_1", FromRunID: "run_1", FromAccountID: "acct_synth-o", Outcome: outcomePending, PlannedAt: e.clock.Now(), Reason: ProactivePrefix + ": in progress"}
	if err := putFallback(ctx, e.svc.db, claim); err != nil {
		t.Fatal(err)
	}
	if p, err := e.svc.HandleLimitEvent(ctx, "ses_1", "run_1", contracts.LimitEvent{Kind: LimitUsage}); err != nil || p.Outcome != outcomePending {
		t.Fatalf("during the move: %+v %v", p, err)
	}
	e.svc.unclaim(ctx, "run_1")
	if p, _ := getFallback(ctx, e.svc.db, "run_1"); p == nil || p.Outcome != contracts.FallbackNativeResume || p.ToAccountID != "acct_synth-b" {
		t.Fatalf("deferred limit not handled: %+v", p)
	}
}

// Claims a restarted pyred left behind are settled: a live run just drops
// the claim; a run that ended resumes on its original account.
func TestRebalanceSettlesStaleClaims(t *testing.T) {
	e := rebalanceEnv(t)
	ctx := context.Background()
	e.startRun("ses_1", "run_1", contracts.HarnessClaude, softPin, claudeCaps)
	e.startRun("ses_2", "run_2", contracts.HarnessClaude, softPin, claudeCaps)
	r := e.ctrl.runs["ses_2"]
	r.Live = false
	e.ctrl.runs["ses_2"] = r
	old := e.clock.Now().Add(-moveTimeout - 2*time.Minute)
	for _, run := range []string{"run_1", "run_2"} {
		ses := "ses_" + run[len(run)-1:]
		putFallback(ctx, e.svc.db, contracts.FallbackPlan{SessionID: ses, FromRunID: run, FromAccountID: "acct_synth-o", Outcome: outcomePending, PlannedAt: old})
	}
	e.svc.settleStaleClaims(ctx)
	if p, _ := getFallback(ctx, e.svc.db, "run_1"); p != nil {
		t.Fatalf("live run's claim kept: %+v", p)
	}
	p, _ := getFallback(ctx, e.svc.db, "run_2")
	if p == nil || p.Outcome != contracts.FallbackNativeResume || p.ToAccountID != "acct_synth-o" || !strings.Contains(p.Reason, "interrupted") {
		t.Fatalf("ended run: %+v", p)
	}
	if len(e.ctrl.order) != 1 || e.ctrl.order[0] != "start:acct_synth-o" {
		t.Fatalf("order %v", e.ctrl.order)
	}
}

// Successors carry their predecessor's pin, not the planner's strict bind.
func TestSuccessorCarriesPin(t *testing.T) {
	e := rebalanceEnv(t)
	ctx := context.Background()
	e.startRun("ses_1", "run_1", contracts.HarnessClaude, softPin, claudeCaps)
	plan, err := e.svc.HandleLimitEvent(ctx, "ses_1", "run_1", contracts.LimitEvent{Kind: LimitUsage})
	if err != nil || plan.NewRunID == "" {
		t.Fatalf("%+v %v", plan, err)
	}
	var pinned int
	e.svc.db.QueryRow(`SELECT pinned FROM accounts_assignment WHERE run_id=?`, plan.NewRunID).Scan(&pinned)
	if pinned != 0 {
		t.Fatal("successor of a soft pin marked pinned")
	}
}

// A failed move resumes the session on its account, records that, and is not
// retried within MoveFailCooldown.
func TestRebalanceFailedMoveResumesOnOriginal(t *testing.T) {
	e := rebalanceEnv(t)
	ctx := context.Background()
	e.startRun("ses_1", "run_1", contracts.HarnessClaude, softPin, claudeCaps)
	e.setState("ses_1", contracts.RunWaitingInput)
	e.ctrl.startErr["acct_synth-b"] = contracts.Errorf(contracts.ErrUnsupported, "synthetic: native resume refused")
	plans := e.sweep()
	if len(plans) != 1 || plans[0].ToAccountID != "acct_synth-o" || !strings.Contains(plans[0].Reason, "failed") {
		t.Fatalf("plans %+v", plans)
	}
	want := []string{"quiesce:run_1", "start:acct_synth-b", "start:acct_synth-o"}
	if strings.Join(e.ctrl.order, ",") != strings.Join(want, ",") {
		t.Fatalf("order %v", e.ctrl.order)
	}
	if !e.ctrl.continuations[0].NativeOnly || e.ctrl.continuations[1].NativeOnly {
		t.Fatalf("native-only flags %+v", e.ctrl.continuations)
	}
	e.setState("ses_1", contracts.RunWaitingInput)
	if again := e.sweep2(); len(again) != 0 {
		t.Fatalf("retried at once: %+v", again)
	}
	// The suppression survives a restart (read from the recorded plan).
	e.svc.rb.mu.Lock()
	e.svc.rb.failed = nil
	e.svc.rb.mu.Unlock()
	if !e.svc.moveFailedRecently(ctx, "ses_1") {
		t.Fatal("failed-move suppression lost on restart")
	}
}

// sweep2 is sweep without the first-sighting assertion.
func (e *testEnv) sweep2() []contracts.FallbackPlan {
	e.svc.Rebalance(context.Background())
	e.clock.Advance(IdleBeforeMove + time.Second)
	return e.svc.Rebalance(context.Background())
}

// Both failed: the session is reported stopped (intervention), not dropped.
func TestRebalanceReportsSessionLeftStopped(t *testing.T) {
	e := rebalanceEnv(t)
	e.startRun("ses_1", "run_1", contracts.HarnessClaude, softPin, claudeCaps)
	e.setState("ses_1", contracts.RunWaitingInput)
	e.ctrl.startErr["acct_synth-b"] = contracts.Errorf(contracts.ErrConflict, "synthetic")
	e.ctrl.startErr["acct_synth-o"] = contracts.Errorf(contracts.ErrConflict, "synthetic")
	plans := e.sweep()
	if len(plans) != 1 || plans[0].Outcome != contracts.FallbackIntervention || !strings.Contains(plans[0].Reason, "the session is stopped") {
		t.Fatalf("plans %+v", plans)
	}
	if len(e.rep.of(contracts.ReportFallback)) != 1 {
		t.Fatal("not reported")
	}
}

// The preflight's dry run and the launch agree: with every Pyre login
// limited, both use the node's own login; a soft pin keeps its note.
func TestSelectOwnLoginFallbackMatchesResolve(t *testing.T) {
	e := ownEnv(t)
	ctx := context.Background()
	e.addAccount("acct_synth-a", contracts.HarnessCodex, "A")
	limited(t, e, "acct_synth-a")
	sel, err := e.svc.Select(ctx, contracts.AccountSelectParams{Provider: contracts.ProviderOpenAI, Policy: eligible})
	if err != nil || sel.Outcome != "" || sel.AccountID != "acct_synth-own" {
		t.Fatalf("dry run %+v %v", sel, err)
	}
	res, err := e.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessCodex, RunID: "run_1",
		Policy: contracts.AccountPolicy{Mode: contracts.AccountExplicit, AccountID: "acct_synth-a"}})
	if err != nil || res.CredentialKind != contracts.CredAmbient || !strings.HasPrefix(res.Assignment.Reason, `account "A" is unavailable`) {
		t.Fatalf("soft pin on own login: %+v %v", res.Assignment, err)
	}
}

// A Codex launch with no free OpenAI account says how to fix it.
func TestCodexNoneEligibleSaysWhatToDo(t *testing.T) {
	e := newEnv(t)
	e.addAccount("acct_synth-x", contracts.HarnessCodex, "code")
	limited(t, e, "acct_synth-x")
	_, err := e.svc.ResolveFor(context.Background(), ResolveRequest{Harness: contracts.HarnessCodex, Policy: eligible, RunID: "run_1"})
	ce := mustCode(t, err, contracts.ErrCapacityExhausted)
	for _, want := range []string{`account "code" (acct_synth-x): cooldown until`, "no free OpenAI account for Codex on node_synth", "--fallback-harness claude"} {
		if !strings.Contains(ce.Message, want) {
			t.Fatalf("message lacks %q: %s", want, ce.Message)
		}
	}
}
