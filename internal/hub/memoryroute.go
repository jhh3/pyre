package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
)

// routeMemorySearch picks the node that answers a memory.search sent
// without node_id (a client whose own node has no Hive): a connected node
// advertising contracts.NodeCapMemoryHive, the hub's own machine first,
// then by name. Older pyreds never advertise it, so a fleet of them gets
// the no-node error (contracts.MemoryNoNodeReason).
func (h *Hub) routeMemorySearch(ctx context.Context) (string, error) {
	nodes, err := h.searchNodes(ctx)
	if err != nil {
		return "", err
	}
	type cand struct {
		id, name string
		own      bool
	}
	var cs []cand
	for _, n := range nodes {
		if ok, _ := h.nodeConnected(ctx, h.db, n.NodeID); !ok {
			continue
		}
		caps, host := n.Capabilities, n.Hostname
		if hc, hh, ok := h.helloCaps(ctx, n.NodeID); ok {
			caps, host = hc, hh // the latest hello is the node's own word
		}
		if !contracts.HasCap(caps, contracts.NodeCapMemoryHive) {
			continue
		}
		cs = append(cs, cand{id: n.NodeID, name: n.Name, own: sameHost(host, h.opt.Hostname)})
	}
	if len(cs) == 0 {
		return "", contracts.Errorf(contracts.ErrUnavailable,
			"no connected node serves Hive memory (none advertises %s): install Hive on a node, or update pyred on the nodes that have it", contracts.NodeCapMemoryHive).
			WithDetail("reason", contracts.MemoryNoNodeReason)
	}
	sort.SliceStable(cs, func(i, j int) bool {
		if cs[i].own != cs[j].own {
			return cs[i].own
		}
		if cs[i].name != cs[j].name {
			return cs[i].name < cs[j].name
		}
		return cs[i].id < cs[j].id
	})
	return cs[0].id, nil
}

// helloCaps reads the capabilities and hostname from a node's stored hello.
func (h *Hub) helloCaps(ctx context.Context, nodeID string) ([]string, string, bool) {
	var hello sql.NullString
	if err := h.db.QueryRowContext(ctx, `SELECT hello FROM node_state WHERE node_id=?`, nodeID).Scan(&hello); err != nil || !hello.Valid {
		return nil, "", false
	}
	var nh contracts.NodeHello
	if json.Unmarshal([]byte(hello.String), &nh) != nil {
		return nil, "", false
	}
	return nh.Capabilities, nh.Hostname, true
}

// sameHost compares hostnames case-insensitively, ignoring a domain suffix
// ("server" matches "server.local").
func sameHost(a, b string) bool {
	short := func(s string) string {
		s = strings.ToLower(strings.TrimSpace(s))
		if i := strings.IndexByte(s, '.'); i > 0 {
			s = s[:i]
		}
		return s
	}
	return a != "" && b != "" && short(a) == short(b)
}
