// Package hub is the Pyre hub core (WP05). *Hub implements the complete
// contracts.API over one SQLite database:
//
//   - in-hub: system, sessions/runs, leases, approvals, messages (via
//     internal/messaging/store), accounts, events (internal/events), node
//     action routing and the durable node command queue;
//   - delegated: fleet, projects, documents, artifacts, reviews, notes and
//     attention are supplied by other packages through Config; a nil domain
//     returns ErrUnsupported ("not configured").
//
// The node transport (WP04) feeds node frames into the hub through the
// NodeLink methods (HandleHello, HandleReport, HandleCommandAck,
// HandleDisconnect, ResumeNode) and delivers hub commands with
// contracts.NodeChannel.
package hub

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jhh3/pyre/internal/buildinfo"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/events"
	"github.com/jhh3/pyre/internal/messaging/store"
	"github.com/jhh3/pyre/internal/persistence"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// Config composes the hub.
type Config struct {
	DB      *sql.DB
	DataDir string
	Clock   contracts.Clock
	// Events is the event log; nil creates one on DB with Options.EventRetention.
	Events *events.Log

	// Pluggable domains (nil => ErrUnsupported "not configured").
	Fleet     contracts.FleetAPI
	Projects  contracts.ProjectAPI
	Documents contracts.DocumentAPI
	Artifacts contracts.ArtifactAPI
	Reviews   contracts.ReviewAPI
	Notes     contracts.NoteAPI
	Attention contracts.AttentionAPI
	// Connections is the Outfit connection catalog (internal/connections).
	// When set, ListConnections delegates to it and connection reports are
	// applied to it; otherwise the hub merges reported catalogs itself.
	Connections ConnectionCatalog
	// ConnectionGrants decides use-time connection grants against the
	// catalog (internal/connections.Catalog.CheckGrant); the hub supplies
	// the authoritative session context. Nil: CheckConnectionGrant is
	// unsupported.
	ConnectionGrants func(ctx context.Context, connectionID string, s GrantSession) (contracts.GrantDecision, error)
	// RouteConnectionInvoke picks the executing node for a connection.invoke
	// sent without node_id (internal/connections.Catalog.RouteInvoke): it
	// checks the grant and returns a node with verified remote_invoke
	// availability. Nil: node_id stays required.
	RouteConnectionInvoke func(ctx context.Context, connectionID string, s GrantSession) (string, error)
	// RecordConnectionGrant records an owner's connection.grant (active)
	// or connection.revoke in the catalog before the action is pushed to
	// the node, so hub-side use-time checks see it. opID is derived from the
	// action's operation ID. Nil: grants are only pushed to nodes.
	RecordConnectionGrant func(ctx context.Context, p contracts.Principal, opID string, g contracts.ConnectionGrantParams, active bool) error

	// Tokens mints session tokens and one-time tickets (nil: none minted).
	Tokens contracts.TokenIssuer
	// Nodes sends commands to connected nodes (nil: commands stay queued and
	// node connectivity is taken from HandleHello/HandleDisconnect).
	Nodes contracts.NodeChannel
	// Terminal is the web-terminal relay (internal/terminal). When set,
	// sessions.attach preferring web_terminal turns a node's tmux attach
	// into a relayed web terminal.
	Terminal TerminalRelay

	Options Options
}

// ConnectionCatalog is implemented by internal/connections.Catalog.
type ConnectionCatalog interface {
	List(ctx context.Context, req contracts.ListConnectionsRequest) (contracts.Page[contracts.Connection], error)
	ApplyReport(ctx context.Context, nodeID string, conns []contracts.Connection) error
}

// Options tunes limits and timeouts. Zero values select defaults.
type Options struct {
	MessageLimits  store.Limits
	EventRetention *events.Retention // nil => events.DefaultRetention
	AuditRetention *AuditRetention   // nil => DefaultAuditRetention (90 days / 100k entries)

	CommandWait     time.Duration // synchronous wait for attach/read/action results (10s)
	ControlWait     time.Duration // brief wait for interrupt/stop/answer receipts (2s)
	StartCommandTTL time.Duration // start/resume commands expire if undelivered (15m)
	ActionTTL       time.Duration // node actions expire if undelivered (10m)
	InterruptTTL    time.Duration // interrupt commands expire if undelivered (2m)
	StopTTL         time.Duration // stop commands expire if undelivered (30m)

	MaxPendingCommandsPerNode int // durable command backlog per node (10000)

	DefaultLeaseTTL time.Duration // 120s
	MaxLeaseTTL     time.Duration // 1h

	AccountStaleAfter time.Duration // observations older than this are stale (30m)
	HeartbeatSeconds  int           // advertised to nodes (15)

	MaintenanceInterval time.Duration // background expiry/prune/flush cadence (10s)

	// CommandRetention keeps finished node commands (done, expired,
	// cancelled) this long after completion (7 days); OperationRetention
	// keeps operation-idempotency records (30 days): a retry of a mutation
	// older than that executes again. Both are pruned at most every
	// PruneInterval (1h).
	CommandRetention   time.Duration
	OperationRetention time.Duration
	PruneInterval      time.Duration
	PollInterval       time.Duration // long-poll fallback cadence (250ms)

	HubVersion string // default buildinfo.Version
}

