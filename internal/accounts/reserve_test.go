package accounts

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// setTier changes the node's copy of an account's tier (what the token
// sync mirrors from the hub).
func (e *testEnv) setTier(id string, t contracts.AccountTier) {
	e.t.Helper()
	a, err := e.svc.Account(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	a.Tier = t
	if err := e.svc.UpsertAccount(context.Background(), a); err != nil {
		e.t.Fatal(err)
	}
}

// reserveEnv: normal account A, reserve account R with plenty of room.
func reserveEnv(t *testing.T, mod ...func(*Config)) *testEnv {
	e := newEnv(t, mod...)
	e.addAccount("acct_synth-a", contracts.HarnessClaude, "A")
	e.addAccount("acct_synth-r", contracts.HarnessClaude, "R")
	e.setTier("acct_synth-r", contracts.TierReserve)
	e.obsWin("acct_synth-r", 1, 1)
	return e
}

func (e *testEnv) selectFor(pol contracts.AccountPolicy) contracts.AccountSelection {
	e.t.Helper()
	sel, err := e.svc.Select(context.Background(), contracts.AccountSelectParams{Provider: contracts.ProviderAnthropic, Policy: pol})
	if err != nil {
		e.t.Fatal(err)
	}
	return sel
}

// The owner's threshold: a reserve account takes new work only when every
// normal account is out (limit, cooldown, ineligible) or near its limit
// (usage.NearPercent, 80 %, on the 5-hour or the weekly window: what
// `pyre accounts` shows as "near limit"). Below it the normal account wins
// even with far less room than the reserve one.
func TestReserveOnlyWhenNormalAccountsAreOutOrNear(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(e *testEnv)
		reserve bool
	}{
		{"normal has room", func(e *testEnv) { e.obsWin("acct_synth-a", 60, 60) }, false},
		{"normal just below near on both windows", func(e *testEnv) { e.obsWin("acct_synth-a", 79, 79) }, false},
		{"normal 5h near", func(e *testEnv) { e.obsWin("acct_synth-a", 80, 10) }, true},
		{"normal weekly near", func(e *testEnv) { e.obsWin("acct_synth-a", 10, 85) }, true},
		{"normal at its limit", func(e *testEnv) { e.obsWin("acct_synth-a", 20, 20); limited(t, e, "acct_synth-a") }, true},
		{"normal disabled", func(e *testEnv) {
			e.obsWin("acct_synth-a", 5, 5)
			a, _ := e.svc.Account(context.Background(), "acct_synth-a")
			a.Disabled = true
			e.svc.UpsertAccount(context.Background(), a)
		}, true},
		// Unknown is not room, but the one observation pass runs before a
		// reserve account takes the run.
		{"normal unknown, observed with room", func(e *testEnv) {}, false},
		{"normal unknown, observed near", func(e *testEnv) {
			e.fake.usage["A"] = `{"five_hour":{"utilization":10,"resets_at":"2026-10-05T03:00:00Z"},"seven_day":{"utilization":92,"resets_at":"2026-10-09T00:00:00Z"}}`
		}, true},
		{"normal unknown and unobservable", func(e *testEnv) { e.fake.status["A"] = 503 }, true},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := reserveEnv(t)
			tc.setup(e)
			want := "acct_synth-a"
			if tc.reserve {
				want = "acct_synth-r"
			}
			a := e.resolve("run_" + string(rune('a'+i)))
			if a.AccountID != want {
				t.Fatalf("chose %s (%s), want %s", a.AccountID, a.Reason, want)
			}
			if got := strings.HasPrefix(a.Reason, "reserve account: no normal anthropic account has room"); got != tc.reserve {
				t.Fatalf("reason %q", a.Reason)
			}
			if sel := e.selectFor(eligible); sel.AccountID != want {
				t.Fatalf("dry run chose %s (%s), want %s", sel.AccountID, sel.Reason, want)
			}
		})
	}
}

