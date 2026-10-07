// Package refhub is a reference hub-side node channel for resilience tests:
// the merged WP04 pieces (transport.AuthStore, enrollment.Store,
// topology.Store, transport.NodeServer) composed over a real on-disk SQLite
// database plus a minimal durable report/command store standing in for the
// WP05 hub core (internal/hub, not merged yet).
//
// It implements NodeHub, the interface resilience scenarios drive, so the
// real hub can replace it once WP05 lands (see tests/resilience/README.md).
// Kill simulates a crash: live TCP connections are reset, no disconnect is
// recorded, and the database is closed without a graceful drain; Restart
// reopens the same files on the same address and runs startup recovery
// (topology.ResetConnections), as a restarted hub process would.
package refhub

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/enrollment"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
	"github.com/jhh3/pyre/internal/topology"
	"github.com/jhh3/pyre/internal/transport"
)

// NodeHub is the hub-side surface resilience scenarios need. Implementations:
// *RefHub (now); a wrapper over internal/hub + cmd/pyre-hub (WP05, pending).
type NodeHub interface {
	// URL is the base URL a node dials (http:// so frame faults can be
	// injected; TLS behaviour is covered by tests/security).
	URL() string
	// Addr is the listen address (host:port).
	Addr() string
	// EnrollNode creates and redeems an enrollment ticket, returning the
	// node ID and its node credential.
	EnrollNode(ctx context.Context, name string) (nodeID, token string, err error)
	// QueueCommand durably queues a command (next per-node Seq) and sends
	// it if the node is connected.
	QueueCommand(ctx context.Context, nodeID string, kind contracts.CommandKind, payload any, expires *time.Time) (contracts.NodeCommand, error)
	// Reports returns durably committed reports in origin_seq order.
	Reports(ctx context.Context, nodeID string) ([]contracts.NodeReport, error)
	// CommandResults returns the committed result per command ID.
	CommandResults(ctx context.Context, nodeID string) (map[string]contracts.CommandResult, error)
	// Node returns the registry record.
	Node(ctx context.Context, nodeID string) (contracts.Node, error)
	// Nodes lists every registry record (duplicate detection).
	Nodes(ctx context.Context) ([]contracts.Node, error)
	// Connected reports a live control connection.
	Connected(nodeID string) bool
	// Counters exposes delivery statistics since the last (re)start.
	Counters() Counters
	// Kill crashes the hub; Restart brings it back on the same address.
	Kill()
	Restart() error
	// Close shuts down gracefully.
	Close() error
}

// Counters count what the hub saw since its last (re)start.
type Counters struct {
	Hellos           int64
	DurableReports   int64 // OnReport calls with OriginSeq > 0 (incl. duplicates)
	DuplicateReports int64 // of those, already committed
	StatusReports    int64
	CommandAcks      int64
	DuplicateResults int64 // command_result reports for an already-resulted command
	Disconnects      int64
}

// Options configure a RefHub.
type Options struct {
	Dir               string        // data directory (hub.db); required
	Addr              string        // listen address; default 127.0.0.1:0 (kept across restarts)
	HeartbeatInterval time.Duration // default 200ms
	Logger            *slog.Logger  // default discard
	// FailReports, if set, is consulted before committing each durable
	// report; returning an error makes the commit fail (the connection is
	// closed with unavailable and the node must resend).
	FailReports func(nodeID string, r contracts.NodeReport) error
}

// RefHub is the reference implementation.
type RefHub struct {
	opts Options

	mu    sync.Mutex
	db    *sql.DB
	auth  *transport.AuthStore
	enr   *enrollment.Store
	fleet *topology.Store
	ns    *transport.NodeServer
	srv   *http.Server
	ln    *trackingListener
	addr  string
	dead  atomic.Bool

	hellos, durable, dups, statuses, acks, dupResults, disconnects atomic.Int64
}

var _ NodeHub = (*RefHub)(nil)

// Owner is the synthetic owner principal used for enrollment.
var Owner = contracts.Principal{Kind: contracts.PrincipalOwner, ID: "cli_resilience-owner", Name: "resilience owner (synthetic)"}

