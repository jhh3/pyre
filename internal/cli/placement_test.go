package cli

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	c "github.com/jhh3/pyre/internal/contracts"
)

func synthNode(id string, cpuPct float64, memAvail, memTotal, swapUsed, swapTotal int64, runs int, harness ...c.Harness) c.Node {
	n := c.Node{NodeID: id, Name: strings.TrimPrefix(id, "node_"), Platform: "linux",
		Connection: c.NodeConnection{State: c.ConnConnected},
		Resources: c.NodeResources{CPUCount: 8, CPUPct: cpuPct, MemAvailMB: memAvail, MemTotalMB: memTotal,
			SwapUsedMB: swapUsed, SwapTotalMB: swapTotal, ActiveRuns: runs}}
	for _, h := range harness {
		n.Harnesses = append(n.Harnesses, c.HarnessInfo{Harness: h, Available: true})
	}
	return n
}

func TestPickPlacementLeastLoaded(t *testing.T) {
	busy := synthNode("node_synth-busy", 70, 6000, 64000, 7000, 8000, 30, c.HarnessClaude)
	idle := synthNode("node_synth-idle", 5, 28000, 32000, 0, 4000, 1, c.HarnessClaude)
	nocodex := synthNode("node_synth-nocodex", 1, 30000, 32000, 0, 0, 0, c.HarnessClaude)
	off := synthNode("node_synth-off", 0, 32000, 32000, 0, 0, 0, c.HarnessClaude, c.HarnessCodex)
	off.Connection.State = c.ConnDisconnected
	nodes := []c.Node{busy, idle, nocodex, off}

	pl, err := pickPlacement(nodes, placeReq{Harness: "claude", Self: busy.NodeID})
	if err != nil || pl.NodeID != nocodex.NodeID {
		t.Fatalf("claude: %+v %v", pl, err)
	}
	if !strings.Contains(pl.Reason, "least loaded of 3") || !strings.Contains(pl.Reason, "synth-busy") {
		t.Fatalf("reason: %q", pl.Reason)
	}
	// Restricted to nodes holding the checkout; tie order follows among.
	if pl, _ = pickPlacement(nodes, placeReq{Harness: "claude", Self: busy.NodeID, Among: []string{busy.NodeID, idle.NodeID}}); pl.NodeID != idle.NodeID {
		t.Fatalf("among: %+v", pl)
	}
	// Nobody connected has codex.
	if _, err = pickPlacement(nodes, placeReq{Harness: "codex"}); !c.IsCode(err, c.ErrUnavailable) {
		t.Fatalf("codex: %v", err)
	}
	// Near-equal load keeps the caller's machine.
	a := synthNode("node_synth-a", 10, 20000, 32000, 0, 0, 2, c.HarnessClaude)
	b := synthNode("node_synth-b", 8, 20500, 32000, 0, 0, 2, c.HarnessClaude)
	if pl, _ = pickPlacement([]c.Node{b, a}, placeReq{Harness: "claude", Self: a.NodeID}); pl.NodeID != a.NodeID {
		t.Fatalf("self bonus: %+v", pl)
	}
	// A node at its advertised capacity is not eligible.
	full := synthNode("node_synth-full", 0, 32000, 32000, 0, 0, 4, c.HarnessClaude)
	full.Resources.MaxRuns = 4
	if pl, _ = pickPlacement([]c.Node{full, busy}, placeReq{Harness: "claude"}); pl.NodeID != busy.NodeID {
		t.Fatalf("capacity: %+v", pl)
	}
	// Windows nodes are skipped unless the caller is on Windows or nothing
	// else is eligible.
	win := synthNode("node_synth-win", 0, 60000, 64000, 0, 0, 0, c.HarnessClaude)
	win.Platform = "windows"
	if pl, _ = pickPlacement([]c.Node{win, busy}, placeReq{Harness: "claude"}); pl.NodeID != busy.NodeID || !strings.Contains(pl.Reason, "1 Windows node(s) skipped") {
		t.Fatalf("windows skipped: %+v", pl)
	}
	if pl, _ = pickPlacement([]c.Node{win, busy}, placeReq{Harness: "claude", Self: win.NodeID}); pl.NodeID != win.NodeID {
		t.Fatalf("caller on windows: %+v", pl)
	}
	if pl, _ = pickPlacement([]c.Node{win}, placeReq{Harness: "claude"}); pl.NodeID != win.NodeID || !strings.Contains(pl.Reason, "no other node") {
		t.Fatalf("only windows: %+v", pl)
	}
	// Nodes without a usable account are out, and the error says why.
	if pl, _ = pickPlacement(nodes, placeReq{Harness: "claude", Reject: map[string]string{nocodex.NodeID: "no eligible anthropic account now"}}); pl.NodeID != idle.NodeID ||
		!strings.Contains(pl.Reason, "usable account") || !strings.Contains(pl.Reason, "synth-nocodex: no eligible") {
		t.Fatalf("reject: %+v", pl)
	}
	if _, err = pickPlacement([]c.Node{idle}, placeReq{Harness: "claude", Reject: map[string]string{idle.NodeID: "account acct_x has no grant there"}}); err == nil || !strings.Contains(err.Error(), "no grant there") {
		t.Fatalf("all rejected: %v", err)
	}
	// Launches still starting count as load: a burst spreads out.
	twinA := synthNode("node_synth-twa", 5, 30000, 32000, 0, 0, 0, c.HarnessClaude)
	twinB := synthNode("node_synth-twb", 6, 30000, 32000, 0, 0, 0, c.HarnessClaude)
	if pl, _ = pickPlacement([]c.Node{twinA, twinB}, placeReq{Harness: "claude", Pending: map[string]int{twinA.NodeID: 2}}); pl.NodeID != twinB.NodeID || !strings.Contains(pl.Reason, "starting") {
		t.Fatalf("pending: %+v", pl)
	}
	// No machine stats never beats a reporting idle node.
	silent := synthNode("node_synth-silent", 0, 0, 0, 0, 0, 0, c.HarnessClaude)
	if pl, _ = pickPlacement([]c.Node{silent, idle}, placeReq{Harness: "claude"}); pl.NodeID != idle.NodeID {
		t.Fatalf("silent: %+v", pl)
	}
}