// Once normal accounts are near, the usual ranking applies: among reserve
// accounts the one with more room, and a normal account with more room
// than every reserve one still wins.
func TestReserveRankingOnceUnlocked(t *testing.T) {
	e := reserveEnv(t)
	e.addAccount("acct_synth-s", contracts.HarnessClaude, "S")
	e.setTier("acct_synth-s", contracts.TierReserve)
	e.obsWin("acct_synth-r", 40, 40)
	e.obsWin("acct_synth-s", 10, 10)
	e.obsWin("acct_synth-a", 85, 20)
	if a := e.resolve("run_1"); a.AccountID != "acct_synth-s" {
		t.Fatalf("chose %s (%s)", a.AccountID, a.Reason)
	}

	e2 := reserveEnv(t)
	e2.obsWin("acct_synth-a", 90, 90)
	e2.obsWin("acct_synth-r", 97, 97)
	if a := e2.resolve("run_1"); a.AccountID != "acct_synth-a" {
		t.Fatalf("reserve nearer its limit than the normal account won: %s (%s)", a.AccountID, a.Reason)
	}
}

// An explicit account and a Prefer list win over the tier.
func TestReserveExplicitAndPreferWin(t *testing.T) {
	e := reserveEnv(t)
	e.obsWin("acct_synth-a", 10, 10)
	soft := contracts.AccountPolicy{Mode: contracts.AccountExplicit, AccountID: "acct_synth-r"}
	r, err := e.svc.ResolveFor(context.Background(), ResolveRequest{Harness: contracts.HarnessClaude, Policy: soft, RunID: "run_1", SessionID: "ses_1"})
	if err != nil || r.Assignment.AccountID != "acct_synth-r" || !strings.HasPrefix(r.Assignment.Reason, "explicit account") {
		t.Fatalf("explicit reserve: %+v %v", r.Assignment, err)
	}
	pref := contracts.AccountPolicy{Mode: contracts.AccountEligible, Prefer: []string{"acct_synth-r"}}
	r, err = e.svc.ResolveFor(context.Background(), ResolveRequest{Harness: contracts.HarnessClaude, Policy: pref, RunID: "run_2", SessionID: "ses_2"})
	if err != nil || r.Assignment.AccountID != "acct_synth-r" || !strings.HasPrefix(r.Assignment.Reason, "preferred account") {
		t.Fatalf("preferred reserve: %+v %v", r.Assignment, err)
	}
	// Both runs are recorded as named (the rebalancer never drains them).
	for _, run := range []string{"run_1", "run_2"} {
		var named int
		e.svc.db.QueryRow(`SELECT named FROM accounts_assignment WHERE run_id=?`, run).Scan(&named)
		if named != 1 {
			t.Fatalf("%s not marked named", run)
		}
	}
	if a := e.resolve("run_3"); a.AccountID != "acct_synth-a" {
		t.Fatalf("automatic choice took the reserve account: %+v", a)
	}
}

// tierDir is a hub directory that lists accounts with their tiers.
type tierDir struct {
	tiers map[string]contracts.AccountTier
}

func (d *tierDir) ListAccounts(context.Context, contracts.ListAccountsRequest) (contracts.Page[contracts.AccountView], error) {
	var p contracts.Page[contracts.AccountView]
	for id, t := range d.tiers {
		p.Items = append(p.Items, contracts.AccountView{Account: contracts.Account{AccountID: id, Tier: t}})
	}
	return p, nil
}

// The hub's tier (fleet view) applies before the token sync updates the
// node's copy; an account the hub lists without a tier is normal.
func TestReserveTierFromHub(t *testing.T) {
	dir := &tierDir{tiers: map[string]contracts.AccountTier{"acct_synth-a": contracts.TierReserve, "acct_synth-r": ""}}
	e := reserveEnv(t, func(c *Config) { c.Fleet, c.FleetCache = dir, -1 })
	e.obsWin("acct_synth-a", 1, 1)
	e.obsWin("acct_synth-r", 50, 50)
	if a := e.resolve("run_1"); a.AccountID != "acct_synth-r" {
		t.Fatalf("chose %s (%s); the hub says A is reserve and R normal", a.AccountID, a.Reason)
	}
}

