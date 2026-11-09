package cli

import (
	"context"
	"strings"
	"testing"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/mock"
)

// snapNode is a node snapshot as `pyre --json nodes` reports it.
type snapNode struct {
	name                 string
	platform             string
	cores                int
	cpuPct, load1, load5 float64
	memTotal, memAvail   int64 // MB
	swapTotal, swapUsed  int64 // MB
	runs                 int
	pressure             string
}

func (s snapNode) node() c.Node {
	p := s.platform
	if p == "" {
		p = "linux"
	}
	return c.Node{NodeID: "node_synth-" + s.name, Name: s.name, Platform: p,
		Connection: c.NodeConnection{State: c.ConnConnected},
		Harnesses:  []c.HarnessInfo{{Harness: c.HarnessClaude, Available: true}, {Harness: c.HarnessCodex, Available: true}},
		Resources: c.NodeResources{CPUCount: s.cores, CPUPct: s.cpuPct, Load1: s.load1, Load5: s.load5,
			MemTotalMB: s.memTotal, MemAvailMB: s.memAvail, SwapTotalMB: s.swapTotal, SwapUsedMB: s.swapUsed,
			ActiveRuns: s.runs, MemPressure: s.pressure}}
}

// The fleet on Oct 31 at 00:56 UTC (iss_…), from
// `pyre --json nodes`: devbox had taken new sessions while server sat
// nearly idle.
var (
	snapHome      = snapNode{name: "devbox", cores: 16, cpuPct: 67.4, load1: 23.54, load5: 35.39, memTotal: 48162, memAvail: 24136, swapTotal: 16383, swapUsed: 9400, runs: 14}
	snapS76       = snapNode{name: "server", cores: 32, cpuPct: 7.7, load1: 2.12, load5: 2.1, memTotal: 124118, memAvail: 83597, swapTotal: 98303, swapUsed: 35082, runs: 37}
	snapMac       = snapNode{name: "mac", cores: 10, cpuPct: 3, load1: 1.64, load5: 1.67, memTotal: 16384, memAvail: 3418, swapTotal: 3072, swapUsed: 1866, runs: 1}
	snapMacmini   = snapNode{name: "laptop", cores: 10, cpuPct: 8, load1: 2.84, load5: 2.87, memTotal: 16384, memAvail: 10507, swapTotal: 2048, swapUsed: 844, runs: 5}
	snapOffice1   = snapNode{name: "office-box1", cores: 8, cpuPct: 67.1, load1: 10.79, load5: 9.82, memTotal: 30984, memAvail: 13083, swapTotal: 8191, swapUsed: 1533, runs: 6}
	snapOffice2   = snapNode{name: "office-box2", cores: 8, cpuPct: 69.8, load1: 8.71, load5: 9.34, memTotal: 30984, memAvail: 16746, swapTotal: 8191, swapUsed: 2378, runs: 6}
	snapThinkpad  = snapNode{name: "thinkpad", cores: 4, cpuPct: 5.9, load1: 0.18, load5: 0.65, memTotal: 7103, memAvail: 5417, swapTotal: 4095, swapUsed: 18, runs: 3}
	snapWinOffice = snapNode{name: "win-office", platform: "windows", cores: 24, cpuPct: 1.6, memTotal: 97991, memAvail: 62296, swapTotal: 19656, swapUsed: 2254}
	// The issue's own numbers at 00:53, when the two reviewers landed.
	snapHomePeak = snapNode{name: "devbox", cores: 16, cpuPct: 76, load1: 38, load5: 35, memTotal: 48162, memAvail: 5120, swapTotal: 16383, swapUsed: 10240, runs: 14}
)

