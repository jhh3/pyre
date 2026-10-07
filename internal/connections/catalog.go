// Package connections is Pyre's connection catalog (hub side) and the node
// action handlers for connection.check / invoke / grant / revoke / list.
//
// The hub catalog stores metadata only: connection names/services/modes,
// per-node availability reported by nodes, and grant records with generation
// numbers. Credential values never enter this package's tables, events,
// results, errors, or logs; they stay in Outfit or a node's OS store
// (internal/credentials).
//
// Grant model. A grant is a tuple (node?, project?, session?) on one
// connection; each non-empty field must match the using session's context.
// The empty tuple is an explicit owner-broad grant. A session may use a
// connection only while some active grant matches it, and every use
// (connection.invoke, launch provisioning) re-checks the CURRENT grants, so a
// revocation takes effect on the next use rather than when a cached label
// expires. Revoking removes a grant; it never broadens access (no grants means
// no access).
package connections

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// Namespace is the sqlitex migration namespace owned by this package.
const Namespace = "connections"

// EvConnectionChanged is the event kind emitted on catalog mutations.
// Proposed for internal/contracts/events.go (not yet frozen there).
const EvConnectionChanged contracts.EventKind = "connection.changed"

// Migrations are the append-only schema steps for the catalog.
var Migrations = []sqlitex.Migration{{Namespace: Namespace, Version: 1, Name: "catalog", SQL: `
CREATE TABLE connections (
	connection_id TEXT PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	service TEXT NOT NULL DEFAULT '',
	modes TEXT NOT NULL DEFAULT '[]',
	generation INTEGER NOT NULL DEFAULT 1,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	removed_at TEXT
);
CREATE TABLE connection_grants (
	grant_id TEXT PRIMARY KEY,
	connection_id TEXT NOT NULL REFERENCES connections(connection_id),
	node_id TEXT NOT NULL DEFAULT '',
	project_id TEXT NOT NULL DEFAULT '',
	session_id TEXT NOT NULL DEFAULT '',
	granted_by TEXT NOT NULL,
	granted_generation INTEGER NOT NULL,
	granted_at TEXT NOT NULL,
	revoked_by TEXT,
	revoked_generation INTEGER,
	revoked_at TEXT
);
CREATE UNIQUE INDEX connection_grants_active ON connection_grants(connection_id, node_id, project_id, session_id) WHERE revoked_at IS NULL;
CREATE INDEX connection_grants_session ON connection_grants(session_id) WHERE revoked_at IS NULL;
CREATE TABLE connection_availability (
	connection_id TEXT NOT NULL REFERENCES connections(connection_id),
	node_id TEXT NOT NULL,
	mode TEXT NOT NULL,
	state TEXT NOT NULL,
	verified_at TEXT NOT NULL DEFAULT '',
	detail TEXT NOT NULL DEFAULT '',
	reported_at TEXT NOT NULL,
	PRIMARY KEY (connection_id, node_id, mode)
);
`}}

// Options configure a Catalog.
type Options struct {
	Events contracts.EventSink // optional; events are appended inside mutations
	Clock  contracts.Clock     // default SystemClock
}

// Catalog is the hub-side connection catalog.
type Catalog struct {
	db     *sql.DB
	events contracts.EventSink
	clock  contracts.Clock
}

// Open migrates (ops + connections namespaces) and returns the catalog.
func Open(ctx context.Context, db *sql.DB, opt Options) (*Catalog, error) {
	if err := sqlitex.Migrate(ctx, db, append([]sqlitex.Migration{sqlitex.OperationsMigration}, Migrations...)...); err != nil {
		return nil, err
	}
	if opt.Clock == nil {
		opt.Clock = contracts.SystemClock{}
	}
	return &Catalog{db: db, events: opt.Events, clock: opt.Clock}, nil
}

// ConnectionSpec creates or updates catalog metadata (owner action).
type ConnectionSpec struct {
	contracts.Mutation
	ConnectionID string   `json:"connection_id,omitempty"` // default: derived from Name
	Name         string   `json:"name"`
	Service      string   `json:"service"`
	Modes        []string `json:"modes"`
}

// GrantRequest is a mutating grant/revoke request.
type GrantRequest struct {
	contracts.Mutation
	contracts.ConnectionGrantParams
}

