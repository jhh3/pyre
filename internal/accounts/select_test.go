package accounts

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

var eligible = contracts.AccountPolicy{Mode: contracts.AccountEligible}

func explicit(id string) contracts.AccountPolicy {
	return contracts.AccountPolicy{Mode: contracts.AccountExplicit, AccountID: id}
}

// A11 (synthetic, one node): two accounts of one provider run concurrently
// with different profiles/env; the default login is untouched.
func TestTwoAccountsSameProviderDifferentProfiles(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	defaults := fileState(t, filepath.Dir(e.userClaude))
	pa := e.addAccount("acct_synth-a", contracts.HarnessClaude, "A")
	pb := e.addAccount("acct_synth-b", contracts.HarnessClaude, "B")
	e.observe("acct_synth-a", contracts.ProviderAnthropic, 10, time.Minute)
	e.observe("acct_synth-b", contracts.ProviderAnthropic, 50, time.Minute)

	dirA, envA, asgA, err := e.svc.Resolve(ctx, contracts.HarnessClaude, eligible, "run_1")
	if err != nil {
		t.Fatal(err)
	}
	dirB, envB, asgB, err := e.svc.Resolve(ctx, contracts.HarnessClaude, explicit("acct_synth-b"), "run_2")
	if err != nil {
		t.Fatal(err)
	}
	if asgA.AccountID != "acct_synth-a" || asgB.AccountID != "acct_synth-b" {
		t.Fatalf("assignments %s %s", asgA.AccountID, asgB.AccountID)
	}
	if !strings.HasPrefix(asgB.Reason, "explicit account (") {
		t.Fatalf("explicit reason: %q", asgB.Reason)
	}
	if dirA != pa.Dir || dirB != pb.Dir || dirA == dirB {
		t.Fatalf("profiles %q %q", dirA, dirB)
	}
	if envA["CLAUDE_CONFIG_DIR"] != pa.Dir || envB["CLAUDE_CONFIG_DIR"] != pb.Dir || len(envA) != 2 || envA["DISABLE_AUTOUPDATER"] != "1" {
		t.Fatalf("env %v %v", envA, envB)
	}
	if ClaudeKeychainService(dirA) == ClaudeKeychainService(dirB) {
		t.Fatal("keychain items would collide")
	}
	if asgA.GrantGeneration != 1 || asgA.Freshness.Stale {
		t.Fatalf("assignment %+v", asgA)
	}
	// Each profile's local identity is its own account.
	if id, _ := claudeIdentity(dirA); id != claudeUUID("A") {
		t.Fatal("A identity")
	}
	if id, _ := claudeIdentity(dirB); id != claudeUUID("B") {
		t.Fatal("B identity")
	}
	res, err := e.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: eligible, RunID: "run_1"})
	if err != nil || res.Assignment.AssignmentID != asgA.AssignmentID {
		t.Fatalf("resolve must be idempotent per run: %v", err)
	}
	if !contains(res.Unset, "ANTHROPIC_API_KEY") || !contains(res.Unset, "CLAUDE_CODE_OAUTH_TOKEN") || !contains(res.Unset, "CLAUDE_SECURESTORAGE_CONFIG_DIR") {
		t.Fatalf("launcher must strip API-billing/override vars: %v", res.Unset)
	}
	// Switching/recovering one session does not touch the other's binding.
	e.svc.Release("run_1")
	if err := e.svc.CheckAssignment(ctx, "run_2"); err != nil {
		t.Fatal(err)
	}
	sameState(t, "default login", defaults, fileState(t, filepath.Dir(e.userClaude)))
	if e.fake.refreshes() != 0 {
		t.Fatal("refresh endpoint called")
	}
}

