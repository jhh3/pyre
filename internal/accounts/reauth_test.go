package accounts

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/usage"
)

// The server incident (2026-10-06): the only rota-held Codex account's login
// had expired; checkout verification got "wham/usage failed: 401
// Unauthorized" and the launch failed with "no eligible openai
// subscription". A rejected login is now "needs re-auth" (with the fix),
// held in a cooldown, and the launch falls through to the node's own login.

var err401 = errors.New("account/rateLimits/read: wham/usage failed: 401 Unauthorized (synthetic)")

func setMtime(t *testing.T, p string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(p, at, at); err != nil {
		t.Fatal(err)
	}
}

func TestCodexRejectedLoginNeedsReauthAndFallsBackToOwnLogin(t *testing.T) {
	r := newRotaEnv(t)
	ctx := context.Background()
	snap := r.writeRotaCodex("X")
	setMtime(t, snap, r.clock.Now().Add(-time.Hour))
	info := r.addRotaAccount("acct_synth-x", contracts.HarnessCodex, "X", true)
	r.reader.errs = map[string]error{info.Dir: err401}

	res, err := r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessCodex, Policy: eligible, RunID: "run_1", SessionID: "ses_1"})
	if err != nil {
		t.Fatalf("a dead login must not fail the launch: %v", err)
	}
	a := res.Assignment
	if res.CredentialKind != contracts.CredAmbient || a.AccountID != "" ||
		!strings.Contains(a.Reason, "your own login") || !strings.Contains(a.Reason, "X needs re-auth") ||
		!strings.Contains(a.Reason, "pyre accounts login --node") {
		t.Fatalf("fallback assignment kind=%s %+v", res.CredentialKind, a)
	}
	if r.reader.count(info.Dir) != 1 || exists(codexLock(snap)) {
		t.Fatal("verification probe once from the profile, then checked straight back in")
	}
	o := latest(t, r.testEnv, "acct_synth-x")
	if o.Eligibility != contracts.EligibleUnusable || !strings.Contains(o.Detail, "pyre accounts login --node") {
		t.Fatalf("observation %+v", o)
	}
	if as := usage.Assess(contracts.Account{AccountID: "acct_synth-x", Provider: contracts.ProviderOpenAI}, &o, nil, r.clock.Now()); as.State != usage.NeedsReauth {
		t.Fatalf("shown as %v (%s)", as.State, as.Why)
	}
	cd, _ := getCooldown(ctx, r.svc.db, "acct_synth-x")
	if cd == nil || cd.Reason != reauthCooldownReason || cd.Until.Sub(r.clock.Now()) != time.Hour {
		t.Fatalf("cooldown %+v", cd)
	}

	// Next launches during the cooldown: own login at once, no checkout or
	// probe, even after observer polls (the checked-in hold keeps the state).
	r.clock.Advance(10 * time.Minute)
	r.svc.ObserveDue(ctx, "")
	res, err = r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessCodex, Policy: eligible, RunID: "run_2", SessionID: "ses_2"})
	if err != nil || res.CredentialKind != contracts.CredAmbient {
		t.Fatalf("second launch: %v %+v", err, res.Assignment)
	}
	if r.reader.count(info.Dir) != 1 || exists(codexLock(snap)) {
		t.Fatalf("dead login retried during its cooldown: %d probes", r.reader.count(info.Dir))
	}
	if o := latest(t, r.testEnv, "acct_synth-x"); o.Eligibility != contracts.EligibleUnusable {
		t.Fatalf("poll replaced needs re-auth: %+v", o)
	}

	// The owner logs in again (rota snapshot rewritten): the hold ends at the
	// next poll and the account is verified and used again.
	r.clock.Advance(5 * time.Minute)
	setMtime(t, snap, r.clock.Now())
	r.reader.errs = nil
	r.clock.Advance(time.Minute)
	r.svc.ObserveDue(ctx, "")
	res, err = r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessCodex, Policy: eligible, RunID: "run_3", SessionID: "ses_3"})
	if err != nil || res.Assignment.AccountID != "acct_synth-x" || !strings.Contains(res.Assignment.Reason, "verified at checkout") {
		t.Fatalf("after re-login: %v %+v", err, res.Assignment)
	}
	if cd, _ := getCooldown(ctx, r.svc.db, "acct_synth-x"); cd != nil {
		t.Fatalf("re-auth cooldown kept after re-login: %+v", cd)
	}
	r.svc.Release("run_3")
}

// With another usable account the rejected one is skipped for it.
func TestCodexRejectedLoginFallsThroughToNextAccount(t *testing.T) {
	r := newRotaEnv(t)
	ctx := context.Background()
	sx := r.writeRotaCodex("X")
	sy := r.writeRotaCodex("Y")
	setMtime(t, sx, r.clock.Now().Add(-time.Hour))
	setMtime(t, sy, r.clock.Now().Add(-time.Hour))
	r.svc.cfg.Rota.Independent = map[string]bool{"acct_synth-x": true, "acct_synth-y": true}
	px := r.addRotaAccount("acct_synth-x", contracts.HarnessCodex, "X", false)
	py := r.addRotaAccount("acct_synth-y", contracts.HarnessCodex, "Y", false)
	r.reader.errs = map[string]error{px.Dir: err401}
	pol := contracts.AccountPolicy{Mode: contracts.AccountEligible, Prefer: []string{"acct_synth-x"}}
	res, err := r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessCodex, Policy: pol, RunID: "run_1"})
	if err != nil || res.Assignment.AccountID != "acct_synth-y" {
		t.Fatalf("want fall-through to Y: %v %+v", err, res.Assignment)
	}
	if r.reader.count(px.Dir) != 1 || r.reader.count(py.Dir) != 1 {
		t.Fatalf("probes x=%d y=%d", r.reader.count(px.Dir), r.reader.count(py.Dir))
	}
	// The next launch does not try X again.
	r.svc.Release("run_1")
	if _, err := r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessCodex, Policy: pol, RunID: "run_2"}); err != nil {
		t.Fatal(err)
	}
	if r.reader.count(px.Dir) != 1 {
		t.Fatal("rejected login re-probed during its cooldown")
	}
}

// --default-account subscription never uses the node's own login: the
// launch fails as an intervention that names the fix.
func TestCodexRejectedLoginSubscriptionDefaultNamesFix(t *testing.T) {
	r := newRotaEnv(t, func(c *Config) { c.DefaultAccount = DefaultAccountSubscription })
	ctx := context.Background()
	snap := r.writeRotaCodex("X")
	setMtime(t, snap, r.clock.Now().Add(-time.Hour))
	info := r.addRotaAccount("acct_synth-x", contracts.HarnessCodex, "X", true)
	r.reader.errs = map[string]error{info.Dir: err401}
	_, err := r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessCodex, Policy: eligible, RunID: "run_1"})
	e := mustCode(t, err, contracts.ErrUnavailable)
	if e.Details["outcome"] != string(contracts.FallbackIntervention) || !strings.Contains(e.Message, "pyre accounts login --node") {
		t.Fatalf("%+v", e)
	}
}

func TestAuthRejected(t *testing.T) {
	for _, m := range []string{"wham/usage failed: 401 Unauthorized", "Your access token could not be refreshed; please sign in again",
		"refresh_token_reused", "refresh token expired"} {
		if !authRejected(m) {
			t.Errorf("%q not classified as rejected", m)
		}
	}
	for _, m := range []string{"dial unix: connection refused", "429 Too Many Requests", "context deadline exceeded"} {
		if authRejected(m) {
			t.Errorf("%q classified as rejected", m)
		}
	}
}