// GrantRecord is one grant row (metadata).
type GrantRecord struct {
	GrantID           string     `json:"grant_id"`
	ConnectionID      string     `json:"connection_id"`
	NodeID            string     `json:"node_id,omitempty"`
	ProjectID         string     `json:"project_id,omitempty"`
	SessionID         string     `json:"session_id,omitempty"`
	GrantedBy         string     `json:"granted_by"`
	GrantedGeneration int64      `json:"granted_generation"`
	GrantedAt         time.Time  `json:"granted_at"`
	RevokedBy         string     `json:"revoked_by,omitempty"`
	RevokedGeneration int64      `json:"revoked_generation,omitempty"`
	RevokedAt         *time.Time `json:"revoked_at,omitempty"`
}

// Broad reports whether the grant is owner-broad.
func (g GrantRecord) Broad() bool { return g.NodeID == "" && g.ProjectID == "" && g.SessionID == "" }

// SessionContext is the authoritative context of a session using a connection.
type SessionContext struct {
	SessionID string
	NodeID    string // node the session runs on
	ProjectID string // "" for projectless sessions
	Archived  bool
}

// SessionLookup resolves a session's authoritative context (implemented by
// the hub core, which owns sessions).
type SessionLookup interface {
	SessionContext(ctx context.Context, sessionID string) (SessionContext, error)
}

// SessionLookupFunc adapts a function.
type SessionLookupFunc func(ctx context.Context, sessionID string) (SessionContext, error)

func (f SessionLookupFunc) SessionContext(ctx context.Context, id string) (SessionContext, error) {
	return f(ctx, id)
}

// GrantDecision is the result of a use-time grant check.
type GrantDecision struct {
	ConnectionID string    `json:"connection_id"`
	SessionID    string    `json:"session_id"`
	Allowed      bool      `json:"allowed"`
	GrantID      string    `json:"grant_id,omitempty"`
	Generation   int64     `json:"generation"`
	Reason       string    `json:"reason"`
	CheckedAt    time.Time `json:"checked_at"`
}

// GrantChecker answers "may this session use this connection right now?".
// Node handlers call it on every use. On the hub it is backed by the catalog
// (Catalog.Checker); a node's implementation asks the hub. Errors must be
// treated as denial (fail closed).
type GrantChecker interface {
	CheckGrant(ctx context.Context, connectionID, sessionID string) (GrantDecision, error)
}

// GrantCheckerFunc adapts a function.
type GrantCheckerFunc func(ctx context.Context, connectionID, sessionID string) (GrantDecision, error)

func (f GrantCheckerFunc) CheckGrant(ctx context.Context, c, s string) (GrantDecision, error) {
	return f(ctx, c, s)
}

func canMutate(p contracts.Principal) error {
	switch p.Kind {
	case contracts.PrincipalOwner, contracts.PrincipalSystem:
		return nil
	}
	return contracts.Errorf(contracts.ErrForbidden, "only the owner can change connection grants or catalog entries (principal %s)", p.Kind)
}

func who(p contracts.Principal) string { return string(p.Kind) + ":" + p.ID }

func validModes(modes []string) error {
	for _, m := range modes {
		if m != "local_credential" && m != "remote_invoke" {
			return contracts.Errorf(contracts.ErrInvalid, "unknown connection mode %q", m)
		}
	}
	return nil
}

// DefaultConnectionID derives the fleet-wide ID from a name (same mapping as
// integrations/outfit.ConnectionIDFor).
func DefaultConnectionID(name string) string {
	return contracts.PrefixConnection + "_" + strings.ReplaceAll(name, "_", ":")
}

