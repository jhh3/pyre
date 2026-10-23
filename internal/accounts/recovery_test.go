package accounts

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Recovery after a limit (A12, WP13 defect from the Codex full-stack run):
// a checked-in rota account cannot be observed for Codex (that needs an
// app-server on the profile), so after its cooldown it used to stay
// "unknown" forever: selection refused unknown, and an account that is
// never checked out is never probed. Pyre now verifies such an account at
// checkout: ownership moves into the profile (never a copy), the usage probe
// runs there (never refreshing), and the run proceeds only on a fresh
// eligible result; otherwise the account is checked straight back in.

const codexLimited = `{"rateLimits":{"limitId":"codex","rateLimitReachedType":"rate_limit_reached","primary":{"usedPercent":100,"windowDurationMins":300,"resetsAt":%d},"secondary":{"usedPercent":40,"windowDurationMins":10080,"resetsAt":%d}}}`

func codexLock(snap string) string { return filepath.Join(filepath.Dir(snap), "pyre-lock") }

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// TestCodexRotaRecoversAfterReset is the A12 repro: inject a limit on the
// only Codex account, refuse during the cooldown, then resume on the SAME
// account once the reset has passed.
func TestCodexRotaRecoversAfterReset(t *testing.T) {
	r := newRotaEnv(t, func(c *Config) { c.InjectReset = 5 * time.Minute })
	ctx := context.Background()
	snap := r.writeRotaCodex("X")
	snapBefore := mustRead(t, snap)
	info := r.addRotaAccount("acct_synth-x", contracts.HarnessCodex, "X", true)
	caps := []string{contracts.CapResumeNative, contracts.CapResumeHandoff}

	// Never observed (checked in, unobservable): eligible selection verifies it
	// at checkout instead of refusing "unknown".
	r.svc.ObserveDue(ctx, "")
	if r.reader.count(info.Dir) != 0 {
		t.Fatal("a checked-in codex account was probed (would start codex on rota's home)")
	}
	asg := r.startRun("ses_1", "run_1", contracts.HarnessCodex, eligible, caps)
	if asg.AccountID != "acct_synth-x" || !strings.Contains(asg.Reason, "verified at checkout") {
		t.Fatalf("assignment %+v", asg)
	}
	if r.reader.count(info.Dir) != 1 || !exists(codexLock(snap)) || !exists(filepath.Join(info.Dir, "auth.json")) {
		t.Fatal("verification probe must run from the checked-out profile")
	}

	// Injected limit: waiting (only account), run quiesced, checked in.
	plan, err := r.svc.InjectLimit(ctx, contracts.InjectLimitParams{SessionID: "ses_1", Kind: LimitUsage})
	if err != nil || plan.Outcome != contracts.FallbackWaiting {
		t.Fatalf("plan %+v %v", plan, err)
	}
	if exists(codexLock(snap)) || exists(filepath.Join(info.Dir, "auth.json")) {
		t.Fatal("limited account not checked in")
	}
	cd, _ := getCooldown(ctx, r.svc.db, "acct_synth-x")
	if cd == nil || cd.Until.Sub(r.clock.Now()) != 5*time.Minute {
		t.Fatalf("injected reset %+v (want InjectReset 5m)", cd)
	}

	// During the cooldown: refused, no checkout, no probe.
	_, err = r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessCodex, Policy: eligible, RunID: "run_2", SessionID: "ses_1"})
	if e := mustCode(t, err, contracts.ErrCapacityExhausted); !strings.Contains(e.Message, "cooldown until") {
		t.Fatalf("during cooldown: %v", err)
	}
	if r.reader.count(info.Dir) != 1 || exists(codexLock(snap)) {
		t.Fatal("cooldown account checked out or probed")
	}

	// Reset passes; the observer runs meanwhile (checked in: unknown).
	r.clock.Advance(6 * time.Minute)
	r.svc.ObserveDue(ctx, "")
	res, err := r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessCodex, Policy: eligible, RunID: "run_3", SessionID: "ses_1"})
	if err != nil {
		t.Fatalf("resume after reset: %v", err)
	}
	if res.Assignment.AccountID != "acct_synth-x" || res.Env["CODEX_HOME"] != info.Dir || !strings.Contains(res.Assignment.Reason, "verified at checkout") {
		t.Fatalf("resume binding %+v", res.Assignment)
	}
	if r.reader.count(info.Dir) != 2 || !exists(codexLock(snap)) {
		t.Fatal("resume did not verify from the checked-out profile")
	}
	if string(mustRead(t, filepath.Join(info.Dir, "auth.json"))) != string(snapBefore) {
		t.Fatal("codex login not moved verbatim")
	}
	o := latest(t, r.testEnv, "acct_synth-x")
	if o.Eligibility != contracts.EligibleYes || o.Stale {
		t.Fatalf("verification observation %+v", o)
	}
	// The verification probe left no process lease of its own: only the run's.
	g, _ := r.svc.getGrant(ctx, r.svc.db, "acct_synth-x", contracts.HarnessCodex)
	if l, _ := getLease(ctx, r.svc.db, g.GrantID); l == nil || l.Refcount != 1 {
		t.Fatalf("lease after verification %+v", l)
	}
	if r.fake.refreshes() != 0 {
		t.Fatal("refresh attempted")
	}
	r.svc.Release("run_3")
	if exists(codexLock(snap)) || string(mustRead(t, snap)) != string(snapBefore) {
		t.Fatal("not checked in cleanly")
	}
}

