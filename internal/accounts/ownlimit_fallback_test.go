package accounts

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// iss_…: two Codex runs named "personal", the
// node's own login, while that login was at its weekly limit (and refused
// every turn). Both were bound to it and stopped 20 s later; "code" on the
// same node had room.

func weeklyAtLimit(e *testEnv) *contracts.AccountObservation {
	reset := e.clock.Now().Add(72 * time.Hour)
	return &contracts.AccountObservation{AccountID: "acct_synth-own", Provider: contracts.ProviderOpenAI, ObservedAt: e.clock.Now().Add(-time.Minute),
		Source: "own-login", Confidence: "high", Eligibility: contracts.EligibleYes,
		Windows: []contracts.UsageWindow{{Name: "five_hour", UsedPercent: 3}, {Name: "seven_day", UsedPercent: 100, ResetAt: &reset}}}
}

func nonStrict(id string) contracts.AccountPolicy {
	return contracts.AccountPolicy{Mode: contracts.AccountExplicit, AccountID: id}
}

// A non-strict explicit own login at its limit falls back to an eligible
// account with the reason on the assignment, and the dry run (the launch
// preflight) agrees; strict refuses instead of launching on it.
func TestExplicitOwnLoginAtLimitFallsBack(t *testing.T) {
	probe := newEnv(t) // the clock the observation is relative to
	e := ownEnvLatest(t, weeklyAtLimit(probe))
	e.clock.t = probe.clock.t
	ctx := context.Background()
	e.addAccount("acct_synth-code", contracts.HarnessCodex, "code")
	e.observe("acct_synth-code", contracts.ProviderOpenAI, 10, time.Minute)

	res, err := e.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessCodex, Policy: nonStrict("acct_synth-own"), RunID: "run_1", SessionID: "ses_1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Assignment.AccountID != "acct_synth-code" || res.CredentialKind == contracts.CredAmbient {
		t.Fatalf("bound %+v (%s), want the eligible account", res.Assignment, res.CredentialKind)
	}
	if r := res.Assignment.Reason; !strings.Contains(r, `"acct_synth-own" is unavailable`) || !strings.Contains(r, "at its limit until") || !strings.Contains(r, "strict pinning would wait") {
		t.Fatalf("reason %q", r)
	}
	sel, err := e.svc.Select(ctx, contracts.AccountSelectParams{Provider: contracts.ProviderOpenAI, Policy: nonStrict("acct_synth-own")})
	if err != nil || sel.AccountID != "acct_synth-code" || sel.Outcome != "" || !strings.Contains(sel.Reason, "is unavailable") {
		t.Fatalf("dry run %+v %v", sel, err)
	}

	_, err = e.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessCodex, Policy: explicit("acct_synth-own"), RunID: "run_2", SessionID: "ses_2"})
	ce := mustCode(t, err, contracts.ErrCapacityExhausted)
	if !strings.Contains(ce.Message, "own codex login at its limit") {
		t.Fatalf("strict message %q", ce.Message)
	}
	if _, err := e.svc.assignmentForRun(ctx, e.svc.db, "run_2"); err == nil {
		t.Fatal("a strict pin on an at-limit own login got an assignment")
	}
	sel, err = e.svc.Select(ctx, contracts.AccountSelectParams{Provider: contracts.ProviderOpenAI, Policy: explicit("acct_synth-own")})
	if err != nil || sel.Outcome != contracts.FallbackWaiting {
		t.Fatalf("strict dry run %+v %v", sel, err)
	}

	// With no other account the non-strict pin waits instead of taking the
	// limited own login.
	e2 := ownEnvLatest(t, weeklyAtLimit(probe))
	e2.clock.t = probe.clock.t
	_, err = e2.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessCodex, Policy: nonStrict("acct_synth-own"), RunID: "run_3"})
	if err == nil {
		t.Fatal("bound the at-limit own login")
	}
}

// A run on a non-strict own login whose login is refused moves to an
// eligible account (it had no output, so the continuation re-runs the
// request); the refusal is remembered for this node's own login until the
// login file is rewritten, so the next launch does not pick it again.
func TestOwnLoginRefusedMovesRunAndIsAvoided(t *testing.T) {
	e := ownEnv(t)
	ctx := context.Background()
	e.addAccount("acct_synth-code", contracts.HarnessCodex, "code")
	e.observe("acct_synth-code", contracts.ProviderOpenAI, 10, time.Minute)
	auth := filepath.Join(e.svc.cfg.UserCodexDir, "auth.json")
	old := e.clock.Now().Add(-9 * 24 * time.Hour)
	if err := os.Chtimes(auth, old, old); err != nil {
		t.Fatal(err)
	}

	a := e.startRun("ses_1", "run_1", contracts.HarnessCodex, nonStrict("acct_synth-own"), []string{contracts.CapResumeNative, contracts.CapResumeHandoff})
	if a.AccountID != "acct_synth-own" {
		t.Fatalf("first launch %+v", a)
	}
	plan, err := e.svc.HandleLimitEvent(ctx, "ses_1", "run_1", contracts.LimitEvent{Kind: LimitAuthInvalid, Detail: "workspace routing discovery unauthorized (401)"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.ToAccountID != "acct_synth-code" || plan.NewRunID == "" || plan.Outcome != contracts.FallbackHandoff {
		t.Fatalf("plan %+v", plan)
	}
	if len(e.ctrl.continuations) != 1 || !strings.Contains(e.ctrl.continuations[0].Prompt, "login stopped working") {
		t.Fatalf("continuations %+v", e.ctrl.continuations)
	}

	res, err := e.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessCodex, Policy: nonStrict("acct_synth-own"), RunID: "run_9", SessionID: "ses_9"})
	if err != nil || res.Assignment.AccountID != "acct_synth-code" || !strings.Contains(res.Assignment.Reason, "login was refused") {
		t.Fatalf("relaunch %+v %v", res.Assignment, err)
	}
	// Automatic selection skips it too.
	if _, ok := e.svc.ownLoginCandidate(ctx, contracts.HarnessCodex, nil); ok {
		t.Fatal("refused own login offered as a fallback")
	}
	// Signing in again rewrites the login: it is usable again.
	fresh := e.clock.Now().Add(time.Minute)
	if err := os.Chtimes(auth, fresh, fresh); err != nil {
		t.Fatal(err)
	}
	if why, _, blocked := e.svc.ownLoginBlocked(ctx, contracts.HarnessCodex, "acct_synth-own", nil); blocked {
		t.Fatalf("still blocked after a new login: %s", why)
	}
}

// A strict pin on the own login, and a run started on the ambient login
// itself, are not moved.
func TestOwnLoginStrictOrAmbientRunStays(t *testing.T) {
	for _, pol := range []contracts.AccountPolicy{explicit("acct_synth-own"), {Mode: contracts.AccountAmbient}} {
		e := ownEnv(t)
		ctx := context.Background()
		e.addAccount("acct_synth-code", contracts.HarnessCodex, "code")
		e.observe("acct_synth-code", contracts.ProviderOpenAI, 10, time.Minute)
		e.startRun("ses_1", "run_1", contracts.HarnessCodex, pol, nil)
		plan, err := e.svc.HandleLimitEvent(ctx, "ses_1", "run_1", contracts.LimitEvent{Kind: LimitAuthInvalid, Detail: "unauthorized (401)"})
		if err != nil || plan.Outcome != contracts.FallbackIntervention || len(e.ctrl.continuations) != 0 {
			t.Fatalf("%+v: plan %+v %v", pol, plan, err)
		}
	}
}
