package cli

import (
	"context"
	"encoding/json"
	"strings"

	c "github.com/jhh3/pyre/internal/contracts"
)

// memoryRouteSeconds bounds the wait for a routed memory search. The first
// attempt fails at once on a node without Hive, so a search takes at most
// about this long more.
const memoryRouteSeconds = 20

// memorySearchRouted retries a memory search this node could not answer
// (localErr, ErrUnavailable: no Hive here) without a node, so the hub picks
// a connected node that advertises Hive memory (c.NodeCapMemoryHive). The
// result names the node that answered (ServedBy). An older hub (node_id
// required), an unreachable hub, or a hub that sent it back here leave
// localErr as it was; a hub with no Hive node says so after it.
func (o *Ops) memorySearchRouted(ctx context.Context, params c.MemorySearchParams, localErr error) (c.MemorySearchResult, error) {
	local := c.AsError(localErr).Message
	raw, err := json.Marshal(params)
	if err != nil {
		return c.MemorySearchResult{}, localErr
	}
	res, err := o.API.InvokeNodeAction(ctx, c.NodeActionRequest{Mutation: mut(""), Action: c.ActionMemorySearch, Params: raw, TimeoutSeconds: memoryRouteSeconds})
	if err != nil {
		if ce := c.AsError(err); ce.Code == c.ErrUnavailable && ce.Details["reason"] == c.MemoryNoNodeReason {
			return c.MemorySearchResult{}, c.Errorf(c.ErrUnavailable, "%s; %s", local, ce.Message).WithDetail("reason", c.MemoryNoNodeReason)
		}
		return c.MemorySearchResult{}, localErr
	}
	self, _ := o.NodeID(ctx, "") // the node that just failed (resolves as the first attempt did)
	if res.NodeID == "" || res.NodeID == self {
		// Not routed (the node socket answered while the hub is down), or
		// routed back to this node: nothing new to say.
		var out c.MemorySearchResult
		if res.Outcome == c.OutcomeSucceeded && res.Error == nil && json.Unmarshal(res.Result, &out) == nil {
			return out, nil
		}
		return c.MemorySearchResult{}, localErr
	}
	name := o.nodeName(ctx, res.NodeID)
	if res.Error != nil {
		return c.MemorySearchResult{}, c.Errorf(res.Error.Code, "%s; node %s, which the hub chose to answer, failed: %s", local, name, res.Error.Message).WithRef(c.KindNode, res.NodeID)
	}
	if res.Outcome != c.OutcomeSucceeded {
		return c.MemorySearchResult{}, c.Errorf(c.ErrUnavailable, "%s; node %s, which the hub chose to answer, did not within %ds (outcome %s, command %s)",
			local, name, memoryRouteSeconds, res.Outcome, res.CommandID).WithRef(c.KindNode, res.NodeID)
	}
	var out c.MemorySearchResult
	if err := json.Unmarshal(res.Result, &out); err != nil {
		return out, c.Errorf(c.ErrInternal, "decode %s result: %v", c.ActionMemorySearch, err)
	}
	out.ServedBy = &c.MemoryServedBy{NodeID: res.NodeID, Name: name}
	note := "served by node " + name + " (no Hive on this node)"
	out.Freshness.Reason = strings.TrimPrefix(out.Freshness.Reason+"; "+note, "; ")
	return out, nil
}

// nodeName is a node's name for messages, or its ID when it cannot be read.
func (o *Ops) nodeName(ctx context.Context, nodeID string) string {
	if n, err := o.API.GetNode(ctx, c.GetRequest{ID: nodeID}); err == nil && n.Name != "" {
		return n.Name
	}
	return nodeID
}
