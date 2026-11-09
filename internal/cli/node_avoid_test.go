package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/mock"
)

// avoidNow is a fixed local time for --for/--until parsing: Friday 14:00
// in a synthetic UTC-7 zone.
var avoidZone = time.FixedZone("SYN", -7*3600)
var avoidNow = time.Date(2026, 10, 23, 14, 0, 0, 0, avoidZone)

func TestAvoidExpiryParsing(t *testing.T) {
	at := func(d time.Duration) time.Time { return avoidNow.Add(d) }
	day := func(dd, h, m int) time.Time { return time.Date(2026, 10, 23+dd, h, m, 0, 0, avoidZone) }
	ok := []struct {
		forS, until string
		want        time.Time
	}{
		{"", "", at(2 * time.Hour)}, // default
		{"2h", "", at(2 * time.Hour)},
		{"90m", "", at(90 * time.Minute)},
		{"1h30m", "", at(90 * time.Minute)},
		{"1d", "", at(24 * time.Hour)},
		{"2d12h", "", at(60 * time.Hour)},
		{"7d", "", at(7 * 24 * time.Hour)},
		{"", "18:00", day(0, 18, 0)},
		{"", "6pm", day(0, 18, 0)},
		{"", "6:30PM", day(0, 18, 30)},
		{"", "6 pm", day(0, 18, 0)},
		{"", "13:00", day(1, 13, 0)},  // passed today: tomorrow
		{"", "14:00", day(1, 14, 0)},  // now: tomorrow
		{"", "9:30am", day(1, 9, 30)}, // tomorrow
		{"", "2026-10-24 09:00", day(1, 9, 0)},
		{"", "2026-10-24T09:00", day(1, 9, 0)},
		{"", "2026-10-23T23:00:00Z", time.Date(2026, 10, 23, 23, 0, 0, 0, time.UTC)},
	}
	for _, tc := range ok {
		got, err := avoidExpiry(avoidNow, tc.forS, tc.until)
		if err != nil || !got.Equal(tc.want) {
			t.Errorf("--for %q --until %q: got %v %v, want %v", tc.forS, tc.until, got, err, tc.want)
		}
	}
	bad := []struct{ forS, until, want string }{
		{"2h", "18:00", "not both"},
		{"8d", "", "at most 7 days"},
		{"0h", "", "positive"},
		{"-1h", "", "positive"},
		{"soon", "", "not a duration"},
		{"", "teatime", "not a time"},
		{"", "2026-10-22 09:00", "future"},
		{"", "2026-11-23 09:00", "at most 7 days"},
	}
	for _, tc := range bad {
		if _, err := avoidExpiry(avoidNow, tc.forS, tc.until); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("--for %q --until %q: err %v, want %q", tc.forS, tc.until, err, tc.want)
		}
	}
}

// Expiry is shown in the reader's zone: the clock today, the weekday
// within a week.
func TestAvoidWhenText(t *testing.T) {
	if got := avoidWhen(avoidNow.Add(2*time.Hour).UTC(), avoidNow); got != "16:00 SYN (in 2h)" {
		t.Fatalf("today: %q", got)
	}
	if got := avoidWhen(avoidNow.Add(26*time.Hour), avoidNow); got != "Sat 16:00 SYN (in 26h)" {
		t.Fatalf("tomorrow: %q", got)
	}
	a := c.NodeAvoid{Hard: true, Reason: "synthetic: presenting", OwnerSet: true, ExpiresAt: avoidNow.Add(time.Hour)}
	if got := avoidLine(a, avoidNow); got != "hard until 15:00 SYN (in 1h), set by the owner — synthetic: presenting" {
		t.Fatalf("line: %q", got)
	}
	var b bytes.Buffer
	n := c.Node{NodeID: "node_synth-a", Name: "synth-a", Avoid: []c.NodeAvoid{a, {ExpiresAt: avoidNow.Add(-time.Minute)}}}
	printAvoids(&b, []c.Node{n}, avoidNow)
	if b.String() != "avoid synth-a: hard until 15:00 SYN (in 1h), set by the owner — synthetic: presenting\n" {
		t.Fatalf("printAvoids: %q", b.String())
	}
}

