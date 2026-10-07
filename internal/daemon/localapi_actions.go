package daemon

import (
	"context"
	"encoding/json"
	"os"
	"runtime"
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
)

// localFallbackActions are read-only node actions this node answers from its
// own handlers when the hub is unreachable, so agents keep local memory
// retrieval through an outage (A10). Everything else still needs the hub.
var localFallbackActions = map[string]bool{
	contracts.ActionMemorySearch: true,
	contracts.ActionMemoryGet:    true,
	contracts.ActionMemoryStatus: true,
}

// localNode describes this node from local state (hub view unavailable).
func (l *LocalAPI) localNode(ctx context.Context) contracts.Node {
	host, _ := os.Hostname()
	name := l.d.cfg.NodeName
	if name == "" {
		name = host
	}
	return contracts.Node{NodeID: l.d.nodeID, Name: name, Hostname: host, SSHAlias: l.d.cfg.SSHAlias,
		Platform: runtime.GOOS, Arch: runtime.GOARCH, Harnesses: l.d.harnessInfo(ctx), Versions: l.d.versions(),
		Capabilities: l.d.capabilities(), Connection: contracts.NodeConnection{State: contracts.ConnDisconnected},
		Labels: map[string]string{"pyre.source": "node-local (hub unavailable)"}}
}

// GetNode answers for this node from local state when the hub is down.
func (l *LocalAPI) GetNode(ctx context.Context, r contracts.GetRequest) (contracts.Node, error) {
	n, err := l.proxy.GetNode(ctx, r)
	if err == nil || !hubDown(err) || r.ID != l.d.nodeID {
		return n, err
	}
	return l.localNode(ctx), nil
}

// ListNodes lists only this node (from local state) when the hub is down,
// so node names still resolve for local-fallback actions.
func (l *LocalAPI) ListNodes(ctx context.Context, r contracts.ListNodesRequest) (contracts.Page[contracts.Node], error) {
	page, err := l.proxy.ListNodes(ctx, r)
	if err == nil || !hubDown(err) || r.Cursor != "" {
		return page, err
	}
	return contracts.Page[contracts.Node]{Items: []contracts.Node{l.localNode(ctx)}, SnapshotAt: l.d.now()}, nil
}

// InvokeNodeAction proxies to the hub; when the hub is unreachable, a
// read-only memory action targeting this node runs on this node's own
// handler. The result's freshness (memory actions carry one) is marked
// node-local so callers can tell.
func (l *LocalAPI) InvokeNodeAction(ctx context.Context, r contracts.NodeActionRequest) (contracts.NodeActionResult, error) {
	res, err := l.proxy.InvokeNodeAction(ctx, r)
	if err == nil || !hubDown(err) || !localFallbackActions[r.Action] || (r.NodeID != "" && r.NodeID != l.d.nodeID) {
		return res, err
	}
	l.d.mu.Lock()
	e, ok := l.d.actions[r.Action]
	l.d.mu.Unlock()
	if !ok {
		return res, err
	}
	v, aerr := e.fn(ctx, json.RawMessage(r.Params))
	if aerr != nil {
		return contracts.NodeActionResult{Outcome: contracts.OutcomeFailed, Error: contracts.AsError(aerr)}, nil
	}
	raw, merr := json.Marshal(v)
	if merr != nil {
		return res, err
	}
	return contracts.NodeActionResult{Outcome: contracts.OutcomeSucceeded, Result: markLocal(raw, l.d.nodeID)}, nil
}

// markLocal annotates a memory result: MemorySearchResult.freshness gains a
// node-local reason; MemoryStatus.detail gains a note. Other shapes pass
// through unchanged.
func markLocal(raw []byte, nodeID string) contracts.RawJSON {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return raw
	}
	note := "hub unreachable; answered by node " + nodeID + " locally"
	if fr, ok := m["freshness"]; ok {
		var f contracts.Freshness
		if json.Unmarshal(fr, &f) == nil {
			f.Reason = strings.TrimPrefix(strings.Join([]string{f.Reason, note}, "; "), "; ")
			if b, err := json.Marshal(f); err == nil {
				m["freshness"] = b
			}
		}
	} else if _, ok := m["backend"]; ok {
		var d string
		_ = json.Unmarshal(m["detail"], &d)
		d = strings.TrimPrefix(strings.Join([]string{d, note}, "; "), "; ")
		m["detail"], _ = json.Marshal(d)
	}
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}
