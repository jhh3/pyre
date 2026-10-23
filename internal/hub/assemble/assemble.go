// Package assemble composes the complete hub process from its domain
// packages over one SQLite database:
//
//	events (WP05)        durable event log; the EventSink for every domain
//	transport, enrollment,
//	topology (WP04)      credentials, TLS identity, enrollment/pairing,
//	                     node registry (FleetAPI), node WebSocket channel
//	projects, artifacts,
//	review (WP11)        projects, documents, notes, artifacts, comments
//	connections          Outfit connection catalog
//	attention,
//	notifications (WP17) attention inbox, deriver, push dispatcher
//	hub (WP05)           sessions, runs, messages, leases, approvals,
//	                     accounts, node command queue; the contracts.API
//	httpapi (WP03)       the HTTP binding
//
// cmd/pyre-hub serve uses Open + Start and serves Handler (and Preview on
// a separate origin).
package assemble

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/artifacts"
	"github.com/jhh3/pyre/internal/attention"
	"github.com/jhh3/pyre/internal/connections"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/enrollment"
	"github.com/jhh3/pyre/internal/events"
	"github.com/jhh3/pyre/internal/httpapi"
	"github.com/jhh3/pyre/internal/hub"
	"github.com/jhh3/pyre/internal/notifications"
	"github.com/jhh3/pyre/internal/projects"
	"github.com/jhh3/pyre/internal/review"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
	"github.com/jhh3/pyre/internal/terminal"
	"github.com/jhh3/pyre/internal/topology"
	"github.com/jhh3/pyre/internal/transport"
)

// Data directory layout.
const (
	DBFile       = "hub.db"
	ArtifactsDir = "artifacts"
	TLSDir       = "tls"
)

// Config configures Open.
type Config struct {
	// DataDir holds hub.db, artifacts/ and tls/. Required.
	DataDir string
	// DB overrides opening DataDir/hub.db (tests).
	DB    *sql.DB
	Clock contracts.Clock
	// HubURL is advertised in enrollment and pairing tickets.
	HubURL string
	// PreviewURL is the origin of the separate preview listener (serving
	// Stack.Preview), e.g. "https://hub.example:8444". Minted preview links
	// use it. When empty, Stack.Handler also serves /preview/ and links use
	// HubURL (same origin, still sandboxed) or are hub-relative.
	PreviewURL string
	// TLSHosts are SANs for a newly generated hub certificate.
	TLSHosts []string
	// Nodes overrides the transport node channel (tests drive nodes
	// directly through Stack.Hub's NodeLink methods). When set, no
	// NodeServer is created.
	Nodes contracts.NodeChannel
	// FCM is the push provider (nil: hub-stream only, push disabled).
	FCM notifications.Provider
	// Hub tunes the hub core.
	Hub    hub.Options
	Logger *slog.Logger
	// RequireConnectionGrants turns off the owner default for connections
	// (sessions use verified connections without a grant): every use then
	// needs a matching grant (connections.Options.RequireGrants).
	RequireConnectionGrants bool
}