func TestSimultaneousSelectionRace(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.MaxAssignmentsPerAccount = 1 })
	ctx := context.Background()
	ids := []string{"acct_synth-a", "acct_synth-b", "acct_synth-c"}
	for i, id := range ids {
		e.addAccount(id, contracts.HarnessClaude, string(rune('A'+i)))
		e.observe(id, contracts.ProviderAnthropic, 10, time.Minute)
	}
	// A second Service (separate connection pool) on the same database file
	// models another selector racing through SQLite.
	db2, err := sqlitex.Open(e.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	cfg2 := e.cfg
	cfg2.DB = db2
	svc2, err := New(ctx, cfg2)
	if err != nil {
		t.Fatal(err)
	}
	const n = 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	got := map[string]string{}
	var waiting int
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			svc := e.svc
			if i%2 == 1 {
				svc = svc2
			}
			_, _, asg, err := svc.Resolve(ctx, contracts.HarnessClaude, eligible, fmt.Sprintf("run_%d", i))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if !contracts.IsCode(err, contracts.ErrCapacityExhausted) {
					t.Errorf("unexpected error: %v", err)
				}
				waiting++
				return
			}
			if prev, dup := got[asg.AccountID]; dup {
				t.Errorf("account %s assigned twice (%s and run_%d)", asg.AccountID, prev, i)
			}
			got[asg.AccountID] = asg.RunID
		}(i)
	}
	wg.Wait()
	if len(got) != 3 || waiting != n-3 {
		t.Fatalf("got %d assignments (%v), %d waiting", len(got), got, waiting)
	}
}

func TestCodexLeaseConflict(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	px := e.addAccount("acct_synth-x", contracts.HarnessCodex, "X")
	e.observe("acct_synth-x", contracts.ProviderOpenAI, 10, time.Minute)

	// Runs through the shared per-home app-server share its single lease.
	r1, err := e.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessCodex, Policy: eligible, RunID: "run_1"})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := e.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessCodex, Policy: eligible, RunID: "run_2"})
	if err != nil {
		t.Fatal(err)
	}
	if r1.Env["CODEX_HOME"] != px.Dir || r2.ProfileDir != px.Dir || r1.Assignment.Lease != "codex-app-server" {
		t.Fatalf("codex binding %+v", r1)
	}
	l, _ := getLease(ctx, e.svc.db, r1.Grant.GrantID)
	if l == nil || l.Refcount != 2 {
		t.Fatalf("lease %+v", l)
	}
	// A second process on the same home (another holder) conflicts.
	_, err = e.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessCodex, Policy: explicit("acct_synth-x"), RunID: "run_3", LeaseHolder: "run:run_3"})
	ce := mustCode(t, e.err(err), contracts.ErrConflict)
	if ce.Details["outcome"] != "intervention" {
		t.Fatalf("details %v", ce.Details)
	}
	// Eligible policy routes around the held home to another account.
	py := e.addAccount("acct_synth-y", contracts.HarnessCodex, "Y")
	e.observe("acct_synth-y", contracts.ProviderOpenAI, 60, time.Minute)
	r3, err := e.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessCodex, Policy: eligible, RunID: "run_3", LeaseHolder: "run:run_3"})
	if err != nil || r3.ProfileDir != py.Dir {
		t.Fatalf("expected Y, got %+v %v", r3.Assignment, err)
	}
	// Releasing both shared runs frees the home.
	e.svc.Release("run_1")
	e.svc.Release("run_1") // idempotent
	e.svc.Release("run_2")
	if l, _ := getLease(ctx, e.svc.db, r1.Grant.GrantID); l != nil {
		t.Fatalf("lease still held: %+v", l)
	}
	if _, err := e.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessCodex, Policy: explicit("acct_synth-x"), RunID: "run_4", LeaseHolder: "run:run_4"}); err != nil {
		t.Fatal(err)
	}
}