// A verification that finds the account still limited checks it straight
// back in, reports waiting with the provider's reset, and does not check it
// out again before that reset (even after observer polls).
func TestCodexRotaVerifyStillLimited(t *testing.T) {
	r := newRotaEnv(t)
	ctx := context.Background()
	snap := r.writeRotaCodex("X")
	info := r.addRotaAccount("acct_synth-x", contracts.HarnessCodex, "X", true)
	reset := r.clock.Now().Add(40 * time.Minute)
	r.reader.resp[info.Dir] = fmt.Sprintf(codexLimited, reset.Unix(), reset.Unix())

	_, err := r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessCodex, Policy: eligible, RunID: "run_1"})
	e := mustCode(t, err, contracts.ErrCapacityExhausted)
	if e.Details["outcome"] != string(contracts.FallbackWaiting) || !strings.Contains(e.Message, "verified at checkout") {
		t.Fatalf("still limited: %+v", e)
	}
	if exists(codexLock(snap)) || exists(filepath.Join(info.Dir, "auth.json")) {
		t.Fatal("failed verification left the account checked out")
	}
	if a, err := r.svc.assignmentForRun(ctx, r.svc.db, "run_1"); err == nil {
		t.Fatalf("never-launched assignment kept: %+v", a)
	}
	g, _ := r.svc.getGrant(ctx, r.svc.db, "acct_synth-x", contracts.HarnessCodex)
	if l, _ := getLease(ctx, r.svc.db, g.GrantID); l != nil {
		t.Fatalf("lease leaked %+v", l)
	}
	if o := latest(t, r.testEnv, "acct_synth-x"); o.Eligibility != contracts.EligibleExhausted || o.ResetAt == nil {
		t.Fatalf("observation %+v", o)
	}
	// Observer polls while checked in must not erase the verified reset.
	r.clock.Advance(10 * time.Minute)
	r.svc.ObserveDue(ctx, "")
	if _, err := r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessCodex, Policy: eligible, RunID: "run_2"}); !contracts.IsCode(err, contracts.ErrCapacityExhausted) {
		t.Fatalf("before reset: %v", err)
	}
	if r.reader.count(info.Dir) != 1 {
		t.Fatalf("re-verified before the known reset: %d probes", r.reader.count(info.Dir))
	}
	// After the reset it is verified again and launches.
	delete(r.reader.resp, info.Dir)
	r.clock.Advance(31 * time.Minute)
	if _, err := r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessCodex, Policy: eligible, RunID: "run_3"}); err != nil {
		t.Fatalf("after reset: %v", err)
	}
}

// An expired access token in the rota snapshot cannot be verified without a
// refresh, which a probe never does: no checkout, no probe, unknown stays
// unknown for eligible selection (explicit --account still launches).
func TestCodexRotaVerifyNeedsValidToken(t *testing.T) {
	r := newRotaEnv(t)
	ctx := context.Background()
	snap := r.writeRotaCodex("X")
	loginCodex(t, filepath.Dir(snap), "X", true) // expired access token at rest
	info := r.addRotaAccount("acct_synth-x", contracts.HarnessCodex, "X", true)
	_, err := r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessCodex, Policy: eligible, RunID: "run_1"})
	e := mustCode(t, err, contracts.ErrCapacityExhausted)
	if !strings.Contains(e.Message, "access token expired") {
		t.Fatalf("reason must name the expired token: %s", e.Message)
	}
	if r.reader.count(info.Dir) != 0 || exists(codexLock(snap)) || r.fake.refreshes() != 0 {
		t.Fatal("expired snapshot was checked out, probed or refreshed")
	}
	if _, err := r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessCodex, Policy: explicit("acct_synth-x"), RunID: "run_2"}); err != nil {
		t.Fatalf("explicit account must still launch (the harness refreshes in the profile): %v", err)
	}
	if r.reader.count(info.Dir) != 0 {
		t.Fatal("expired token probed")
	}
}