// Migrations are the reference store's own tables (namespace "refhub").
var Migrations = []sqlitex.Migration{{Namespace: "refhub", Version: 1, Name: "reports_commands", SQL: `
CREATE TABLE refhub_reports (
	node_id    TEXT NOT NULL,
	origin_seq INTEGER NOT NULL,
	kind       TEXT NOT NULL,
	at         TEXT NOT NULL,
	payload    BLOB,
	PRIMARY KEY (node_id, origin_seq)
);
CREATE TABLE refhub_commands (
	node_id    TEXT NOT NULL,
	seq        INTEGER NOT NULL,
	command_id TEXT NOT NULL UNIQUE,
	command    BLOB NOT NULL,
	acked_at   TEXT,
	result     BLOB,
	PRIMARY KEY (node_id, seq)
);
CREATE TABLE refhub_events (cursor INTEGER PRIMARY KEY AUTOINCREMENT, kind TEXT NOT NULL, body BLOB NOT NULL);
`}}

type sink struct{}

func (sink) AppendTx(ctx context.Context, tx *sql.Tx, ev contracts.Event) (contracts.Event, error) {
	b, err := json.Marshal(ev)
	if err != nil {
		return ev, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO refhub_events(kind, body) VALUES(?,?)`, string(ev.Kind), b)
	if err != nil {
		return ev, err
	}
	ev.Cursor, _ = res.LastInsertId()
	return ev, nil
}

// New opens (or creates) the hub in opts.Dir and starts serving.
func New(opts Options) (*RefHub, error) {
	if opts.Dir == "" {
		return nil, errors.New("refhub: Dir is required")
	}
	if opts.Addr == "" {
		opts.Addr = "127.0.0.1:0"
	}
	if opts.HeartbeatInterval <= 0 {
		opts.HeartbeatInterval = 200 * time.Millisecond
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	h := &RefHub{opts: opts, addr: opts.Addr}
	if err := h.start(); err != nil {
		return nil, err
	}
	return h, nil
}

// DBPath is the hub database path.
func (h *RefHub) DBPath() string { return filepath.Join(h.opts.Dir, "hub.db") }

// DB returns the open database (nil while killed).
func (h *RefHub) DB() *sql.DB {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.db
}

// Auth returns the credential store.
func (h *RefHub) Auth() *transport.AuthStore {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.auth
}

// Fleet returns the node registry.
func (h *RefHub) Fleet() *topology.Store {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.fleet
}

func (h *RefHub) start() error {
	ctx := context.Background()
	db, err := sqlitex.Open(h.DBPath())
	if err != nil {
		return err
	}
	if err := sqlitex.Migrate(ctx, db, Migrations...); err != nil {
		db.Close()
		return err
	}
	auth, err := transport.NewAuthStore(db, nil)
	if err != nil {
		db.Close()
		return err
	}
	enr, err := enrollment.NewStore(db, auth, nil, enrollment.Options{HubURL: "https://" + h.addr, RateLimit: enrollment.RateLimitConfig{Disabled: true}})
	if err != nil {
		db.Close()
		return err
	}
	fleet, err := topology.NewStore(db, auth, enr, sink{}, nil)
	if err != nil {
		db.Close()
		return err
	}
	if _, err := fleet.ResetConnections(ctx); err != nil {
		db.Close()
		return err
	}
	cfg, err := fleet.NodeServerConfig(topology.HubHooks{
		Welcome:      h.welcome,
		OnReport:     h.onReport,
		OnCommandAck: h.onAck,
		OnDisconnect: func(context.Context, string, transport.DisconnectInfo) { h.disconnects.Add(1) },
	})
	if err != nil {
		db.Close()
		return err
	}
	// While "dead" (between Kill and Restart) hooks must not touch storage:
	// a crashed process records nothing.
	inner := cfg.OnDisconnect
	cfg.OnDisconnect = func(ctx context.Context, nodeID string, info transport.DisconnectInfo) {
		if h.dead.Load() {
			return
		}
		inner(ctx, nodeID, info)
	}
	cfg.Auth = auth
	cfg.HubID = "hub_refhub"
	cfg.HeartbeatInterval = h.opts.HeartbeatInterval
	cfg.Logger = h.opts.Logger
	ns, err := transport.NewNodeServer(cfg)
	if err != nil {
		db.Close()
		return err
	}
	ns.DisconnectOnRevoke(auth)
	mux := http.NewServeMux()
	mux.Handle(contracts.PathNodeConnect, ns)
	enr.Register(mux, enrollment.HandlerOptions{Logger: h.opts.Logger})
	var raw net.Listener
	deadline := time.Now().Add(10 * time.Second)
	for {
		raw, err = net.Listen("tcp", h.addr)
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		ns.Close()
		db.Close()
		return fmt.Errorf("refhub: listen %s: %w", h.addr, err)
	}
	ln := &trackingListener{Listener: raw, conns: map[net.Conn]struct{}{}}
	srv := &http.Server{Handler: mux, ErrorLog: log.New(io.Discard, "", 0), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	h.mu.Lock()
	h.db, h.auth, h.enr, h.fleet, h.ns, h.srv, h.ln = db, auth, enr, fleet, ns, srv, ln
	h.addr = raw.Addr().String()
	h.mu.Unlock()
	h.dead.Store(false)
	h.resetCounters()
	return nil
}

func (h *RefHub) resetCounters() {
	for _, c := range []*atomic.Int64{&h.hellos, &h.durable, &h.dups, &h.statuses, &h.acks, &h.dupResults, &h.disconnects} {
		c.Store(0)
	}
}

// URL implements NodeHub.
func (h *RefHub) URL() string { return "http://" + h.Addr() }

// Addr implements NodeHub.
func (h *RefHub) Addr() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.addr
}

// Counters implements NodeHub.
func (h *RefHub) Counters() Counters {
	return Counters{Hellos: h.hellos.Load(), DurableReports: h.durable.Load(), DuplicateReports: h.dups.Load(),
		StatusReports: h.statuses.Load(), CommandAcks: h.acks.Load(), DuplicateResults: h.dupResults.Load(), Disconnects: h.disconnects.Load()}
}

func (h *RefHub) welcome(ctx context.Context, nodeID string, hello contracts.NodeHello) (transport.WelcomeInfo, error) {
	h.hellos.Add(1)
	db := h.DB()
	var committed sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT MAX(origin_seq) FROM refhub_reports WHERE node_id=?`, nodeID).Scan(&committed); err != nil {
		return transport.WelcomeInfo{}, contracts.Errorf(contracts.ErrUnavailable, "hub store: %v", err)
	}
	// Like internal/hub: resend every command the node has not acknowledged,
	// whatever its seq (commands can arrive out of order, so LastCommandSeq
	// may cover a lost lower command); the node dedupes by command_id.
	rows, err := db.QueryContext(ctx, `SELECT command FROM refhub_commands WHERE node_id=? AND acked_at IS NULL ORDER BY seq`, nodeID)
	if err != nil {
		return transport.WelcomeInfo{}, contracts.Errorf(contracts.ErrUnavailable, "hub store: %v", err)
	}
	defer rows.Close()
	var pending []contracts.NodeCommand
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return transport.WelcomeInfo{}, err
		}
		var cmd contracts.NodeCommand
		if err := json.Unmarshal(raw, &cmd); err != nil {
			return transport.WelcomeInfo{}, err
		}
		pending = append(pending, cmd)
	}
	return transport.WelcomeInfo{CommittedReportSeq: uint64(committed.Int64), PendingCommands: pending}, rows.Err()
}

