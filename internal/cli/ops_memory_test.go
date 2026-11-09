package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	c "github.com/jhh3/pyre/internal/contracts"
)

const noHiveMsg = "hive memory unavailable on this node: Hive is not installed on this node (no hive CLI and no memory index)"

// memoryAPI answers memory.search: the asking node has no Hive; a request
// without node_id gets routed (or refused, like an older hub).
type memoryAPI struct {
	c.API
	routed func(req c.NodeActionRequest) (c.NodeActionResult, error) // node_id ""
	calls  []c.NodeActionRequest
}

func (f *memoryAPI) InvokeNodeAction(_ context.Context, req c.NodeActionRequest) (c.NodeActionResult, error) {
	f.calls = append(f.calls, req)
	if req.NodeID == "" {
		return f.routed(req)
	}
	return c.NodeActionResult{CommandID: "cmd_synth-local", Outcome: c.OutcomeFailed, NodeID: req.NodeID,
		Error: c.Errorf(c.ErrUnavailable, noHiveMsg).WithDetail("backend", "unavailable")}, nil
}

func (f *memoryAPI) GetNode(_ context.Context, r c.GetRequest) (c.Node, error) {
	return c.Node{NodeID: r.ID, Name: strings.TrimPrefix(r.ID, "node_")}, nil
}

func memoryOps(api *memoryAPI) *Ops {
	return &Ops{API: api, Self: Identity{NodeID: "node_synth-macbook"}}
}

// A node without Hive: the search is served by the node the hub picks, and
// the result says which.
func TestMemorySearchServedByHiveNode(t *testing.T) {
	api := &memoryAPI{routed: func(req c.NodeActionRequest) (c.NodeActionResult, error) {
		var p c.MemorySearchParams
		_ = json.Unmarshal(req.Params, &p)
		raw, _ := json.Marshal(c.MemorySearchResult{Hits: []c.MemoryHit{{ID: "mem_synth-1", Title: p.Query}}, Freshness: c.Freshness{Source: "hive:cli"}})
		return c.NodeActionResult{CommandID: "cmd_synth-routed", Outcome: c.OutcomeSucceeded, Result: raw, NodeID: "node_synth-server"}, nil
	}}
	res, err := memoryOps(api).MemorySearch(context.Background(), MemorySearchParams{Query: "Mac target Skia", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 1 || res.Hits[0].Title != "Mac target Skia" {
		t.Fatalf("hits: %+v", res.Hits)
	}
	if res.ServedBy == nil || res.ServedBy.NodeID != "node_synth-server" || res.ServedBy.Name != "synth-server" {
		t.Fatalf("served_by: %+v", res.ServedBy)
	}
	if !strings.Contains(res.Freshness.Reason, "served by node synth-server") {
		t.Fatalf("freshness reason: %q", res.Freshness.Reason)
	}
	if len(api.calls) != 2 || api.calls[0].NodeID != "node_synth-macbook" || api.calls[1].NodeID != "" ||
		api.calls[1].TimeoutSeconds != memoryRouteSeconds || api.calls[1].OperationID == api.calls[0].OperationID {
		t.Fatalf("calls: %+v", api.calls)
	}
	var sent c.MemorySearchParams
	if json.Unmarshal(api.calls[1].Params, &sent) != nil || sent.Query != "Mac target Skia" || sent.Limit != 5 {
		t.Fatalf("routed params: %s", api.calls[1].Params)
	}
}

// No connected node has Hive: a clear error that keeps this node's reason.
func TestMemorySearchNoHiveAnywhere(t *testing.T) {
	api := &memoryAPI{routed: func(c.NodeActionRequest) (c.NodeActionResult, error) {
		return c.NodeActionResult{}, c.Errorf(c.ErrUnavailable, "no connected node serves Hive memory (none advertises memory.hive): install Hive on a node, or update pyred on the nodes that have it").
			WithDetail("reason", c.MemoryNoNodeReason)
	}}
	_, err := memoryOps(api).MemorySearch(context.Background(), MemorySearchParams{Query: "x"})
	ce := c.AsError(err)
	if ce.Code != c.ErrUnavailable || !strings.Contains(ce.Message, noHiveMsg) || !strings.Contains(ce.Message, "no connected node serves Hive memory") {
		t.Fatalf("error: %+v", ce)
	}
	if ExitCodeFor(err) != ExitUnavailable {
		t.Fatalf("exit %d", ExitCodeFor(err))
	}
}

// An older hub (node_id required), an unreachable hub, or a hub that sends
// the search back to this node: today's error, unchanged.
func TestMemorySearchOldHubKeepsTodaysError(t *testing.T) {
	for name, routed := range map[string]func(c.NodeActionRequest) (c.NodeActionResult, error){
		"old hub": func(c.NodeActionRequest) (c.NodeActionResult, error) {
			return c.NodeActionResult{}, c.Errorf(c.ErrInvalid, "node_id and action are required")
		},
		"mock-like hub": func(c.NodeActionRequest) (c.NodeActionResult, error) {
			return c.NodeActionResult{}, c.Errorf(c.ErrNotFound, "node  not found")
		},
		"hub down, node socket answers": func(c.NodeActionRequest) (c.NodeActionResult, error) {
			return c.NodeActionResult{Outcome: c.OutcomeFailed, Error: c.Errorf(c.ErrUnavailable, noHiveMsg)}, nil
		},
		"routed back here": func(c.NodeActionRequest) (c.NodeActionResult, error) {
			return c.NodeActionResult{Outcome: c.OutcomeFailed, NodeID: "node_synth-macbook", Error: c.Errorf(c.ErrUnavailable, noHiveMsg)}, nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := memoryOps(&memoryAPI{routed: routed}).MemorySearch(context.Background(), MemorySearchParams{Query: "x"})
			if ce := c.AsError(err); ce.Code != c.ErrUnavailable || ce.Message != noHiveMsg {
				t.Fatalf("error: %+v", ce)
			}
		})
	}
}

// The routed node fails or does not answer in time: the error names it.
func TestMemorySearchRoutedNodeFails(t *testing.T) {
	api := &memoryAPI{routed: func(c.NodeActionRequest) (c.NodeActionResult, error) {
		return c.NodeActionResult{CommandID: "cmd_synth-slow", Outcome: c.OutcomePending, NodeID: "node_synth-server"}, nil
	}}
	_, err := memoryOps(api).MemorySearch(context.Background(), MemorySearchParams{Query: "x"})
	if ce := c.AsError(err); ce.Code != c.ErrUnavailable || !strings.Contains(ce.Message, "node synth-server") || !strings.Contains(ce.Message, "did not within 20s") {
		t.Fatalf("error: %+v", ce)
	}
}

// An explicit node is never rerouted.
func TestMemorySearchExplicitNodeNotRerouted(t *testing.T) {
	api := &memoryAPI{routed: func(c.NodeActionRequest) (c.NodeActionResult, error) {
		t.Fatal("rerouted")
		return c.NodeActionResult{}, nil
	}}
	_, err := memoryOps(api).MemorySearch(context.Background(), MemorySearchParams{Query: "x", Node: "node_synth-macbook"})
	if !c.IsCode(err, c.ErrUnavailable) || len(api.calls) != 1 {
		t.Fatalf("%v %+v", err, api.calls)
	}
}
