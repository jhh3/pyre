package cli

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/mock"
)

// needsAPI serves a fixed fleet and answers needs.check from per-node
// synthetic disks (nil: an older pyred, the action is unsupported).
type needsAPI struct {
	c.API
	nodes []c.Node
	disk  map[string]map[string]bool // node -> tool or path -> present
	mu    sync.Mutex
	asked map[string]int
}

func (a *needsAPI) ListNodes(context.Context, c.ListNodesRequest) (c.Page[c.Node], error) {
	return c.Page[c.Node]{Items: a.nodes}, nil
}

func (a *needsAPI) InvokeNodeAction(ctx context.Context, req c.NodeActionRequest) (c.NodeActionResult, error) {
	if req.Action != c.ActionNeedsCheck {
		return a.API.InvokeNodeAction(ctx, req)
	}
	a.mu.Lock()
	a.asked[req.NodeID]++
	a.mu.Unlock()
	d := a.disk[req.NodeID]
	if d == nil {
		return c.NodeActionResult{Outcome: c.OutcomeUnsupported}, nil
	}
	var p c.NeedsCheckParams
	_ = json.Unmarshal(req.Params, &p)
	out := c.NeedsCheckResult{Tools: map[string]bool{}, Paths: map[string]bool{}}
	for _, t := range p.Tools {
		out.Tools[t] = d[t]
	}
	for _, x := range p.Paths {
		out.Paths[x] = d[x]
	}
	b, _ := json.Marshal(out)
	return c.NodeActionResult{Outcome: c.OutcomeSucceeded, Result: b}, nil
}

func needsFleet(t *testing.T) (*Ops, *needsAPI) {
	t.Helper()
	b := mock.New(mock.Options{Clock: mock.NewOffsetClock(mock.FixtureEpoch), DefaultPrincipal: &testOwner})
	server, home, off1, old := snapS76.node(), snapHome.node(), snapOffice1.node(), snapThinkpad.node()
	// The office NUC is the least loaded of the three with facts; it lacks doppler.
	off1.Resources = snapThinkpad.node().Resources
	server.Resources.Tools = map[string]bool{"doppler": true, "gcloud": true, "docker": true}
	home.Resources.Tools = map[string]bool{"doppler": true, "gcloud": true, "docker": true}
	off1.Resources.Tools = map[string]bool{"doppler": false, "gcloud": false, "docker": true}
	old.Name, old.NodeID = "old", "node_synth-old" // an older pyred: no facts, no needs.check
	old.Resources = snapThinkpad.node().Resources
	api := &needsAPI{API: b, nodes: []c.Node{server, home, off1, old}, asked: map[string]int{},
		disk: map[string]map[string]bool{
			server.NodeID:  {"/home/synth/fast/roadmap/pt.sh": true, "ffmpeg": true},
			home.NodeID: {},
			off1.NodeID: {"ffmpeg": true},
		}}
	return &Ops{API: api}, api
}

func TestParseNeeds(t *testing.T) {
	ns, err := parseNeeds([]string{"doppler", "gcloud,~/x", "/abs/path", "doppler"})
	if err != nil || strings.Join(ns.tools, " ") != "doppler gcloud" || strings.Join(ns.paths, " ") != "~/x /abs/path" {
		t.Fatalf("parse: %+v %v", ns, err)
	}
	for _, bad := range []string{"rel/path", "a b", "~user/x"} {
		if _, err := parseNeeds([]string{bad}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestPlaceNeeds(t *testing.T) {
	ctx := context.Background()
	o, api := needsFleet(t)
	// Without needs the idle office NUC (or the old node) would win; doppler keeps it to server/home.
	pl, err := o.place(ctx, &StartParams{Harness: "claude", Needs: []string{"doppler"}}, nil)
	if err != nil || (pl.NodeID != "node_synth-server" && pl.NodeID != "node_synth-devbox") {
		t.Fatalf("doppler placement: %+v %v", pl, err)
	}
	if !strings.Contains(pl.Reason, "only nodes with doppler (devbox, server)") {
		t.Fatalf("reason: %s", pl.Reason)
	}
	// Facts answer known tools without asking the node.
	if api.asked["node_synth-server"] != 0 || api.asked["node_synth-office-box1"] != 0 {
		t.Fatalf("asked nodes for a reported tool: %v", api.asked)
	}
	// A path is checked over the node link: only server has it.
	pl, err = o.place(ctx, &StartParams{Harness: "claude", Needs: []string{"/home/synth/fast/roadmap/pt.sh"}}, nil)
	if err != nil || pl.NodeID != "node_synth-server" {
		t.Fatalf("path placement: %+v %v", pl, err)
	}
	// Nothing qualifies: an error naming what each lacks, the old node as unknown.
	_, err = o.place(ctx, &StartParams{Harness: "claude", Needs: []string{"doppler", "/nowhere"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "office-box1: no doppler, no /nowhere") ||
		!strings.Contains(err.Error(), "old: cannot check doppler, /nowhere there (its pyred is older; unknown counts as missing)") {
		t.Fatalf("no qualifying node: %v", err)
	}
}

func TestCheckNeedsNamedNode(t *testing.T) {
	ctx := context.Background()
	o, _ := needsFleet(t)
	err := o.checkNeeds(ctx, "node_synth-office-box1", []string{"doppler"})
	if err == nil || !strings.Contains(err.Error(), "office-box1: no doppler") || !strings.Contains(err.Error(), "qualifying: devbox, server") {
		t.Fatalf("named node refusal: %v", err)
	}
	if err := o.checkNeeds(ctx, "node_synth-server", []string{"doppler", "ffmpeg"}); err != nil {
		t.Fatalf("server has both: %v", err)
	}
}