func (h *RefHub) onReport(ctx context.Context, nodeID string, r contracts.NodeReport) error {
	if r.OriginSeq == 0 {
		h.statuses.Add(1)
		return nil
	}
	h.durable.Add(1)
	if h.dead.Load() {
		return contracts.Errorf(contracts.ErrUnavailable, "hub is down")
	}
	if f := h.opts.FailReports; f != nil {
		if err := f(nodeID, r); err != nil {
			return err
		}
	}
	db := h.DB()
	return sqlitex.Tx(ctx, db, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO refhub_reports(node_id, origin_seq, kind, at, payload) VALUES(?,?,?,?,?)`,
			nodeID, int64(r.OriginSeq), string(r.Kind), sqlitex.FormatTime(r.At), []byte(r.Payload))
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			h.dups.Add(1)
			return nil
		}
		if r.Kind == contracts.ReportCommandResult {
			var cr contracts.CommandResult
			if err := json.Unmarshal(r.Payload, &cr); err != nil {
				return contracts.Errorf(contracts.ErrInvalid, "malformed command result")
			}
			res, err := tx.ExecContext(ctx, `UPDATE refhub_commands SET result=? WHERE node_id=? AND command_id=? AND result IS NULL`, []byte(r.Payload), nodeID, cr.CommandID)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				h.dupResults.Add(1)
			}
		}
		return nil
	})
}

func (h *RefHub) onAck(ctx context.Context, nodeID string, a contracts.CommandAck) error {
	h.acks.Add(1)
	if h.dead.Load() {
		return nil
	}
	_, err := h.DB().ExecContext(ctx, `UPDATE refhub_commands SET acked_at=COALESCE(acked_at, ?) WHERE node_id=? AND command_id=?`,
		sqlitex.FormatTime(time.Now()), nodeID, a.CommandID)
	return err
}

// EnrollNode implements NodeHub.
func (h *RefHub) EnrollNode(ctx context.Context, name string) (string, string, error) {
	h.mu.Lock()
	fleet, enr := h.fleet, h.enr
	h.mu.Unlock()
	octx := contracts.WithPrincipal(ctx, Owner)
	t, err := fleet.CreateEnrollment(octx, contracts.EnrollmentRequest{Mutation: contracts.Mutation{OperationID: contracts.NewID(contracts.PrefixOperation)}, Name: name})
	if err != nil {
		return "", "", err
	}
	cred, err := enr.RedeemNode(ctx, contracts.NodeEnrollExchange{NodeID: t.NodeID, Secret: t.Secret, Hostname: name + ".test", Platform: "linux", Arch: "amd64"})
	if err != nil {
		return "", "", err
	}
	return cred.NodeID, cred.Token, nil
}

// QueueCommand implements NodeHub.
func (h *RefHub) QueueCommand(ctx context.Context, nodeID string, kind contracts.CommandKind, payload any, expires *time.Time) (contracts.NodeCommand, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return contracts.NodeCommand{}, err
	}
	cmd := contracts.NodeCommand{CommandID: contracts.NewID(contracts.PrefixCommand), Kind: kind, Actor: Owner, IssuedAt: time.Now().UTC(), ExpiresAt: expires, Payload: raw}
	db := h.DB()
	err = sqlitex.Tx(ctx, db, func(tx *sql.Tx) error {
		var max sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT MAX(seq) FROM refhub_commands WHERE node_id=?`, nodeID).Scan(&max); err != nil {
			return err
		}
		cmd.Seq = max.Int64 + 1
		b, err := json.Marshal(cmd)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO refhub_commands(node_id, seq, command_id, command) VALUES(?,?,?,?)`, nodeID, cmd.Seq, cmd.CommandID, b)
		return err
	})
	if err != nil {
		return cmd, err
	}
	h.mu.Lock()
	ns := h.ns
	h.mu.Unlock()
	_ = ns.Send(ctx, nodeID, cmd) // unavailable = stays queued for the next hello
	return cmd, nil
}

// Reports implements NodeHub.
func (h *RefHub) Reports(ctx context.Context, nodeID string) ([]contracts.NodeReport, error) {
	rows, err := h.DB().QueryContext(ctx, `SELECT origin_seq, kind, at, payload FROM refhub_reports WHERE node_id=? ORDER BY origin_seq`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []contracts.NodeReport
	for rows.Next() {
		var r contracts.NodeReport
		var seq int64
		var kind, at string
		var payload []byte
		if err := rows.Scan(&seq, &kind, &at, &payload); err != nil {
			return nil, err
		}
		r.OriginSeq, r.Kind, r.Payload = uint64(seq), contracts.ReportKind(kind), payload
		r.At, _ = sqlitex.ParseTime(at)
		out = append(out, r)
	}
	return out, rows.Err()
}

// CommandResults implements NodeHub.
func (h *RefHub) CommandResults(ctx context.Context, nodeID string) (map[string]contracts.CommandResult, error) {
	rows, err := h.DB().QueryContext(ctx, `SELECT command_id, result FROM refhub_commands WHERE node_id=? AND result IS NOT NULL`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]contracts.CommandResult{}
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		var cr contracts.CommandResult
		if err := json.Unmarshal(raw, &cr); err != nil {
			return nil, err
		}
		out[id] = cr
	}
	return out, rows.Err()
}

// AckedCommands returns how many commands the node acknowledged durably.
func (h *RefHub) AckedCommands(ctx context.Context, nodeID string) (int, error) {
	var n int
	err := h.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM refhub_commands WHERE node_id=? AND acked_at IS NOT NULL`, nodeID).Scan(&n)
	return n, err
}