// Stack is the composed hub.
type Stack struct {
	DB          *sql.DB
	ownDB       bool
	Events      *events.Log
	Auth        *transport.AuthStore
	Cert        *transport.HubCert
	Enrollment  *enrollment.Store
	Fleet       *topology.Store
	Projects    *projects.Store
	Artifacts   *artifacts.Store
	Review      *review.Store
	Connections *connections.Catalog
	Attention   *attention.Store
	Deriver     *attention.Deriver
	attn        *syncAttention
	Dispatcher  *notifications.Dispatcher
	NodeServer  *transport.NodeServer
	Terminal    *terminal.Relay
	Hub         *hub.Hub

	// Handler serves the API, node channel, enrollment/pairing exchanges and
	// health. Preview serves isolated artifact previews (separate origin).
	Handler http.Handler
	Preview http.Handler

	api    *httpapi.Server
	log    *slog.Logger
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// Open composes the hub. Close releases it.
func Open(ctx context.Context, cfg Config) (*Stack, error) {
	if cfg.DataDir == "" {
		return nil, errors.New("assemble: DataDir is required")
	}
	if cfg.Clock == nil {
		cfg.Clock = contracts.SystemClock{}
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	s := &Stack{DB: cfg.DB, log: log}
	if s.DB == nil {
		db, err := sqlitex.Open(filepath.Join(cfg.DataDir, DBFile))
		if err != nil {
			return nil, err
		}
		s.DB, s.ownDB = db, true
	}
	fail := func(err error) (*Stack, error) {
		s.Close()
		return nil, err
	}
	var err error
	if s.Events, err = events.Open(ctx, s.DB, events.Options{Clock: cfg.Clock, Retention: retention(cfg.Hub), PollInterval: cfg.Hub.PollInterval}); err != nil {
		return fail(err)
	}
	if s.Auth, err = transport.NewAuthStore(s.DB, cfg.Clock); err != nil {
		return fail(err)
	}
	if s.Cert, err = transport.EnsureHubCert(filepath.Join(cfg.DataDir, TLSDir), cfg.TLSHosts); err != nil {
		return fail(err)
	}
	if s.Enrollment, err = enrollment.NewStore(s.DB, s.Auth, cfg.Clock, enrollment.Options{HubURL: cfg.HubURL, HubCertSHA256: s.Cert.Fingerprint}); err != nil {
		return fail(err)
	}
	if s.Fleet, err = topology.NewStore(s.DB, s.Auth, s.Enrollment, s.Events, cfg.Clock); err != nil {
		return fail(err)
	}
	if _, err = s.Fleet.ResetConnections(ctx); err != nil {
		return fail(err)
	}
	if s.Projects, err = projects.New(ctx, s.DB, s.Events, projects.Options{Clock: cfg.Clock}); err != nil {
		return fail(err)
	}
	if s.Artifacts, err = artifacts.New(ctx, s.DB, s.Events, artifacts.Options{DataDir: filepath.Join(cfg.DataDir, ArtifactsDir), Projects: s.Projects, Clock: cfg.Clock,
		Tickets: s.Auth, PreviewBase: previewBase(cfg)}); err != nil {
		return fail(err)
	}
	s.Projects.SetArtifactIndex(s.Artifacts)
	if s.Review, err = review.New(ctx, s.DB, s.Events, s.Projects, s.Artifacts, review.Options{Clock: cfg.Clock}); err != nil {
		return fail(err)
	}
	if s.Connections, err = connections.Open(ctx, s.DB, connections.Options{Events: s.Events, Clock: cfg.Clock, RequireGrants: cfg.RequireConnectionGrants}); err != nil {
		return fail(err)
	}
	if s.Attention, err = attention.NewStore(ctx, s.DB, attention.Options{Sink: s.Events, Clock: cfg.Clock, FCMConfigured: cfg.FCM != nil, Logger: log}); err != nil {
		return fail(err)
	}

	s.attn = &syncAttention{Store: s.Attention, stack: s}

	nodes := cfg.Nodes
	if nodes == nil {
		nodes = &lateChannel{s: s}
	}
	if s.Terminal, err = terminal.NewRelay(terminal.RelayConfig{Tickets: s.Auth, Auth: s.Auth, Backend: lateTerminalBackend{s: s},
		Clock: cfg.Clock, Logger: log.With("component", "terminal")}); err != nil {
		return fail(err)
	}
	hubCfg := hub.Config{
		DB: s.DB, DataDir: cfg.DataDir, Clock: cfg.Clock, Events: s.Events,
		Fleet: s.Fleet, Projects: s.Projects, Documents: s.Projects, Artifacts: s.Artifacts, Reviews: s.Review, Notes: s.Projects,
		Attention: s.attn, Connections: s.Connections, Tokens: s.Auth, Nodes: nodes, Terminal: s.Terminal, Options: cfg.Hub,
		ConnectionGrants: func(ctx context.Context, connectionID string, gs hub.GrantSession) (contracts.GrantDecision, error) {
			d, err := s.Connections.CheckGrant(ctx, connectionID, connections.SessionContext{
				SessionID: gs.SessionID, NodeID: gs.NodeID, ProjectID: gs.ProjectID, Archived: gs.Archived, RunActive: gs.RunActive})
			return contracts.GrantDecision{Allowed: d.Allowed, Generation: d.Generation, Reason: d.Reason}, err
		},
		RouteConnectionInvoke: func(ctx context.Context, connectionID string, gs hub.GrantSession) (string, error) {
			r, err := s.Connections.RouteInvoke(ctx, connectionID, connections.SessionContext{
				SessionID: gs.SessionID, NodeID: gs.NodeID, ProjectID: gs.ProjectID, Archived: gs.Archived, RunActive: gs.RunActive})
			return r.NodeID, err
		},
		RecordConnectionGrant: func(ctx context.Context, p contracts.Principal, opID string, g contracts.ConnectionGrantParams, active bool) error {
			req := connections.GrantRequest{Mutation: contracts.Mutation{OperationID: opID}, ConnectionGrantParams: g}
			var err error
			if active {
				_, err = s.Connections.Grant(ctx, p, req)
			} else {
				_, err = s.Connections.Revoke(ctx, p, req)
			}
			return err
		},
	}
	if s.Hub, err = hub.New(hubCfg); err != nil {
		return fail(err)
	}
	if cfg.Nodes == nil {
		nsCfg, err := s.Fleet.NodeServerConfig(topology.HubHooks{
			Welcome:      s.welcome,
			OnReport:     s.onReport,
			OnCommandAck: s.Hub.HandleCommandAck,
			OnDisconnect: s.onDisconnect,
		})
		if err != nil {
			return fail(err)
		}
		nsCfg.Auth, nsCfg.HubID, nsCfg.Logger = s.Auth, s.Hub.HubID(), log
		if s.NodeServer, err = transport.NewNodeServer(nsCfg); err != nil {
			return fail(err)
		}
		s.NodeServer.DisconnectOnRevoke(s.Auth)
	}

	s.Deriver = attention.NewDeriver(s.Attention, s.Hub, attention.DeriverOptions{
		Lookup:   &attention.APILookup{Accounts: s.Hub, Fleet: s.Hub},
		Snapshot: attention.APISnapshot{Interaction: s.Hub, Sessions: s.Hub, Messages: s.Hub},
		// Short polls so a read-your-writes catch-up never waits long for
		// the deriver lock.
		WaitSeconds: 1,
		Logger:      log,
	})
	s.Dispatcher = notifications.NewDispatcher(s.Attention, notifications.DispatcherOptions{FCM: cfg.FCM, Clock: cfg.Clock, Logger: log})

	extra := map[string]http.Handler{
		contracts.PathEnrollExchange: s.Enrollment.EnrollExchangeHandler(enrollment.HandlerOptions{Logger: log}),
		contracts.PathPairExchange:   s.Enrollment.PairExchangeHandler(enrollment.HandlerOptions{Logger: log}),
		contracts.PathTerminal:       s.Terminal.ClientHandler(), // public: one-time ticket in the first frame
		contracts.PathNodeTerminal:   s.Terminal.NodeHandler(),   // node bearer token
	}
	if s.NodeServer != nil {
		extra[contracts.PathNodeConnect] = s.NodeServer
	}
	s.Preview = artifacts.PreviewHandler(s.Artifacts, s.Auth)
	if cfg.PreviewURL == "" {
		extra[contracts.PathPreview] = s.Preview // same origin; links use HubURL
	}
	s.api = httpapi.New(s.Hub, s.Auth, httpapi.Options{Extra: extra, Logger: log})
	s.Handler = s.api
	return s, nil
}

// previewBase is the origin minted preview links point at.
func previewBase(cfg Config) string {
	if cfg.PreviewURL != "" {
		return strings.TrimRight(cfg.PreviewURL, "/")
	}
	return strings.TrimRight(cfg.HubURL, "/")
}

func retention(o hub.Options) events.Retention {
	if o.EventRetention != nil {
		return *o.EventRetention
	}
	return events.DefaultRetention
}

// welcome adapts the hub's hello handling to the transport: the committed
// report watermark plus queued commands (Seq > LastCommandSeq, unacked)
// sent right after the Welcome frame.
func (s *Stack) welcome(ctx context.Context, nodeID string, hello contracts.NodeHello) (transport.WelcomeInfo, error) {
	w, err := s.Hub.HandleHello(ctx, nodeID, hello)
	if err != nil {
		return transport.WelcomeInfo{}, err
	}
	cmds, err := s.Hub.WelcomeCommands(ctx, nodeID)
	if err != nil {
		return transport.WelcomeInfo{}, err
	}
	return transport.WelcomeInfo{CommittedReportSeq: w.CommittedReportSeq, PendingCommands: cmds}, nil
}

func (s *Stack) onReport(ctx context.Context, nodeID string, r contracts.NodeReport) error {
	_, err := s.Hub.HandleReport(ctx, nodeID, r)
	return err
}

func (s *Stack) onDisconnect(ctx context.Context, nodeID string, info transport.DisconnectInfo) {
	if info.Replaced {
		return
	}
	if err := s.Hub.HandleDisconnect(ctx, nodeID); err != nil {
		s.log.Warn("record node disconnect", "node", nodeID, "err", err)
	}
}

// lateChannel forwards to the NodeServer, which is created after the hub.
type lateChannel struct{ s *Stack }

func (l *lateChannel) Send(ctx context.Context, nodeID string, cmd contracts.NodeCommand) error {
	if l.s.NodeServer == nil {
		return contracts.Errorf(contracts.ErrUnavailable, "node channel not ready")
	}
	return l.s.NodeServer.Send(ctx, nodeID, cmd)
}

func (l *lateChannel) Connected(nodeID string) bool {
	return l.s.NodeServer != nil && l.s.NodeServer.Connected(nodeID)
}

// lateTerminalBackend forwards the relay to the hub, which is created after
// the relay (the hub issues the relay's tickets).
type lateTerminalBackend struct{ s *Stack }

func (b lateTerminalBackend) PrepareTerminal(ctx context.Context, p contracts.Principal, runID string, readOnly bool) (string, error) {
	return b.s.Hub.PrepareTerminal(ctx, p, runID, readOnly)
}

func (b lateTerminalBackend) QueueTerminalOpen(ctx context.Context, p contracts.Principal, nodeID string, cmd contracts.TerminalOpenCommand, ttl time.Duration) (string, error) {
	return b.s.Hub.QueueTerminalOpen(ctx, p, nodeID, cmd, ttl)
}

func (b lateTerminalBackend) TerminalOpenResult(ctx context.Context, commandID string, wait time.Duration) (bool, error) {
	return b.s.Hub.TerminalOpenResult(ctx, commandID, wait)
}

func (b lateTerminalBackend) TerminalInputAllowed(ctx context.Context, p contracts.Principal, runID string) error {
	return b.s.Hub.TerminalInputAllowed(ctx, p, runID)
}

// Start runs background work: hub maintenance, the attention deriver, the
// notification dispatcher, and artifact upload expiry.
func (s *Stack) Start(ctx context.Context) {
	ctx, s.cancel = context.WithCancel(ctx)
	s.Hub.Start()
	run := func(name string, fn func(context.Context) error) {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			if err := fn(ctx); err != nil && !errors.Is(err, context.Canceled) {
				s.log.Warn("background task ended", "task", name, "err", err)
			}
		}()
	}
	run("attention-deriver", s.attn.run)
	run("notification-dispatcher", s.Dispatcher.Run)
	run("artifact-cleanup", func(ctx context.Context) error {
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		for {
			if _, err := s.Artifacts.CleanupExpired(ctx); err != nil {
				s.log.Warn("artifact upload cleanup", "err", err)
			}
			select {
			case <-ctx.Done():
				return nil
			case <-t.C:
			}
		}
	})
}

// Close stops background work and releases the database if Open opened it.
func (s *Stack) Close() error {
	if s.api != nil {
		// End event streams before their storage goes away.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = s.api.CloseStreams(ctx)
		cancel()
	}
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
	if s.NodeServer != nil {
		s.NodeServer.Close()
	}
	if s.Hub != nil {
		s.Hub.Close()
	}
	if s.ownDB && s.DB != nil {
		return s.DB.Close()
	}
	return nil
}

// syncAttention gives attention reads read-your-writes consistency: before
// listing or updating, it applies any events the deriver has not yet
// consumed. The background loop and catch-ups share one lock so the
// deriver's cursor is advanced by one stepper at a time.
type syncAttention struct {
	*attention.Store
	stack *Stack
	mu    sync.Mutex
}

func (a *syncAttention) catchUp(ctx context.Context) {
	// A session list asks for every listed session's flag: catch up once
	// per request, not once per session (W-dbhygiene).
	if !hub.FirstInRequest(ctx, "attention.catchup") {
		return
	}
	behind := func() bool {
		head, err := a.stack.Events.Head(ctx)
		if err != nil {
			return false
		}
		cur, err := a.Store.Cursor(ctx, attention.DefaultConsumer)
		return err == nil && cur < head
	}
	// Caught up already: no lock (the background loop may hold it).
	if !behind() {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := 0; i < 100 && behind(); i++ {
		// No long poll: when only events the deriver does not consume are
		// pending, a waiting step held this request for the full wait.
		if _, err := a.stack.Deriver.StepNow(ctx); err != nil {
			return
		}
	}
}

func (a *syncAttention) run(ctx context.Context) error {
	backoff := 250 * time.Millisecond
	for {
		// Wait for work without the lock, so read-your-writes catch-ups
		// never queue behind an idle long poll; then apply under it.
		err := a.stack.Deriver.WaitPending(ctx)
		if err == nil {
			a.mu.Lock()
			_, err = a.stack.Deriver.StepNow(ctx)
			a.mu.Unlock()
		}
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			a.stack.log.Warn("attention deriver step", "err", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = 250 * time.Millisecond
		// Yield so catch-ups can take the lock between steps.
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (a *syncAttention) ListAttention(ctx context.Context, req contracts.ListAttentionRequest) (contracts.Page[contracts.AttentionItem], error) {
	a.catchUp(ctx)
	return a.Store.ListAttention(ctx, req)
}

func (a *syncAttention) GetAttention(ctx context.Context, req contracts.GetRequest) (contracts.AttentionItem, error) {
	a.catchUp(ctx)
	return a.Store.GetAttention(ctx, req)
}

func (a *syncAttention) UpdateAttention(ctx context.Context, req contracts.UpdateAttentionRequest) (contracts.AttentionItem, error) {
	a.catchUp(ctx)
	return a.Store.UpdateAttention(ctx, req)
}

func (a *syncAttention) CountOpen(ctx context.Context, sessionID string) (int, error) {
	a.catchUp(ctx)
	return a.Store.CountOpen(ctx, sessionID)
}

func (a *syncAttention) SessionAttention(ctx context.Context, sessionID string) (int, *contracts.SessionFlag, error) {
	a.catchUp(ctx)
	return a.Store.SessionAttention(ctx, sessionID)
}

func (a *syncAttention) SessionAttentionRun(ctx context.Context, sessionID string, run *contracts.Run) (int, *contracts.SessionFlag, error) {
	a.catchUp(ctx)
	return a.Store.SessionAttentionRun(ctx, sessionID, run)
}

// OwnerEngaged catches up first so a message derived from a not yet
// consumed event is answered too.
func (a *syncAttention) OwnerEngaged(ctx context.Context, sessionID, summary string, kinds ...contracts.AttentionKind) (int, error) {
	a.catchUp(ctx)
	return a.Store.OwnerEngaged(ctx, sessionID, summary, kinds...)
}

// RaiseWaiting catches up first so the deriver has applied what happened
// before the turn ended (the hub's waiting.go).
func (a *syncAttention) RaiseWaiting(ctx context.Context, sessionID string, projectID *string, question, summary string) (contracts.AttentionItem, error) {
	a.catchUp(ctx)
	return a.Store.RaiseWaiting(ctx, sessionID, projectID, question, summary)
}

func (a *syncAttention) FlagAttention(ctx context.Context, req contracts.FlagAttentionRequest) (contracts.AttentionItem, error) {
	a.catchUp(ctx)
	return a.Store.FlagAttention(ctx, req)
}
