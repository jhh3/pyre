package cli

import (
	"context"
	"encoding/json"
	"sort"

	c "github.com/jhh3/pyre/internal/contracts"
)

// ConnectionGrantCLIParams describe a grant or revocation. Exactly one
// scope applies: a session, a project, or every session on a node.
type ConnectionGrantCLIParams struct {
	Connection  string `json:"connection"`
	Session     string `json:"session,omitempty"`
	Project     string `json:"project,omitempty"`
	Node        string `json:"node,omitempty"` // node whose sessions may use it (default: the session's node)
	OperationID string `json:"operation_id,omitempty"`
}

// ConnectionGrantResult reports the recorded grant tuple and the nodes the
// grant/revocation was pushed to (every node holding the connection, so
// its executing node denies locally at once after a revoke).
type ConnectionGrantResult struct {
	Grant      c.ConnectionGrantParams `json:"grant"`
	PushedTo   []string                `json:"pushed_to"`
	Connection c.Connection            `json:"connection"`
}

// ConnectionInvokeCLIParams is one read operation over a connection on
// behalf of a session. Node empty = the hub routes to the holder.
type ConnectionInvokeCLIParams struct {
	Connection  string            `json:"connection"`
	Operation   string            `json:"operation"`
	Args        map[string]string `json:"args,omitempty"`
	Session     string            `json:"session,omitempty"` // default: this session
	Node        string            `json:"node,omitempty"`
	OperationID string            `json:"operation_id,omitempty"`
}

// ConnectionCheckCLIParams asks a node to verify a connection now.
type ConnectionCheckCLIParams struct {
	Connection string `json:"connection"`
	Node       string `json:"node,omitempty"` // default: every node holding it
}

// connectionHolders lists nodes that report the connection (not missing).
func (o *Ops) connectionHolders(ctx context.Context, connID string) ([]string, c.Connection, error) {
	page, err := listAll(ctx, func(ctx context.Context, cur string) (c.Page[c.Connection], error) {
		return o.API.ListConnections(ctx, c.ListConnectionsRequest{PageRequest: c.PageRequest{Cursor: cur}})
	})
	if err != nil {
		return nil, c.Connection{}, err
	}
	for _, cn := range page.Items {
		if cn.ConnectionID != connID {
			continue
		}
		seen := map[string]bool{}
		var nodes []string
		for _, a := range cn.Availability {
			if a.State != "missing" && a.NodeID != "" && !seen[a.NodeID] {
				seen[a.NodeID] = true
				nodes = append(nodes, a.NodeID)
			}
		}
		sort.Strings(nodes)
		return nodes, cn, nil
	}
	return nil, c.Connection{}, notFoundf(c.KindConnection, connID)
}

// GrantConnection records (active=true) or revokes a connection grant and
// pushes it to every node holding the connection.
func (o *Ops) GrantConnection(ctx context.Context, p ConnectionGrantCLIParams, active bool) (ConnectionGrantResult, error) {
	var res ConnectionGrantResult
	connID, err := o.ResolveConnection(ctx, p.Connection)
	if err != nil {
		return res, err
	}
	g := c.ConnectionGrantParams{ConnectionID: connID}
	if p.Session != "" && p.Project != "" {
		return res, usagef("give --session or --project, not both")
	}
	if p.Session != "" {
		sv, err := o.ResolveSession(ctx, p.Session)
		if err != nil {
			return res, err
		}
		g.SessionID = sv.Session.SessionID
		if p.Node == "" && sv.Run != nil {
			g.NodeID = sv.Run.NodeID
		}
	}
	if p.Project != "" {
		pid, err := o.projectPtr(ctx, p.Project)
		if err != nil {
			return res, err
		}
		g.ProjectID = *pid
	}
	if p.Node != "" {
		if g.NodeID, err = o.NodeID(ctx, p.Node); err != nil {
			return res, err
		}
	}
	if g.NodeID == "" {
		return res, usagef("--node is required: which node's sessions may use the connection")
	}
	holders, _, err := o.connectionHolders(ctx, connID)
	if err != nil {
		return res, err
	}
	if len(holders) == 0 {
		holders = []string{g.NodeID}
	}
	action := c.ActionConnRevoke
	if active {
		action = c.ActionConnGrant
	}
	base := mut(p.OperationID).OperationID
	for _, node := range holders {
		var conn c.Connection
		if _, err := o.NodeAction(ctx, node, action, g, &conn, base+"/"+node); err != nil {
			return res, err
		}
		res.PushedTo = append(res.PushedTo, node)
		res.Connection = conn
	}
	res.Grant = g
	return res, nil
}

