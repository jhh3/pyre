package hub

import (
	"context"
	"database/sql"
	"slices"

	"github.com/jhh3/pyre/internal/contracts"
)

// applyRunSnapshotTx reconciles a node's run_snapshot report (contract
// v1.5). A node sends it when the hub's committed report watermark is behind
// what the hub acknowledged before, i.e. the hub lost reports (restore from
// an older backup). Each record is applied as a run observation, so runs the
// hub marked unknown at reconnect but that ended meanwhile become terminal.
// Sessions the hub has never seen are recorded with the node's metadata, and
// untitled placeholders the hub created from a bare run observation are
// filled in. A session the hub already knows with its own metadata is never
// overwritten: the hub stays authoritative for session metadata.
//
// Records the hub refuses individually (another node's run, a run moved to
// another session) are skipped rather than failing the whole snapshot, which
// would otherwise be retried forever by the node's outbox.
func (h *Hub) applyRunSnapshotTx(ctx context.Context, tx *sql.Tx, nodeID string, snap contracts.RunSnapshot, seq uint64) error {
	for _, rec := range snap.Records {
		r := rec.Run
		if r.RunID == "" || r.SessionID == "" || (r.NodeID != "" && r.NodeID != nodeID) {
			continue
		}
		if rec.Session.SessionID != "" && rec.Session.SessionID != r.SessionID {
			continue
		}
		if err := h.reconcileSnapshotSessionTx(ctx, tx, nodeID, rec.Session, r); err != nil {
			return err
		}
		if err := h.observeRunTx(ctx, tx, nodeID, r, seq, "snapshot"); err != nil {
			if c := contracts.CodeOf(err); c == contracts.ErrForbidden || c == contracts.ErrConflict || c == contracts.ErrInvalid {
				continue
			}
			return err
		}
	}
	return nil
}

func (h *Hub) reconcileSnapshotSessionTx(ctx context.Context, tx *sql.Tx, nodeID string, ns contracts.Session, r contracts.Run) error {
	if ns.SessionID == "" {
		return nil // run only; observeRunTx records a placeholder if needed
	}
	origin := "node:" + nodeID
	cur, err := h.getSession(ctx, tx, r.SessionID)
	if contracts.IsCode(err, contracts.ErrNotFound) {
		s := ns
		if s.Harness == "" {
			s.Harness = r.Harness
		}
		if s.CreatedBy.Kind == "" {
			s.CreatedBy = contracts.Principal{Kind: contracts.PrincipalNode, ID: nodeID}
		}
		if s.CreatedAt.IsZero() {
			s.CreatedAt = r.StartedAt
		}
		if s.CreatedAt.IsZero() {
			s.CreatedAt = h.now()
		}
		s.CurrentRunID = "" // set by the run observation that follows
		if err := h.insertSessionTx(ctx, tx, s); err != nil {
			return err
		}
		return h.emitSession(ctx, tx, contracts.EvSessionCreated, s, origin, 0)
	}
	if err != nil {
		return err
	}
	// Only a placeholder the hub created from a bare run observation is
	// filled in from the node's copy.
	if cur.Title != "" || cur.CreatedBy.Kind != contracts.PrincipalNode || cur.CreatedBy.ID != nodeID {
		return nil
	}
	changed := false
	if ns.Title != "" {
		cur.Title, changed = ns.Title, true
	}
	if cur.ProjectID == nil && ns.ProjectID != nil && *ns.ProjectID != "" {
		cur.ProjectID, changed = ns.ProjectID, true
	}
	if len(cur.Tags) == 0 && len(ns.Tags) > 0 {
		cur.Tags, changed = slices.Clone(ns.Tags), true
	}
	if cur.Lineage == (contracts.Lineage{}) && ns.Lineage != (contracts.Lineage{}) {
		cur.Lineage, changed = ns.Lineage, true
	}
	if !changed {
		return nil
	}
	if err := h.updateSessionTx(ctx, tx, cur); err != nil {
		return err
	}
	return h.emitSession(ctx, tx, contracts.EvSessionUpdated, cur, origin, 0)
}