// Events returns committed registry events of the given kind.
func (h *RefHub) Events(ctx context.Context, kind contracts.EventKind) ([]contracts.Event, error) {
	rows, err := h.DB().QueryContext(ctx, `SELECT cursor, body FROM refhub_events WHERE kind=? ORDER BY cursor`, string(kind))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []contracts.Event
	for rows.Next() {
		var c int64
		var b []byte
		if err := rows.Scan(&c, &b); err != nil {
			return nil, err
		}
		var ev contracts.Event
		if err := json.Unmarshal(b, &ev); err != nil {
			return nil, err
		}
		ev.Cursor = c
		out = append(out, ev)
	}
	return out, rows.Err()
}

// Node implements NodeHub.
func (h *RefHub) Node(ctx context.Context, nodeID string) (contracts.Node, error) {
	return h.Fleet().GetNode(contracts.WithPrincipal(ctx, Owner), contracts.GetRequest{ID: nodeID})
}

// Nodes implements NodeHub.
func (h *RefHub) Nodes(ctx context.Context) ([]contracts.Node, error) {
	p, err := h.Fleet().ListNodes(contracts.WithPrincipal(ctx, Owner), contracts.ListNodesRequest{IncludeRevoked: true, PageRequest: contracts.PageRequest{Limit: 500}})
	return p.Items, err
}