// The rebalancer never moves work onto a reserve account while a normal one
// has room; with no other normal account it may (the hot one is near).
func TestRebalanceNotOntoReserve(t *testing.T) {
	setup := func(t *testing.T) *testEnv {
		e := rebalanceEnv(t) // opus hot (weekly 88), B with room
		e.setTier("acct_synth-b", contracts.TierReserve)
		e.startRun("ses_1", "run_1", contracts.HarnessClaude, softPin, claudeCaps)
		e.setState("ses_1", contracts.RunWaitingInput)
		return e
	}
	t.Run("normal with room", func(t *testing.T) {
		e := setup(t)
		e.addAccount("acct_synth-c", contracts.HarnessClaude, "C")
		e.obsWin("acct_synth-c", 30, 30)
		plans := e.sweep()
		if len(plans) != 1 || plans[0].ToAccountID != "acct_synth-c" {
			t.Fatalf("plans %+v", plans)
		}
	})
	t.Run("normal comfortable but past the move target bar", func(t *testing.T) {
		e := setup(t)
		e.addAccount("acct_synth-c", contracts.HarnessClaude, "C")
		e.obsWin("acct_synth-c", 30, 75) // below near (80), above HomeWeeklyPercent (70)
		if plans := e.sweep(); len(plans) != 0 {
			t.Fatalf("moved onto the reserve account while a normal one has room: %+v", plans)
		}
	})
	t.Run("no other normal account", func(t *testing.T) {
		e := setup(t)
		plans := e.sweep()
		if len(plans) != 1 || plans[0].ToAccountID != "acct_synth-b" {
			t.Fatalf("plans %+v", plans)
		}
	})
}

// Idle sessions the selector put on a reserve account drain back to a
// normal account once one has comfortable room; named and pinned ones stay.
func TestRebalanceDrainsReserve(t *testing.T) {
	e := reserveEnv(t)
	ctx := context.Background()
	e.obsWin("acct_synth-a", 20, 20)
	limited(t, e, "acct_synth-a")
	e.startRun("ses_1", "run_1", contracts.HarnessClaude, eligible, claudeCaps)
	e.startRun("ses_2", "run_2", contracts.HarnessClaude, contracts.AccountPolicy{Mode: contracts.AccountExplicit, AccountID: "acct_synth-r"}, claudeCaps)
	e.startRun("ses_3", "run_3", contracts.HarnessClaude, explicit("acct_synth-r"), claudeCaps)
	for _, s := range []string{"ses_1", "ses_2", "ses_3"} {
		if a, _ := e.svc.assignmentForRun(ctx, e.svc.db, "run_"+s[4:]); a.AccountID != "acct_synth-r" {
			t.Fatalf("%s on %s", s, a.AccountID)
		}
		e.setState(s, contracts.RunWaitingInput)
	}
	// Normal still out: nothing moves.
	if plans := e.sweep(); len(plans) != 0 {
		t.Fatalf("drained while the normal account is limited: %+v", plans)
	}
	// Normal near its limit: still nothing.
	clearCooldown(ctx, e.svc.db, "acct_synth-a")
	e.obsWin("acct_synth-a", 85, 20)
	e.obsWin("acct_synth-r", 1, 1)
	e.clock.Advance(IdleBeforeMove + time.Second)
	if plans := e.svc.Rebalance(ctx); len(plans) != 0 {
		t.Fatalf("drained onto a near-limit normal account: %+v", plans)
	}
	// No room anywhere: the runs on the reserve account were never even
	// claimed (no idle tracking, no pending rows).
	e.svc.rb.mu.Lock()
	tracked := len(e.svc.rb.idleSince)
	e.svc.rb.mu.Unlock()
	if tracked != 0 {
		t.Fatalf("runs claimed for a drain with no normal room: %d", tracked)
	}
	// Room again: only the automatic run moves.
	e.obsWin("acct_synth-a", 10, 10)
	e.obsWin("acct_synth-r", 1, 1)
	plans := e.sweep()
	if len(plans) != 1 || plans[0].SessionID != "ses_1" || plans[0].ToAccountID != "acct_synth-a" || plans[0].Outcome != contracts.FallbackNativeResume {
		t.Fatalf("plans %+v", plans)
	}
	if !strings.HasPrefix(plans[0].Reason, ProactivePrefix+`: account "R" is a reserve account and a normal account has room again; account "A" `) {
		t.Fatalf("reason %q", plans[0].Reason)
	}
	e.clock.Advance(IdleBeforeMove + time.Second)
	if again := e.svc.Rebalance(ctx); len(again) != 0 {
		t.Fatalf("named or pinned session drained: %+v", again)
	}
}