// InvokeConnection runs a read operation over a connection for a session.
func (o *Ops) InvokeConnection(ctx context.Context, p ConnectionInvokeCLIParams) (c.ConnectionInvokeResult, error) {
	var out c.ConnectionInvokeResult
	if p.Operation == "" {
		return out, usagef("an operation is required")
	}
	connID, err := o.ResolveConnection(ctx, p.Connection)
	if err != nil {
		return out, err
	}
	sid := o.Self.SessionID
	if p.Session != "" {
		if sid, err = o.sessionID(ctx, p.Session); err != nil {
			return out, err
		}
	}
	if sid == "" {
		return out, usagef("--session is required outside an agent session")
	}
	node := ""
	if p.Node != "" {
		if node, err = o.NodeID(ctx, p.Node); err != nil {
			return out, err
		}
	}
	params, _ := json.Marshal(c.ConnectionInvokeParams{ConnectionID: connID, SessionID: sid, Operation: p.Operation, Args: p.Args})
	res, err := o.API.InvokeNodeAction(ctx, c.NodeActionRequest{Mutation: mut(p.OperationID), NodeID: node, Action: c.ActionConnInvoke, Params: params, TimeoutSeconds: 60})
	if err != nil {
		return out, err
	}
	if res.Error != nil {
		return out, res.Error
	}
	if res.Outcome != c.OutcomeSucceeded {
		return out, c.Errorf(c.ErrOutcomeIndeterminate, "connection.invoke: outcome %s (command %s); observe it before retrying", res.Outcome, res.CommandID).WithRef(c.KindCommand, res.CommandID)
	}
	if err := json.Unmarshal(res.Result, &out); err != nil {
		return out, c.Errorf(c.ErrInternal, "decode connection.invoke result: %v", err)
	}
	return out, nil
}

// CheckConnection verifies a connection on one node, or on every node that
// holds it.
func (o *Ops) CheckConnection(ctx context.Context, p ConnectionCheckCLIParams) ([]c.ConnectionAvailability, error) {
	connID, err := o.ResolveConnection(ctx, p.Connection)
	if err != nil {
		return nil, err
	}
	var nodes []string
	if p.Node != "" {
		n, err := o.NodeID(ctx, p.Node)
		if err != nil {
			return nil, err
		}
		nodes = []string{n}
	} else if nodes, _, err = o.connectionHolders(ctx, connID); err != nil {
		return nil, err
	}
	out := []c.ConnectionAvailability{}
	for _, n := range nodes {
		var av c.ConnectionAvailability
		if _, err := o.NodeAction(ctx, n, c.ActionConnCheck, c.ConnectionParams{ConnectionID: connID}, &av, ""); err != nil {
			return out, err
		}
		out = append(out, av)
	}
	return out, nil
}

// PromoteNoteParams turn a note into an authoritative document revision:
// a new document (Title, optional Slug) or a successor revision of
// Document based on BaseRevision.
type PromoteNoteParams struct {
	Note         string `json:"note"`
	Document     string `json:"document,omitempty"`
	BaseRevision string `json:"base_revision,omitempty"`
	Title        string `json:"title,omitempty"`
	Slug         string `json:"slug,omitempty"`
	Project      string `json:"project,omitempty"`
	OperationID  string `json:"operation_id,omitempty"`
}

// PromoteNote explicitly promotes a note (never implicit).
func (o *Ops) PromoteNote(ctx context.Context, p PromoteNoteParams) (c.DocumentWithRevision, error) {
	if p.Note == "" {
		return c.DocumentWithRevision{}, usagef("a note ID is required")
	}
	if p.Document != "" && p.Title != "" {
		return c.DocumentWithRevision{}, usagef("give --doc (existing document) or --title (new document), not both")
	}
	if p.Document != "" && p.BaseRevision == "" {
		return c.DocumentWithRevision{}, usagef("--base REV is required with --doc (see `pyre doc show`)")
	}
	pid, err := o.projectPtr(ctx, p.Project)
	if err != nil {
		return c.DocumentWithRevision{}, err
	}
	return o.API.PromoteNote(ctx, c.PromoteNoteRequest{Mutation: mut(p.OperationID), NoteID: p.Note, DocumentID: p.Document,
		BaseRevisionID: p.BaseRevision, ProjectID: pid, Title: p.Title, Slug: p.Slug})
}