// pickPlacement skips soft-avoided nodes, or with UseAvoided takes one
// only when nothing else is eligible.
func TestPickPlacementAvoid(t *testing.T) {
	idle := synthNode("node_synth-idle", 5, 28000, 32000, 0, 4000, 1, c.HarnessClaude)
	busy := synthNode("node_synth-busy", 70, 6000, 64000, 7000, 8000, 30, c.HarnessClaude)
	avoid := map[string]string{idle.NodeID: "soft until 18:00 SYN: presenting"}
	pl, err := pickPlacement([]c.Node{idle, busy}, placeReq{Harness: "claude", Avoid: avoid})
	// busy is overloaded, which beats an avoided node (with a warning).
	if err != nil || pl.NodeID != busy.NodeID || strings.Contains(pl.Warning, "avoided") || !strings.Contains(pl.Warning, "overloaded") || !strings.Contains(pl.Reason, "skipped avoided synth-idle (soft until 18:00 SYN: presenting)") {
		t.Fatalf("skip: %+v %v", pl, err)
	}
	if _, err := pickPlacement([]c.Node{idle}, placeReq{Harness: "claude", Avoid: avoid}); err == nil || !strings.Contains(err.Error(), "avoided: synth-idle") {
		t.Fatalf("only avoided, not used: %v", err)
	}
	pl, err = pickPlacement([]c.Node{idle}, placeReq{Harness: "claude", Avoid: avoid, UseAvoided: true})
	if err != nil || pl.NodeID != idle.NodeID || !strings.Contains(pl.Reason, "although it is avoided") || !strings.Contains(pl.Warning, "no other node") {
		t.Fatalf("only avoided, used: %+v %v", pl, err)
	}
	// With UseAvoided an eligible other node still wins.
	if pl, _ = pickPlacement([]c.Node{idle, busy}, placeReq{Harness: "claude", Avoid: avoid, UseAvoided: true}); pl.NodeID != busy.NodeID {
		t.Fatalf("use avoided with another eligible: %+v", pl)
	}
}

// avoidAPI is an in-memory nodes.avoid over the mock backend: the flags it
// holds show on listed nodes, and it records requests.
type avoidAPI struct {
	c.API
	mu    sync.Mutex
	flags map[string][]c.NodeAvoid
	reqs  []c.AvoidNodeRequest
}

func (a *avoidAPI) decorate(n c.Node) c.Node {
	a.mu.Lock()
	defer a.mu.Unlock()
	n.Avoid = a.flags[n.NodeID]
	return n
}

func (a *avoidAPI) ListNodes(ctx context.Context, req c.ListNodesRequest) (c.Page[c.Node], error) {
	p, err := a.API.ListNodes(ctx, req)
	for i := range p.Items {
		p.Items[i] = a.decorate(p.Items[i])
	}
	return p, err
}

func (a *avoidAPI) GetNode(ctx context.Context, req c.GetRequest) (c.Node, error) {
	n, err := a.API.GetNode(ctx, req)
	return a.decorate(n), err
}