func TestOverloaded(t *testing.T) {
	for _, tc := range []struct {
		name string
		snap snapNode
		want string // substring; "" = healthy
	}{
		{"incident peak", snapHomePeak, "load 2.4 per core"},
		{"incident peak swap", snapHomePeak, "swap 10.0 of 16 GB used"},
		{"load5 counts while load1 dips", snapHome, "load 2.2 per core"},
		{"idle large machine with old swap", snapS76, ""},
		{"busy but under 1.5 per core", snapOffice1, ""},
		{"small mac swap file", snapMac, ""},
		{"small idle laptop", snapThinkpad, ""},
		{"memory under 10%", snapNode{cores: 8, load1: 1, memTotal: 32000, memAvail: 3000}, "9% memory free (2.9 GB)"},
		{"critical pressure", snapNode{cores: 8, load1: 1, memTotal: 32000, memAvail: 20000, pressure: c.MemPressureCritical}, "memory pressure critical"},
		{"no machine stats", snapNode{}, ""},
	} {
		got := overloaded(tc.snap.node().Resources)
		if tc.want == "" && got != "" || tc.want != "" && !strings.Contains(got, tc.want) {
			t.Errorf("%s: overloaded = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestPickPlacementOverload(t *testing.T) {
	nodes := func(ss ...snapNode) []c.Node {
		var out []c.Node
		for _, s := range ss {
			out = append(out, s.node())
		}
		return out
	}
	for _, tc := range []struct {
		name    string
		nodes   []c.Node
		self    string
		want    string
		reason  []string // substrings of Reason
		warning string   // substring of Warning; "" = none
	}{
		{name: "incident: home overloaded, server idle", nodes: nodes(snapHomePeak, snapS76), self: "devbox",
			want: "server", reason: []string{"skipped overloaded devbox (load 2.4 per core"}},
		{name: "whole fleet at 00:56", nodes: nodes(snapHome, snapS76, snapMac, snapMacmini, snapOffice1, snapOffice2, snapThinkpad, snapWinOffice), self: "devbox",
			want: "server", reason: []string{"skipped overloaded devbox", "1 Windows node(s) skipped"}},
		{name: "most headroom beats a small idle machine", nodes: nodes(snapThinkpad, snapS76), want: "server"},
		{name: "busy healthy node beats an overloaded idle-looking one", nodes: nodes(snapOffice1, snapHomePeak), want: "office-box1"},
		{name: "every node overloaded: still placed, with a warning", nodes: nodes(snapHomePeak, snapNode{name: "other", cores: 8, load1: 20, memTotal: 32000, memAvail: 16000}),
			want: "other", reason: []string{"although it is overloaded"}, warning: "other is overloaded (load 2.5 per core); every eligible node is overloaded, so it starts there anyway"},
		{name: "overloaded unix before an idle Windows node, said so", nodes: nodes(snapHomePeak, snapWinOffice),
			want: "devbox", warning: "every eligible non-Windows node is overloaded (a Windows one is not: name it with --node)"},
		{name: "self bonus does not keep work on an overloaded caller", nodes: nodes(snapHomePeak, snapMacmini), self: "devbox", want: "laptop"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			self := ""
			if tc.self != "" {
				self = "node_synth-" + tc.self
			}
			pl, err := pickPlacement(tc.nodes, placeReq{Harness: "codex", Self: self})
			if err != nil || pl.NodeName != tc.want {
				t.Fatalf("placed on %q (%v), want %q: %s", pl.NodeName, err, tc.want, pl.Reason)
			}
			for _, r := range tc.reason {
				if !strings.Contains(pl.Reason, r) {
					t.Errorf("reason %q lacks %q", pl.Reason, r)
				}
			}
			if tc.warning == "" && pl.Warning != "" || !strings.Contains(pl.Warning, tc.warning) {
				t.Errorf("warning %q, want %q", pl.Warning, tc.warning)
			}
		})
	}
}

// Account room is a requirement, not a tiebreak over load: a healthy node
// that would run the launch on a reserve account beats an overloaded node
// with a normal account; between two healthy nodes the normal account wins.
func TestPlaceAmongOverloadBeatsSoftAccount(t *testing.T) {
	b := mock.New(mock.Options{Clock: mock.NewOffsetClock(mock.FixtureEpoch), DefaultPrincipal: &testOwner})
	home, server, laptop := snapHomePeak.node(), snapS76.node(), snapMacmini.node()
	o := &Ops{API: selectAPI{API: b, sel: map[string]*c.AccountSelection{
		home.NodeID:    {AccountID: "acct_synth-a"},
		laptop.NodeID: {AccountID: "acct_synth-a"},
		server.NodeID:     {AccountID: "acct_synth-r", Reserve: true, Reason: "reserve account: no normal account has room"},
	}}}
	ctx := context.Background()
	pl, err := o.placeAmong(ctx, []c.Node{home, server}, "claude", "", false, false, nil, false, "", false, false, false)
	if err != nil || pl.NodeID != server.NodeID || pl.Warning != "" {
		t.Fatalf("reserve on healthy vs normal on overloaded: %+v %v", pl, err)
	}
	pl, err = o.placeAmong(ctx, []c.Node{home, server, laptop}, "claude", "", false, false, nil, false, "", false, false, false)
	if err != nil || pl.NodeID != laptop.NodeID {
		t.Fatalf("normal account on a healthy node first: %+v %v", pl, err)
	}
}

// A start with --cwd (no node) stays on the caller's machine; when that
// machine is overloaded the result says so and how to get placed instead.
func TestWarnNamedOverloaded(t *testing.T) {
	b := mock.New(mock.Options{Clock: mock.NewOffsetClock(mock.FixtureEpoch), DefaultPrincipal: &testOwner})
	page, err := b.ListNodes(context.Background(), c.ListNodesRequest{})
	if err != nil || len(page.Items) == 0 {
		t.Fatal(err)
	}
	n := page.Items[0]
	api := &avoidAPI{API: overloadAPI{API: b, id: n.NodeID}, flags: map[string][]c.NodeAvoid{}}
	o := &Ops{API: api}
	var pl Placement
	o.warnNamed(withPlacement(context.Background(), &pl), n.NodeID, &StartParams{Cwd: "/synthetic/wt"})
	if pl.NodeID != n.NodeID || !strings.Contains(pl.Warning, "is overloaded (load 2.4 per core") || !strings.Contains(pl.Warning, "leave out --cwd") {
		t.Fatalf("cwd: %+v", pl)
	}
	pl = Placement{}
	o.warnNamed(withPlacement(context.Background(), &pl), n.NodeID, &StartParams{Node: n.NodeID})
	if !strings.Contains(pl.Warning, "named with --node") {
		t.Fatalf("named: %+v", pl)
	}
}

// overloadAPI reports node id with the incident's resources.
type overloadAPI struct {
	c.API
	id string
}

func (a overloadAPI) GetNode(ctx context.Context, req c.GetRequest) (c.Node, error) {
	n, err := a.API.GetNode(ctx, req)
	if n.NodeID == a.id {
		n.Resources = snapHomePeak.node().Resources
	}
	return n, err
}