// Fallback (HandleLimitEvent) may pick a checked-in Codex account as the
// successor; the successor's explicit bind verifies it, and a limited one is
// skipped.
func TestFallbackToCheckedInCodexVerifies(t *testing.T) {
	r := newRotaEnv(t)
	ctx := context.Background()
	r.writeRotaCodex("X")
	r.writeRotaCodex("Y")
	r.writeRotaCodex("Z")
	r.svc.cfg.Rota.Independent = map[string]bool{"acct_synth-x": true, "acct_synth-y": true, "acct_synth-z": true}
	r.addRotaAccount("acct_synth-x", contracts.HarnessCodex, "X", false)
	py := r.addRotaAccount("acct_synth-y", contracts.HarnessCodex, "Y", false)
	pz := r.addRotaAccount("acct_synth-z", contracts.HarnessCodex, "Z", false)
	reset := r.clock.Now().Add(2 * time.Hour)
	r.reader.resp[py.Dir] = fmt.Sprintf(codexLimited, reset.Unix(), reset.Unix())
	r.startRun("ses_1", "run_1", contracts.HarnessCodex, explicit("acct_synth-x"), []string{contracts.CapResumeNative, contracts.CapResumeHandoff})
	plan, err := r.svc.HandleLimitEvent(ctx, "ses_1", "run_1", contracts.LimitEvent{Kind: LimitUsage})
	if err != nil || plan.Outcome != contracts.FallbackHandoff || plan.ToAccountID != "acct_synth-z" {
		t.Fatalf("plan %+v %v", plan, err)
	}
	if r.reader.count(py.Dir) != 1 || r.reader.count(pz.Dir) != 1 {
		t.Fatalf("probes y=%d z=%d", r.reader.count(py.Dir), r.reader.count(pz.Dir))
	}
}

// Claude: a checked-in rota account is probed with the snapshot token, so it
// recovers by itself, but the poll schedule must not delay recovery past the
// reset: a passed reset makes the account due for a probe at selection time.
// An expired snapshot token stays unknown (probes never refresh).
func TestClaudeRotaRecoversAfterResetWithoutWaitingForPoll(t *testing.T) {
	r := newRotaEnv(t, func(c *Config) { c.InjectReset = 3 * time.Minute; c.PollInterval = time.Hour })
	ctx := context.Background()
	r.writeRotaClaude("A", "g1", time.Now().Add(4*time.Hour))
	r.writeRotaClaude("E", "g1", time.Now().Add(-time.Hour))
	pa := r.addRotaAccount("acct_synth-a", contracts.HarnessClaude, "A", false)
	r.svc.cfg.Rota.Independent = map[string]bool{"acct_synth-a": true, "acct_synth-e": true}
	r.addRotaAccount("acct_synth-e", contracts.HarnessClaude, "E", false)
	r.svc.ObserveDue(ctx, "")
	asg := r.startRun("ses_1", "run_1", contracts.HarnessClaude, eligible, claudeCaps)
	if asg.AccountID != "acct_synth-a" {
		t.Fatalf("assignment %+v", asg)
	}
	plan, err := r.svc.InjectLimit(ctx, contracts.InjectLimitParams{SessionID: "ses_1"})
	if err != nil || plan.Outcome != contracts.FallbackWaiting || !strings.Contains(plan.Reason, "acct_synth-e") {
		t.Fatalf("plan %+v %v", plan, err)
	}
	lock := filepath.Join(r.claudeAcc, rotaName("A")+".pyre-lock")
	if exists(lock) {
		t.Fatal("not checked in")
	}
	callsBefore := r.fake.calls("A")
	r.clock.Advance(4 * time.Minute) // past the reset, far inside the 1h poll interval
	res, err := r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: eligible, RunID: "run_2", SessionID: "ses_1"})
	if err != nil || res.ProfileDir != pa.Dir {
		t.Fatalf("resume after reset: %+v %v", res.Assignment, err)
	}
	if r.fake.calls("A") != callsBefore+1 || r.fake.calls("E") != 0 || r.fake.refreshes() != 0 {
		t.Fatalf("probes A=%d (before %d) E=%d refresh=%d", r.fake.calls("A"), callsBefore, r.fake.calls("E"), r.fake.refreshes())
	}
	if !exists(lock) {
		t.Fatal("not checked out")
	}
	// The expired account remains unknown with an explicit reason.
	o := latest(t, r.testEnv, "acct_synth-e")
	if o.Eligibility != contracts.EligibleUnknown || !strings.Contains(o.Detail, "never refreshes") {
		t.Fatalf("expired snapshot observation %+v", o)
	}
	var sel contracts.AccountSelection
	sel, err = r.svc.Select(ctx, contracts.AccountSelectParams{Provider: contracts.ProviderAnthropic, Policy: contracts.AccountPolicy{Mode: contracts.AccountExplicit, AccountID: "acct_synth-e", Strict: true}})
	if err != nil || sel.AccountID != "acct_synth-e" {
		b, _ := json.Marshal(sel)
		t.Fatalf("explicit select of expired account: %s %v", b, err)
	}
}
