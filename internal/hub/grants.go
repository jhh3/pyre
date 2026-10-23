package hub

import (
	"context"
	"encoding/json"

	"github.com/jhh3/pyre/internal/contracts"
)

// GrantSession is the hub's authoritative context for a session at grant
// check time.
type GrantSession struct {
	SessionID string
	NodeID    string // node of the session's current run ("" if none)
	ProjectID string // "" for standalone sessions
	Archived  bool
	RunActive bool // the current run exists and has not ended
}

// CheckConnectionGrant decides from current hub state whether a session may
// use a connection now. Callers: a node principal for sessions whose current
// run is on that node, a session for itself, the owner, or the system. An
// unknown session is a denial (Allowed=false), not an error, so nodes fail
// closed without retrying.
func (h *Hub) CheckConnectionGrant(ctx context.Context, req contracts.CheckGrantRequest) (contracts.GrantDecision, error) {
	p, err := principal(ctx)
	if err != nil {
		return contracts.GrantDecision{}, err
	}
	if h.cfg.ConnectionGrants == nil {
		return contracts.GrantDecision{}, notConfigured("connection grants")
	}
	if req.ConnectionID == "" || req.SessionID == "" {
		return contracts.GrantDecision{}, contracts.Errorf(contracts.ErrInvalid, "connection_id and session_id are required")
	}
	switch {
	case isOwner(p), p.Kind == contracts.PrincipalSystem, p.Kind == contracts.PrincipalNode:
	case p.Kind == contracts.PrincipalSession:
		if p.ID != req.SessionID {
			return contracts.GrantDecision{}, contracts.Errorf(contracts.ErrForbidden, "a session may only check its own grants")
		}
	default:
		return contracts.GrantDecision{}, contracts.Errorf(contracts.ErrForbidden, "principal %s may not check connection grants", p.Kind)
	}
	gs, err := h.grantSession(ctx, req.SessionID)
	if err != nil {
		return contracts.GrantDecision{}, err
	}
	if p.Kind == contracts.PrincipalNode && !isOwner(p) && gs.SessionID != "" && gs.NodeID != p.ID && !h.nodeHoldsConnection(ctx, p.ID, req.ConnectionID) {
		return contracts.GrantDecision{}, contracts.Errorf(contracts.ErrForbidden, "node %s may only check grants for sessions running on it or for connections it holds", p.ID).WithRef(contracts.KindSession, req.SessionID)
	}
	return h.cfg.ConnectionGrants(ctx, req.ConnectionID, gs)
}

// grantSession builds the authoritative grant context for a session. An
// unknown session yields an empty SessionID (the catalog denies).
func (h *Hub) grantSession(ctx context.Context, sessionID string) (GrantSession, error) {
	gs := GrantSession{SessionID: sessionID}
	s, err := h.getSession(ctx, h.db, sessionID)
	switch {
	case contracts.IsCode(err, contracts.ErrNotFound):
		gs.SessionID = ""
	case err != nil:
		return gs, err
	default:
		gs.Archived = s.Archived
		if s.ProjectID != nil {
			gs.ProjectID = *s.ProjectID
		}
		if s.CurrentRunID != "" {
			r, err := h.getRun(ctx, h.db, s.CurrentRunID)
			if err != nil && !contracts.IsCode(err, contracts.ErrNotFound) {
				return gs, err
			}
			gs.NodeID = r.NodeID
			gs.RunActive = err == nil && !r.State.Terminal()
		}
	}
	return gs, nil
}

// routeConnectionInvoke picks the node that executes a connection.invoke
// sent without node_id: the catalog's verified remote_invoke holder,
// preferring the session's own node. The session's grant is checked here
// too (and again by the executing node at use time).
func (h *Hub) routeConnectionInvoke(ctx context.Context, p contracts.Principal, params contracts.RawJSON) (string, error) {
	if h.cfg.RouteConnectionInvoke == nil {
		return "", contracts.Errorf(contracts.ErrInvalid, "node_id is required (this hub cannot route connection.invoke)")
	}
	var cp contracts.ConnectionInvokeParams
	if err := json.Unmarshal(params, &cp); err != nil || cp.ConnectionID == "" || cp.SessionID == "" {
		return "", contracts.Errorf(contracts.ErrInvalid, "connection.invoke needs connection_id and session_id")
	}
	if p.Kind == contracts.PrincipalSession && cp.SessionID != p.ID {
		return "", contracts.Errorf(contracts.ErrForbidden, "connection.invoke must name the calling session")
	}
	gs, err := h.grantSession(ctx, cp.SessionID)
	if err != nil {
		return "", err
	}
	return h.cfg.RouteConnectionInvoke(ctx, cp.ConnectionID, gs)
}

// nodeHoldsConnection reports whether nodeID itself reported availability
// for connectionID: it is then the executing node of a cross-node
// connection.invoke and must be able to check the calling session's grant.
func (h *Hub) nodeHoldsConnection(ctx context.Context, nodeID, connectionID string) bool {
	if h.cfg.Connections == nil {
		return false
	}
	cur := ""
	for i := 0; i < 20; i++ {
		page, err := h.cfg.Connections.List(ctx, contracts.ListConnectionsRequest{NodeID: nodeID, PageRequest: contracts.PageRequest{Cursor: cur}})
		if err != nil {
			return false
		}
		for _, c := range page.Items {
			if c.ConnectionID != connectionID {
				continue
			}
			for _, a := range c.Availability {
				if a.NodeID == nodeID {
					return true
				}
			}
		}
		if page.NextCursor == "" {
			return false
		}
		cur = page.NextCursor
	}
	return false
}

// recordGrant mirrors an owner's connection.grant/revoke action into the
// hub catalog (authoritative for CheckConnectionGrant) before the action is
// queued for the node (the node keeps its pushed mirror for immediate local
// denial). Revoking a grant the catalog never recorded is not an
// error: the node-side revoke still runs.
func (h *Hub) recordGrant(ctx context.Context, p contracts.Principal, req contracts.NodeActionRequest) error {
	if h.cfg.RecordConnectionGrant == nil || (req.Action != contracts.ActionConnGrant && req.Action != contracts.ActionConnRevoke) {
		return nil
	}
	var g contracts.ConnectionGrantParams
	if err := json.Unmarshal(req.Params, &g); err != nil {
		return contracts.Errorf(contracts.ErrInvalid, "%s params: %v", req.Action, err)
	}
	// A grant pushed to one node without a node scope has only ever
	// authorized sessions on that node; record it as node-scoped so the
	// catalog never broadens it to every node.
	if g.NodeID == "" {
		g.NodeID = req.NodeID
	}
	active := req.Action == contracts.ActionConnGrant
	err := h.cfg.RecordConnectionGrant(ctx, p, req.OperationID+"/catalog", g, active)
	if !active && contracts.IsCode(err, contracts.ErrNotFound) {
		return nil
	}
	return err
}
