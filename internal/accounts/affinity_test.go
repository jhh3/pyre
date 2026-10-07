package accounts

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/usage"
)

// ---------- ranking (pure) ----------

// fleetOf builds a fleet view: account -> other nodes using it.
func fleetOf(use map[string][]string) *fleetUse {
	f := &fleetUse{ok: true, since: map[string]map[string]time.Time{}}
	for acct, nodes := range use {
		f.since[acct] = map[string]time.Time{}
		for _, n := range nodes {
			f.since[acct][n] = time.Unix(0, 0)
		}
	}
	return f
}

type affCand struct {
	id     string
	five   float64
	week   float64
	home   bool
	senior bool
	pref   int
}

func TestRankAffinityTable(t *testing.T) {
	cases := []struct {
		name  string
		fleet *fleetUse // nil: no fleet configured
		cands []affCand
		use   map[string][]string
		want  string // first choice
		order string // optional full order
	}{
		{name: "home wins over more headroom elsewhere",
			cands: []affCand{{id: "a", five: 40, week: 50, home: true}, {id: "b", five: 1, week: 1}},
			use:   map[string][]string{}, want: "a"},
		{name: "free beats in use elsewhere despite less headroom",
			cands: []affCand{{id: "a", five: 1, week: 1}, {id: "b", five: 50, week: 60}},
			use:   map[string][]string{"a": {"node_mac"}}, want: "b"},
		{name: "home near limit (weekly 95%) loses to a free 10% account",
			cands: []affCand{{id: "a", five: 5, week: 95, home: true}, {id: "b", five: 10, week: 10}},
			use:   map[string][]string{}, want: "b"},
		{name: "home 5h near limit loses too",
			cands: []affCand{{id: "a", five: 85, week: 10, home: true}, {id: "b", five: 30, week: 70}},
			use:   map[string][]string{}, want: "b"},
		{name: "home shared, this machine first: stays",
			cands: []affCand{{id: "a", five: 30, week: 30, home: true, senior: true}, {id: "b", five: 1, week: 1}},
			use:   map[string][]string{"a": {"node_mac"}}, want: "a"},
		{name: "home shared, other machine first: moves to a free account",
			cands: []affCand{{id: "a", five: 30, week: 30, home: true}, {id: "b", five: 60, week: 60}},
			use:   map[string][]string{"a": {"node_mac"}}, want: "b"},
		{name: "home shared, other first, nothing free: stays home",
			cands: []affCand{{id: "a", five: 30, week: 30, home: true}, {id: "b", five: 1, week: 1}},
			use:   map[string][]string{"a": {"node_mac"}, "b": {"node_server"}}, want: "a"},
		{name: "all in use elsewhere: fewest other machines, then headroom",
			cands: []affCand{{id: "a", five: 1, week: 1}, {id: "b", five: 40, week: 40}, {id: "c", five: 20, week: 20}},
			use:   map[string][]string{"a": {"node_mac", "node_server"}, "b": {"node_mac"}, "c": {"node_nuc"}},
			want:  "c", order: "c,b,a"},
		{name: "all near limit: plain headroom, fewer machines on a tie",
			cands: []affCand{{id: "a", five: 85, week: 10}, {id: "b", five: 82, week: 90}, {id: "c", five: 82, week: 90}},
			use:   map[string][]string{"b": {"node_mac"}}, want: "c", order: "c,b,a"},
		{name: "free near-limit account ranks after a shared healthy one",
			cands: []affCand{{id: "a", five: 10, week: 90}, {id: "b", five: 30, week: 30}},
			use:   map[string][]string{"b": {"node_mac"}}, want: "b"},
		{name: "preference still first",
			cands: []affCand{{id: "a", five: 1, week: 1, home: true}, {id: "b", five: 50, week: 50, pref: -1}},
			use:   map[string][]string{"b": {"node_mac"}}, want: "b"},
		{name: "no fleet: home sticks, otherwise headroom as before",
			fleet: &fleetUse{}, cands: []affCand{{id: "a", five: 1, week: 1}, {id: "b", five: 50, week: 50, home: true}, {id: "c", five: 5, week: 5}},
			want: "b", order: "b,a,c"},
		{name: "no fleet, no home: today's 5h-then-weekly order",
			fleet: &fleetUse{}, cands: []affCand{{id: "a", five: 12, week: 80}, {id: "b", five: 12, week: 40}, {id: "c", five: 3, week: 99}},
			want: "c", order: "c,b,a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.fleet
			if f == nil {
				f = fleetOf(tc.use)
			}
			var cs []*candidate
			for _, a := range tc.cands {
				c := &candidate{acct: contracts.Account{AccountID: a.id}, fleet: f, home: a.home, senior: a.senior, pref: 1 + a.pref,
					room: usage.HeadroomOf([]contracts.UsageWindow{win("five_hour", a.five), win("seven_day", a.week)})}
				c.near = nearLimit(c.room)
				if f.ok {
					for n := range f.since[a.id] {
						c.others = append(c.others, n)
					}
					sort.Strings(c.others)
				}
				cs = append(cs, c)
			}
			rank(cs)
			var got []string
			for _, c := range cs {
				got = append(got, c.acct.AccountID)
			}
			if got[0] != tc.want || tc.order != "" && strings.Join(got, ",") != tc.order {
				t.Fatalf("order %v, want first %s (order %q)", got, tc.want, tc.order)
			}
		})
	}
}