// A limit fallback onto a reserve account is not the starter's choice: the
// successor is drainable even when the starter named the limited account.
func TestSuccessorOnAnotherAccountIsNotNamed(t *testing.T) {
	e := reserveEnv(t)
	ctx := context.Background()
	e.obsWin("acct_synth-a", 20, 20)
	e.startRun("ses_1", "run_1", contracts.HarnessClaude, contracts.AccountPolicy{Mode: contracts.AccountExplicit, AccountID: "acct_synth-a"}, claudeCaps)
	plan, err := e.svc.HandleLimitEvent(ctx, "ses_1", "run_1", contracts.LimitEvent{Kind: LimitUsage})
	if err != nil || plan.ToAccountID != "acct_synth-r" {
		t.Fatalf("%+v %v", plan, err)
	}
	var named int
	e.svc.db.QueryRow(`SELECT named FROM accounts_assignment WHERE run_id=?`, plan.NewRunID).Scan(&named)
	if named != 0 {
		t.Fatal("successor on another account marked named")
	}
}

// Migration v5 adds assignment.named, backfilled for active runs from the
// pin and the selector's wording: explicit and preferred runs stay, a run
// the selector chose (or a fallback moved) may drain.
func TestMigrationAssignmentNamed(t *testing.T) {
	ctx := context.Background()
	db, err := sqlitex.Open(filepath.Join(t.TempDir(), "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ms := Migrations()
	if err := sqlitex.Migrate(ctx, db, ms[:4]...); err != nil {
		t.Fatal(err)
	}
	for _, r := range []struct{ run, reason, released string }{
		{"run_auto", "home account for synth; not used on other machines (5h 3% used, weekly 20%)", ""},
		{"run_explicit", "explicit account (5h 3% used, weekly 20%)", ""},
		{"run_explicit_unknown", "explicit account; availability unknown", ""},
		{"run_preferred", "preferred account (5h 3% used, weekly 20%)", ""},
		{"run_unpinned", `account "opus" is unavailable (cooldown); using the best eligible anthropic account instead (strict pinning would wait for it); most headroom`, ""},
		{"run_strict", "synthetic", ""},
		{"run_done", "explicit account (5h 3% used, weekly 20%)", "2026-01-01T00:00:00Z"},
	} {
		if _, err := db.Exec(`INSERT INTO accounts_assignment(assignment_id,run_id,account_id,credential_kind,reason,assigned_at,released_at,pinned)
			VALUES(?,?,?,?,?,?,?,?)`, "asg_"+r.run, r.run, "acct_synth-a", "claude-profile-store", r.reason, "2026-01-01T00:00:00Z", r.released, map[bool]int{true: 1}[r.run == "run_strict"]); err != nil {
			t.Fatal(err)
		}
	}
	if err := sqlitex.Migrate(ctx, db, ms...); err != nil {
		t.Fatal(err)
	}
	for run, want := range map[string]int{"run_auto": 0, "run_explicit": 1, "run_explicit_unknown": 1, "run_preferred": 1, "run_unpinned": 0, "run_strict": 1, "run_done": 0} {
		var n int
		if err := db.QueryRow(`SELECT named FROM accounts_assignment WHERE run_id=?`, run).Scan(&n); err != nil || n != want {
			t.Fatalf("%s named=%d (%v), want %d", run, n, err, want)
		}
	}
}

// The token sync mirrors the hub's tier into the node's copy (and back).
func TestTokenSyncCarriesTier(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addAccount("acct_synth-a", contracts.HarnessClaude, "A")
	hub, _ := e.svc.Account(ctx, "acct_synth-a")
	hub.Tier = contracts.TierReserve
	dir := &fakeDir{accts: []contracts.Account{hub}}
	e.svc.SyncTokenAccounts(ctx, dir, nil)
	if a, _ := e.svc.Account(ctx, "acct_synth-a"); !a.Reserve() {
		t.Fatalf("tier not synced: %+v", a)
	}
	dir.accts[0].Tier = ""
	e.svc.SyncTokenAccounts(ctx, dir, nil)
	if a, _ := e.svc.Account(ctx, "acct_synth-a"); a.Reserve() {
		t.Fatalf("normal tier not synced: %+v", a)
	}
}

// Drains are capped per sweep (MaxDrainsPerSweep) inside MaxMovesPerSweep.
func TestRebalanceDrainCap(t *testing.T) {
	e := reserveEnv(t)
	ctx := context.Background()
	e.obsWin("acct_synth-a", 20, 20)
	limited(t, e, "acct_synth-a")
	for _, s := range []string{"ses_1", "ses_2"} {
		e.startRun(s, "run_"+s[4:], contracts.HarnessClaude, eligible, claudeCaps)
		e.setState(s, contracts.RunWaitingInput)
	}
	clearCooldown(ctx, e.svc.db, "acct_synth-a")
	e.obsWin("acct_synth-a", 10, 10)
	e.obsWin("acct_synth-r", 1, 1)
	if plans := e.sweep(); len(plans) != MaxDrainsPerSweep {
		t.Fatalf("drains in one sweep: %+v", plans)
	}
}

// Normal accounts that fail at checkout do not use up ResolveFor's rounds
// while a reserve account has room: after the first failure the reserve
// competes as usual.
func TestReserveAfterNormalCheckoutsFail(t *testing.T) {
	r := newRotaEnv(t)
	ctx := context.Background()
	for _, l := range []string{"A", "B", "C", "D", "E", "R"} {
		r.writeRotaClaude(l, "g1", time.Now().Add(time.Hour))
		id := "acct_synth-" + strings.ToLower(l)
		r.addRotaAccount(id, contracts.HarnessClaude, l, false)
		if l == "R" {
			r.setTier(id, contracts.TierReserve)
			r.observe(id, contracts.ProviderAnthropic, 1, time.Minute)
			continue
		}
		r.observe(id, contracts.ProviderAnthropic, 40, time.Minute)
		r.fleet.held[id] = FleetLease{LeaseID: "x-" + l, NodeID: "node_far", ExpiresAt: time.Now().Add(time.Hour)}
	}
	res, err := r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: eligible, RunID: "run_1", SessionID: "ses_1"})
	if err != nil || res.Assignment == nil || res.Assignment.AccountID != "acct_synth-r" {
		t.Fatalf("want the reserve account after normal checkouts failed: %+v %v", res.Assignment, err)
	}
}