func TestStaleUsageIsNotFresh(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.CodexReader = nil })
	ctx := context.Background()
	e.addAccount("acct_synth-a", contracts.HarnessClaude, "A")
	e.addAccount("acct_synth-b", contracts.HarnessClaude, "B")
	// A: lots of headroom but 20 minutes old (fresh window 15m). B: none.
	e.observe("acct_synth-a", contracts.ProviderAnthropic, 1, 20*time.Minute)
	// Make the on-demand probe unable to refresh A's data.
	e.fake.status["A"] = 503
	e.fake.status["B"] = 503
	_, _, _, err := e.svc.Resolve(ctx, contracts.HarnessClaude, eligible, "run_1")
	ce := mustCode(t, e.err(err), contracts.ErrCapacityExhausted)
	if ce.Details["outcome"] != "waiting" {
		t.Fatalf("stale/unknown must mean waiting, got %v", ce.Details)
	}
	sel, err := e.svc.Select(ctx, contracts.AccountSelectParams{Provider: contracts.ProviderAnthropic, Policy: eligible})
	if err != nil || sel.Outcome != contracts.FallbackWaiting || sel.AccountID != "" {
		t.Fatalf("select %+v %v", sel, err)
	}
	// Fresh beats stale even with less headroom.
	e.clock.Advance(time.Second)
	e.observe("acct_synth-b", contracts.ProviderAnthropic, 80, 0)
	_, _, asg, err := e.svc.Resolve(ctx, contracts.HarnessClaude, eligible, "run_2")
	if err != nil || asg.AccountID != "acct_synth-b" {
		t.Fatalf("want fresh B, got %+v %v", asg, err)
	}
	// Explicit selection of an unknown account is the owner's choice and is labelled stale.
	_, _, asg, err = e.svc.Resolve(ctx, contracts.HarnessClaude, explicit("acct_synth-a"), "run_3")
	if err != nil || !asg.Freshness.Stale {
		t.Fatalf("explicit unknown: %+v %v", asg, err)
	}
	// Unknown is allowed for eligible policy only when configured.
	e2 := newEnv(t, func(c *Config) { c.AllowUnknown = true })
	e2.addAccount("acct_synth-a", contracts.HarnessClaude, "A")
	e2.fake.status["A"] = 503
	_, _, asg, err = e2.svc.Resolve(ctx, contracts.HarnessClaude, eligible, "run_1")
	if err != nil || !asg.Freshness.Stale {
		t.Fatalf("AllowUnknown: %+v %v", asg, err)
	}
}

func TestExhaustedAndCooldownExcluded(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addAccount("acct_synth-a", contracts.HarnessClaude, "A")
	e.addAccount("acct_synth-b", contracts.HarnessClaude, "B")
	e.observe("acct_synth-a", contracts.ProviderAnthropic, 100, time.Minute)
	// The on-demand probe sees the same: A exhausted (no reset hint).
	e.fake.usage["A"] = `{"five_hour":{"utilization":100},"seven_day":{"utilization":40}}`
	reset := e.clock.Now().Add(2 * time.Hour)
	putCooldown(ctx, e.svc.db, "acct_synth-b", reset, "usage_limit")
	e.observe("acct_synth-b", contracts.ProviderAnthropic, 5, time.Minute)
	_, _, _, err := e.svc.Resolve(ctx, contracts.HarnessClaude, eligible, "run_1")
	ce := mustCode(t, e.err(err), contracts.ErrCapacityExhausted)
	if ce.Details["earliest_reset"] != reset.Format(time.RFC3339) {
		t.Fatalf("earliest reset %v", ce.Details)
	}
	_, _, _, err = e.svc.Resolve(ctx, contracts.HarnessClaude, explicit("acct_synth-b"), "run_2")
	mustCode(t, e.err(err), contracts.ErrCapacityExhausted)
	e.clock.Advance(3 * time.Hour)
	e.observe("acct_synth-b", contracts.ProviderAnthropic, 5, time.Minute)
	if _, _, asg, err := e.svc.Resolve(ctx, contracts.HarnessClaude, eligible, "run_3"); err != nil || asg.AccountID != "acct_synth-b" {
		t.Fatalf("after reset: %v", err)
	}
}

func TestPreferOrder(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addAccount("acct_synth-a", contracts.HarnessClaude, "A")
	e.addAccount("acct_synth-b", contracts.HarnessClaude, "B")
	e.observe("acct_synth-a", contracts.ProviderAnthropic, 10, time.Minute)
	e.observe("acct_synth-b", contracts.ProviderAnthropic, 70, time.Minute)
	pol := contracts.AccountPolicy{Mode: contracts.AccountEligible, Prefer: []string{"acct_synth-b"}}
	_, _, asg, err := e.svc.Resolve(ctx, contracts.HarnessClaude, pol, "run_1")
	if err != nil || asg.AccountID != "acct_synth-b" {
		t.Fatalf("preferred account not chosen: %+v %v", asg, err)
	}
}

