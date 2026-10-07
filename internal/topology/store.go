package topology

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/enrollment"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
	"github.com/jhh3/pyre/internal/transport"
)

// Migrations are the "fleet" namespace migrations.
var Migrations = []sqlitex.Migration{{
	Namespace: "fleet", Version: 1, Name: "nodes", SQL: `
CREATE TABLE fleet_nodes (
	node_id         TEXT PRIMARY KEY,
	name            TEXT NOT NULL,
	hostname        TEXT NOT NULL DEFAULT '',
	ssh_alias       TEXT NOT NULL DEFAULT '',
	platform        TEXT NOT NULL DEFAULT '',
	arch            TEXT NOT NULL DEFAULT '',
	harnesses       TEXT NOT NULL DEFAULT '[]',
	versions        TEXT NOT NULL DEFAULT '{}',
	capabilities    TEXT NOT NULL DEFAULT '[]',
	resources       TEXT NOT NULL DEFAULT '{}',
	labels          TEXT NOT NULL DEFAULT '{}',
	enrolled_at     TEXT NOT NULL,
	conn_state      TEXT NOT NULL,
	conn_id         TEXT,
	last_seen_at    TEXT,
	connected_since TEXT,
	remote_addr     TEXT NOT NULL DEFAULT '',
	protocol        INTEGER NOT NULL DEFAULT 0,
	revoked_at      TEXT,
	revoked_reason  TEXT NOT NULL DEFAULT '',
	status_event_at TEXT
);
`}}

// DefaultStatusEventInterval bounds node.status event volume: a status
// report emits an event only when harnesses/versions change or this long
// has passed since the last one. Stored resources are always updated.
const DefaultStatusEventInterval = 5 * time.Minute

// Store is the hub's node registry. It implements contracts.FleetAPI using
// the enrollment and auth stores and records connection observations.
type Store struct {
	db    *sql.DB
	auth  *transport.AuthStore
	enr   *enrollment.Store
	sink  contracts.EventSink
	clock contracts.Clock

	// StatusEventInterval may be changed before use.
	StatusEventInterval time.Duration
}

var _ contracts.FleetAPI = (*Store)(nil)

// NewStore migrates the "fleet" and "ops" namespaces and registers itself as
// enr's node-enrolled hook, so redeeming an enrollment ticket creates the
// node record and emits node.enrolled in the same transaction. sink may be
// nil only in tests that do not care about events.
func NewStore(db *sql.DB, auth *transport.AuthStore, enr *enrollment.Store, sink contracts.EventSink, clock contracts.Clock) (*Store, error) {
	if db == nil || auth == nil || enr == nil {
		return nil, errors.New("topology: db, auth and enrollment stores are required")
	}
	if clock == nil {
		clock = contracts.SystemClock{}
	}
	if err := sqlitex.Migrate(context.Background(), db, append([]sqlitex.Migration{sqlitex.OperationsMigration}, Migrations...)...); err != nil {
		return nil, err
	}
	s := &Store{db: db, auth: auth, enr: enr, sink: sink, clock: clock, StatusEventInterval: DefaultStatusEventInterval}
	enr.SetNodeEnrolledHook(s.registerNodeTx)
	return s, nil
}

// ---- authorization ----

func principal(ctx context.Context) (contracts.Principal, error) {
	p, ok := contracts.PrincipalFrom(ctx)
	if !ok || p.ID == "" {
		return p, contracts.Errorf(contracts.ErrUnauthorized, "authentication required")
	}
	return p, nil
}

func requireOwner(ctx context.Context) (contracts.Principal, error) {
	p, err := principal(ctx)
	if err != nil {
		return p, err
	}
	if p.Kind != contracts.PrincipalOwner && p.Kind != contracts.PrincipalSystem {
		return p, contracts.Errorf(contracts.ErrForbidden, "only the owner can manage nodes and clients")
	}
	return p, nil
}

// ---- row mapping ----

const nodeCols = `node_id, name, hostname, ssh_alias, platform, arch, harnesses, versions, capabilities, resources, labels,
	enrolled_at, conn_state, last_seen_at, connected_since, remote_addr, protocol`

