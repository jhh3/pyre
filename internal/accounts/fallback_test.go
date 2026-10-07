package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

var claudeCaps = []string{contracts.CapResumeNative, contracts.CapResumeHandoff, contracts.CapAccountProfile, contracts.CapAccountCrossResume}

// startRun binds an account to a session's first run and registers it with
// the fake daemon.
func (e *testEnv) startRun(sessionID, runID string, h contracts.Harness, pol contracts.AccountPolicy, caps []string) *contracts.AccountAssignment {
	e.t.Helper()
	r, err := e.svc.ResolveFor(context.Background(), ResolveRequest{Harness: h, Policy: pol, RunID: runID, SessionID: sessionID})
	if err != nil {
		e.t.Fatalf("resolve %s: %v", runID, err)
	}
	ref := &contracts.NativeRef{Kind: "claude_session", ID: "00000000-0000-4000-8000-0000000000fb"}
	if h == contracts.HarnessCodex {
		ref = &contracts.NativeRef{Kind: "codex_thread", ID: "thr-synthetic"}
	}
	e.ctrl.runs[sessionID] = RunInfo{SessionID: sessionID, RunID: runID, Harness: h, Capabilities: caps, NativeRef: ref}
	return r.Assignment
}

func exhaustedEvents(e *testEnv) []ExhaustedPayload {
	var out []ExhaustedPayload
	for _, b := range e.rep.of(contracts.ReportEvent) {
		var ev contracts.Event
		json.Unmarshal(b, &ev)
		if ev.Kind == contracts.EvAccountExhausted {
			var p ExhaustedPayload
			json.Unmarshal(ev.Payload, &p)
			out = append(out, p)
		}
	}
	return out
}