func TestNoCrossProvider(t *testing.T) {
	// subscription: the node never falls back to its own login (auto would
	// run on the ambient login here, as no Claude account exists).
	e := newEnv(t, func(c *Config) { c.DefaultAccount = DefaultAccountSubscription })
	ctx := context.Background()
	e.addAccount("acct_synth-x", contracts.HarnessCodex, "X")
	e.observe("acct_synth-x", contracts.ProviderOpenAI, 0, time.Minute)
	_, _, _, err := e.svc.Resolve(ctx, contracts.HarnessClaude, explicit("acct_synth-x"), "run_1")
	mustCode(t, e.err(err), contracts.ErrInvalid)
	// Eligible Claude selection never considers OpenAI accounts.
	_, _, _, err = e.svc.Resolve(ctx, contracts.HarnessClaude, eligible, "run_2")
	if err == nil {
		t.Fatal("claude run bound to a codex account")
	}
	ce := contracts.AsError(e.err(err))
	if ce.Details["outcome"] != "intervention" {
		t.Fatalf("no anthropic accounts at all => intervention, got %v", ce.Details)
	}
	if _, err := e.svc.Select(ctx, contracts.AccountSelectParams{Provider: contracts.ProviderAnthropic, Policy: explicit("acct_synth-x")}); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("select cross-provider: %v", err)
	}
	// Non-subscription (API billing) accounts are never selected.
	if err := e.svc.UpsertAccount(ctx, contracts.Account{AccountID: "acct_synth-api", Provider: contracts.ProviderAnthropic, Kind: "api"}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := e.svc.Resolve(ctx, contracts.HarnessClaude, eligible, "run_3"); err == nil {
		t.Fatal("API billing account selected")
	}
}

func TestIdentityMismatchRefused(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	// Account expects A's uuid but the profile is logged in as B.
	info := e.addAccount("acct_synth-a", contracts.HarnessClaude, "B", claudeUUID("A"))
	e.observe("acct_synth-a", contracts.ProviderAnthropic, 10, time.Minute)
	_, _, _, err := e.svc.Resolve(ctx, contracts.HarnessClaude, explicit("acct_synth-a"), "run_1")
	if err == nil {
		t.Fatal("mismatched identity launched")
	}
	ce := contracts.AsError(e.err(err))
	if ce.Details["outcome"] != "intervention" {
		t.Fatalf("details %v", ce.Details)
	}
	g, _ := e.svc.getGrant(ctx, e.svc.db, "acct_synth-a", contracts.HarnessClaude)
	if g.State == contracts.GrantValid {
		t.Fatal("grant became valid despite mismatch")
	}
	// Codex: id decoded locally from auth.json.
	e.addAccount("acct_synth-x", contracts.HarnessCodex, "Y", codexAcct("X"))
	_, _, _, err = e.svc.Resolve(ctx, contracts.HarnessCodex, explicit("acct_synth-x"), "run_2")
	if err == nil {
		t.Fatal("codex identity mismatch launched")
	}
	// Missing ProviderRef: refused until the owner confirms (BindIdentity).
	a := contracts.Account{AccountID: "acct_synth-c", Provider: contracts.ProviderAnthropic, Kind: "subscription"}
	pc, err := e.svc.CreateProfile(ctx, a, contracts.HarnessClaude)
	if err != nil {
		t.Fatal(err)
	}
	loginClaude(t, pc.Dir, "C", false)
	if _, err := e.svc.VerifyGrant(ctx, "acct_synth-c", contracts.HarnessClaude); err == nil {
		t.Fatal("unbound identity verified")
	}
	ref, err := e.svc.BindIdentity(ctx, "acct_synth-c", contracts.HarnessClaude)
	if err != nil || ref != claudeUUID("C") {
		t.Fatalf("bind %q %v", ref, err)
	}
	if _, err := e.svc.VerifyGrant(ctx, "acct_synth-c", contracts.HarnessClaude); err != nil {
		t.Fatal(err)
	}
	// BindIdentity never overwrites a different confirmed identity.
	if _, err := e.svc.BindIdentity(ctx, "acct_synth-a", contracts.HarnessClaude); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("rebind: %v", err)
	}
	_ = info
}

