package accounts

import (
	"context"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/client"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/httpapi"
	"github.com/jhh3/pyre/internal/hub"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// Isolated-hub integration (W-affinity): a real hub over HTTP and three
// nodes, each an accounts service with its own database, profiles, fake
// provider and node credential. Assignments reach the hub through its report
// handler; each node reads the fleet view through its own HTTP client, as
// pyred does. Synthetic logins only.

// barrierDir holds every fleet read until n readers have read, so the
// nodes resolve at the same moment (the race the design accepts).
type barrierDir struct {
	FleetDirectory
	wg *sync.WaitGroup
	on *bool
}

func (b barrierDir) ListAccounts(ctx context.Context, req contracts.ListAccountsRequest) (contracts.Page[contracts.AccountView], error) {
	p, err := b.FleetDirectory.ListAccounts(ctx, req)
	if *b.on {
		b.wg.Done()
		b.wg.Wait()
	}
	return p, err
}

type affinityFleet struct {
	t     *testing.T
	h     *hub.Hub
	own   *client.Client
	nodes []*testEnv
	reps  []*hubReporter
	gate  *sync.WaitGroup
	gated bool
}

func newAffinityFleet(t *testing.T, n int, used map[string]float64) *affinityFleet {
	t.Helper()
	hubDir := t.TempDir()
	db, err := sqlitex.Open(filepath.Join(hubDir, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	h, err := hub.New(hub.Config{DB: db, DataDir: hubDir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	creds := map[string]contracts.Principal{"synthetic-owner-cred": {Kind: contracts.PrincipalOwner, ID: "own_synth", Name: "synthetic owner"}}
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("node_synth-%d", i+1)
		creds["synthetic-node-"+ids[i]] = contracts.Principal{Kind: contracts.PrincipalNode, ID: ids[i]}
	}
	srv := httptest.NewServer(httpapi.NewServer(h, httpapi.NewStaticTokens(creds), httpapi.Options{StreamWait: time.Second}))
	t.Cleanup(srv.Close)
	opts := client.Options{RetryBase: time.Millisecond, MaxAttempts: 2}
	f := &affinityFleet{t: t, h: h, own: client.New(srv.URL, "synthetic-owner-cred", opts), gate: &sync.WaitGroup{}}
	for _, id := range ids {
		rep := &hubReporter{h: h, nodeID: id}
		cl := client.New(srv.URL, "synthetic-node-"+id, opts)
		e := newEnv(t, func(c *Config) {
			c.NodeID, c.Reporter, c.FleetCache = id, rep, -1
			c.Fleet = barrierDir{FleetDirectory: cl, wg: f.gate, on: &f.gated}
		})
		for acct, u := range used {
			e.addAccount(acct, contracts.HarnessClaude, strings.TrimPrefix(acct, "acct_synth-"))
			e.obsWin(acct, u, u)
		}
		f.nodes = append(f.nodes, e)
		f.reps = append(f.reps, rep)
	}
	return f
}

// hubUse is the hub's view: account -> nodes with active runs.
func (f *affinityFleet) hubUse() map[string][]string {
	f.t.Helper()
	page, err := f.own.ListAccounts(context.Background(), contracts.ListAccountsRequest{})
	if err != nil {
		f.t.Fatal(err)
	}
	out := map[string][]string{}
	for _, v := range page.Items {
		for _, a := range v.Assignments {
			out[v.Account.AccountID] = append(out[v.Account.AccountID], a.NodeID)
		}
	}
	return out
}

func (f *affinityFleet) noRejectedReports() {
	for _, r := range f.reps {
		if len(r.errs) != 0 {
			f.t.Fatalf("hub rejected reports from %s: %v", r.nodeID, r.errs)
		}
	}
}

func distinct(asg []*contracts.AccountAssignment) bool {
	seen := map[string]bool{}
	for _, a := range asg {
		if seen[a.AccountID] {
			return false
		}
		seen[a.AccountID] = true
	}
	return true
}

func TestAffinityIntegrationSequentialSpread(t *testing.T) {
	// Four accounts with different headroom; without affinity every node
	// would pick acct_synth-1 (least used).
	f := newAffinityFleet(t, 3, map[string]float64{"acct_synth-1": 5, "acct_synth-2": 10, "acct_synth-3": 20, "acct_synth-4": 30})
	var first []*contracts.AccountAssignment
	for i, e := range f.nodes {
		first = append(first, e.resolve(fmt.Sprintf("run_synth-n%d-1", i+1)))
	}
	want := []string{"acct_synth-1", "acct_synth-2", "acct_synth-3"}
	for i, a := range first {
		if a.AccountID != want[i] {
			t.Fatalf("node %d got %s (%s), want %s", i+1, a.AccountID, a.Reason, want[i])
		}
	}
	if !strings.HasPrefix(first[1].Reason, "not used on other machines; most headroom") {
		t.Fatalf("node 2 reason %q", first[1].Reason)
	}
	// Second sessions: each machine stays on its home account.
	for i, e := range f.nodes {
		a := e.resolve(fmt.Sprintf("run_synth-n%d-2", i+1))
		if a.AccountID != want[i] || !strings.HasPrefix(a.Reason, "home account for node_synth-") {
			t.Fatalf("node %d second run %s %q", i+1, a.AccountID, a.Reason)
		}
	}
	use := f.hubUse()
	for i, acct := range want {
		if len(use[acct]) != 2 || use[acct][0] != f.nodes[i].cfg.NodeID || use[acct][1] != f.nodes[i].cfg.NodeID {
			t.Fatalf("hub use of %s: %v", acct, use[acct])
		}
	}
	// Node 1's home reaches the near-limit threshold: it moves to the one
	// account no other machine uses.
	f.nodes[0].clock.Advance(time.Second)
	f.nodes[0].obsWin("acct_synth-1", 85, 40)
	a := f.nodes[0].resolve("run_synth-n1-3")
	if a.AccountID != "acct_synth-4" || !strings.HasPrefix(a.Reason, "home account 1 is near its limit") {
		t.Fatalf("node 1 after near limit: %s %q", a.AccountID, a.Reason)
	}
	f.noRejectedReports()
}

func TestAffinityIntegrationConcurrentRaceThenRebalance(t *testing.T) {
	f := newAffinityFleet(t, 3, map[string]float64{"acct_synth-1": 5, "acct_synth-2": 10, "acct_synth-3": 20, "acct_synth-4": 30})
	// Round 1: all three read the fleet view before any assignment exists.
	f.gated = true
	f.gate.Add(len(f.nodes))
	round1 := make([]*contracts.AccountAssignment, len(f.nodes))
	var wg sync.WaitGroup
	errs := make([]error, len(f.nodes))
	for i, e := range f.nodes {
		wg.Add(1)
		go func(i int, e *testEnv) {
			defer wg.Done()
			r, err := e.svc.ResolveFor(context.Background(), ResolveRequest{Harness: contracts.HarnessClaude, Policy: eligible,
				RunID: fmt.Sprintf("run_synth-n%d-1", i+1), SessionID: "ses_synth"})
			round1[i], errs[i] = r.Assignment, err
		}(i, e)
	}
	wg.Wait()
	f.gated = false
	for i, err := range errs {
		if err != nil {
			t.Fatalf("node %d: %v", i+1, err)
		}
	}
	// The accepted race: every node saw nothing in use and took the account
	// with the most headroom.
	for i, a := range round1 {
		if a.AccountID != "acct_synth-1" {
			t.Fatalf("round 1 node %d: %s (%s)", i+1, a.AccountID, a.Reason)
		}
	}
	if use := f.hubUse(); len(use["acct_synth-1"]) != 3 {
		t.Fatalf("hub use after the race: %v", use)
	}
	// Round 2 (sequential, the next run on each machine): the machine that
	// was on acct_synth-1 first keeps it (equal start times: the lowest node
	// id); the others move to accounts no other machine uses.
	var round2 []*contracts.AccountAssignment
	for i, e := range f.nodes {
		e.clock.Advance(time.Minute)
		round2 = append(round2, e.resolve(fmt.Sprintf("run_synth-n%d-2", i+1)))
	}
	if !distinct(round2) {
		t.Fatalf("round 2 not spread: %s %s %s", round2[0].AccountID, round2[1].AccountID, round2[2].AccountID)
	}
	if round2[0].AccountID != "acct_synth-1" || !strings.Contains(round2[0].Reason, "this machine was on it first") {
		t.Fatalf("node 1 (first on it) should keep acct_synth-1: %s %q", round2[0].AccountID, round2[0].Reason)
	}
	for i := 1; i < 3; i++ {
		if !strings.HasPrefix(round2[i].Reason, "home account 1 is also in use on node_synth-") {
			t.Fatalf("node %d reason %q", i+1, round2[i].Reason)
		}
	}
	// Round 3: the moved machines are home on their new accounts.
	for i, e := range f.nodes {
		a := e.resolve(fmt.Sprintf("run_synth-n%d-3", i+1))
		if a.AccountID != round2[i].AccountID {
			t.Fatalf("node %d round 3 %s, want %s (%s)", i+1, a.AccountID, round2[i].AccountID, a.Reason)
		}
	}
	// Once the raced runs end, every account has at most one machine.
	for i, e := range f.nodes {
		e.svc.Release(fmt.Sprintf("run_synth-n%d-1", i+1))
	}
	for acct, nodes := range f.hubUse() {
		m := map[string]bool{}
		for _, n := range nodes {
			m[n] = true
		}
		if len(m) > 1 {
			t.Fatalf("%s still on %d machines: %v", acct, len(m), nodes)
		}
	}
	f.noRejectedReports()
}