func TestStartWithoutNodeIsPlaced(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	r := run(t, e, nil, "--json", "start", "--harness", "claude", "--prompt", "synthetic task")
	expectCode(t, r, 0)
	res := mustJSON[c.StartSessionResponse](t, r)
	if !strings.Contains(r.stderr, "placed on "+res.Session.NodeName+": ") || !strings.Contains(r.stderr, "least loaded") {
		t.Fatalf("placement note missing:\n%s", r)
	}
	// MCP returns the placement beside the session.
	ops := &Ops{API: h.b}
	var pl Placement
	if _, err := ops.StartSession(withPlacement(context.Background(), &pl), StartParams{Harness: "codex", Prompt: "synthetic"}); err != nil || pl.NodeID == "" {
		t.Fatalf("ops placement: %+v %v", pl, err)
	}
}

// selectAPI answers account.select per node (the mock's is node-agnostic).
type selectAPI struct {
	c.API
	sel map[string]*c.AccountSelection // nil entry: the node cannot answer
}

func (s selectAPI) InvokeNodeAction(ctx context.Context, req c.NodeActionRequest) (c.NodeActionResult, error) {
	if req.Action != c.ActionAccountSelect {
		return s.API.InvokeNodeAction(ctx, req)
	}
	sel := s.sel[req.NodeID]
	if sel == nil {
		return c.NodeActionResult{}, c.Errorf(c.ErrUnavailable, "synthetic: no answer")
	}
	var p c.AccountSelectParams
	_ = json.Unmarshal(req.Params, &p)
	out := *sel
	if p.Policy.Mode == c.AccountExplicit && out.AccountID != p.Policy.AccountID {
		out.Reason = "synthetic: explicit account has no grant here"
	}
	b, _ := json.Marshal(out)
	return c.NodeActionResult{Outcome: c.OutcomeSucceeded, Result: b}, nil
}

