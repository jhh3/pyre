package contracts

import (
	"context"
	"database/sql"
	"time"
)

// Hub-internal composition interfaces. The hub process composes several
// domain packages that share one SQLite database:
//
//	internal/hub          core: system, sessions/runs, leases, approvals,
//	                      messages, events, node command routing, accounts (WP05)
//	internal/topology,
//	internal/enrollment,
//	internal/transport    nodes, enrollment, pairing, clients, auth, node
//	                      WebSocket channel (WP04)
//	internal/projects,
//	internal/artifacts,
//	internal/review       projects, documents, artifacts, comments, notes (WP11)
//	internal/attention,
//	internal/notifications attention inbox and push (WP17)
//
// Each domain owns its tables via a sqlitex migration namespace, emits
// events through EventSink inside its own write transaction, and consumes
// other domains' events by reading EventAPI with a persisted cursor.

// EventSink appends a durable event inside the caller's transaction and
// returns it with the hub-assigned cursor. Implemented by internal/hub
// (events table, namespace "events"). Tests may use an in-memory fake.
type EventSink interface {
	AppendTx(ctx context.Context, tx *sql.Tx, ev Event) (Event, error)
}

// Authenticator resolves a bearer token to a Principal. Implemented by
// internal/transport. Returns ErrUnauthorized for unknown/revoked tokens.
type Authenticator interface {
	Authenticate(ctx context.Context, bearer string) (Principal, error)
}

// TokenIssuer mints scoped credentials. Implemented by internal/transport.
// Only token hashes are persisted; plaintext is returned once.
type TokenIssuer interface {
	// IssueSessionToken mints a credential identifying an agent session; it
	// is revoked when the session is archived or explicitly revoked.
	IssueSessionToken(ctx context.Context, sessionID string) (string, error)
	RevokeSessionTokens(ctx context.Context, sessionID string) error
	// IssueTicket mints a short-lived one-time ticket (terminal attach,
	// preview links) bound to a purpose and object.
	IssueTicket(ctx context.Context, purpose string, ref ObjectRef, ttl time.Duration) (string, time.Time, error)
	// RedeemTicket consumes a ticket exactly once.
	RedeemTicket(ctx context.Context, ticket, purpose string) (ObjectRef, error)
}

// NodeChannel sends commands to a connected node. Implemented by the hub's
// node connection manager (internal/transport server side, wired by WP05).
// Send must only be called after the command is durably queued; it returns
// ErrUnavailable if the node is not connected (the command stays queued).
type NodeChannel interface {
	Send(ctx context.Context, nodeID string, cmd NodeCommand) error
	Connected(nodeID string) bool
}

// Clock is injectable for deterministic tests.
type Clock interface{ Now() time.Time }

// SystemClock is the real clock.
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now().UTC() }