// Connected implements NodeHub.
func (h *RefHub) Connected(nodeID string) bool {
	h.mu.Lock()
	ns := h.ns
	h.mu.Unlock()
	return ns != nil && ns.Connected(nodeID)
}

// Kill implements NodeHub: reset every TCP connection, then tear down
// without recording anything.
func (h *RefHub) Kill() {
	h.dead.Store(true)
	h.mu.Lock()
	srv, ns, ln, db := h.srv, h.ns, h.ln, h.db
	h.mu.Unlock()
	_ = ln.Close()
	ln.resetAll()
	_ = srv.Close()
	_ = ns.Close()
	_ = db.Close()
}

// Restart implements NodeHub.
func (h *RefHub) Restart() error { return h.start() }

// Close implements NodeHub (graceful).
func (h *RefHub) Close() error {
	h.mu.Lock()
	srv, ns, db := h.srv, h.ns, h.db
	h.mu.Unlock()
	if h.dead.Load() {
		return nil
	}
	_ = ns.Close()
	_ = srv.Close()
	h.dead.Store(true)
	return db.Close()
}

// trackingListener remembers accepted connections so Kill can reset them
// (http.Server.Close does not close hijacked WebSocket connections).
type trackingListener struct {
	net.Listener
	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

func (l *trackingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	l.conns[c] = struct{}{}
	l.mu.Unlock()
	return c, nil
}

func (l *trackingListener) resetAll() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for c := range l.conns {
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.SetLinger(0)
		}
		_ = c.Close()
	}
	l.conns = map[net.Conn]struct{}{}
}
