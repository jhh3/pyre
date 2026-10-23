package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/persistence"
)

// RemoteClient is the AttachCommand.ClientNodeID the hub sends when the
// attaching client's node is unknown: never equal to a node ID, so the
// node answers with a remote (ssh) descriptor rather than a local one.
const RemoteClient = "remote-client"

// Synchronous node round trips: the command is durably queued, then the
// caller waits a bounded time for its result.

func (h *Hub) requireConnected(ctx context.Context, nodeID string) error {
	if c, _ := h.nodeConnected(ctx, h.db, nodeID); !c {
		return contracts.Errorf(contracts.ErrUnavailable, "node %s is disconnected", nodeID).WithRef(contracts.KindNode, nodeID)
	}
	return nil
}

func timeoutErr(rec commandRec, d time.Duration) error {
	e := contracts.Errorf(contracts.ErrUnavailable, "node did not answer within %s", d).WithRef(contracts.KindCommand, rec.CommandID).
		WithDetail("command_id", rec.CommandID).WithDetail("outcome", pendingOutcome(rec))
	return e
}

func commandError(rec commandRec) error {
	if rec.Error != nil {
		e := *rec.Error
		return &e
	}
	switch rec.State {
	case cmdExpired:
		return contracts.Errorf(contracts.ErrExpired, "command expired before the node received it").WithRef(contracts.KindCommand, rec.CommandID)
	case cmdCancelled:
		return contracts.Errorf(contracts.ErrConflict, "command was cancelled").WithRef(contracts.KindCommand, rec.CommandID)
	}
	if rec.Outcome == contracts.OutcomeUnsupported {
		return contracts.Errorf(contracts.ErrUnsupported, "node reports the action is unsupported").WithRef(contracts.KindCommand, rec.CommandID)
	}
	return contracts.Errorf(contracts.ErrInternal, "command %s", rec.Outcome).WithRef(contracts.KindCommand, rec.CommandID)
}

// ReadSession reads bounded transcript/activity from the run's node.
func (h *Hub) ReadSession(ctx context.Context, req contracts.ReadSessionRequest) (contracts.ReadSessionResponse, error) {
	p, err := principal(ctx)
	if err != nil {
		return contracts.ReadSessionResponse{}, err
	}
	sess, err := h.getSession(ctx, h.db, req.SessionID)
	if err != nil {
		return contracts.ReadSessionResponse{}, err
	}
	runID := req.RunID
	if runID == "" {
		runID = sess.CurrentRunID
	}
	if runID == "" {
		return contracts.ReadSessionResponse{}, contracts.Errorf(contracts.ErrConflict, "session has no run").WithRef(contracts.KindSession, sess.SessionID)
	}
	run, err := h.getRun(ctx, h.db, runID)
	if err != nil {
		return contracts.ReadSessionResponse{}, err
	}
	if run.SessionID != sess.SessionID {
		return contracts.ReadSessionResponse{}, contracts.Errorf(contracts.ErrInvalid, "run %s belongs to another session", runID)
	}
	if !contracts.HasCap(run.Capabilities, contracts.CapTranscript) && !contracts.HasCap(run.Capabilities, contracts.CapScreen) {
		return contracts.ReadSessionResponse{}, contracts.Errorf(contracts.ErrUnsupported, "run adapter supports neither transcript.read nor screen.read").WithRef(contracts.KindRun, runID)
	}
	if err := h.requireConnected(ctx, run.NodeID); err != nil {
		return contracts.ReadSessionResponse{}, err
	}
	req.RunID = runID
	var cmd commandRec
	wait := h.opt.CommandWait
	if err := h.tx(ctx, func(tx *sql.Tx) error {
		var err error
		cmd, err = h.enqueueTx(ctx, tx, run.NodeID, contracts.CmdReadTranscript, p, req, wait, contracts.ObjectRef{Kind: contracts.KindRun, ID: runID})
		return err
	}); err != nil {
		return contracts.ReadSessionResponse{}, err
	}
	rec, err := h.waitCommand(ctx, cmd.CommandID, wait)
	if err != nil {
		return contracts.ReadSessionResponse{}, err
	}
	if !rec.finished() {
		return contracts.ReadSessionResponse{}, timeoutErr(rec, wait)
	}
	if rec.Outcome != contracts.OutcomeSucceeded {
		return contracts.ReadSessionResponse{}, commandError(rec)
	}
	var resp contracts.ReadSessionResponse
	if err := json.Unmarshal(rec.Result, &resp); err != nil {
		return resp, contracts.Errorf(contracts.ErrInternal, "malformed transcript result: %v", err)
	}
	if resp.Entries == nil {
		resp.Entries = []contracts.TranscriptEntry{}
	}
	if resp.Freshness.Source == "" {
		resp.Freshness = contracts.Freshness{ObservedAt: h.now(), Source: "node:" + run.NodeID}
	}
	return resp, nil
}