func TestAPIKeyLoginRefused(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := contracts.Account{AccountID: "acct_synth-x", Provider: contracts.ProviderOpenAI, Kind: "subscription", ProviderRef: codexAcct("X")}
	info, err := e.svc.CreateProfile(ctx, a, contracts.HarnessCodex)
	if err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(info.Dir, "auth.json"), map[string]any{"OPENAI_API_KEY": "pyre-synthetic-X-apikey", "auth_mode": "apikey"}, 0o600)
	if _, err := e.svc.VerifyGrant(ctx, a.AccountID, contracts.HarnessCodex); err == nil {
		t.Fatal("API-key login accepted as a subscription")
	}
}

func TestDeadGrantNoRetriesAndGeneration(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	pa := e.addAccount("acct_synth-a", contracts.HarnessClaude, "A")
	e.observe("acct_synth-a", contracts.ProviderAnthropic, 10, time.Minute)
	_, _, asg, err := e.svc.Resolve(ctx, contracts.HarnessClaude, eligible, "run_1")
	if err != nil {
		t.Fatal(err)
	}
	// Claude blanks the profile store after a rejected refresh.
	writeJSON(t, filepath.Join(pa.Dir, ".credentials.json"), map[string]any{}, 0o600)
	// Never selected; the auto default then runs on the node's own login
	// (a login that needs re-auth counts as no account; reauth.go).
	_, _, asg2, err := e.svc.Resolve(ctx, contracts.HarnessClaude, eligible, "run_2")
	if err != nil || asg2 == nil || asg2.AccountID != "" {
		t.Fatalf("dead login selected or launch failed: %v %+v", err, asg2)
	}
	g, _ := e.svc.getGrant(ctx, e.svc.db, "acct_synth-a", contracts.HarnessClaude)
	if g.State != contracts.GrantDead {
		t.Fatalf("grant state %s, want dead", g.State)
	}
	if len(e.rep.of(contracts.ReportGrant)) == 0 {
		t.Fatal("dead grant not reported")
	}
	// No retries: probes skip it entirely.
	e.clock.Advance(time.Hour)
	e.svc.ObserveDue(ctx, "")
	e.clock.Advance(time.Hour)
	e.svc.ObserveDue(ctx, "")
	if e.fake.calls("A") != 0 || e.fake.refreshes() != 0 {
		t.Fatalf("dead grant probed %d times, refresh %d", e.fake.calls("A"), e.fake.refreshes())
	}
	// Even if the store looks populated again, a dead grant needs Reverify.
	loginClaude(t, pa.Dir, "A", false)
	if _, err := e.svc.VerifyGrant(ctx, "acct_synth-a", contracts.HarnessClaude); err == nil {
		t.Fatal("dead grant auto-revived")
	}
	g2, err := e.svc.Reverify(ctx, "acct_synth-a", contracts.HarnessClaude)
	if err != nil || g2.State != contracts.GrantValid || g2.Generation != asg.GrantGeneration+1 {
		t.Fatalf("reverify %+v %v", g2, err)
	}
	// The old assignment pinned the previous generation and is rejected.
	mustCode(t, e.err(e.svc.CheckAssignment(ctx, "run_1")), contracts.ErrConflict)
}

func TestAmbientPolicy(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	dir, env, asg, err := e.svc.Resolve(ctx, contracts.HarnessClaude, contracts.AccountPolicy{Mode: contracts.AccountAmbient}, "run_1")
	if err != nil || dir != "" || len(env) != 0 || asg == nil || asg.AccountID != "" {
		t.Fatalf("ambient: %q %v %+v %v", dir, env, asg, err)
	}
	r, _ := e.svc.assignmentForRun(ctx, e.svc.db, "run_1")
	if r.CredentialKind != contracts.CredAmbient {
		t.Fatalf("kind %s", r.CredentialKind)
	}
	res, _ := e.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: contracts.AccountPolicy{Mode: contracts.AccountAmbient}, RunID: "run_1"})
	if !contains(res.Unset, "ANTHROPIC_API_KEY") || contains(res.Unset, "CLAUDE_CONFIG_DIR") {
		t.Fatalf("ambient unset %v", res.Unset)
	}
	// Shell runs have no subscription binding.
	if dir, env, asg, err := e.svc.Resolve(ctx, contracts.HarnessShell, eligible, "run_2"); err != nil || dir != "" || env != nil || asg != nil {
		t.Fatal("shell should resolve to nothing")
	}
}