type rowScanner interface{ Scan(...any) error }

func scanNode(r rowScanner) (contracts.Node, error) {
	var n contracts.Node
	var harnesses, versions, caps, resources, labels, enrolled, state string
	var lastSeen, since sql.NullString
	if err := r.Scan(&n.NodeID, &n.Name, &n.Hostname, &n.SSHAlias, &n.Platform, &n.Arch, &harnesses, &versions, &caps, &resources, &labels,
		&enrolled, &state, &lastSeen, &since, &n.Connection.RemoteAddr, &n.Connection.Protocol); err != nil {
		return n, err
	}
	_ = json.Unmarshal([]byte(harnesses), &n.Harnesses)
	_ = json.Unmarshal([]byte(versions), &n.Versions)
	_ = json.Unmarshal([]byte(caps), &n.Capabilities)
	_ = json.Unmarshal([]byte(resources), &n.Resources)
	_ = json.Unmarshal([]byte(labels), &n.Labels)
	if n.Harnesses == nil {
		n.Harnesses = []contracts.HarnessInfo{}
	}
	if n.Versions == nil {
		n.Versions = map[string]string{}
	}
	if n.Capabilities == nil {
		n.Capabilities = []string{}
	}
	if len(n.Labels) == 0 {
		n.Labels = nil
	}
	n.EnrolledAt, _ = sqlitex.ParseTime(enrolled)
	n.Connection.State = contracts.ConnectionState(state)
	if lastSeen.Valid {
		t, _ := sqlitex.ParseTime(lastSeen.String)
		n.Connection.LastSeenAt = &t
	}
	if since.Valid {
		t, _ := sqlitex.ParseTime(since.String)
		n.Connection.ConnectedSince = &t
	}
	return n, nil
}

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func getNode(ctx context.Context, q querier, id string) (contracts.Node, error) {
	n, err := scanNode(q.QueryRowContext(ctx, `SELECT `+nodeCols+` FROM fleet_nodes WHERE node_id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return n, contracts.Errorf(contracts.ErrNotFound, "node not found").WithRef(contracts.KindNode, id)
	}
	return n, err
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(b)
}

func (s *Store) emit(ctx context.Context, tx *sql.Tx, kind contracts.EventKind, n contracts.Node) error {
	if s.sink == nil {
		return nil
	}
	// Payload shapes are pinned in contracts/events.go: node.connection
	// carries NodeConnection; node.enrolled/status/revoked carry Node.
	var v any = n
	if kind == contracts.EvNodeConnection {
		v = n.Connection
	}
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.sink.AppendTx(ctx, tx, contracts.Event{
		EventID:        contracts.NewID(contracts.PrefixEvent),
		Origin:         "hub",
		Kind:           kind,
		Object:         contracts.ObjectRef{Kind: contracts.KindNode, ID: n.NodeID},
		PayloadVersion: 1,
		Payload:        payload,
		OccurredAt:     s.clock.Now(),
	})
	return err
}

// ---- enrollment hook ----

func (s *Store) registerNodeTx(ctx context.Context, tx *sql.Tx, e enrollment.NodeEnrollment) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO fleet_nodes(node_id, name, hostname, ssh_alias, platform, arch, enrolled_at, conn_state) VALUES(?,?,?,?,?,?,?,?)`,
		e.NodeID, e.Name, e.Hostname, e.SSHAlias, e.Platform, e.Arch, sqlitex.FormatTime(e.EnrolledAt), string(contracts.ConnNever)); err != nil {
		return err
	}
	n, err := getNode(ctx, tx, e.NodeID)
	if err != nil {
		return err
	}
	return s.emit(ctx, tx, contracts.EvNodeEnrolled, n)
}

// ---- FleetAPI ----

// ListNodes pages enrolled nodes in enrollment order. Any authenticated
// principal may discover hosts.
func (s *Store) ListNodes(ctx context.Context, req contracts.ListNodesRequest) (contracts.Page[contracts.Node], error) {
	page := contracts.Page[contracts.Node]{Items: []contracts.Node{}, SnapshotAt: s.clock.Now()}
	if _, err := principal(ctx); err != nil {
		return page, err
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 50
	}
	limit = min(limit, 200)
	q := `SELECT ` + nodeCols + ` FROM fleet_nodes WHERE node_id > ?`
	if !req.IncludeRevoked {
		q += ` AND conn_state != 'revoked'`
	}
	q += ` ORDER BY node_id LIMIT ?`
	rs, err := s.db.QueryContext(ctx, q, req.Cursor, limit+1)
	if err != nil {
		return page, err
	}
	defer rs.Close()
	for rs.Next() {
		n, err := scanNode(rs)
		if err != nil {
			return page, err
		}
		page.Items = append(page.Items, n)
	}
	if err := rs.Err(); err != nil {
		return page, err
	}
	// Merge redeemable enrollment tickets (state "never", labelled pending)
	// in node-ID order so keyset pagination stays exact.
	pend, err := s.enr.PendingTickets(ctx, req.Cursor, limit+1)
	if err != nil {
		return page, err
	}
	if len(pend) > 0 {
		for _, p := range pend {
			page.Items = append(page.Items, pendingNode(p))
		}
		sort.Slice(page.Items, func(i, j int) bool { return page.Items[i].NodeID < page.Items[j].NodeID })
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.NextCursor = page.Items[limit-1].NodeID
	}
	return page, nil
}

// GetNode returns one node (including revoked nodes, and nodes whose
// enrollment ticket is still unredeemed, in state "never").
func (s *Store) GetNode(ctx context.Context, req contracts.GetRequest) (contracts.Node, error) {
	if _, err := principal(ctx); err != nil {
		return contracts.Node{}, err
	}
	n, err := getNode(ctx, s.db, req.ID)
	if contracts.IsCode(err, contracts.ErrNotFound) {
		if p, ok, perr := s.enr.PendingTicket(ctx, s.db, req.ID); perr != nil {
			return n, perr
		} else if ok {
			return pendingNode(p), nil
		}
	}
	return n, err
}

// pendingNode presents a redeemable enrollment ticket as a node that has
// never connected.
func pendingNode(p enrollment.PendingTicket) contracts.Node {
	return contracts.Node{
		NodeID: p.NodeID, Name: p.Name, SSHAlias: p.SSHAlias,
		Harnesses: []contracts.HarnessInfo{}, Versions: map[string]string{}, Capabilities: []string{},
		Connection: contracts.NodeConnection{State: contracts.ConnNever},
		EnrolledAt: p.CreatedAt,
		Labels: map[string]string{
			contracts.NodeLabelEnrollment: "pending",
			"pyre.enrollment_expires_at":  p.ExpiresAt.UTC().Format(time.RFC3339),
		},
	}
}

// CreateEnrollment creates a one-time node enrollment ticket (owner only).
// A retry with the same operation_id returns the same node ID with a fresh
// secret (the original is never stored) as long as it is unredeemed.
func (s *Store) CreateEnrollment(ctx context.Context, req contracts.EnrollmentRequest) (contracts.EnrollmentTicket, error) {
	p, err := requireOwner(ctx)
	if err != nil {
		return contracts.EnrollmentTicket{}, err
	}
	var out contracts.EnrollmentTicket
	err = sqlitex.Tx(ctx, s.db, func(tx *sql.Tx) error {
		var stored contracts.EnrollmentTicket
		var secret string
		replayed, err := sqlitex.Idempotent(ctx, tx, req.OperationID, contracts.OpCreateEnrollment, p, req, &stored, func() (any, error) {
			t, err := s.enr.CreateTicketTx(ctx, tx, req, p)
			if err != nil {
				return nil, err
			}
			secret, t.Secret = t.Secret, "" // the operations table never holds the secret
			return t, nil
		})
		if err != nil {
			return err
		}
		if replayed {
			out, err = s.enr.RotateTicketSecretTx(ctx, tx, stored.NodeID)
			return err
		}
		stored.Secret = secret
		out = stored
		return nil
	})
	return out, err
}

// RevokeNode revokes a node's credentials, marks it revoked, emits
// node.revoked, and (after commit) notifies revocation listeners so its live
// connection is closed. Revoking a revoked node returns it unchanged.
func (s *Store) RevokeNode(ctx context.Context, req contracts.RevokeRequest) (contracts.Node, error) {
	p, err := requireOwner(ctx)
	if err != nil {
		return contracts.Node{}, err
	}
	var out contracts.Node
	err = sqlitex.Tx(ctx, s.db, func(tx *sql.Tx) error {
		_, err := sqlitex.Idempotent(ctx, tx, req.OperationID, contracts.OpRevokeNode, p, req, &out, func() (any, error) {
			n, err := getNode(ctx, tx, req.ID)
			if contracts.IsCode(err, contracts.ErrNotFound) {
				// A node that never redeemed its ticket: invalidate the
				// ticket so it can no longer enroll.
				pt, ok, perr := s.enr.PendingTicket(ctx, tx, req.ID)
				if perr != nil {
					return nil, perr
				}
				if ok {
					if _, ierr := s.enr.InvalidateTicketTx(ctx, tx, req.ID); ierr != nil {
						return nil, ierr
					}
					pn := pendingNode(pt)
					pn.Connection.State = contracts.ConnRevoked
					delete(pn.Labels, "pyre.enrollment_expires_at")
					pn.Labels[contracts.NodeLabelEnrollment] = "revoked"
					return pn, nil
				}
			}
			if err != nil {
				return nil, err
			}
			if n.Connection.State == contracts.ConnRevoked {
				return n, nil
			}
			now := sqlitex.FormatTime(s.clock.Now())
			if _, err := tx.ExecContext(ctx, `UPDATE fleet_nodes SET conn_state='revoked', revoked_at=?, revoked_reason=?, conn_id=NULL, connected_since=NULL,
				last_seen_at=CASE WHEN conn_state='connected' THEN ? ELSE last_seen_at END WHERE node_id=?`,
				now, req.Reason, now, req.ID); err != nil {
				return nil, err
			}
			if _, err := s.auth.RevokePrincipalTx(ctx, tx, contracts.PrincipalNode, req.ID); err != nil {
				return nil, err
			}
			if n, err = getNode(ctx, tx, req.ID); err != nil {
				return nil, err
			}
			return n, s.emit(ctx, tx, contracts.EvNodeRevoked, n)
		})
		return err
	})
	if err != nil {
		return contracts.Node{}, err
	}
	s.auth.NotifyRevoked(contracts.Principal{Kind: contracts.PrincipalNode, ID: req.ID})
	return out, nil
}

// CreatePairing creates a one-time owner-client pairing (owner only). A
// retry with the same operation_id rotates the code while unredeemed.
func (s *Store) CreatePairing(ctx context.Context, req contracts.PairingRequest) (contracts.PairingTicket, error) {
	p, err := requireOwner(ctx)
	if err != nil {
		return contracts.PairingTicket{}, err
	}
	var out contracts.PairingTicket
	err = sqlitex.Tx(ctx, s.db, func(tx *sql.Tx) error {
		var stored contracts.PairingTicket
		var code, uri string
		replayed, err := sqlitex.Idempotent(ctx, tx, req.OperationID, contracts.OpCreatePairing, p, req, &stored, func() (any, error) {
			t, err := s.enr.CreatePairingTx(ctx, tx, req, p)
			if err != nil {
				return nil, err
			}
			code, uri = t.Code, t.URI
			t.Code, t.URI = "", ""
			return t, nil
		})
		if err != nil {
			return err
		}
		if replayed {
			out, err = s.enr.RotatePairingCodeTx(ctx, tx, stored.PairingID)
			return err
		}
		stored.Code, stored.URI = code, uri
		out = stored
		return nil
	})
	return out, err
}

// ListClients pages paired owner clients (owner only).
func (s *Store) ListClients(ctx context.Context, req contracts.PageRequest) (contracts.Page[contracts.ClientInfo], error) {
	if _, err := requireOwner(ctx); err != nil {
		return contracts.Page[contracts.ClientInfo]{Items: []contracts.ClientInfo{}}, err
	}
	return s.enr.ListClients(ctx, req)
}

// RevokeClient revokes an owner client's credentials (owner only).
func (s *Store) RevokeClient(ctx context.Context, req contracts.RevokeRequest) (contracts.ClientInfo, error) {
	p, err := requireOwner(ctx)
	if err != nil {
		return contracts.ClientInfo{}, err
	}
	var out contracts.ClientInfo
	err = sqlitex.Tx(ctx, s.db, func(tx *sql.Tx) error {
		_, err := sqlitex.Idempotent(ctx, tx, req.OperationID, contracts.OpRevokeClient, p, req, &out, func() (any, error) {
			return s.enr.RevokeClientTx(ctx, tx, req.ID)
		})
		return err
	})
	if err != nil {
		return contracts.ClientInfo{}, err
	}
	s.auth.NotifyRevoked(contracts.Principal{Kind: contracts.PrincipalOwner, ID: req.ID})
	if c, err := s.enr.GetClient(ctx, req.ID); err == nil {
		out.LastSeenAt = c.LastSeenAt
	}
	return out, nil
}

// ---- connection observations (called by the hub's NodeServer glue) ----

// ObserveHello records an accepted node connection: it marks the node
// connected under conn.ConnID, refreshes descriptive fields from the hello,
// and emits node.connection. The same node_id always updates the same row,
// so reconnects never create duplicates. Unknown or revoked nodes are
// refused (the error is sent to the node).
func (s *Store) ObserveHello(ctx context.Context, nodeID string, conn transport.ConnInfo, hello contracts.NodeHello) (contracts.Node, error) {
	var out contracts.Node
	err := sqlitex.Tx(ctx, s.db, func(tx *sql.Tx) error {
		n, err := getNode(ctx, tx, nodeID)
		if contracts.IsCode(err, contracts.ErrNotFound) {
			return contracts.Errorf(contracts.ErrForbidden, "node %s is not enrolled with this hub; re-enroll it", nodeID)
		}
		if err != nil {
			return err
		}
		if n.Connection.State == contracts.ConnRevoked {
			return contracts.Errorf(contracts.ErrForbidden, "node %s has been revoked; re-enroll it", nodeID)
		}
		versions := maps.Clone(hello.Versions)
		if versions == nil {
			versions = map[string]string{}
		}
		if hello.PyredVersion != "" {
			versions["pyred"] = hello.PyredVersion
		}
		versions["protocol"] = strconv.Itoa(conn.Protocol)
		harnesses := hello.Harnesses
		if harnesses == nil {
			harnesses = []contracts.HarnessInfo{}
		}
		caps := hello.Capabilities
		if caps == nil {
			caps = []string{}
		}
		now := s.clock.Now()
		at := conn.ConnectedAt
		if at.IsZero() {
			at = now
		}
		hostname := hello.Hostname
		if hostname == "" {
			hostname = n.Hostname
		}
		platform, arch := hello.Platform, hello.Arch
		if platform == "" {
			platform = n.Platform
		}
		if arch == "" {
			arch = n.Arch
		}
		res := hello.Resources
		res.ObservedAt = now // hub clock; the node's clock may be skewed (see ObserveStatus)
		if _, err := tx.ExecContext(ctx, `UPDATE fleet_nodes SET hostname=?, platform=?, arch=?, harnesses=?, versions=?, capabilities=?, resources=?,
			conn_state='connected', conn_id=?, connected_since=?, last_seen_at=?, remote_addr=?, protocol=?, status_event_at=? WHERE node_id=?`,
			hostname, platform, arch, mustJSON(harnesses), mustJSON(versions), mustJSON(caps), mustJSON(res),
			conn.ConnID, sqlitex.FormatTime(at), sqlitex.FormatTime(now), conn.RemoteAddr, conn.Protocol, sqlitex.FormatTime(now), nodeID); err != nil {
			return err
		}
		if out, err = getNode(ctx, tx, nodeID); err != nil {
			return err
		}
		return s.emit(ctx, tx, contracts.EvNodeConnection, out)
	})
	return out, err
}

// ObserveDisconnect marks the node disconnected with lastSeen, but only if
// connID is still its current connection (a replaced connection's late
// disconnect is ignored). It reports whether the state changed.
func (s *Store) ObserveDisconnect(ctx context.Context, nodeID, connID string, lastSeen time.Time) (bool, error) {
	changed := false
	err := sqlitex.Tx(ctx, s.db, func(tx *sql.Tx) error {
		if lastSeen.IsZero() {
			lastSeen = s.clock.Now()
		}
		res, err := tx.ExecContext(ctx, `UPDATE fleet_nodes SET conn_state='disconnected', conn_id=NULL, connected_since=NULL, last_seen_at=?
			WHERE node_id=? AND conn_id=? AND conn_state='connected'`, sqlitex.FormatTime(lastSeen), nodeID, connID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		changed = true
		n, err := getNode(ctx, tx, nodeID)
		if err != nil {
			return err
		}
		return s.emit(ctx, tx, contracts.EvNodeConnection, n)
	})
	return changed, err
}

// ObserveStatus applies a replaceable status report (resources, and
// harnesses/versions when present) and refreshes last_seen_at. node.status
// is emitted when harnesses/versions change or StatusEventInterval passed.
func (s *Store) ObserveStatus(ctx context.Context, nodeID string, st contracts.NodeStatusReport) error {
	return sqlitex.Tx(ctx, s.db, func(tx *sql.Tx) error {
		n, err := getNode(ctx, tx, nodeID)
		if err != nil {
			return err
		}
		if n.Connection.State == contracts.ConnRevoked {
			return contracts.Errorf(contracts.ErrForbidden, "node %s has been revoked", nodeID)
		}
		var lastEvent sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT status_event_at FROM fleet_nodes WHERE node_id=?`, nodeID).Scan(&lastEvent); err != nil {
			return err
		}
		changed := false
		harnesses, versions := n.Harnesses, n.Versions
		if st.Harnesses != nil && !slices.EqualFunc(st.Harnesses, n.Harnesses, harnessEqual) {
			harnesses, changed = st.Harnesses, true
		}
		if st.Versions != nil {
			merged := maps.Clone(n.Versions)
			maps.Copy(merged, st.Versions)
			if !maps.Equal(merged, n.Versions) {
				versions, changed = merged, true
			}
		}
		now := s.clock.Now()
		res := st.Resources
		// Status reports are sent as they are sampled and never queued, so
		// the hub's receive time is the observation time. The node's own
		// timestamp is not trusted: with node clock skew it would make the
		// node look fresher (or older) than it is and keep a silent node
		// from ever turning stale.
		res.ObservedAt = now
		last, _ := sqlitex.ParseTime(lastEvent.String)
		emit := changed || !lastEvent.Valid || now.Sub(last) >= s.StatusEventInterval
		eventAt := lastEvent
		if emit {
			eventAt = sql.NullString{String: sqlitex.FormatTime(now), Valid: true}
		}
		seen := n.Connection.LastSeenAt
		lastSeen := sqlitex.FormatTime(now)
		if n.Connection.State != contracts.ConnConnected && seen != nil {
			lastSeen = sqlitex.FormatTime(*seen) // a late report does not claim liveness
		}
		if _, err := tx.ExecContext(ctx, `UPDATE fleet_nodes SET resources=?, harnesses=?, versions=?, last_seen_at=?, status_event_at=? WHERE node_id=?`,
			mustJSON(res), mustJSON(harnesses), mustJSON(versions), lastSeen, eventAt, nodeID); err != nil {
			return err
		}
		if !emit {
			return nil
		}
		n, err = getNode(ctx, tx, nodeID)
		if err != nil {
			return err
		}
		return s.emit(ctx, tx, contracts.EvNodeStatus, n)
	})
}

func harnessEqual(a, b contracts.HarnessInfo) bool {
	return a.Harness == b.Harness && a.Version == b.Version && a.Available == b.Available &&
		a.Executable == b.Executable && a.Reason == b.Reason && slices.Equal(a.Capabilities, b.Capabilities)
}

// ObserveSeen refreshes last_seen_at for a connected node (no event).
func (s *Store) ObserveSeen(ctx context.Context, nodeID string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE fleet_nodes SET last_seen_at=? WHERE node_id=? AND conn_state='connected'`, sqlitex.FormatTime(at), nodeID)
	return err
}

// ResetConnections marks every node recorded as connected as disconnected
// (keeping its last_seen_at). Call once at hub startup, before serving: no
// connection survives a hub restart, so "connected" rows are stale.
func (s *Store) ResetConnections(ctx context.Context) (int, error) {
	count := 0
	err := sqlitex.Tx(ctx, s.db, func(tx *sql.Tx) error {
		rs, err := tx.QueryContext(ctx, `SELECT node_id FROM fleet_nodes WHERE conn_state='connected'`)
		if err != nil {
			return err
		}
		var ids []string
		for rs.Next() {
			var id string
			if err := rs.Scan(&id); err != nil {
				rs.Close()
				return err
			}
			ids = append(ids, id)
		}
		rs.Close()
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `UPDATE fleet_nodes SET conn_state='disconnected', conn_id=NULL, connected_since=NULL,
				last_seen_at=COALESCE(last_seen_at, ?) WHERE node_id=?`, sqlitex.FormatTime(s.clock.Now()), id); err != nil {
				return err
			}
			n, err := getNode(ctx, tx, id)
			if err != nil {
				return err
			}
			if err := s.emit(ctx, tx, contracts.EvNodeConnection, n); err != nil {
				return err
			}
			count++
		}
		return nil
	})
	return count, err
}

// ---- freshness ----

// StaleAfter is how old a connected node's latest observation may be before
// NodeFreshness reports it stale.
const StaleAfter = 90 * time.Second

// NodeFreshness describes how current a node's observation is. Anything but
// a recently observed connected node is stale; stale never implies idle.
func NodeFreshness(n contracts.Node, now time.Time) contracts.Freshness {
	return NodeFreshnessWithin(n, now, StaleAfter)
}

// NodeFreshnessWithin is NodeFreshness with an explicit threshold.
func NodeFreshnessWithin(n contracts.Node, now time.Time, staleAfter time.Duration) contracts.Freshness {
	f := contracts.Freshness{Source: "node:" + n.NodeID}
	c := n.Connection
	latest := n.EnrolledAt
	for _, t := range []*time.Time{c.LastSeenAt, c.ConnectedSince, &n.Resources.ObservedAt} {
		if t != nil && t.After(latest) {
			latest = *t
		}
	}
	if latest.After(now) {
		latest = now // never report an observation from the future
	}
	if c.State == contracts.ConnDisconnected && c.LastSeenAt != nil {
		latest = *c.LastSeenAt // the last frame received, not a late report
	}
	f.ObservedAt = latest
	switch c.State {
	case contracts.ConnRevoked:
		f.Stale, f.Reason = true, "node revoked"
	case contracts.ConnNever:
		f.Stale, f.Reason = true, "enrolled but never connected"
	case contracts.ConnDisconnected:
		f.Stale = true
		if c.LastSeenAt != nil {
			f.Reason = fmt.Sprintf("disconnected; last seen %s (%s ago)", c.LastSeenAt.UTC().Format(time.RFC3339), now.Sub(*c.LastSeenAt).Round(time.Second))
		} else {
			f.Reason = "disconnected"
		}
	case contracts.ConnConnected:
		if age := now.Sub(latest); age > staleAfter {
			f.Stale, f.Reason = true, fmt.Sprintf("connected but no observation for %s", age.Round(time.Second))
		}
	default:
		f.Stale, f.Reason = true, "unknown connection state"
	}
	return f
}