// Upsert creates or updates catalog metadata.
func (c *Catalog) Upsert(ctx context.Context, p contracts.Principal, spec ConnectionSpec) (contracts.Connection, error) {
	if err := canMutate(p); err != nil {
		return contracts.Connection{}, err
	}
	if spec.ConnectionID == "" {
		spec.ConnectionID = DefaultConnectionID(spec.Name)
	}
	if !contracts.ValidID(spec.ConnectionID, contracts.PrefixConnection) || spec.Name == "" || len(spec.Name) > 64 {
		return contracts.Connection{}, contracts.Errorf(contracts.ErrInvalid, "connection needs a valid id and a 1-64 character name")
	}
	if err := validModes(spec.Modes); err != nil {
		return contracts.Connection{}, err
	}
	var out contracts.Connection
	err := c.mutate(ctx, func(tx *sql.Tx) error {
		_, err := sqlitex.Idempotent(ctx, tx, spec.OperationID, "connections.upsert", p, spec, &out, func() (any, error) {
			now := sqlitex.FormatTime(c.clock.Now())
			modes, _ := json.Marshal(sortedUnique(spec.Modes))
			var existing string
			switch err := tx.QueryRowContext(ctx, `SELECT connection_id FROM connections WHERE name=? AND connection_id<>?`, spec.Name, spec.ConnectionID).Scan(&existing); {
			case err == nil:
				return nil, contracts.Errorf(contracts.ErrConflict, "connection name %q is already used by %s", spec.Name, existing)
			case !errors.Is(err, sql.ErrNoRows):
				return nil, err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO connections(connection_id,name,service,modes,generation,created_at,updated_at) VALUES(?,?,?,?,1,?,?)
				ON CONFLICT(connection_id) DO UPDATE SET name=excluded.name, service=excluded.service, modes=excluded.modes, generation=generation+1, updated_at=excluded.updated_at, removed_at=NULL`,
				spec.ConnectionID, spec.Name, spec.Service, string(modes), now, now); err != nil {
				return nil, err
			}
			return c.emitAndLoad(ctx, tx, spec.ConnectionID, "upsert")
		})
		return err
	})
	return out, err
}

// Remove soft-deletes a connection and revokes all of its grants.
func (c *Catalog) Remove(ctx context.Context, p contracts.Principal, m contracts.Mutation, connectionID string) (contracts.Connection, error) {
	if err := canMutate(p); err != nil {
		return contracts.Connection{}, err
	}
	var out contracts.Connection
	err := c.mutate(ctx, func(tx *sql.Tx) error {
		req := struct {
			contracts.Mutation
			ID string `json:"id"`
		}{m, connectionID}
		_, err := sqlitex.Idempotent(ctx, tx, m.OperationID, "connections.remove", p, req, &out, func() (any, error) {
			gen, err := c.bump(ctx, tx, connectionID)
			if err != nil {
				return nil, err
			}
			now := sqlitex.FormatTime(c.clock.Now())
			if _, err := tx.ExecContext(ctx, `UPDATE connection_grants SET revoked_by=?, revoked_generation=?, revoked_at=? WHERE connection_id=? AND revoked_at IS NULL`, who(p), gen, now, connectionID); err != nil {
				return nil, err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE connections SET removed_at=? WHERE connection_id=?`, now, connectionID); err != nil {
				return nil, err
			}
			return c.emitAndLoad(ctx, tx, connectionID, "remove")
		})
		return err
	})
	return out, err
}

// mutate runs fn in a transaction. Contract errors recorded by
// sqlitex.Idempotent are committed so a retry of the same operation replays
// the same answer; other errors roll back.
func (c *Catalog) mutate(ctx context.Context, fn func(*sql.Tx) error) error {
	var keep error
	err := sqlitex.Tx(ctx, c.db, func(tx *sql.Tx) error {
		err := fn(tx)
		var ce *contracts.Error
		if err != nil && errors.As(err, &ce) && ce.Code != contracts.ErrInternal && ce.Code != contracts.ErrUnavailable && ce.Code != contracts.ErrConflict {
			keep = ce // bare *contracts.Error, not the sqlitex.StoredRejection wrapper
			return nil
		}
		return err
	})
	if err != nil {
		return err
	}
	return keep
}

func validateGrantParams(g contracts.ConnectionGrantParams) error {
	if !contracts.ValidID(g.ConnectionID, contracts.PrefixConnection) {
		return contracts.Errorf(contracts.ErrInvalid, "invalid connection_id %q", g.ConnectionID)
	}
	if g.NodeID != "" && !contracts.ValidID(g.NodeID, contracts.PrefixNode) {
		return contracts.Errorf(contracts.ErrInvalid, "invalid node_id %q", g.NodeID)
	}
	if g.ProjectID != "" && !contracts.ValidID(g.ProjectID, contracts.PrefixProject) {
		return contracts.Errorf(contracts.ErrInvalid, "invalid project_id %q", g.ProjectID)
	}
	if g.SessionID != "" && !contracts.ValidID(g.SessionID, contracts.PrefixSession) {
		return contracts.Errorf(contracts.ErrInvalid, "invalid session_id %q", g.SessionID)
	}
	return nil
}