// ---------- selection with a fake fleet directory ----------

type fakeFleetDir struct {
	mu    sync.Mutex
	asg   []contracts.AccountAssignment
	err   error
	calls int
}

func (f *fakeFleetDir) ListAccounts(_ context.Context, _ contracts.ListAccountsRequest) (contracts.Page[contracts.AccountView], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return contracts.Page[contracts.AccountView]{}, f.err
	}
	by := map[string]*contracts.AccountView{}
	var order []string
	for _, a := range f.asg {
		v := by[a.AccountID]
		if v == nil {
			v = &contracts.AccountView{Account: contracts.Account{AccountID: a.AccountID}}
			by[a.AccountID] = v
			order = append(order, a.AccountID)
		}
		v.Assignments = append(v.Assignments, a)
	}
	page := contracts.Page[contracts.AccountView]{}
	for _, id := range order {
		page.Items = append(page.Items, *by[id])
	}
	return page, nil
}

func (f *fakeFleetDir) ListNodes(context.Context, contracts.ListNodesRequest) (contracts.Page[contracts.Node], error) {
	return contracts.Page[contracts.Node]{Items: []contracts.Node{{NodeID: "node_synth-mac", Name: "mac"}, {NodeID: "node_synth-server", Name: "server"}}}, nil
}

func (f *fakeFleetDir) use(acct, node string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asg = append(f.asg, contracts.AccountAssignment{AssignmentID: contracts.NewID(contracts.PrefixAssignment), AccountID: acct, NodeID: node,
		RunID: contracts.NewID(contracts.PrefixRun), AssignedAt: at})
}

func affinityEnv(t *testing.T, dir *fakeFleetDir) *testEnv {
	e := newEnv(t, func(c *Config) { c.Fleet, c.FleetCache, c.NodeName = dir, -1, "synth-node" })
	for _, l := range []string{"A", "B", "C"} {
		e.addAccount("acct_synth-"+strings.ToLower(l), contracts.HarnessClaude, l)
	}
	return e
}

func (e *testEnv) obsWin(id string, five, week float64) {
	e.t.Helper()
	o := contracts.AccountObservation{AccountID: id, Provider: contracts.ProviderAnthropic, ReportingNode: "node_synth",
		ObservedAt: e.clock.Now().Add(-time.Minute), Source: "mock", Confidence: "high", Eligibility: contracts.EligibleYes,
		Windows: []contracts.UsageWindow{win("five_hour", five), win("seven_day", week)}}
	if err := e.svc.IngestObservation(context.Background(), o); err != nil {
		e.t.Fatal(err)
	}
}

func (e *testEnv) resolve(run string) *contracts.AccountAssignment {
	e.t.Helper()
	r, err := e.svc.ResolveFor(context.Background(), ResolveRequest{Harness: contracts.HarnessClaude, Policy: eligible, RunID: run, SessionID: "ses_" + run})
	if err != nil {
		e.t.Fatalf("resolve %s: %v", run, e.err(err))
	}
	return r.Assignment
}

func TestSelectAvoidsAccountsInUseElsewhere(t *testing.T) {
	dir := &fakeFleetDir{}
	e := affinityEnv(t, dir)
	e.obsWin("acct_synth-a", 2, 5)
	e.obsWin("acct_synth-b", 20, 30)
	e.obsWin("acct_synth-c", 40, 50)
	dir.use("acct_synth-a", "node_synth-mac", e.clock.Now().Add(-time.Hour))
	// This node's own runs in the hub view never count as "elsewhere".
	dir.use("acct_synth-b", e.cfg.NodeID, e.clock.Now())

	a := e.resolve("run_1")
	if a.AccountID != "acct_synth-b" {
		t.Fatalf("want B (A in use on mac), got %s: %s", a.AccountID, a.Reason)
	}
	if a.Reason != "not used on other machines; most headroom (5h 20% used, weekly 30%)" {
		t.Fatalf("reason %q", a.Reason)
	}
	// Stickiness: the next run here stays on B although A frees up and has
	// more headroom.
	dir.mu.Lock()
	dir.asg = nil
	dir.mu.Unlock()
	a2 := e.resolve("run_2")
	if a2.AccountID != "acct_synth-b" || a2.Reason != "home account for synth-node; not used on other machines (5h 20% used, weekly 30%)" {
		t.Fatalf("home: %s %q", a2.AccountID, a2.Reason)
	}
	// Home near its weekly limit: move to the best account not used elsewhere.
	e.clock.Advance(time.Second)
	e.obsWin("acct_synth-b", 20, 95)
	dir.use("acct_synth-a", "node_synth-mac", e.clock.Now().Add(-time.Hour))
	a3 := e.resolve("run_3")
	if a3.AccountID != "acct_synth-c" {
		t.Fatalf("want C (home B near limit, A on mac), got %s: %s", a3.AccountID, a3.Reason)
	}
	if !strings.HasPrefix(a3.Reason, "home account B is near its limit (5h 20% used, weekly 95%); not used on other machines;") {
		t.Fatalf("reason %q", a3.Reason)
	}
	// C is the new home.
	if a4 := e.resolve("run_4"); a4.AccountID != "acct_synth-c" {
		t.Fatalf("new home: %s %q", a4.AccountID, a4.Reason)
	}
}