// AttachSession asks the run's node for an attach descriptor. Web-terminal
// tickets are minted by the hub and never persisted.
func (h *Hub) AttachSession(ctx context.Context, req contracts.AttachRequest) (contracts.AttachDescriptor, error) {
	p, err := principal(ctx)
	if err != nil {
		return contracts.AttachDescriptor{}, err
	}
	if err := requireOp(req.Mutation); err != nil {
		return contracts.AttachDescriptor{}, err
	}
	wait := h.opt.CommandWait
	// A relayed web terminal runs the node's local attach command on the
	// node itself, so the node is asked for its local descriptor.
	web := h.cfg.Terminal != nil && prefersWebTerminal(req.Preferred)
	clientNode := req.ClientNodeID
	if !h.opExists(ctx, req.OperationID) {
		_, run, err := h.currentRunFor(ctx, h.db, req.SessionID, "")
		if err != nil {
			return contracts.AttachDescriptor{}, err
		}
		if req.ReadOnly {
			if !contracts.HasCap(run.Capabilities, contracts.CapAttachReadOnly) && !contracts.HasCap(run.Capabilities, contracts.CapTranscript) {
				return contracts.AttachDescriptor{}, contracts.Errorf(contracts.ErrUnsupported, "run adapter offers no read-only attach").WithRef(contracts.KindRun, run.RunID)
			}
		} else if !contracts.HasCap(run.Capabilities, contracts.CapAttachNative) && !contracts.HasCap(run.Capabilities, contracts.CapAttachWeb) {
			return contracts.AttachDescriptor{}, contracts.Errorf(contracts.ErrUnsupported, "run adapter offers no interactive attach; request read_only").WithRef(contracts.KindRun, run.RunID)
		}
		if err := h.requireConnected(ctx, run.NodeID); err != nil {
			return contracts.AttachDescriptor{}, err
		}
		// A relayed web terminal needs nothing from the node yet: the
		// node's descriptor was only converted (its command discarded), and
		// the relay's terminal_open runs the node's own attach when the
		// client dials, failing there if the terminal is gone. Minting the
		// ticket here keeps attach off pyred's serial command queue, where
		// it waited behind transcript reads (server: attach p50 393 ms).
		if web && contracts.HasCap(run.Capabilities, contracts.CapAttachNative) && !run.State.Terminal() {
			d, err := h.webDescriptor(ctx, p, contracts.AttachDescriptor{RunID: run.RunID, NodeID: run.NodeID}, req.ReadOnly)
			if err != nil {
				return contracts.AttachDescriptor{}, err
			}
			if l, err := h.activeLease(ctx, h.db, d.RunID); err == nil && l != nil {
				d.Lease = l
			}
			return d, nil
		}
	}
	var cmdID string
	err = h.mutate(ctx, req.OperationID, contracts.OpAttachSession, p, req, &cmdID, func(tx *sql.Tx) (any, error) {
		_, run, err := h.currentRunFor(ctx, tx, req.SessionID, "")
		if err != nil {
			return nil, err
		}
		// Tell the node where the client is: itself for a relayed web
		// terminal (the relay runs the local attach on the node), the
		// client's node when known, else "remote" — an unknown client is
		// never assumed to be on the run's node (it would get a local
		// command that only works there).
		target := clientNode
		preferred := req.Preferred
		switch {
		case web:
			// The relay needs the node's own local attach command; the
			// client's list (e.g. ssh_tmux fallbacks) does not apply.
			target = run.NodeID
			preferred = []contracts.AttachMechanism{contracts.AttachLocalTmux}
		case target == "":
			target = RemoteClient
		}
		cmd, err := h.enqueueTx(ctx, tx, run.NodeID, contracts.CmdAttach, p, contracts.AttachCommand{
			SessionID: run.SessionID, RunID: run.RunID, ClientNodeID: target, Preferred: preferred, ReadOnly: req.ReadOnly,
		}, wait, contracts.ObjectRef{Kind: contracts.KindRun, ID: run.RunID})
		if err != nil {
			return nil, err
		}
		return cmd.CommandID, nil
	})
	if err != nil {
		return contracts.AttachDescriptor{}, err
	}
	rec, err := h.waitCommand(ctx, cmdID, wait)
	if err != nil {
		return contracts.AttachDescriptor{}, err
	}
	if !rec.finished() {
		return contracts.AttachDescriptor{}, timeoutErr(rec, wait)
	}
	if rec.Outcome != contracts.OutcomeSucceeded {
		return contracts.AttachDescriptor{}, commandError(rec)
	}
	var d contracts.AttachDescriptor
	if err := json.Unmarshal(rec.Result, &d); err != nil {
		return d, contracts.Errorf(contracts.ErrInternal, "malformed attach result: %v", err)
	}
	if d.RunID == "" {
		d.RunID = rec.RefID
	}
	if d.NodeID == "" {
		d.NodeID = rec.NodeID
	}
	if h.cfg.Terminal != nil && (d.Mechanism == contracts.AttachWebTerminal || (web && d.Mechanism == contracts.AttachLocalTmux)) {
		if d, err = h.webDescriptor(ctx, p, d, req.ReadOnly || d.ReadOnly); err != nil {
			return contracts.AttachDescriptor{}, err
		}
	} else if d.Mechanism == contracts.AttachWebTerminal {
		if h.cfg.Tokens == nil {
			return d, contracts.Errorf(contracts.ErrUnsupported, "web terminal tickets are not configured on this hub")
		}
		tk, exp, err := h.cfg.Tokens.IssueTicket(ctx, "terminal", contracts.ObjectRef{Kind: contracts.KindRun, ID: d.RunID}, time.Minute)
		if err != nil {
			return d, err
		}
		d.Ticket, d.ExpiresAt = tk, exp
		if d.URL == "" {
			d.URL = contracts.PathTerminal
		}
	}
	if l, err := h.activeLease(ctx, h.db, d.RunID); err == nil && l != nil {
		d.Lease = l
	}
	return d, nil
}