// bump increments and returns the connection generation (ErrNotFound if
// absent or removed).
func (c *Catalog) bump(ctx context.Context, tx *sql.Tx, id string) (int64, error) {
	var gen int64
	err := tx.QueryRowContext(ctx, `UPDATE connections SET generation=generation+1, updated_at=? WHERE connection_id=? AND removed_at IS NULL RETURNING generation`, sqlitex.FormatTime(c.clock.Now()), id).Scan(&gen)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, contracts.Errorf(contracts.ErrNotFound, "connection %s is not in the catalog", id).WithRef(contracts.KindConnection, id)
	}
	return gen, err
}

// Grant adds an active grant (no-op when an identical one is active).
func (c *Catalog) Grant(ctx context.Context, p contracts.Principal, req GrantRequest) (contracts.Connection, error) {
	if err := canMutate(p); err != nil {
		return contracts.Connection{}, err
	}
	if err := validateGrantParams(req.ConnectionGrantParams); err != nil {
		return contracts.Connection{}, err
	}
	g := req.ConnectionGrantParams
	var out contracts.Connection
	err := c.mutate(ctx, func(tx *sql.Tx) error {
		_, err := sqlitex.Idempotent(ctx, tx, req.OperationID, "connections.grant", p, req, &out, func() (any, error) {
			var existing string
			err := tx.QueryRowContext(ctx, `SELECT grant_id FROM connection_grants WHERE connection_id=? AND node_id=? AND project_id=? AND session_id=? AND revoked_at IS NULL`, g.ConnectionID, g.NodeID, g.ProjectID, g.SessionID).Scan(&existing)
			if err == nil {
				return c.load(ctx, tx, g.ConnectionID)
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
			gen, err := c.bump(ctx, tx, g.ConnectionID)
			if err != nil {
				return nil, err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO connection_grants(grant_id,connection_id,node_id,project_id,session_id,granted_by,granted_generation,granted_at) VALUES(?,?,?,?,?,?,?,?)`,
				contracts.NewID(contracts.PrefixGrant), g.ConnectionID, g.NodeID, g.ProjectID, g.SessionID, who(p), gen, sqlitex.FormatTime(c.clock.Now())); err != nil {
				return nil, err
			}
			return c.emitAndLoad(ctx, tx, g.ConnectionID, "grant")
		})
		return err
	})
	return out, err
}

// Revoke ends the active grant exactly matching the tuple. Other grants that
// still match a session continue to authorize it (revocation never adds a
// deny rule and never broadens access).
func (c *Catalog) Revoke(ctx context.Context, p contracts.Principal, req GrantRequest) (contracts.Connection, error) {
	if err := canMutate(p); err != nil {
		return contracts.Connection{}, err
	}
	if err := validateGrantParams(req.ConnectionGrantParams); err != nil {
		return contracts.Connection{}, err
	}
	g := req.ConnectionGrantParams
	var out contracts.Connection
	err := c.mutate(ctx, func(tx *sql.Tx) error {
		_, err := sqlitex.Idempotent(ctx, tx, req.OperationID, "connections.revoke", p, req, &out, func() (any, error) {
			var grantID string
			err := tx.QueryRowContext(ctx, `SELECT grant_id FROM connection_grants WHERE connection_id=? AND node_id=? AND project_id=? AND session_id=? AND revoked_at IS NULL`, g.ConnectionID, g.NodeID, g.ProjectID, g.SessionID).Scan(&grantID)
			if errors.Is(err, sql.ErrNoRows) {
				return nil, contracts.Errorf(contracts.ErrNotFound, "no active grant on %s matches node=%q project=%q session=%q", g.ConnectionID, g.NodeID, g.ProjectID, g.SessionID).WithRef(contracts.KindConnection, g.ConnectionID)
			}
			if err != nil {
				return nil, err
			}
			gen, err := c.bump(ctx, tx, g.ConnectionID)
			if err != nil {
				return nil, err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE connection_grants SET revoked_by=?, revoked_generation=?, revoked_at=? WHERE grant_id=?`, who(p), gen, sqlitex.FormatTime(c.clock.Now()), grantID); err != nil {
				return nil, err
			}
			return c.emitAndLoad(ctx, tx, g.ConnectionID, "revoke")
		})
		return err
	})
	return out, err
}

func (c *Catalog) emitAndLoad(ctx context.Context, tx *sql.Tx, id, change string) (contracts.Connection, error) {
	conn, err := c.load(ctx, tx, id)
	if err != nil {
		return conn, err
	}
	if c.events != nil {
		var gen int64
		_ = tx.QueryRowContext(ctx, `SELECT generation FROM connections WHERE connection_id=?`, id).Scan(&gen)
		payload, _ := json.Marshal(map[string]any{"connection_id": id, "change": change, "generation": gen})
		now := c.clock.Now()
		if _, err := c.events.AppendTx(ctx, tx, contracts.Event{EventID: contracts.NewID(contracts.PrefixEvent), Origin: "hub", Kind: EvConnectionChanged, Object: contracts.ObjectRef{Kind: contracts.KindConnection, ID: id}, PayloadVersion: 1, Payload: payload, OccurredAt: now, IngestedAt: now}); err != nil {
			return conn, err
		}
	}
	return conn, nil
}

type querier interface {
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
}

func (c *Catalog) load(ctx context.Context, q querier, id string) (contracts.Connection, error) {
	var conn contracts.Connection
	var modes string
	var removed sql.NullString
	err := q.QueryRowContext(ctx, `SELECT connection_id,name,service,modes,removed_at FROM connections WHERE connection_id=?`, id).Scan(&conn.ConnectionID, &conn.Name, &conn.Service, &modes, &removed)
	if errors.Is(err, sql.ErrNoRows) {
		return conn, contracts.Errorf(contracts.ErrNotFound, "connection %s is not in the catalog", id).WithRef(contracts.KindConnection, id)
	}
	if err != nil {
		return conn, err
	}
	_ = json.Unmarshal([]byte(modes), &conn.Modes)
	if conn.Modes == nil {
		conn.Modes = []string{}
	}
	conn.Availability = []contracts.ConnectionAvailability{}
	rows, err := q.QueryContext(ctx, `SELECT node_id,mode,state,verified_at,detail FROM connection_availability WHERE connection_id=? ORDER BY node_id, mode`, id)
	if err != nil {
		return conn, err
	}
	for rows.Next() {
		var av contracts.ConnectionAvailability
		var verified string
		if err := rows.Scan(&av.NodeID, &av.Mode, &av.State, &verified, &av.Detail); err != nil {
			rows.Close()
			return conn, err
		}
		av.VerifiedAt, _ = sqlitex.ParseTime(verified)
		conn.Availability = append(conn.Availability, av)
	}
	rows.Close()
	grants, err := c.grants(ctx, q, id, false)
	if err != nil {
		return conn, err
	}
	conn.Scope = scopeOf(grants)
	return conn, rows.Err()
}

// scopeOf projects active grants onto the contract scope. A broad grant
// yields the empty (owner-broad) scope. NOTE: a connection with no active
// grants also renders an empty scope in contract v1; authorization never
// relies on this projection (see CheckGrant). A contract field distinguishing
// "none" from "broad" is proposed in the WP12 report.
func scopeOf(grants []GrantRecord) contracts.ConnectionScope {
	var s contracts.ConnectionScope
	for _, g := range grants {
		if g.Broad() {
			return contracts.ConnectionScope{}
		}
		if g.NodeID != "" {
			s.Nodes = append(s.Nodes, g.NodeID)
		}
		if g.ProjectID != "" {
			s.Projects = append(s.Projects, g.ProjectID)
		}
		if g.SessionID != "" {
			s.Sessions = append(s.Sessions, g.SessionID)
		}
	}
	s.Nodes, s.Projects, s.Sessions = sortedUniqueOrNil(s.Nodes), sortedUniqueOrNil(s.Projects), sortedUniqueOrNil(s.Sessions)
	return s
}

func (c *Catalog) grants(ctx context.Context, q querier, id string, includeRevoked bool) ([]GrantRecord, error) {
	query := `SELECT grant_id,connection_id,node_id,project_id,session_id,granted_by,granted_generation,granted_at,COALESCE(revoked_by,''),COALESCE(revoked_generation,0),revoked_at FROM connection_grants WHERE connection_id=?`
	if !includeRevoked {
		query += ` AND revoked_at IS NULL`
	}
	rows, err := q.QueryContext(ctx, query+` ORDER BY granted_generation`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GrantRecord
	for rows.Next() {
		var g GrantRecord
		var granted string
		var revoked sql.NullString
		if err := rows.Scan(&g.GrantID, &g.ConnectionID, &g.NodeID, &g.ProjectID, &g.SessionID, &g.GrantedBy, &g.GrantedGeneration, &granted, &g.RevokedBy, &g.RevokedGeneration, &revoked); err != nil {
			return nil, err
		}
		g.GrantedAt, _ = sqlitex.ParseTime(granted)
		if revoked.Valid {
			t, _ := sqlitex.ParseTime(revoked.String)
			g.RevokedAt = &t
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// Grants lists a connection's grants (optionally including revoked ones).
func (c *Catalog) Grants(ctx context.Context, connectionID string, includeRevoked bool) ([]GrantRecord, error) {
	return c.grants(ctx, c.db, connectionID, includeRevoked)
}

// Get returns one connection.
func (c *Catalog) Get(ctx context.Context, id string) (contracts.Connection, error) {
	return c.load(ctx, c.db, id)
}

// Generation returns a connection's current generation.
func (c *Catalog) Generation(ctx context.Context, id string) (int64, error) {
	var gen int64
	err := c.db.QueryRowContext(ctx, `SELECT generation FROM connections WHERE connection_id=?`, id).Scan(&gen)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, contracts.Errorf(contracts.ErrNotFound, "connection %s is not in the catalog", id)
	}
	return gen, err
}

// List implements NodeActionAPI.ListConnections: NodeID filters to
// connections with availability on that node; ProjectID to connections
// usable by every session of that project (broad, node-only, or matching
// project grants).
func (c *Catalog) List(ctx context.Context, req contracts.ListConnectionsRequest) (contracts.Page[contracts.Connection], error) {
	limit := req.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	after := ""
	if req.Cursor != "" {
		b, err := base64.RawURLEncoding.DecodeString(req.Cursor)
		if err != nil {
			return contracts.Page[contracts.Connection]{}, contracts.Errorf(contracts.ErrInvalid, "invalid cursor")
		}
		after = string(b)
	}
	q := `SELECT c.connection_id, c.name FROM connections c WHERE c.removed_at IS NULL AND c.name > ?`
	args := []any{after}
	if req.NodeID != "" {
		q += ` AND EXISTS (SELECT 1 FROM connection_availability a WHERE a.connection_id=c.connection_id AND a.node_id=?)`
		args = append(args, req.NodeID)
	}
	if req.ProjectID != "" {
		q += ` AND EXISTS (SELECT 1 FROM connection_grants g WHERE g.connection_id=c.connection_id AND g.revoked_at IS NULL AND g.session_id='' AND (g.project_id='' OR g.project_id=?))`
		args = append(args, req.ProjectID)
	}
	q += ` ORDER BY c.name LIMIT ?`
	args = append(args, limit+1)
	rows, err := c.db.QueryContext(ctx, q, args...)
	if err != nil {
		return contracts.Page[contracts.Connection]{}, err
	}
	var ids, names []string
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			rows.Close()
			return contracts.Page[contracts.Connection]{}, err
		}
		ids, names = append(ids, id), append(names, name)
	}
	rows.Close()
	page := contracts.Page[contracts.Connection]{Items: []contracts.Connection{}, SnapshotAt: c.clock.Now()}
	if len(ids) > limit {
		ids = ids[:limit]
		page.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(names[limit-1]))
	}
	for _, id := range ids {
		conn, err := c.load(ctx, c.db, id)
		if err != nil {
			return page, err
		}
		page.Items = append(page.Items, conn)
	}
	return page, nil
}

// CheckGrant decides, from current state, whether sc may use connectionID.
func (c *Catalog) CheckGrant(ctx context.Context, connectionID string, sc SessionContext) (GrantDecision, error) {
	d := GrantDecision{ConnectionID: connectionID, SessionID: sc.SessionID, CheckedAt: c.clock.Now()}
	var gen int64
	var removed sql.NullString
	err := c.db.QueryRowContext(ctx, `SELECT generation, removed_at FROM connections WHERE connection_id=?`, connectionID).Scan(&gen, &removed)
	if errors.Is(err, sql.ErrNoRows) {
		d.Reason = "connection is not in the catalog"
		return d, nil
	}
	if err != nil {
		return d, err
	}
	d.Generation = gen
	if removed.Valid {
		d.Reason = "connection was removed from the catalog"
		return d, nil
	}
	if sc.SessionID == "" || sc.Archived {
		d.Reason = "session is unknown or archived"
		return d, nil
	}
	grants, err := c.grants(ctx, c.db, connectionID, false)
	if err != nil {
		return d, err
	}
	best, bestScore := GrantRecord{}, -1
	for _, g := range grants {
		if (g.NodeID != "" && g.NodeID != sc.NodeID) || (g.ProjectID != "" && g.ProjectID != sc.ProjectID) || (g.SessionID != "" && g.SessionID != sc.SessionID) {
			continue
		}
		score := 0
		for _, f := range []string{g.NodeID, g.ProjectID, g.SessionID} {
			if f != "" {
				score++
			}
		}
		if score > bestScore {
			best, bestScore = g, score
		}
	}
	if bestScore < 0 {
		d.Reason = "no active grant matches this session (node, project, session)"
		return d, nil
	}
	d.Allowed, d.GrantID = true, best.GrantID
	switch {
	case best.SessionID != "":
		d.Reason = "session grant"
	case best.ProjectID != "":
		d.Reason = "project grant"
	case best.NodeID != "":
		d.Reason = "node grant"
	default:
		d.Reason = "owner-broad grant"
	}
	return d, nil
}

// Checker returns a GrantChecker backed by the catalog and the hub's session
// records. Lookup failures deny.
func (c *Catalog) Checker(lookup SessionLookup) GrantChecker {
	return GrantCheckerFunc(func(ctx context.Context, connectionID, sessionID string) (GrantDecision, error) {
		sc, err := lookup.SessionContext(ctx, sessionID)
		if err != nil {
			return GrantDecision{ConnectionID: connectionID, SessionID: sessionID, Reason: "session lookup failed", CheckedAt: c.clock.Now()}, err
		}
		sc.SessionID = sessionID
		return c.CheckGrant(ctx, connectionID, sc)
	})
}

// Route is where a connection.invoke should execute.
type Route struct {
	NodeID   string        `json:"node_id"`
	Decision GrantDecision `json:"decision"`
}

// RouteInvoke checks the session's grant and picks the node that executes a
// remote invocation: verified remote_invoke availability, preferring the
// session's own node, then the most recent verification.
func (c *Catalog) RouteInvoke(ctx context.Context, connectionID string, sc SessionContext) (Route, error) {
	d, err := c.CheckGrant(ctx, connectionID, sc)
	if err != nil {
		return Route{}, err
	}
	if !d.Allowed {
		return Route{Decision: d}, contracts.Errorf(contracts.ErrForbidden, "session %s may not use %s: %s", sc.SessionID, connectionID, d.Reason).WithRef(contracts.KindConnection, connectionID)
	}
	conn, err := c.Get(ctx, connectionID)
	if err != nil {
		return Route{}, err
	}
	var best *contracts.ConnectionAvailability
	for i := range conn.Availability {
		av := &conn.Availability[i]
		if av.Mode != "remote_invoke" || av.State != "verified" {
			continue
		}
		if best == nil || (av.NodeID == sc.NodeID && best.NodeID != sc.NodeID) || (best.NodeID != sc.NodeID && av.VerifiedAt.After(best.VerifiedAt)) {
			best = av
		}
	}
	if best == nil {
		return Route{Decision: d}, contracts.Errorf(contracts.ErrUnavailable, "connection %s has no verified remote_invoke availability; run connection.check on the node that holds it", connectionID).WithRef(contracts.KindConnection, connectionID)
	}
	return Route{NodeID: best.NodeID, Decision: d}, nil
}

// ApplyReport ingests a node's full connection snapshot (ReportConnections).
// A node reports only for itself: availability entries naming other nodes are
// ignored, and connections this node previously reported but omitted are
// marked "missing" for it. Unknown connections are added to the catalog.
func (c *Catalog) ApplyReport(ctx context.Context, nodeID string, conns []contracts.Connection) error {
	if !contracts.ValidID(nodeID, contracts.PrefixNode) {
		return contracts.Errorf(contracts.ErrInvalid, "invalid node id %q", nodeID)
	}
	return sqlitex.Tx(ctx, c.db, func(tx *sql.Tx) error {
		now := sqlitex.FormatTime(c.clock.Now())
		reported := map[string]bool{}
		for _, conn := range conns {
			if !contracts.ValidID(conn.ConnectionID, contracts.PrefixConnection) || conn.Name == "" || validModes(conn.Modes) != nil {
				continue
			}
			var curModes string
			err := tx.QueryRowContext(ctx, `SELECT modes FROM connections WHERE connection_id=?`, conn.ConnectionID).Scan(&curModes)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				var other string
				if tx.QueryRowContext(ctx, `SELECT connection_id FROM connections WHERE name=?`, conn.Name).Scan(&other) == nil {
					continue // name owned by a different catalog entry; owner must reconcile
				}
				modes, _ := json.Marshal(sortedUnique(conn.Modes))
				if _, err := tx.ExecContext(ctx, `INSERT INTO connections(connection_id,name,service,modes,generation,created_at,updated_at) VALUES(?,?,?,?,1,?,?)`, conn.ConnectionID, conn.Name, conn.Service, string(modes), now, now); err != nil {
					return err
				}
			case err != nil:
				return err
			default:
				var existing []string
				_ = json.Unmarshal([]byte(curModes), &existing)
				merged, _ := json.Marshal(sortedUnique(append(existing, conn.Modes...)))
				if _, err := tx.ExecContext(ctx, `UPDATE connections SET modes=?, service=CASE WHEN service='' THEN ? ELSE service END WHERE connection_id=?`, string(merged), conn.Service, conn.ConnectionID); err != nil {
					return err
				}
			}
			for _, av := range conn.Availability {
				if av.NodeID != "" && av.NodeID != nodeID {
					continue
				}
				if av.Mode == "" {
					continue
				}
				if err := upsertAvailability(ctx, tx, conn.ConnectionID, nodeID, av, now); err != nil {
					return err
				}
				reported[conn.ConnectionID+"|"+av.Mode] = true
			}
		}
		rows, err := tx.QueryContext(ctx, `SELECT connection_id, mode FROM connection_availability WHERE node_id=? AND state<>'missing'`, nodeID)
		if err != nil {
			return err
		}
		var stale [][2]string
		for rows.Next() {
			var id, mode string
			if err := rows.Scan(&id, &mode); err != nil {
				rows.Close()
				return err
			}
			if !reported[id+"|"+mode] {
				stale = append(stale, [2]string{id, mode})
			}
		}
		rows.Close()
		for _, s := range stale {
			if _, err := tx.ExecContext(ctx, `UPDATE connection_availability SET state='missing', detail='no longer reported by this node', reported_at=? WHERE connection_id=? AND node_id=? AND mode=?`, now, s[0], nodeID, s[1]); err != nil {
				return err
			}
		}
		return nil
	})
}