func TestFallbackNativeResume(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addAccount("acct_synth-a", contracts.HarnessClaude, "A")
	e.addAccount("acct_synth-b", contracts.HarnessClaude, "B")
	e.observe("acct_synth-a", contracts.ProviderAnthropic, 10, time.Minute)
	e.observe("acct_synth-b", contracts.ProviderAnthropic, 30, time.Minute)
	e.startRun("ses_1", "run_1", contracts.HarnessClaude, explicit("acct_synth-a"), claudeCaps)

	reset := e.clock.Now().Add(90 * time.Minute)
	plan, err := e.svc.HandleLimitEvent(ctx, "ses_1", "run_1", contracts.LimitEvent{Kind: LimitUsage, ResetAt: &reset, Detail: "usage limit reached"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Outcome != contracts.FallbackNativeResume || plan.ToAccountID != "acct_synth-b" || plan.FromAccountID != "acct_synth-a" || plan.NewRunID == "" || plan.UncertainWork {
		t.Fatalf("plan %+v", plan)
	}
	// Quiesce strictly before the successor starts.
	if len(e.ctrl.order) != 2 || e.ctrl.order[0] != "quiesce:run_1" || e.ctrl.order[1] != "start:acct_synth-b" {
		t.Fatalf("order %v", e.ctrl.order)
	}
	k := e.ctrl.continuations[0]
	if k.Kind != contracts.ContinuationNativeResume || k.Resume == nil || k.Resume.ID != "00000000-0000-4000-8000-0000000000fb" || k.SessionID != "ses_1" {
		t.Fatalf("continuation %+v", k)
	}
	// Old assignment released (kept for audit), new one bound to B.
	old, _ := e.svc.assignmentForRun(ctx, e.svc.db, "run_1")
	if old.ReleasedAt == nil || old.AccountID != "acct_synth-a" {
		t.Fatalf("old assignment %+v", old)
	}
	nw, _ := e.svc.assignmentForRun(ctx, e.svc.db, plan.NewRunID)
	if nw.AccountID != "acct_synth-b" {
		t.Fatalf("new assignment %+v", nw)
	}
	// A is cooling down until its reset.
	if cd, _ := getCooldown(ctx, e.svc.db, "acct_synth-a"); cd == nil || !cd.Until.Equal(reset.UTC()) {
		t.Fatalf("cooldown %+v", cd)
	}
	// Repeated limit events for the same run return the same plan; nothing new starts.
	again, err := e.svc.HandleLimitEvent(ctx, "ses_1", "run_1", contracts.LimitEvent{Kind: LimitUsage})
	if err != nil || again.NewRunID != plan.NewRunID || len(e.ctrl.continuations) != 1 {
		t.Fatalf("duplicate event: %+v %v", again, err)
	}
	if len(e.rep.of(contracts.ReportFallback)) != 1 {
		t.Fatal("fallback plan not reported exactly once")
	}
}

func TestFallbackHandoff(t *testing.T) {
	t.Run("codex without cross-account capability", func(t *testing.T) {
		e := newEnv(t)
		ctx := context.Background()
		e.addAccount("acct_synth-x", contracts.HarnessCodex, "X")
		e.addAccount("acct_synth-y", contracts.HarnessCodex, "Y")
		e.observe("acct_synth-x", contracts.ProviderOpenAI, 10, time.Minute)
		e.observe("acct_synth-y", contracts.ProviderOpenAI, 20, time.Minute)
		e.startRun("ses_x", "run_x1", contracts.HarnessCodex, explicit("acct_synth-x"), []string{contracts.CapResumeNative, contracts.CapResumeHandoff})
		e.ctrl.turnInFlight = true
		plan, err := e.svc.HandleLimitEvent(ctx, "ses_x", "run_x1", contracts.LimitEvent{Kind: LimitUsage})
		if err != nil {
			t.Fatal(err)
		}
		if plan.Outcome != contracts.FallbackHandoff || plan.ToAccountID != "acct_synth-y" || !plan.UncertainWork {
			t.Fatalf("plan %+v", plan)
		}
		k := e.ctrl.continuations[0]
		if k.Kind != contracts.ContinuationHandoff || k.Resume != nil || !k.UncertainWork {
			t.Fatalf("continuation %+v", k)
		}
		if !strings.Contains(plan.Reason, "not replayed") {
			t.Fatalf("reason must disclose no replay: %s", plan.Reason)
		}
		// The codex home lease moved with the run: X released, Y held.
		gx, _ := e.svc.getGrant(ctx, e.svc.db, "acct_synth-x", contracts.HarnessCodex)
		if l, _ := getLease(ctx, e.svc.db, gx.GrantID); l != nil {
			t.Fatalf("old home lease still held: %+v", l)
		}
	})
	t.Run("claude profiles not sharing the transcript store", func(t *testing.T) {
		e := newEnv(t)
		ctx := context.Background()
		e.addAccount("acct_synth-a", contracts.HarnessClaude, "A")
		pb := e.addAccount("acct_synth-b", contracts.HarnessClaude, "B")
		e.observe("acct_synth-a", contracts.ProviderAnthropic, 10, time.Minute)
		e.observe("acct_synth-b", contracts.ProviderAnthropic, 20, time.Minute)
		os.Remove(filepath.Join(pb.Dir, "projects"))
		os.Mkdir(filepath.Join(pb.Dir, "projects"), 0o700)
		e.startRun("ses_1", "run_1", contracts.HarnessClaude, explicit("acct_synth-a"), claudeCaps)
		plan, err := e.svc.HandleLimitEvent(ctx, "ses_1", "run_1", contracts.LimitEvent{Kind: LimitUsage})
		if err != nil || plan.Outcome != contracts.FallbackHandoff || !strings.Contains(plan.Reason, "transcript store") {
			t.Fatalf("plan %+v %v", plan, err)
		}
	})
}

func TestFallbackWaiting(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addAccount("acct_synth-a", contracts.HarnessClaude, "A")
	e.addAccount("acct_synth-b", contracts.HarnessClaude, "B")
	e.observe("acct_synth-a", contracts.ProviderAnthropic, 10, time.Minute)
	bReset := e.clock.Now().Add(3 * time.Hour)
	putCooldown(ctx, e.svc.db, "acct_synth-b", bReset, "usage_limit")
	e.startRun("ses_1", "run_1", contracts.HarnessClaude, explicit("acct_synth-a"), claudeCaps)
	aReset := e.clock.Now().Add(45 * time.Minute)
	plan, err := e.svc.HandleLimitEvent(ctx, "ses_1", "run_1", contracts.LimitEvent{Kind: LimitUsage, ResetAt: &aReset})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Outcome != contracts.FallbackWaiting || plan.NewRunID != "" || plan.ToAccountID != "" {
		t.Fatalf("plan %+v", plan)
	}
	if !strings.Contains(plan.Reason, aReset.Format(time.RFC3339)) || !strings.Contains(plan.Reason, "preserved") {
		t.Fatalf("reason %q", plan.Reason)
	}
	evs := exhaustedEvents(e)
	if len(evs) != 1 || evs[0].Outcome != contracts.FallbackWaiting || evs[0].EarliestReset == nil || !evs[0].EarliestReset.Equal(aReset) || evs[0].Provider != contracts.ProviderAnthropic {
		t.Fatalf("exhausted events %+v", evs)
	}
	if len(e.ctrl.continuations) != 0 {
		t.Fatal("nothing should start while waiting")
	}
	if e.ctrl.order[0] != "quiesce:run_1" {
		t.Fatal("old run not quiesced")
	}
}

func TestFallbackIntervention(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addAccount("acct_synth-x", contracts.HarnessCodex, "X")
	// Y's profile is logged in as Z: identity mismatch.
	e.addAccount("acct_synth-y", contracts.HarnessCodex, "Z", codexAcct("Y"))
	e.observe("acct_synth-x", contracts.ProviderOpenAI, 10, time.Minute)
	e.observe("acct_synth-y", contracts.ProviderOpenAI, 10, time.Minute)
	e.startRun("ses_x", "run_x1", contracts.HarnessCodex, explicit("acct_synth-x"), nil)
	plan, err := e.svc.HandleLimitEvent(ctx, "ses_x", "run_x1", contracts.LimitEvent{Kind: LimitAuthInvalid,
		Detail: "Your access token could not be refreshed. Please log out and sign in again."})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Outcome != contracts.FallbackIntervention || plan.NewRunID != "" {
		t.Fatalf("plan %+v", plan)
	}
	gx, _ := e.svc.getGrant(ctx, e.svc.db, "acct_synth-x", contracts.HarnessCodex)
	if gx.State != contracts.GrantDead {
		t.Fatalf("codex 'sign in again' => dead, got %s", gx.State)
	}
	evs := exhaustedEvents(e)
	if len(evs) != 1 || evs[0].Outcome != contracts.FallbackIntervention {
		t.Fatalf("exhausted events %+v", evs)
	}
	if !strings.Contains(strings.Join(evs[0].Reasons, ";"), "identity mismatch") {
		t.Fatalf("reasons %v", evs[0].Reasons)
	}
}

func TestFallbackQuiesceFailureNoReplacement(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addAccount("acct_synth-a", contracts.HarnessClaude, "A")
	e.addAccount("acct_synth-b", contracts.HarnessClaude, "B")
	e.observe("acct_synth-a", contracts.ProviderAnthropic, 10, time.Minute)
	e.observe("acct_synth-b", contracts.ProviderAnthropic, 10, time.Minute)
	e.startRun("ses_1", "run_1", contracts.HarnessClaude, explicit("acct_synth-a"), claudeCaps)
	e.ctrl.quiesceErr = errors.New("tmux unreachable")
	plan, err := e.svc.HandleLimitEvent(ctx, "ses_1", "run_1", contracts.LimitEvent{Kind: LimitUsage})
	if err != nil || plan.Outcome != contracts.FallbackIntervention || len(e.ctrl.continuations) != 0 {
		t.Fatalf("plan %+v %v (continuations %d)", plan, err, len(e.ctrl.continuations))
	}
	// The run keeps its assignment: nothing else took ownership.
	if a, _ := e.svc.assignmentForRun(ctx, e.svc.db, "run_1"); a.ReleasedAt != nil {
		t.Fatal("assignment released although the run may still be live")
	}
}

func TestFallbackNeverCrossesProvider(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addAccount("acct_synth-a", contracts.HarnessClaude, "A")
	e.addAccount("acct_synth-x", contracts.HarnessCodex, "X") // fresh, idle, wrong provider
	e.observe("acct_synth-a", contracts.ProviderAnthropic, 10, time.Minute)
	e.observe("acct_synth-x", contracts.ProviderOpenAI, 0, time.Minute)
	e.startRun("ses_1", "run_1", contracts.HarnessClaude, explicit("acct_synth-a"), claudeCaps)
	plan, err := e.svc.HandleLimitEvent(ctx, "ses_1", "run_1", contracts.LimitEvent{Kind: LimitUsage})
	if err != nil || plan.Outcome != contracts.FallbackWaiting || plan.ToAccountID != "" {
		t.Fatalf("plan %+v %v", plan, err)
	}
	for _, k := range e.ctrl.continuations {
		if k.AccountID == "acct_synth-x" || k.Harness != contracts.HarnessClaude {
			t.Fatal("crossed providers")
		}
	}
}

func TestFallbackSkipsAccountThatFailsToBind(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addAccount("acct_synth-a", contracts.HarnessClaude, "A")
	e.addAccount("acct_synth-b", contracts.HarnessClaude, "B")
	e.addAccount("acct_synth-c", contracts.HarnessClaude, "C")
	e.observe("acct_synth-a", contracts.ProviderAnthropic, 10, time.Minute)
	e.observe("acct_synth-b", contracts.ProviderAnthropic, 5, time.Minute)
	e.observe("acct_synth-c", contracts.ProviderAnthropic, 50, time.Minute)
	e.startRun("ses_1", "run_1", contracts.HarnessClaude, explicit("acct_synth-a"), claudeCaps)
	e.ctrl.startErr["acct_synth-b"] = contracts.Errorf(contracts.ErrConflict, "raced")
	plan, err := e.svc.HandleLimitEvent(ctx, "ses_1", "run_1", contracts.LimitEvent{Kind: LimitUsage})
	if err != nil || plan.ToAccountID != "acct_synth-c" {
		t.Fatalf("plan %+v %v", plan, err)
	}
}

func TestAmbientRunNoAutomaticFallback(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addAccount("acct_synth-b", contracts.HarnessClaude, "B")
	e.observe("acct_synth-b", contracts.ProviderAnthropic, 10, time.Minute)
	e.startRun("ses_1", "run_1", contracts.HarnessClaude, contracts.AccountPolicy{Mode: contracts.AccountAmbient}, claudeCaps)
	plan, err := e.svc.HandleLimitEvent(ctx, "ses_1", "run_1", contracts.LimitEvent{Kind: LimitUsage})
	if err != nil || plan.Outcome != contracts.FallbackIntervention || len(e.ctrl.continuations) != 0 {
		t.Fatalf("plan %+v %v", plan, err)
	}
}

func TestActionsAndInjectLimit(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addAccount("acct_synth-a", contracts.HarnessClaude, "A")
	e.addAccount("acct_synth-b", contracts.HarnessClaude, "B")
	e.startRun("ses_1", "run_1", contracts.HarnessClaude, contracts.AccountPolicy{Mode: contracts.AccountEligible, Prefer: []string{"acct_synth-a"}}, claudeCaps)
	h := e.svc.Handlers()
	for _, name := range []string{contracts.ActionAccountList, contracts.ActionAccountSelect, contracts.ActionAccountObserve, contracts.ActionAccountInjectLimit} {
		if h[name] == nil {
			t.Fatalf("missing handler %s", name)
		}
	}
	out, err := h[contracts.ActionAccountObserve](ctx, json.RawMessage(`{"provider":"anthropic"}`))
	if err != nil || len(out.([]contracts.AccountObservation)) != 2 {
		t.Fatalf("observe %v %v", out, err)
	}
	out, err = h[contracts.ActionAccountList](ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	views := out.([]contracts.AccountView)
	if len(views) != 2 || len(views[0].Grants) != 1 || len(views[0].Assignments) != 1 || views[0].Latest == nil {
		t.Fatalf("views %+v", views)
	}
	out, err = h[contracts.ActionAccountSelect](ctx, json.RawMessage(`{"provider":"anthropic","policy":{"mode":"eligible"}}`))
	if err != nil || out.(contracts.AccountSelection).AccountID == "" {
		t.Fatalf("select %+v %v", out, err)
	}
	if a, _ := e.svc.activeAssignments(ctx, e.svc.db, ""); len(a) != 1 {
		t.Fatal("dry-run select created an assignment")
	}
	out, err = h[contracts.ActionAccountInjectLimit](ctx, json.RawMessage(`{"session_id":"ses_1","kind":"usage_limit"}`))
	if err != nil {
		t.Fatal(err)
	}
	if p := out.(contracts.FallbackPlan); p.Outcome != contracts.FallbackNativeResume || p.ToAccountID != "acct_synth-b" {
		t.Fatalf("inject plan %+v", p)
	}
	if _, err := h[contracts.ActionAccountSelect](ctx, json.RawMessage(`{bad`)); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("bad params: %v", err)
	}
	// Outside test mode inject_limit refuses.
	cfg := e.cfg
	cfg.TestMode = false
	s2, _ := New(ctx, cfg)
	if _, err := s2.Handlers()[contracts.ActionAccountInjectLimit](ctx, json.RawMessage(`{"session_id":"ses_1"}`)); !contracts.IsCode(err, contracts.ErrForbidden) {
		t.Fatalf("inject outside test mode: %v", err)
	}
}