// sessionActions are the node actions a session principal may invoke.
var sessionActions = map[string]bool{
	contracts.ActionConnCheck: true, contracts.ActionConnInvoke: true, contracts.ActionConnList: true,
	contracts.ActionMemorySearch: true, contracts.ActionMemoryGet: true, contracts.ActionMemoryStatus: true,
	contracts.ActionDirList: true, contracts.ActionInventory: true, contracts.ActionUpdateList: true, contracts.ActionAccountList: true,
	contracts.ActionTranscriptSearch: true, // read-only, like sessions.read (W-search)
	contracts.ActionAccountSelect:    true, // dry run: the Codex launch preflight (cli harnessPreflight)
}

// InvokeNodeAction routes a typed action to a node and waits up to
// TimeoutSeconds for its result. On timeout the result carries the
// CommandID and Outcome pending (not yet received) or delivered (received,
// still running); observe command.result events for completion.
func (h *Hub) InvokeNodeAction(ctx context.Context, req contracts.NodeActionRequest) (contracts.NodeActionResult, error) {
	p, err := principal(ctx)
	if err != nil {
		return contracts.NodeActionResult{}, err
	}
	if err := requireOp(req.Mutation); err != nil {
		return contracts.NodeActionResult{}, err
	}
	if req.Action == contracts.ActionConnInvoke {
		// A connection is used by the session itself or by the owner; no
		// other principal (a node credential included) may act for a session.
		var cp contracts.ConnectionInvokeParams
		_ = json.Unmarshal(req.Params, &cp)
		if !isOwner(p) && (p.Kind != contracts.PrincipalSession || cp.SessionID != p.ID) {
			return contracts.NodeActionResult{}, contracts.Errorf(contracts.ErrForbidden, "connection.invoke may be sent only by the session it names or the owner (caller: %s)", p.Kind)
		}
	}
	if req.Action == contracts.ActionConnInvoke && req.NodeID == "" {
		// Route to the node holding the connection (catalog RouteInvoke).
		if req.NodeID, err = h.routeConnectionInvoke(ctx, p, req.Params); err != nil {
			return contracts.NodeActionResult{}, err
		}
	}
	if req.Action == "" || req.NodeID == "" {
		return contracts.NodeActionResult{}, contracts.Errorf(contracts.ErrInvalid, "node_id and action are required")
	}
	if p.Kind == contracts.PrincipalSession {
		if !sessionActions[req.Action] {
			return contracts.NodeActionResult{}, contracts.Errorf(contracts.ErrForbidden, "session agents may not invoke %s", req.Action)
		}
		if req.Action == contracts.ActionConnInvoke {
			var cp contracts.ConnectionInvokeParams
			if err := json.Unmarshal(req.Params, &cp); err != nil || cp.SessionID != p.ID {
				return contracts.NodeActionResult{}, contracts.Errorf(contracts.ErrForbidden, "connection.invoke must name the calling session")
			}
		}
	}
	if req.TimeoutSeconds < 0 || req.TimeoutSeconds > 300 {
		return contracts.NodeActionResult{}, contracts.Errorf(contracts.ErrInvalid, "timeout_seconds must be within 0..300")
	}
	wait := h.opt.CommandWait
	if req.TimeoutSeconds > 0 {
		wait = time.Duration(req.TimeoutSeconds) * time.Second
	}
	if !h.opExists(ctx, req.OperationID) {
		if _, err := h.nodeInfo(ctx, req.NodeID); err != nil {
			return contracts.NodeActionResult{}, err
		}
	}
	if err := h.recordGrant(ctx, p, req); err != nil {
		return contracts.NodeActionResult{}, err
	}
	var cmdID string
	err = h.mutate(ctx, req.OperationID, contracts.OpInvokeNodeAction, p, req, &cmdID, func(tx *sql.Tx) (any, error) {
		cmd, err := h.enqueueTx(ctx, tx, req.NodeID, contracts.CmdAction, p, req, h.opt.ActionTTL, contracts.ObjectRef{Kind: contracts.KindNode, ID: req.NodeID})
		if err != nil {
			return nil, err
		}
		return cmd.CommandID, nil
	})
	if err != nil {
		return contracts.NodeActionResult{}, err
	}
	rec, err := h.waitCommand(ctx, cmdID, h.waitIfConnected(ctx, cmdID, wait))
	if err != nil {
		return contracts.NodeActionResult{}, err
	}
	res := contracts.NodeActionResult{CommandID: rec.CommandID, Outcome: pendingOutcome(rec), Result: rec.Result, Error: rec.Error}
	if rec.State == cmdExpired || rec.State == cmdCancelled {
		res.Error = contracts.AsError(commandError(rec))
	}
	if req.Action == contracts.ActionConnCheck && res.Outcome == contracts.OutcomeSucceeded && res.Error == nil {
		h.recordCheck(ctx, req, rec.Result)
	}
	return res, nil
}

