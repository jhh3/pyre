package cli

import (
	"context"

	c "github.com/jhh3/pyre/internal/contracts"
)

// mcpToolProviders contribute extra MCP tools defined outside mcp.go.
// MCPTools appends every provider's tools after its own list.
var mcpToolProviders []func() []MCPTool

func init() { mcpToolProviders = append(mcpToolProviders, connectionMCPTools) }

// ConnectionAccess is connection_check's answer: may this session use the
// connection now, and where is it available.
type ConnectionAccess struct {
	ConnectionID string                     `json:"connection_id"`
	Session      string                     `json:"session_id"`
	Allowed      bool                       `json:"allowed"`
	Reason       string                     `json:"reason,omitempty"`
	Availability []c.ConnectionAvailability `json:"availability"`
}

// ConnectionAccessParams are connection_check's arguments.
type ConnectionAccessParams struct {
	Connection string `json:"connection"`
	Session    string `json:"session,omitempty"`
}

// CheckConnectionAccess combines the hub's use-time grant decision with
// the catalog's per-node availability. Read-only: it does not re-verify
// the credential on a node (`pyre connections verify` does).
func (o *Ops) CheckConnectionAccess(ctx context.Context, p ConnectionAccessParams) (ConnectionAccess, error) {
	connID, err := o.ResolveConnection(ctx, p.Connection)
	if err != nil {
		return ConnectionAccess{}, err
	}
	sid := o.Self.SessionID
	if p.Session != "" {
		if sid, err = o.sessionID(ctx, p.Session); err != nil {
			return ConnectionAccess{}, err
		}
	}
	if sid == "" {
		return ConnectionAccess{}, usagef("session is required outside an agent session")
	}
	d, err := o.API.CheckConnectionGrant(ctx, c.CheckGrantRequest{ConnectionID: connID, SessionID: sid})
	if err != nil {
		return ConnectionAccess{}, err
	}
	_, conn, err := o.connectionHolders(ctx, connID)
	if err != nil {
		return ConnectionAccess{}, err
	}
	av := conn.Availability
	if av == nil {
		av = []c.ConnectionAvailability{}
	}
	return ConnectionAccess{ConnectionID: connID, Session: sid, Allowed: d.Allowed, Reason: d.Reason, Availability: av}, nil
}

type connectionInvokeToolParams struct {
	Connection  string            `json:"connection"`
	Operation   string            `json:"operation"`
	Args        map[string]string `json:"args,omitempty"`
	Node        string            `json:"node,omitempty"`
	OperationID string            `json:"operation_id,omitempty"`
}

func connectionMCPTools() []MCPTool {
	conn := str("connection ID (conn_...) or name (see `pyre connections`)")
	// A read through an external service: no Pyre state changes, but the
	// provider is outside the fleet (open world).
	invokeHints := hints("Use connection", readOnly, true)
	invokeHints.OpenWorldHint = true
	return []MCPTool{
		tool("connection_check", "Check connection", "Check whether you may use a named connection now and where it is available. Connections verified on your node, or for remote use on another, need no grant. Never returns credentials.",
			obj(props{"connection": conn, "session": str("session ID; default: you")}, "connection"), hints("Check connection", readOnly, true),
			func(ctx context.Context, o *Ops, p ConnectionAccessParams) (ConnectionAccess, error) {
				return o.CheckConnectionAccess(ctx, p)
			}),
		tool("connection_invoke", "Use connection", "Run a read operation on a connection (GitHub, Linear, ...); no grant needed for connections your machines hold. The holding node runs it via Outfit; the credential never reaches you. Writes are refused.",
			obj(props{"connection": conn, "operation": str("operation name (see the connection's skill or `outfit describe`)"),
				"args": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}, "description": "operation arguments"},
				"node": str("executing node; default: the hub routes to the verified holder"), "operation_id": opIDProp}, "connection", "operation"), invokeHints,
			func(ctx context.Context, o *Ops, p connectionInvokeToolParams) (c.ConnectionInvokeResult, error) {
				// Always attributed to the calling session (no session argument).
				return o.InvokeConnection(ctx, ConnectionInvokeCLIParams{Connection: p.Connection, Operation: p.Operation, Args: p.Args, Node: p.Node, OperationID: p.OperationID})
			}),
	}
}
