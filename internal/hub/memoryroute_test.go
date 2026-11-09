package hub

import (
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

const nodeHub = "node_synth-zhub"

// connectCaps connects nodeID with a hello advertising caps.
func (x *harness) connectCaps(nodeID string, caps ...string) {
	x.t.Helper()
	x.ch.setConnected(nodeID, true)
	hello := helloFor(nodeID, 4)
	hello.Capabilities = caps
	if _, err := x.h.HandleHello(nodeCtx(nodeID), nodeID, hello); err != nil {
		x.t.Fatalf("hello %s: %v", nodeID, err)
	}
}

// A memory.search without node_id goes to a connected node advertising
// Hive memory, the hub's own machine first, and the result names it.
func TestMemorySearchRoutesToHiveNode(t *testing.T) {
	x := newHarness(t)
	x.h.opt.Hostname = "Synth-Zhub.local"                            // case and domain ignored
	x.connectCaps(nodeMac)                                           // no Hive (the asking laptop)
	x.connectCaps(nodeLin, contracts.NodeCapMemoryHive)              // has Hive
	x.connectCaps(nodeHub, "local_api", contracts.NodeCapMemoryHive) // has Hive, on the hub's machine
	x.ch.onSend = func(node string, cmd contracts.NodeCommand) {
		if cmd.Kind == contracts.CmdAction {
			go x.result(node, cmd, contracts.OutcomeSucceeded, contracts.MemorySearchResult{Hits: []contracts.MemoryHit{{ID: "mem_synth", Title: "Mac target Skia"}}})
		}
	}
	req := contracts.NodeActionRequest{Mutation: contracts.Mutation{OperationID: "op_mem1"}, Action: contracts.ActionMemorySearch, Params: contracts.RawJSON(`{"query":"Mac target Skia"}`), TimeoutSeconds: 2}
	res, err := x.h.InvokeNodeAction(as(owner), req)
	if err != nil || res.Outcome != contracts.OutcomeSucceeded || res.NodeID != nodeHub {
		t.Fatalf("routed search: %v %+v", err, res)
	}
	if got := x.ch.sentKinds(nodeHub, contracts.CmdAction); len(got) != 1 {
		t.Fatalf("hub's own node got %d actions", len(got))
	}
	if len(x.ch.sentKinds(nodeMac, contracts.CmdAction))+len(x.ch.sentKinds(nodeLin, contracts.CmdAction)) != 0 {
		t.Fatal("search sent to a node other than the chosen one")
	}

	// A session may search the same way (memory.search is a session action).
	req.OperationID = "op_mem2"
	if res, err = x.h.InvokeNodeAction(as(sesPrincipal("ses_synth-asker")), req); err != nil || res.NodeID != nodeHub {
		t.Fatalf("session routed search: %v %+v", err, res)
	}

	// With the hub's node gone, another Hive node answers.
	x.disconnect(nodeHub)
	req.OperationID = "op_mem3"
	if res, err = x.h.InvokeNodeAction(as(owner), req); err != nil || res.NodeID != nodeLin {
		t.Fatalf("fallback to another Hive node: %v %+v", err, res)
	}

	// An explicit node is never rerouted.
	req.OperationID, req.NodeID = "op_mem4", nodeMac
	if res, err = x.h.InvokeNodeAction(as(owner), req); err != nil || res.NodeID != nodeMac {
		t.Fatalf("explicit node: %v %+v", err, res)
	}
}

// No connected node advertises Hive memory (none has it, or only older
// pyreds that never advertise): a clear unavailable error with a reason the
// client recognizes.
func TestMemorySearchNoHiveNode(t *testing.T) {
	x := newHarness(t)
	x.connectCaps(nodeMac, "local_api")
	x.connectCaps(nodeLin) // an older pyred: no memory.hive even with Hive installed
	x.connectCaps(nodeHub, contracts.NodeCapMemoryHive)
	x.disconnect(nodeHub) // advertises, but offline
	_, err := x.h.InvokeNodeAction(as(owner), contracts.NodeActionRequest{Mutation: contracts.Mutation{OperationID: "op_mem_none"},
		Action: contracts.ActionMemorySearch, Params: contracts.RawJSON(`{"query":"x"}`), TimeoutSeconds: 1})
	wantCode(t, err, contracts.ErrUnavailable)
	if ce := contracts.AsError(err); ce.Details["reason"] != contracts.MemoryNoNodeReason {
		t.Fatalf("reason: %+v", ce)
	}
	if n := len(x.ch.sentKinds(nodeMac, contracts.CmdAction)) + len(x.ch.sentKinds(nodeLin, contracts.CmdAction)); n != 0 {
		t.Fatalf("%d actions sent with no Hive node", n)
	}
	// Other actions still require node_id.
	_, err = x.h.InvokeNodeAction(as(owner), contracts.NodeActionRequest{Mutation: contracts.Mutation{OperationID: "op_mem_status"}, Action: contracts.ActionMemoryStatus})
	wantCode(t, err, contracts.ErrInvalid)
}