func (o *Options) defaults() {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&o.CommandWait, 10*time.Second)
	def(&o.ControlWait, 2*time.Second)
	def(&o.StartCommandTTL, 15*time.Minute)
	def(&o.ActionTTL, 10*time.Minute)
	def(&o.InterruptTTL, 2*time.Minute)
	def(&o.StopTTL, 30*time.Minute)
	def(&o.DefaultLeaseTTL, 120*time.Second)
	def(&o.MaxLeaseTTL, time.Hour)
	def(&o.AccountStaleAfter, 30*time.Minute)
	def(&o.MaintenanceInterval, 10*time.Second)
	def(&o.CommandRetention, 7*24*time.Hour)
	def(&o.OperationRetention, 30*24*time.Hour)
	def(&o.PruneInterval, time.Hour)
	def(&o.PollInterval, 250*time.Millisecond)
	if o.MaxPendingCommandsPerNode <= 0 {
		o.MaxPendingCommandsPerNode = 10_000
	}
	if o.HeartbeatSeconds <= 0 {
		o.HeartbeatSeconds = 15
	}
	if o.HubVersion == "" {
		o.HubVersion = buildinfo.Version
	}
}

// Hub is the hub core. It implements contracts.API.
type Hub struct {
	cfg    Config
	db     *sql.DB
	clock  contracts.Clock
	opt    Options
	events *events.Log
	msgs   *store.Store
	hubID  string

	cmdBC persistence.Broadcaster // command results

	dirtyMu sync.Mutex
	dirty   map[string]bool // nodes with newly queued commands

	nodeMu    sync.Mutex
	nodeLocks map[string]*sync.Mutex

	pruneMu   sync.Mutex
	lastPrune time.Time

	stopOnce sync.Once
	stop     chan struct{}
	wg       sync.WaitGroup

	tokens tokenVault // account tokens (accounts_token.go)

	// Automatic reports to parent sessions (childreport.go).
	reportKick   atomic.Bool   // a committed write may have recorded a report
	reportWake   chan struct{} // wakes the report worker
	reportMu     sync.Mutex    // one report pass at a time
	childSummary func(ctx context.Context, sessionID, runID string) (string, error)
	engage       engageState // owner engagement updates (attention_flags.go)
}

var _ contracts.API = (*Hub)(nil)

// New migrates the hub namespaces and returns a Hub. Call Start to run
// background maintenance and Close to stop it (Close does not close DB).
func New(cfg Config) (*Hub, error) {
	if cfg.DB == nil {
		return nil, errors.New("hub: Config.DB is required")
	}
	if cfg.Clock == nil {
		cfg.Clock = contracts.SystemClock{}
	}
	cfg.Options.defaults()
	ctx := context.Background()
	if err := sqlitex.Migrate(ctx, cfg.DB, sqlitex.OperationsMigration); err != nil {
		return nil, err
	}
	if err := sqlitex.Migrate(ctx, cfg.DB, Migrations...); err != nil {
		return nil, err
	}
	if err := sqlitex.Migrate(ctx, cfg.DB, tokenMigrations...); err != nil {
		return nil, err
	}
	if err := sqlitex.Migrate(ctx, cfg.DB, repoMigrations...); err != nil { // W-quicklaunch
		return nil, err
	}
	if err := sqlitex.Migrate(ctx, cfg.DB, childReportMigrations...); err != nil { // W-childnotify
		return nil, err
	}
	if err := sqlitex.Migrate(ctx, cfg.DB, auditMigrations...); err != nil { // W-audit
		return nil, err
	}
	h := &Hub{
		cfg: cfg, db: cfg.DB, clock: cfg.Clock, opt: cfg.Options,
		dirty: map[string]bool{}, nodeLocks: map[string]*sync.Mutex{}, stop: make(chan struct{}), reportWake: make(chan struct{}, 1),
		tokens: tokenVault{dir: cfg.DataDir},
	}
	if cfg.Events != nil {
		h.events = cfg.Events
	} else {
		ret := events.DefaultRetention
		if cfg.Options.EventRetention != nil {
			ret = *cfg.Options.EventRetention
		}
		l, err := events.Open(ctx, cfg.DB, events.Options{Clock: cfg.Clock, Retention: ret, PollInterval: cfg.Options.PollInterval})
		if err != nil {
			return nil, err
		}
		h.events = l
	}
	h.events.OnAppend(h.auditEvent) // W-audit: fleet/update/automation events -> audit log
	ms, err := store.Open(ctx, store.Config{DB: cfg.DB, Clock: cfg.Clock, Events: h.events, Limits: cfg.Options.MessageLimits, PollInterval: cfg.Options.PollInterval})
	if err != nil {
		return nil, err
	}
	ms.SetHooks(h.validateRecipient, h.routeStored, h.withdrawDelivery, h.afterCommit)
	h.msgs = ms
	if err := h.loadHubID(ctx); err != nil {
		return nil, err
	}
	if err := h.auditHubVersion(ctx); err != nil {
		return nil, err
	}
	// One account per identity: fold token-only duplicates left by older
	// CLIs (idempotent; accounts_merge.go).
	// A failure is retried by maintenance; it never blocks the hub.
	_, _, _ = h.FoldTokenAccounts(ctx)
	return h, nil
}