func TestAccountFilter(t *testing.T) {
	withLogin := synthNode("node_synth-login", 0, 30000, 32000, 0, 0, 0, c.HarnessClaude)
	withLogin.Harnesses[0].Login = &c.HarnessLogin{Email: "synthetic@example.invalid"}
	ok := synthNode("node_synth-ok", 0, 30000, 32000, 0, 0, 0, c.HarnessClaude)
	limited := synthNode("node_synth-limited", 0, 30000, 32000, 0, 0, 0, c.HarnessClaude)
	silent := synthNode("node_synth-silent", 0, 30000, 32000, 0, 0, 0, c.HarnessClaude)
	nodes := []c.Node{withLogin, ok, limited, silent}
	o := &Ops{API: selectAPI{sel: map[string]*c.AccountSelection{
		withLogin.NodeID: {Outcome: c.FallbackWaiting, Reason: "no Pyre account here"},
		ok.NodeID:        {AccountID: "acct_synth-a"},
		limited.NodeID:   {Outcome: c.FallbackWaiting, Reason: "all at their limit"},
	}}}
	bg := context.Background()
	rej, soft := o.accountFilter(bg, nodes, "claude", "", false, false)
	if len(rej) != 1 || len(soft) != 0 || !strings.Contains(rej[limited.NodeID], "all at their limit") {
		t.Fatalf("eligible: %v %v", rej, soft)
	}
	rej, _ = o.accountFilter(bg, nodes, "claude", "acct_synth-a", false, false)
	if _, bad := rej[ok.NodeID]; bad || len(rej) != 2 || rej[withLogin.NodeID] == "" || rej[limited.NodeID] == "" {
		t.Fatalf("explicit: %v", rej)
	}
	// A soft pin: another eligible account there is a soft rejection; strict
	// makes it hard.
	other := synthNode("node_synth-other", 0, 30000, 32000, 0, 0, 0, c.HarnessClaude)
	o.API.(selectAPI).sel[other.NodeID] = &c.AccountSelection{AccountID: "acct_synth-b"}
	if rej, soft = o.accountFilter(bg, []c.Node{other}, "claude", "acct_synth-a", false, false); len(rej) != 0 || soft[other.NodeID] == "" {
		t.Fatalf("soft pin: %v %v", rej, soft)
	}
	if rej, soft = o.accountFilter(bg, []c.Node{other}, "claude", "acct_synth-a", false, true); rej[other.NodeID] == "" || len(soft) != 0 {
		t.Fatalf("strict pin: %v %v", rej, soft)
	}
	rej, _ = o.accountFilter(bg, nodes, "claude", "", true, false)
	if _, bad := rej[withLogin.NodeID]; bad || len(rej) != 3 {
		t.Fatalf("ambient: %v", rej)
	}
	if rej, soft = o.accountFilter(bg, nodes, "shell", "", false, false); rej != nil || soft != nil {
		t.Fatal("shell has no accounts")
	}
	// agy: no managed logins, nothing to balance (every node with agy runs
	// its own login), so no account dry runs reject a node.
	if rej, soft = o.accountFilter(bg, nodes, "agy", "", false, false); rej != nil || soft != nil {
		t.Fatal("agy is never filtered by accounts")
	}
}