// RecordCheck stores a connection.check result from nodeID.
func (c *Catalog) RecordCheck(ctx context.Context, nodeID, connectionID string, av contracts.ConnectionAvailability) error {
	if av.NodeID != "" && av.NodeID != nodeID {
		return contracts.Errorf(contracts.ErrForbidden, "node %s cannot report availability for node %s", nodeID, av.NodeID)
	}
	return sqlitex.Tx(ctx, c.db, func(tx *sql.Tx) error {
		var one int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM connections WHERE connection_id=?`, connectionID).Scan(&one); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return contracts.Errorf(contracts.ErrNotFound, "connection %s is not in the catalog", connectionID)
			}
			return err
		}
		return upsertAvailability(ctx, tx, connectionID, nodeID, av, sqlitex.FormatTime(c.clock.Now()))
	})
}

func upsertAvailability(ctx context.Context, tx *sql.Tx, connID, nodeID string, av contracts.ConnectionAvailability, now string) error {
	verified := ""
	if !av.VerifiedAt.IsZero() {
		verified = sqlitex.FormatTime(av.VerifiedAt)
	}
	switch av.State {
	case "verified", "missing", "expired", "locked_store", "unknown":
	default:
		av.State = "unknown"
	}
	if len(av.Detail) > 300 {
		av.Detail = av.Detail[:300]
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO connection_availability(connection_id,node_id,mode,state,verified_at,detail,reported_at) VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(connection_id,node_id,mode) DO UPDATE SET state=excluded.state, verified_at=excluded.verified_at, detail=excluded.detail, reported_at=excluded.reported_at`,
		connID, nodeID, av.Mode, av.State, verified, av.Detail, now)
	return err
}

func sortedUnique(in []string) []string {
	m := map[string]bool{}
	out := []string{}
	for _, s := range in {
		if s != "" && !m[s] {
			m[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func sortedUniqueOrNil(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	return sortedUnique(in)
}