func TestMissingProfileDir(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	pa := e.addAccount("acct_synth-a", contracts.HarnessClaude, "A")
	e.observe("acct_synth-a", contracts.ProviderAnthropic, 10, time.Minute)
	os.RemoveAll(pa.Dir)
	if _, _, _, err := e.svc.Resolve(ctx, contracts.HarnessClaude, eligible, "run_1"); err == nil {
		t.Fatal("missing profile launched")
	}
}

// A node with no Pyre account for a harness runs launches without an
// explicit account on its own login (auto); a node with accounts keeps
// subscription selection and never silently falls back.
func TestDefaultAccountPolicy(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.addAccount("acct_synth-x", contracts.HarnessCodex, "X")
	_, _, asg, err := e.svc.Resolve(ctx, contracts.HarnessClaude, eligible, "run_1")
	if err != nil || asg == nil || asg.AccountID != "" || !strings.Contains(asg.Reason, "no Pyre anthropic accounts") {
		t.Fatalf("auto, no claude accounts: %+v %v", asg, err)
	}
	if a, _ := e.svc.assignmentForRun(ctx, e.svc.db, "run_1"); a.CredentialKind != contracts.CredAmbient {
		t.Fatalf("assignment kind %s", a.CredentialKind)
	}
	sel, err := e.svc.Select(ctx, contracts.AccountSelectParams{Provider: contracts.ProviderAnthropic, Policy: eligible})
	if err != nil || sel.AccountID != "" || sel.Outcome != "" || !strings.Contains(sel.Reason, "your own login") {
		t.Fatalf("auto preview: %+v %v", sel, err)
	}
	// With a Claude account here, an exhausted one waits, never ambient.
	e.addAccount("acct_synth-a", contracts.HarnessClaude, "A")
	e.fake.status["A"] = 503
	e.observe("acct_synth-a", contracts.ProviderAnthropic, 100, time.Minute)
	if _, _, asg, err := e.svc.Resolve(ctx, contracts.HarnessClaude, eligible, "run_2"); err == nil {
		t.Fatalf("exhausted account fell back to the ambient login: %+v", asg)
	} else if ce := contracts.AsError(e.err(err)); ce.Details["outcome"] != "waiting" {
		t.Fatalf("want waiting, got %v", ce.Details)
	}
	sel, _ = e.svc.Select(ctx, contracts.AccountSelectParams{Provider: contracts.ProviderAnthropic, Policy: eligible})
	if sel.Outcome != contracts.FallbackWaiting {
		t.Fatalf("preview with accounts: %+v", sel)
	}
	// A disabled account does not count: the node is back on its own login.
	e3 := newEnv(t)
	e3.addAccount("acct_synth-d", contracts.HarnessCodex, "D")
	d, _ := getAccount(ctx, e3.svc.db, "acct_synth-d")
	d.Disabled = true
	if err := e3.svc.UpsertAccount(ctx, d); err != nil {
		t.Fatal(err)
	}
	if _, _, asg, err := e3.svc.Resolve(ctx, contracts.HarnessCodex, eligible, "run_d"); err != nil || asg.AccountID != "" || !strings.Contains(asg.Reason, "your own login") {
		t.Fatalf("auto with only a disabled account: %+v %v", asg, err)
	}
	// ambient: always the node's own login, even with accounts.
	e2 := newEnv(t, func(c *Config) { c.DefaultAccount = DefaultAccountAmbient })
	e2.addAccount("acct_synth-x", contracts.HarnessCodex, "X")
	_, _, asg, err = e2.svc.Resolve(ctx, contracts.HarnessCodex, eligible, "run_3")
	if err != nil || asg.AccountID != "" || !strings.Contains(asg.Reason, "--default-account ambient") {
		t.Fatalf("ambient policy: %+v %v", asg, err)
	}
	// An explicit account is never overridden by the default policy.
	if _, _, asg, err := e2.svc.Resolve(ctx, contracts.HarnessCodex, explicit("acct_synth-x"), "run_4"); err != nil || asg.AccountID != "acct_synth-x" {
		t.Fatalf("explicit under ambient default: %+v %v", asg, err)
	}
	if !ValidDefaultAccount("auto") || ValidDefaultAccount("yolo") {
		t.Fatal("ValidDefaultAccount")
	}
}