func (a *avoidAPI) AvoidNode(ctx context.Context, req c.AvoidNodeRequest) (c.AvoidNodeResponse, error) {
	n, err := a.API.GetNode(ctx, c.GetRequest{ID: req.NodeID})
	if err != nil {
		return c.AvoidNodeResponse{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reqs = append(a.reqs, req)
	if req.Clear {
		cleared := len(a.flags[req.NodeID])
		delete(a.flags, req.NodeID)
		return c.AvoidNodeResponse{NodeID: req.NodeID, NodeName: n.Name, Avoid: []c.NodeAvoid{}, Cleared: cleared}, nil
	}
	f := c.NodeAvoid{NodeID: req.NodeID, ProjectID: req.ProjectID, Hard: req.Hard, Reason: req.Reason, SetBy: testOwner, OwnerSet: true, ExpiresAt: *req.ExpiresAt}
	a.flags[req.NodeID] = []c.NodeAvoid{f}
	return c.AvoidNodeResponse{NodeID: req.NodeID, NodeName: n.Name, Avoid: a.flags[req.NodeID]}, nil
}

func (a *avoidAPI) set(nodeID string, hard bool, until time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.flags[nodeID] = []c.NodeAvoid{{NodeID: nodeID, Hard: hard, Reason: "synthetic", SetBy: testOwner, OwnerSet: true, ExpiresAt: until}}
}

func newAvoidAPI(t *testing.T) (*avoidAPI, []c.Node) {
	t.Helper()
	b := mock.New(mock.Options{Clock: mock.NewOffsetClock(mock.FixtureEpoch), DefaultPrincipal: &testOwner})
	page, err := b.ListNodes(context.Background(), c.ListNodesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var eligible []c.Node
	for _, n := range page.Items {
		if placeEligible(n, "claude", 0) && n.Platform != "windows" {
			eligible = append(eligible, n)
		}
	}
	if len(eligible) < 2 {
		t.Skip("fixture has fewer than two claude nodes")
	}
	return &avoidAPI{API: b, flags: map[string][]c.NodeAvoid{}}, eligible
}

// Automatic placement skips a soft-avoided node, falls back to it (with a
// warning) when nothing else fits, leaves hard-avoided nodes out unless
// overridden, and lifts the flag at its expiry. An explicit node starts
// with a warning.
func TestStartPlacementHonorsAvoidFlags(t *testing.T) {
	api, eligible := newAvoidAPI(t)
	now := time.Now()
	start := func(p StartParams) (Placement, error) {
		var pl Placement
		o := &Ops{API: api, Now: func() time.Time { return now }} // fresh: no pending placements
		p.Harness, p.Prompt = "claude", "synthetic"
		_, err := o.StartSession(withPlacement(context.Background(), &pl), p)
		return pl, err
	}
	first, err := start(StartParams{})
	if err != nil {
		t.Fatal(err)
	}
	api.set(first.NodeID, false, now.Add(time.Hour))
	pl, err := start(StartParams{})
	if err != nil || pl.NodeID == first.NodeID || !strings.Contains(pl.Reason, "skipped avoided "+first.NodeName) || pl.Warning != "" {
		t.Fatalf("soft avoid not skipped: %+v %v", pl, err)
	}
	// Every eligible node soft-avoided: one is still chosen, with a warning.
	for _, n := range eligible {
		api.set(n.NodeID, false, now.Add(time.Hour))
	}
	if pl, err = start(StartParams{}); err != nil || pl.NodeID == "" || !strings.Contains(pl.Warning, "no other node") {
		t.Fatalf("all soft-avoided: %+v %v", pl, err)
	}
	// Hard everywhere: no placement, the error names the flags; the
	// owner's override places (as soft).
	for _, n := range eligible {
		api.set(n.NodeID, true, now.Add(time.Hour))
	}
	if _, err = start(StartParams{}); err == nil || !strings.Contains(err.Error(), "hard-avoided") || !strings.Contains(err.Error(), "--ignore-avoid") {
		t.Fatalf("all hard-avoided: %v", err)
	}
	if pl, err = start(StartParams{IgnoreAvoid: true}); err != nil || !strings.Contains(pl.Warning, "avoided") {
		t.Fatalf("owner override: %+v %v", pl, err)
	}
	// Expired flags lift.
	now = now.Add(time.Hour)
	if pl, err = start(StartParams{}); err != nil || pl.Warning != "" || strings.Contains(pl.Reason, "avoided") {
		t.Fatalf("expired flags: %+v %v", pl, err)
	}
	// An explicit node with a soft flag starts there, with a warning.
	now = now.Add(-time.Hour)
	api.set(first.NodeID, false, now.Add(time.Hour))
	if pl, err = start(StartParams{Node: first.NodeID, Cwd: "/tmp/synth"}); err != nil || pl.NodeID != first.NodeID || !strings.Contains(pl.Warning, "is avoided (soft until") || !strings.Contains(pl.Warning, "starting there anyway") {
		t.Fatalf("explicit node: %+v %v", pl, err)
	}
	// A session cannot override the owner's hard flag: placement leaves it out.
	o := &Ops{API: api, Self: Identity{SessionID: "ses_synth-a"}, Now: func() time.Time { return now }}
	api.set(first.NodeID, true, now.Add(time.Hour))
	soft, hard := o.avoidedNodes([]c.Node{api.decorate(c.Node{NodeID: first.NodeID, Name: first.NodeName})}, "", true)
	if len(soft) != 0 || len(hard) != 1 {
		t.Fatalf("session override of the owner's hard flag: soft %v hard %v", soft, hard)
	}
}

// MCP avoid_node round trip: the tool resolves the node, turns for/until
// into an expiry on the injected clock, and passes hard, reason, project
// and clear through; list_nodes then shows the flag.
func TestMCPAvoidNodeRoundTrip(t *testing.T) {
	api, eligible := newAvoidAPI(t)
	target := eligible[0]
	now := time.Date(2026, 10, 23, 14, 0, 0, 0, time.Local)
	cl := startMCP(t, &Ops{API: api, PollInterval: 10 * time.Millisecond, Now: func() time.Time { return now }})
	raw, isErr := cl.tool("avoid_node", map[string]any{"node": target.Name, "for": "3h", "reason": "synthetic: benchmark", "hard": true})
	if isErr {
		t.Fatalf("avoid_node: %s", raw)
	}
	var res c.AvoidNodeResponse
	if err := json.Unmarshal(raw, &res); err != nil || res.NodeID != target.NodeID || len(res.Avoid) != 1 {
		t.Fatalf("result: %s (%v)", raw, err)
	}
	req := api.reqs[0]
	if req.NodeID != target.NodeID || !req.Hard || req.Reason != "synthetic: benchmark" || req.ExpiresAt == nil || !req.ExpiresAt.Equal(now.Add(3*time.Hour)) || req.OperationID == "" {
		t.Fatalf("request: %+v", req)
	}
	raw, isErr = cl.tool("list_nodes", map[string]any{})
	if isErr || !strings.Contains(string(raw), "avoid "+target.Name+": hard until") {
		t.Fatalf("list_nodes lacks the flag: %s", raw)
	}
	if raw, isErr = cl.tool("avoid_node", map[string]any{"node": target.NodeID, "for": "2h", "until": "18:00"}); !isErr || !strings.Contains(string(raw), "not both") {
		t.Fatalf("for+until: %s", raw)
	}
	if raw, isErr = cl.tool("avoid_node", map[string]any{"node": target.NodeID, "for": "30d"}); !isErr || !strings.Contains(string(raw), "at most 7 days") {
		t.Fatalf("too long: %s", raw)
	}
	raw, isErr = cl.tool("avoid_node", map[string]any{"node": target.NodeID, "clear": true})
	if isErr || json.Unmarshal(raw, &res) != nil || res.Cleared != 1 || !api.reqs[len(api.reqs)-1].Clear {
		t.Fatalf("clear: %s", raw)
	}
}

// `pyre node avoid` sets, lists and clears; `pyre nodes` shows the flag.
func TestNodeAvoidCLI(t *testing.T) {
	api, eligible := newAvoidAPI(t)
	target := eligible[0]
	e := newEnv(t, nil)
	app := func() *App { return &App{API: api} }
	r := run(t, e, app(), "node", "avoid", target.Name, "--until", "23:59", "--reason", "synthetic: presenting")
	expectCode(t, r, 0)
	if !strings.Contains(r.stdout, "avoid "+target.Name+": soft until") || !strings.Contains(r.stdout, "synthetic: presenting") {
		t.Fatalf("set:\n%s", r)
	}
	if f := api.reqs[0]; f.Hard || f.ExpiresAt == nil || f.ExpiresAt.Sub(time.Now()) <= 0 || f.ExpiresAt.Sub(time.Now()) > 24*time.Hour {
		t.Fatalf("request: %+v", f)
	}
	r = run(t, e, app(), "nodes")
	expectCode(t, r, 0)
	if !strings.Contains(r.stdout, "avoid "+target.Name+": soft until") {
		t.Fatalf("nodes:\n%s", r)
	}
	r = run(t, e, app(), "--json", "nodes")
	expectCode(t, r, 0)
	if !strings.Contains(r.stdout, `"avoid"`) || !strings.Contains(r.stdout, `"expires_at"`) {
		t.Fatalf("nodes --json:\n%s", r)
	}
	r = run(t, e, app(), "node", "avoid")
	if expectCode(t, r, 0); !strings.Contains(r.stdout, "avoid "+target.Name) {
		t.Fatalf("list:\n%s", r)
	}
	r = run(t, e, app(), "node", "avoid", target.Name, "--for", "8d")
	if r.code == 0 || !strings.Contains(r.stderr, "at most 7 days") {
		t.Fatalf("too long:\n%s", r)
	}
	r = run(t, e, app(), "node", "avoid", "--clear", target.Name, "--for", "1h")
	if r.code == 0 || !strings.Contains(r.stderr, "--clear takes only") {
		t.Fatalf("clear with --for:\n%s", r)
	}
	r = run(t, e, app(), "--json", "node", "avoid", "--clear", target.Name)
	expectCode(t, r, 0)
	if res := mustJSON[c.AvoidNodeResponse](t, r); res.Cleared != 1 {
		t.Fatalf("clear: %+v", res)
	}
}
