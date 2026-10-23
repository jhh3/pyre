package cli

import (
	"context"
	"slices"
	"strings"
	"testing"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/daemon"
)

// pyre start --model/--effort and MCP start_session model/effort travel to
// the node as its per-launch args; nothing chosen sends none (the harness
// default, exactly as before).
func TestStartModelAndEffort(t *testing.T) {
	if modelArg != daemon.ModelArg || effortArg != daemon.EffortArg {
		t.Fatalf("CLI %q/%q != node %q/%q", modelArg, effortArg, daemon.ModelArg, daemon.EffortArg)
	}
	api := &startCapture{harnesses: synthModelHarnesses()}
	o := &Ops{API: api, Self: Identity{NodeID: "node_synth-mac"}}
	ctx := context.Background()
	if _, err := o.StartSession(ctx, StartParams{Harness: "agy", Cwd: "/synthetic", Model: "gemini-synthetic-high", Effort: "High"}); err != nil {
		t.Fatal(err)
	}
	if api.req.Harness != c.HarnessAgy || !slices.Equal(api.req.Args, []string{"--pyre-model=gemini-synthetic-high", "--pyre-effort=high"}) {
		t.Fatalf("args %v harness %s", api.req.Args, api.req.Harness)
	}
	if _, err := o.StartSession(ctx, StartParams{Harness: "codex", Cwd: "/synthetic", Permissions: "bypass", Effort: "xhigh"}); err != nil ||
		!slices.Equal(api.req.Args, []string{"--pyre-permissions=bypass", "--pyre-effort=xhigh"}) {
		t.Fatalf("effort only: %v %v", api.req.Args, err)
	}
	if _, err := o.StartSession(ctx, StartParams{Harness: "claude", Cwd: "/synthetic"}); err != nil || len(api.req.Args) != 0 {
		t.Fatalf("default: %v %v", api.req.Args, err)
	}
	for _, p := range []StartParams{
		{Harness: "claude", Cwd: "/s", Model: "--settings=/tmp/x"},
		{Harness: "claude", Cwd: "/s", Effort: "high; rm -rf"},
		{Harness: "shell", Cwd: "/s", Model: "opus"},
	} {
		if _, err := o.StartSession(ctx, p); err == nil {
			t.Fatalf("%+v accepted", p)
		}
	}
}

type modelsAPI struct{ c.API }

func (modelsAPI) ListNodes(context.Context, c.ListNodesRequest) (c.Page[c.Node], error) {
	return c.Page[c.Node]{Items: []c.Node{
		{NodeID: "node_synth-b", Name: "synth-linux", Harnesses: []c.HarnessInfo{
			{Harness: c.HarnessAgy, Available: true, Models: []c.ModelChoice{{ID: "synthetic-a-medium", Label: "Synthetic A", Default: true}}, Efforts: []string{"low", "high"}},
			{Harness: c.HarnessShell, Available: true},
			{Harness: c.HarnessCodex, Available: false},
		}},
		{NodeID: "node_synth-a", Name: "synth-mac", Harnesses: []c.HarnessInfo{{Harness: c.HarnessClaude, Available: true, Models: []c.ModelChoice{{ID: "opus"}}}}},
	}}, nil
}

func TestModelsList(t *testing.T) {
	o := &Ops{API: modelsAPI{}}
	l, err := o.Models(context.Background(), "", "")
	if err != nil || len(l) != 2 || l[0].NodeName != "synth-linux" || l[0].Harness != c.HarnessAgy || l[0].Models[0].ID != "synthetic-a-medium" ||
		l[1].Harness != c.HarnessClaude {
		t.Fatalf("models %+v %v", l, err)
	}
	if l, _ := o.Models(context.Background(), "", "claude"); len(l) != 1 || l[0].NodeID != "node_synth-a" {
		t.Fatalf("filtered %+v", l)
	}
	if d := modelDesc("opus", "high"); d != "opus, effort high" || !strings.Contains(modelDesc("", "low"), "default model") {
		t.Fatalf("desc %q", d)
	}
}

func synthModelHarnesses() []c.HarnessInfo {
	return []c.HarnessInfo{
		{Harness: c.HarnessClaude, Available: true, Models: []c.ModelChoice{{ID: "opus"}}, Efforts: []string{"low", "medium", "high", "xhigh", "max"}},
		{Harness: c.HarnessCodex, Available: true, Models: []c.ModelChoice{{ID: "gpt-synthetic"}}, Efforts: []string{"low", "medium", "high", "xhigh", "max", "ultra"}},
		{Harness: c.HarnessAgy, Available: true, Models: []c.ModelChoice{{ID: "gemini-synthetic-high"}}, Efforts: []string{"low", "high"}},
	}
}

// Rolling upgrade: a node whose pyred lists no model choices for the
// harness (older than --pyre-model) would pass the arg to the harness,
// which exits on the unknown option. The launch is refused with the reason
// instead, an effort the harness does not offer there too, and placement
// never picks such a node when a model is asked for.
func TestStartModelNeedsANodeThatAppliesIt(t *testing.T) {
	ctx := context.Background()
	old := &startCapture{harnesses: []c.HarnessInfo{{Harness: c.HarnessClaude, Available: true}}}
	o := &Ops{API: old, Self: Identity{NodeID: "node_synth-mac"}}
	if _, err := o.StartSession(ctx, StartParams{Harness: "claude", Cwd: "/synthetic", Model: "opus"}); err == nil ||
		!strings.Contains(err.Error(), "older version") || old.req.Harness != "" {
		t.Fatalf("old node: %v (started %q)", err, old.req.Harness)
	}
	if _, err := o.StartSession(ctx, StartParams{Harness: "claude", Cwd: "/synthetic"}); err != nil || len(old.req.Args) != 0 {
		t.Fatalf("old node without a model: %v %v", err, old.req.Args)
	}
	cur := &startCapture{harnesses: synthModelHarnesses()}
	o = &Ops{API: cur, Self: Identity{NodeID: "node_synth-mac"}}
	if _, err := o.StartSession(ctx, StartParams{Harness: "claude", Cwd: "/synthetic", Effort: "ultra"}); err == nil || !strings.Contains(err.Error(), "not offered by claude") {
		t.Fatalf("claude ultra: %v", err)
	}

	conn := c.NodeConnection{State: c.ConnConnected}
	oldN := c.Node{NodeID: "node_synth-old", Name: "old", Connection: conn, Harnesses: []c.HarnessInfo{{Harness: c.HarnessClaude, Available: true}},
		Resources: c.NodeResources{MemTotalMB: 32000, MemAvailMB: 31000}}
	newN := c.Node{NodeID: "node_synth-new", Name: "new", Connection: conn, Harnesses: synthModelHarnesses(),
		Resources: c.NodeResources{MemTotalMB: 32000, MemAvailMB: 1000, ActiveRuns: 9}}
	pick := &startCapture{nodes: []c.Node{oldN, newN}}
	o = &Ops{API: pick}
	pl, err := o.placeAmong(ctx, pick.nodes, "claude", "", false, false, nil, true)
	if err != nil || pl.NodeID != newN.NodeID {
		t.Fatalf("placement with a model: %+v %v", pl, err)
	}
	if pl, _ := o.placeAmong(ctx, pick.nodes, "claude", "", false, false, nil, false); pl.NodeID != oldN.NodeID {
		t.Fatalf("placement without a model should prefer the idle node: %+v", pl)
	}
}