// With no free Codex account anywhere, placement still picks a node and the
// harness preflight (not a second search) applies --fallback-harness.
func TestPlacedCodexWithoutFreeAccountFallsBack(t *testing.T) {
	h := newHub(t)
	nodes, err := h.b.ListNodes(context.Background(), c.ListNodesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	full := map[string]*c.AccountSelection{}
	for _, n := range nodes.Items {
		full[n.NodeID] = &c.AccountSelection{Outcome: c.FallbackWaiting, Reason: "synthetic: every OpenAI account is at its limit"}
	}
	var calls atomic.Int32
	api := countingAPI{selectAPI: selectAPI{API: h.b, sel: full}, calls: &calls}
	o := &Ops{API: api}
	var pl Placement
	res, err := o.StartSession(withPlacement(context.Background(), &pl), StartParams{Harness: "codex", Prompt: "review this", FallbackHarness: "claude"})
	if err != nil || res.Session.Session.Harness != c.HarnessClaude || !strings.Contains(res.Session.Session.Title, codexUnavailableTitle) {
		t.Fatalf("fallback: %+v %v", res.Session.Session, err)
	}
	if !strings.Contains(pl.Reason, "the preflight decides") {
		t.Fatalf("placement: %+v", pl)
	}
	eligible := 0
	for _, n := range nodes.Items {
		if placeEligible(n, "codex", 0) {
			eligible++
		}
	}
	if n := int(calls.Load()); n != eligible+1 { // one dry run per candidate, one by the preflight on the chosen node
		t.Fatalf("%d account.select calls for %d codex nodes", n, eligible)
	}
	// Without the fallback the launch fails with the reason, before a session exists.
	if _, err := o.StartSession(context.Background(), StartParams{Harness: "codex", Prompt: "review this"}); !c.IsCode(err, c.ErrCapacityExhausted) {
		t.Fatalf("no fallback: %v", err)
	}
}

type countingAPI struct {
	selectAPI
	calls *atomic.Int32 // accountFilter dry-runs nodes concurrently
}

func (a countingAPI) InvokeNodeAction(ctx context.Context, req c.NodeActionRequest) (c.NodeActionResult, error) {
	if req.Action == c.ActionAccountSelect {
		a.calls.Add(1)
	}
	return a.selectAPI.InvokeNodeAction(ctx, req)
}

type agyAccountsAPI struct{ selectAPI }

func (agyAccountsAPI) ListAccounts(context.Context, c.ListAccountsRequest) (c.Page[c.AccountView], error) {
	return c.Page[c.AccountView]{Items: []c.AccountView{{Account: c.Account{AccountID: "acct_synth-agy", Provider: c.ProviderGoogle, Label: "agy", RotaRef: "synth-agy@example.invalid"}}}}, nil
}

// agy with an explicit account runs only where it is the machine's own agy
// login.
func TestAccountFilterAgyExplicit(t *testing.T) {
	mine := synthNode("node_synth-mine", 0, 30000, 32000, 0, 0, 0, c.HarnessAgy)
	mine.Harnesses[0].Login = &c.HarnessLogin{Email: "synth-agy@example.invalid"}
	other := synthNode("node_synth-other", 0, 30000, 32000, 0, 0, 0, c.HarnessAgy)
	other.Harnesses[0].Login = &c.HarnessLogin{Email: "someone-else@example.invalid"}
	o := &Ops{API: agyAccountsAPI{selectAPI{sel: map[string]*c.AccountSelection{}}}}
	rej, soft := o.accountFilter(context.Background(), []c.Node{mine, other}, "agy", "acct_synth-agy", false, false)
	if len(soft) != 0 || len(rej) != 1 || rej[other.NodeID] == "" {
		t.Fatalf("explicit agy: %v %v", rej, soft)
	}
}

// A node that would run the launch on a reserve account only is a soft
// rejection: a node with a normal account with room goes first, and the
// reserve node still takes it when nothing else can.
func TestAccountFilterReserveIsSoft(t *testing.T) {
	normal := synthNode("node_synth-normal", 0, 30000, 32000, 0, 0, 0, c.HarnessClaude)
	reserve := synthNode("node_synth-reserve", 0, 30000, 32000, 0, 0, 0, c.HarnessClaude)
	o := &Ops{API: selectAPI{sel: map[string]*c.AccountSelection{
		normal.NodeID:  {AccountID: "acct_synth-a"},
		reserve.NodeID: {AccountID: "acct_synth-r", Reserve: true, Reason: "reserve account: no normal anthropic account has room"},
	}}}
	rej, soft := o.accountFilter(context.Background(), []c.Node{normal, reserve}, "claude", "", false, false)
	if len(rej) != 0 || len(soft) != 1 || !strings.Contains(soft[reserve.NodeID], "only a reserve anthropic account has room there") {
		t.Fatalf("rej %v soft %v", rej, soft)
	}
	// An explicit account is not weighed by tier.
	if _, soft = o.accountFilter(context.Background(), []c.Node{reserve}, "claude", "acct_synth-r", false, false); len(soft) != 0 {
		t.Fatalf("explicit reserve: %v", soft)
	}
}