// AllowUnknown with no known room anywhere: an unknown normal account goes
// before an unknown reserve one.
func TestReserveLastAmongUnknown(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.AllowUnknown = true })
	e.addAccount("acct_synth-a", contracts.HarnessClaude, "A")
	e.addAccount("acct_synth-0", contracts.HarnessClaude, "Z") // ranks first by id
	e.setTier("acct_synth-0", contracts.TierReserve)
	e.fake.status["A"], e.fake.status["Z"] = 503, 503
	if a := e.resolve("run_1"); a.AccountID != "acct_synth-a" {
		t.Fatalf("chose %s (%s)", a.AccountID, a.Reason)
	}
}

// A machine whose home account is reserve says so when it leaves it.
func TestReserveHomeNote(t *testing.T) {
	e := reserveEnv(t)
	ctx := context.Background()
	e.obsWin("acct_synth-a", 20, 20)
	limited(t, e, "acct_synth-a")
	if a := e.resolve("run_1"); a.AccountID != "acct_synth-r" {
		t.Fatalf("chose %s", a.AccountID)
	}
	e.svc.Release("run_1")
	clearCooldown(ctx, e.svc.db, "acct_synth-a")
	e.obsWin("acct_synth-a", 20, 20)
	a := e.resolve("run_2")
	if a.AccountID != "acct_synth-a" || !strings.Contains(a.Reason, `home account R is a reserve account and a normal account has room`) {
		t.Fatalf("%s: %q", a.AccountID, a.Reason)
	}
}

// The dry run marks a reserve choice for placement; explicit is not marked.
func TestSelectMarksReserve(t *testing.T) {
	e := reserveEnv(t)
	e.obsWin("acct_synth-a", 85, 20)
	if sel := e.selectFor(eligible); sel.AccountID != "acct_synth-r" || !sel.Reserve {
		t.Fatalf("%+v", sel)
	}
	e.obsWin("acct_synth-a", 10, 20)
	if sel := e.selectFor(eligible); sel.AccountID != "acct_synth-a" || sel.Reserve {
		t.Fatalf("%+v", sel)
	}
	if sel := e.selectFor(explicit("acct_synth-r")); sel.AccountID != "acct_synth-r" || sel.Reserve {
		t.Fatalf("explicit: %+v", sel)
	}
}