// checkRecorder is implemented by catalogs that store connection.check
// results (internal/connections.Catalog).
type checkRecorder interface {
	RecordCheck(ctx context.Context, nodeID, connectionID string, av contracts.ConnectionAvailability) error
}

// recordCheck applies a node's connection.check result to the catalog at
// once (the node also pushes a fresh connections report). Best effort: the
// action result is returned to the caller either way.
func (h *Hub) recordCheck(ctx context.Context, req contracts.NodeActionRequest, result contracts.RawJSON) {
	rc, ok := h.cfg.Connections.(checkRecorder)
	if !ok || len(result) == 0 {
		return
	}
	var p contracts.ConnectionParams
	var av contracts.ConnectionAvailability
	if json.Unmarshal(req.Params, &p) != nil || json.Unmarshal(result, &av) != nil || p.ConnectionID == "" || av.Mode == "" {
		return
	}
	_ = rc.RecordCheck(ctx, req.NodeID, p.ConnectionID, av)
}

// ListConnections merges the Outfit connection catalogs reported by nodes.
// Each node contributes its own availability entries.
func (h *Hub) ListConnections(ctx context.Context, req contracts.ListConnectionsRequest) (contracts.Page[contracts.Connection], error) {
	if _, err := principal(ctx); err != nil {
		return contracts.Page[contracts.Connection]{}, err
	}
	if h.cfg.Connections != nil {
		return h.cfg.Connections.List(ctx, req)
	}
	rows, err := h.db.QueryContext(ctx, `SELECT node_id, connections FROM node_connections ORDER BY node_id`)
	if err != nil {
		return contracts.Page[contracts.Connection]{}, err
	}
	merged := map[string]*contracts.Connection{}
	for rows.Next() {
		var nodeID, body string
		if err := rows.Scan(&nodeID, &body); err != nil {
			rows.Close()
			return contracts.Page[contracts.Connection]{}, err
		}
		var cs []contracts.Connection
		if json.Unmarshal([]byte(body), &cs) != nil {
			continue
		}
		for _, c := range cs {
			m, ok := merged[c.ConnectionID]
			if !ok {
				cc := c
				cc.Availability = nil
				m = &cc
				merged[c.ConnectionID] = m
			}
			for _, a := range c.Availability {
				if a.NodeID == "" {
					a.NodeID = nodeID
				}
				if a.NodeID == nodeID { // a node reports only its own availability
					m.Availability = append(m.Availability, a)
				}
			}
		}
	}
	rows.Close()
	var all []contracts.Connection
	for _, c := range merged {
		if req.NodeID != "" && !connOnNode(*c, req.NodeID) {
			continue
		}
		if req.ProjectID != "" && len(c.Scope.Projects) > 0 && !contains(c.Scope.Projects, req.ProjectID) {
			continue
		}
		if c.Availability == nil {
			c.Availability = []contracts.ConnectionAvailability{}
		}
		all = append(all, *c)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ConnectionID < all[j].ConnectionID })
	pc, err := persistence.DecodeCursor(req.Cursor)
	if err != nil {
		return contracts.Page[contracts.Connection]{}, err
	}
	limit := persistence.ClampLimit(req.Limit, persistence.DefaultLimit, persistence.MaxLimit)
	page := contracts.Page[contracts.Connection]{Items: []contracts.Connection{}, SnapshotAt: h.now()}
	for _, c := range all {
		if pc.I != "" && c.ConnectionID <= pc.I {
			continue
		}
		if len(page.Items) == limit {
			page.NextCursor = persistence.EncodeCursor(persistence.PageCursor{I: page.Items[limit-1].ConnectionID})
			break
		}
		page.Items = append(page.Items, c)
	}
	return page, nil
}

func connOnNode(c contracts.Connection, nodeID string) bool {
	for _, a := range c.Availability {
		if a.NodeID == nodeID {
			return true
		}
	}
	return contains(c.Scope.Nodes, nodeID)
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