func TestSelectSharesOnlyWhenEverythingIsInUse(t *testing.T) {
	dir := &fakeFleetDir{}
	e := affinityEnv(t, dir)
	e.obsWin("acct_synth-a", 2, 5)
	e.obsWin("acct_synth-b", 20, 30)
	e.obsWin("acct_synth-c", 40, 50)
	now := e.clock.Now()
	dir.use("acct_synth-a", "node_synth-mac", now)
	dir.use("acct_synth-a", "node_synth-server", now)
	dir.use("acct_synth-b", "node_synth-mac", now)
	dir.use("acct_synth-c", "node_synth-server", now)
	a := e.resolve("run_1")
	// B and C each have one other machine; B has more headroom.
	if a.AccountID != "acct_synth-b" {
		t.Fatalf("want B, got %s: %s", a.AccountID, a.Reason)
	}
	if a.Reason != "every eligible account below 80% is in use on another machine; shared with mac (fewest other machines, 5h 20% used, weekly 30%)" {
		t.Fatalf("reason %q", a.Reason)
	}
}

func TestAffinityNeverPicksIneligible(t *testing.T) {
	dir := &fakeFleetDir{}
	e := affinityEnv(t, dir)
	ctx := context.Background()
	// The only free account is at its limit; the home account is in cooldown.
	e.obsWin("acct_synth-a", 100, 30)
	e.fake.usage["A"] = `{"five_hour":{"utilization":100},"seven_day":{"utilization":30}}`
	e.obsWin("acct_synth-b", 50, 50)
	e.obsWin("acct_synth-c", 10, 10)
	if a := e.resolve("run_0"); a.AccountID != "acct_synth-c" {
		t.Fatalf("setup: %s", a.AccountID)
	}
	putCooldown(ctx, e.svc.db, "acct_synth-c", e.clock.Now().Add(time.Hour), "usage_limit")
	dir.use("acct_synth-b", "node_synth-mac", e.clock.Now())
	a := e.resolve("run_1")
	if a.AccountID != "acct_synth-b" {
		t.Fatalf("want B (shared, but the only usable one), got %s: %s", a.AccountID, a.Reason)
	}
	if !strings.HasPrefix(a.Reason, "home account C is not available now; every eligible account below 80% is in use on another machine; shared with mac") {
		t.Fatalf("reason %q", a.Reason)
	}
}

func TestAffinityExplicitAndPreferUnaffected(t *testing.T) {
	dir := &fakeFleetDir{}
	e := affinityEnv(t, dir)
	ctx := context.Background()
	e.obsWin("acct_synth-a", 2, 5)
	e.obsWin("acct_synth-b", 20, 30)
	e.obsWin("acct_synth-c", 40, 50)
	dir.use("acct_synth-a", "node_synth-mac", e.clock.Now())
	r, err := e.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: explicit("acct_synth-a"), RunID: "run_x"})
	if err != nil || r.Assignment.AccountID != "acct_synth-a" {
		t.Fatalf("explicit: %+v %v", r.Assignment, err)
	}
	if dir.calls != 0 {
		t.Fatalf("explicit selection read the fleet view (%d calls)", dir.calls)
	}
	pol := contracts.AccountPolicy{Mode: contracts.AccountEligible, Prefer: []string{"acct_synth-c"}}
	r, err = e.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: pol, RunID: "run_p"})
	if err != nil || r.Assignment.AccountID != "acct_synth-c" {
		t.Fatalf("prefer: %+v %v", r.Assignment, err)
	}
}

func TestAffinityHubUnreachableFallsBack(t *testing.T) {
	dir := &fakeFleetDir{err: errors.New("dial tcp 127.0.0.1:1: connection refused")}
	e := affinityEnv(t, dir)
	e.obsWin("acct_synth-a", 12, 80)
	e.obsWin("acct_synth-b", 12, 40)
	e.obsWin("acct_synth-c", 3, 99)
	a := e.resolve("run_1")
	if a.AccountID != "acct_synth-c" {
		t.Fatalf("want today's choice C, got %s", a.AccountID)
	}
	if !strings.HasPrefix(a.Reason, "most headroom (5h 3% used, weekly 99%); fleet view unavailable:") {
		t.Fatalf("reason %q", a.Reason)
	}
	// The failure is cached: the next launch does not wait on the hub again.
	e.resolve("run_2")
	if dir.calls != 1 {
		t.Fatalf("fleet calls %d, want 1 (failure cached)", dir.calls)
	}
}
