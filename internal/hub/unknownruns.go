package hub

import (
	"context"
	"database/sql"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/persistence"
)

// Unknown runs a connected node no longer has (W-resilience).
//
// A run becomes "unknown" when its launch outcome is indeterminate (pyred
// restarted while start_run executed: office-box1, Oct 2026, a lead's
// worker sat "unknown" for 30 minutes and the lead was never told) or when
// its node did not list it on reconnect. Nothing used to revisit it. A
// node's hello lists every run it still supervises (live, launching, or
// orphaned with an unknown outcome), so a run that stays unknown while its
// node is connected, and that the node's latest hello left out although
// the node already held the run's launch, is gone: after unknownRunGrace
// (time for the node's queued run reports to land) it ends through the
// ordinary run-state path, so the parent gets its child report and the
// session reads failed, not unknown.
//
// Pyred builds that know (daemon RecoverCommand) report such a launch
// failed themselves; this is the safety net for older nodes and any other
// way a run can go missing.

// unknownRunGrace is how long an unknown run must stay unknown, with its
// node's latest inventory leaving it out, before the hub ends it.
const unknownRunGrace = 2 * time.Minute

// Reasons of runs ended here. The launch one starts with "launch " so it is
// audited as a launch failure (auditLaunchFailedTx).
const (
	unknownLaunchGoneReason = "launch interrupted by a pyred restart; the run never started (its node does not have it)"
	unknownRunGoneReason    = "run gone: its node no longer reports it (exit not observed)"
)

// nodeInventory is a node's latest hello: when, the runs it listed and the
// highest command it held. In memory: after a hub restart every node sends
// a new hello.
type nodeInventory struct {
	at      time.Time
	runs    map[string]bool
	lastCmd int64
}

type inventoryState struct {
	mu sync.Mutex
	m  map[string]nodeInventory
}

// noteInventory records a node's hello (HandleHello, after commit).
func (h *Hub) noteInventory(nodeID string, hello contracts.NodeHello, at time.Time) {
	runs := make(map[string]bool, len(hello.LiveRuns))
	for _, r := range hello.LiveRuns {
		runs[r.RunID] = true
	}
	h.inv.mu.Lock()
	if h.inv.m == nil {
		h.inv.m = map[string]nodeInventory{}
	}
	h.inv.m[nodeID] = nodeInventory{at: at, runs: runs, lastCmd: hello.LastCommandSeq}
	h.inv.mu.Unlock()
}

func (h *Hub) inventoryOf(nodeID string) (nodeInventory, bool) {
	h.inv.mu.Lock()
	defer h.inv.mu.Unlock()
	inv, ok := h.inv.m[nodeID]
	return inv, ok
}

// EndMissingUnknownRuns ends unknown runs their connected node no longer
// has (see above). It returns the run IDs it ended.
func (h *Hub) EndMissingUnknownRuns(ctx context.Context) ([]string, error) {
	rows, err := h.db.QueryContext(ctx, `SELECT run_id, node_id, updated_at FROM runs WHERE state=? ORDER BY run_id LIMIT 500`, string(contracts.RunUnknown))
	if err != nil {
		return nil, err
	}
	type cand struct{ run, node, since string }
	var cands []cand
	for rows.Next() {
		var c cand
		if rows.Scan(&c.run, &c.node, &c.since) == nil {
			cands = append(cands, c)
		}
	}
	rows.Close()
	now := h.now()
	var ended []string
	for _, c := range cands {
		inv, ok := h.inventoryOf(c.node)
		if !ok || inv.runs[c.run] || now.Sub(inv.at) < unknownRunGrace || now.Sub(persistence.ParseTime(c.since)) < unknownRunGrace {
			continue
		}
		if connected, _ := h.nodeConnected(ctx, h.db, c.node); !connected {
			continue
		}
		done := false
		err := h.tx(ctx, func(tx *sql.Tx) error {
			rr, err := h.getRunRow(ctx, tx, c.run)
			if err != nil || rr.State != contracts.RunUnknown {
				return nil
			}
			state, reason := contracts.RunExited, unknownRunGoneReason
			if rr.StartCommandID != "" {
				cmd, err := h.getCommand(ctx, tx, rr.StartCommandID)
				if err != nil || !cmd.finished() {
					return nil // the node may still act on the launch
				}
				if !cmd.IssuedAt.Before(inv.at) || cmd.Seq > inv.lastCmd {
					return nil // the inventory predates the node holding the launch
				}
				if cmd.Outcome == contracts.OutcomeIndeterminate {
					state, reason = contracts.RunFailed, unknownLaunchGoneReason
				}
			} else if !rr.StartedAt.Before(inv.at) {
				return nil
			}
			if err := h.setRunStateTx(ctx, tx, rr, state, reason, "hub"); err != nil {
				return err
			}
			r := rr.Run
			r.State, r.StateReason = state, reason
			h.auditLaunchFailedTx(ctx, tx, r.NodeID, r)
			done = true
			return nil
		})
		if err != nil {
			return ended, err
		}
		if done {
			ended = append(ended, c.run)
		}
	}
	return ended, nil
}