func (h *Hub) loadHubID(ctx context.Context) error {
	return sqlitex.Tx(ctx, h.db, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT value FROM hub_meta WHERE key='hub_id'`).Scan(&h.hubID)
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		h.hubID = "hub_" + contracts.NewID("h")[2:]
		_, err = tx.ExecContext(ctx, `INSERT INTO hub_meta(key,value) VALUES('hub_id',?)`, h.hubID)
		return err
	})
}

// HubID is this hub's stable identity.
func (h *Hub) HubID() string { return h.hubID }

// EventLog exposes the event log (EventSink for co-hosted domains).
func (h *Hub) EventLog() *events.Log { return h.events }

// MessageStore exposes the message store.
func (h *Hub) MessageStore() *store.Store { return h.msgs }

// Start runs background maintenance (expiry, retention, command flush)
// until Close.
func (h *Hub) Start() {
	h.wg.Add(1)
	go h.childReportLoop()
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		t := time.NewTicker(h.opt.MaintenanceInterval)
		defer t.Stop()
		for {
			select {
			case <-h.stop:
				return
			case <-t.C:
				ctx, cancel := context.WithTimeout(context.Background(), h.opt.MaintenanceInterval)
				_ = h.RunMaintenance(ctx)
				cancel()
			}
		}
	}()
}

// Close stops background work. It does not close the database.
func (h *Hub) Close() error {
	h.stopOnce.Do(func() { close(h.stop) })
	h.wg.Wait()
	return nil
}

func (h *Hub) now() time.Time { return h.clock.Now().UTC() }

// afterCommit runs after every committed hub write: wake event and message
// long polls, then hand newly queued commands to connected nodes.
func (h *Hub) afterCommit() {
	h.events.Notify()
	h.msgs.Notify()
	h.cmdBC.Broadcast()
	h.dirtyMu.Lock()
	nodes := make([]string, 0, len(h.dirty))
	for n := range h.dirty {
		nodes = append(nodes, n)
	}
	h.dirty = map[string]bool{}
	h.dirtyMu.Unlock()
	for _, n := range nodes {
		h.flushNode(context.Background(), n)
	}
	h.kickChildReports()
}

func (h *Hub) markDirty(nodeID string) {
	h.dirtyMu.Lock()
	h.dirty[nodeID] = true
	h.dirtyMu.Unlock()
}

// mutate runs an idempotent mutation and the post-commit hooks.
func (h *Hub) mutate(ctx context.Context, opID, op string, p contracts.Principal, req any, out any, fn func(tx *sql.Tx) (any, error)) error {
	_, err := persistence.Mutate(ctx, h.db, opID, op, p, req, out, fn)
	h.afterCommit()
	return err
}

// tx runs a non-idempotent write transaction plus post-commit hooks.
func (h *Hub) tx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	err := sqlitex.Tx(ctx, h.db, fn)
	h.afterCommit()
	return err
}

func (h *Hub) emit(ctx context.Context, tx *sql.Tx, ev contracts.Event) error {
	_, err := h.events.AppendTx(ctx, tx, ev)
	return err
}

func (h *Hub) emitPayload(ctx context.Context, tx *sql.Tx, kind contracts.EventKind, obj contracts.ObjectRef, sessionID string, projectID *string, origin string, originSeq uint64, payload any) error {
	ev := contracts.Event{Kind: kind, Object: obj, SessionID: sessionID, ProjectID: projectID, Origin: origin, OriginSeq: originSeq}
	if payload != nil {
		ev.Payload = contracts.RawJSON(persistence.JSON(payload))
	}
	return h.emit(ctx, tx, ev)
}

// systemCtx carries the hub's own principal for calls into other domains.
func (h *Hub) systemCtx(ctx context.Context) context.Context {
	return contracts.WithPrincipal(ctx, contracts.Principal{Kind: contracts.PrincipalSystem, ID: "hub"})
}

func principal(ctx context.Context) (contracts.Principal, error) {
	p, ok := contracts.PrincipalFrom(ctx)
	if !ok || p.Kind == "" || p.ID == "" {
		return p, contracts.Errorf(contracts.ErrUnauthorized, "no authenticated principal")
	}
	return p, nil
}

func requireOp(m contracts.Mutation) error {
	if m.OperationID == "" {
		return contracts.Errorf(contracts.ErrInvalid, "operation_id is required")
	}
	return nil
}

func isOwner(p contracts.Principal) bool {
	if p.OnBehalfOf != nil {
		return p.OnBehalfOf.Kind == contracts.PrincipalOwner
	}
	return p.Kind == contracts.PrincipalOwner
}

func notConfigured(domain string) error {
	return contracts.Errorf(contracts.ErrUnsupported, "%s not configured on this hub", domain)
}

type queryer interface {
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
}
