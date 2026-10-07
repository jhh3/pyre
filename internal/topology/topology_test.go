package topology

import (
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/enrollment"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
	"github.com/jhh3/pyre/internal/transport"
)

type fakeSink struct {
	mu     sync.Mutex
	events []contracts.Event
	cursor int64
}

func (f *fakeSink) AppendTx(_ context.Context, tx *sql.Tx, ev contracts.Event) (contracts.Event, error) {
	if tx == nil {
		panic("event appended outside a transaction")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cursor++
	ev.Cursor = f.cursor
	f.events = append(f.events, ev)
	return ev, nil
}

func (f *fakeSink) kinds(nodeID string) []contracts.EventKind {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []contracts.EventKind
	for _, e := range f.events {
		if e.Object.ID == nodeID {
			out = append(out, e.Kind)
		}
	}
	return out
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type fx struct {
	db    *sql.DB
	auth  *transport.AuthStore
	enr   *enrollment.Store
	fleet *Store
	sink  *fakeSink
	cert  *transport.HubCert
}

func newFx(t *testing.T, clock contracts.Clock, hubURL string) *fx {
	t.Helper()
	db, err := sqlitex.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	auth, err := transport.NewAuthStore(db, clock)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := transport.EnsureHubCert(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	enr, err := enrollment.NewStore(db, auth, clock, enrollment.Options{HubURL: hubURL, HubCertSHA256: cert.Fingerprint, RateLimit: enrollment.RateLimitConfig{Disabled: true}})
	if err != nil {
		t.Fatal(err)
	}
	sink := &fakeSink{}
	fleet, err := NewStore(db, auth, enr, sink, clock)
	if err != nil {
		t.Fatal(err)
	}
	return &fx{db: db, auth: auth, enr: enr, fleet: fleet, sink: sink, cert: cert}
}

var ownerCtx = contracts.WithPrincipal(context.Background(), contracts.Principal{Kind: contracts.PrincipalOwner, ID: "cli_owner"})

func (f *fx) enroll(t *testing.T, name string) (contracts.EnrollmentTicket, contracts.NodeCredential) {
	t.Helper()
	tk, err := f.fleet.CreateEnrollment(ownerCtx, contracts.EnrollmentRequest{Mutation: contracts.Mutation{OperationID: contracts.NewID("op")}, Name: name, SSHAlias: name})
	if err != nil {
		t.Fatal(err)
	}
	cred, err := f.enr.RedeemNode(context.Background(), contracts.NodeEnrollExchange{NodeID: tk.NodeID, Secret: tk.Secret, Hostname: name + ".lan", Platform: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	return tk, cred
}

func TestFleetEnrollmentAndRevocation(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)}
	f := newFx(t, clock, "https://hub.test:7443")
	opID := contracts.NewID("op")
	req := contracts.EnrollmentRequest{Mutation: contracts.Mutation{OperationID: opID}, Name: "server", SSHAlias: "server"}

	// Authorization.
	if _, err := f.fleet.CreateEnrollment(context.Background(), req); !contracts.IsCode(err, contracts.ErrUnauthorized) {
		t.Fatalf("anonymous enroll: %v", err)
	}
	sessCtx := contracts.WithPrincipal(context.Background(), contracts.Principal{Kind: contracts.PrincipalSession, ID: "ses_1"})
	if _, err := f.fleet.CreateEnrollment(sessCtx, req); !contracts.IsCode(err, contracts.ErrForbidden) {
		t.Fatalf("session enroll: %v", err)
	}

	tk, err := f.fleet.CreateEnrollment(ownerCtx, req)
	if err != nil {
		t.Fatal(err)
	}
	// The operations table never stores the secret.
	var stored int
	f.db.QueryRow(`SELECT COUNT(*) FROM operations WHERE instr(CAST(response AS TEXT), ?) > 0`, tk.Secret).Scan(&stored)
	if stored != 0 {
		t.Fatal("enrollment secret persisted in operations table")
	}
	// Retrying the same operation returns the same node with a rotated secret.
	tk2, err := f.fleet.CreateEnrollment(ownerCtx, req)
	if err != nil || tk2.NodeID != tk.NodeID || tk2.Secret == tk.Secret || tk2.Secret == "" {
		t.Fatalf("replay: %+v %v", tk2, err)
	}
	if _, err := f.enr.RedeemNode(context.Background(), contracts.NodeEnrollExchange{NodeID: tk.NodeID, Secret: tk.Secret}); !contracts.IsCode(err, contracts.ErrUnauthorized) {
		t.Fatalf("superseded secret accepted: %v", err)
	}
	cred, err := f.enr.RedeemNode(context.Background(), contracts.NodeEnrollExchange{NodeID: tk.NodeID, Secret: tk2.Secret, Hostname: "server.lan", Platform: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.fleet.CreateEnrollment(ownerCtx, req); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("replay after redemption: %v", err)
	}

	n, err := f.fleet.GetNode(sessCtx, contracts.GetRequest{ID: tk.NodeID})
	if err != nil || n.Name != "server" || n.Connection.State != contracts.ConnNever || n.Platform != "linux" || n.Hostname != "server.lan" {
		t.Fatalf("node: %+v %v", n, err)
	}
	if fr := NodeFreshness(n, clock.Now()); !fr.Stale || !strings.Contains(fr.Reason, "never") {
		t.Fatalf("freshness never: %+v", fr)
	}

	var notified []contracts.Principal
	f.auth.OnRevoke(func(p contracts.Principal) { notified = append(notified, p) })
	rreq := contracts.RevokeRequest{Mutation: contracts.Mutation{OperationID: contracts.NewID("op")}, ID: tk.NodeID, Reason: "lost laptop"}
	rn, err := f.fleet.RevokeNode(ownerCtx, rreq)
	if err != nil || rn.Connection.State != contracts.ConnRevoked {
		t.Fatalf("revoke: %+v %v", rn, err)
	}
	if _, err := f.auth.Authenticate(context.Background(), cred.Token); !contracts.IsCode(err, contracts.ErrUnauthorized) {
		t.Fatalf("revoked node token works: %v", err)
	}
	if again, err := f.fleet.RevokeNode(ownerCtx, rreq); err != nil || again.NodeID != tk.NodeID {
		t.Fatalf("idempotent revoke: %v", err)
	}
	if len(notified) == 0 || notified[0].ID != tk.NodeID {
		t.Fatalf("revocation not notified: %+v", notified)
	}
	page, _ := f.fleet.ListNodes(sessCtx, contracts.ListNodesRequest{})
	if len(page.Items) != 0 {
		t.Fatalf("revoked node listed by default: %d", len(page.Items))
	}
	page, _ = f.fleet.ListNodes(sessCtx, contracts.ListNodesRequest{IncludeRevoked: true})
	if len(page.Items) != 1 {
		t.Fatalf("include revoked: %d", len(page.Items))
	}
	if _, err := f.fleet.ObserveHello(context.Background(), tk.NodeID, transport.ConnInfo{ConnID: "conn_1"}, contracts.NodeHello{NodeID: tk.NodeID}); !contracts.IsCode(err, contracts.ErrForbidden) {
		t.Fatalf("revoked node hello: %v", err)
	}
	got := f.sink.kinds(tk.NodeID)
	if len(got) != 2 || got[0] != contracts.EvNodeEnrolled || got[1] != contracts.EvNodeRevoked {
		t.Fatalf("events: %v", got)
	}
}

func TestPairingAndClients(t *testing.T) {
	f := newFx(t, contracts.SystemClock{}, "https://hub.test:7443")
	preq := contracts.PairingRequest{Mutation: contracts.Mutation{OperationID: contracts.NewID("op")}, ClientName: "pixel", Kind: "android"}
	p1, err := f.fleet.CreatePairing(ownerCtx, preq)
	if err != nil || p1.URI == "" || p1.Code == "" {
		t.Fatalf("pair: %+v %v", p1, err)
	}
	p2, err := f.fleet.CreatePairing(ownerCtx, preq)
	if err != nil || p2.PairingID != p1.PairingID || p2.Code == p1.Code || !strings.Contains(p2.URI, p2.Code) {
		t.Fatalf("pair replay: %+v %v", p2, err)
	}
	u, err := enrollment.ParsePairingURI(p2.URI)
	if err != nil {
		t.Fatal(err)
	}
	cred, err := f.enr.RedeemPairing(context.Background(), contracts.PairingExchange{PairingID: u.PairingID, Code: u.Code})
	if err != nil {
		t.Fatal(err)
	}
	clients, err := f.fleet.ListClients(ownerCtx, contracts.PageRequest{})
	if err != nil || len(clients.Items) != 1 || clients.Items[0].Kind != "android" {
		t.Fatalf("clients: %+v %v", clients, err)
	}
	nodeCtx := contracts.WithPrincipal(context.Background(), contracts.Principal{Kind: contracts.PrincipalNode, ID: "node_x"})
	if _, err := f.fleet.ListClients(nodeCtx, contracts.PageRequest{}); !contracts.IsCode(err, contracts.ErrForbidden) {
		t.Fatalf("node listing clients: %v", err)
	}
	info, err := f.fleet.RevokeClient(ownerCtx, contracts.RevokeRequest{Mutation: contracts.Mutation{OperationID: contracts.NewID("op")}, ID: cred.ClientID})
	if err != nil || !info.Revoked {
		t.Fatalf("revoke client: %+v %v", info, err)
	}
	if _, err := f.auth.Authenticate(context.Background(), cred.Token); !contracts.IsCode(err, contracts.ErrUnauthorized) {
		t.Fatalf("revoked client works: %v", err)
	}
}

func TestObservationsNeverDuplicateAndRespectConnID(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)}
	f := newFx(t, clock, "https://hub.test:7443")
	tk, _ := f.enroll(t, "mac")
	ctx := context.Background()
	hello := contracts.NodeHello{NodeID: tk.NodeID, PyredVersion: "0.1.0", Hostname: "mac.lan", Platform: "darwin", Arch: "arm64",
		Harnesses: []contracts.HarnessInfo{{Harness: contracts.HarnessClaude, Version: "2.1", Available: true}}}
	for i, conn := range []string{"conn_a", "conn_b", "conn_c"} {
		clock.Advance(time.Second)
		n, err := f.fleet.ObserveHello(ctx, tk.NodeID, transport.ConnInfo{ConnID: conn, Protocol: 1, RemoteAddr: "10.0.0.2:5000"}, hello)
		if err != nil {
			t.Fatal(err)
		}
		if n.Connection.State != contracts.ConnConnected || n.Versions["pyred"] != "0.1.0" || n.Platform != "darwin" || len(n.Harnesses) != 1 {
			t.Fatalf("hello %d: %+v", i, n)
		}
	}
	page, _ := f.fleet.ListNodes(ownerCtx, contracts.ListNodesRequest{})
	if len(page.Items) != 1 {
		t.Fatalf("duplicate nodes: %d", len(page.Items))
	}
	// Pinned payload shapes: node.connection -> NodeConnection, others -> Node.
	f.sink.mu.Lock()
	for _, ev := range f.sink.events {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(ev.Payload, &m); err != nil {
			t.Fatal(err)
		}
		_, hasNodeID := m["node_id"]
		_, hasState := m["state"]
		switch ev.Kind {
		case contracts.EvNodeConnection:
			var c contracts.NodeConnection
			if hasNodeID || !hasState || json.Unmarshal(ev.Payload, &c) != nil || c.State != contracts.ConnConnected {
				t.Errorf("node.connection payload is not a NodeConnection: %s", ev.Payload)
			}
		default:
			if !hasNodeID || ev.PayloadVersion != 1 || ev.Object.Kind != contracts.KindNode {
				t.Errorf("%s payload is not a Node: %s", ev.Kind, ev.Payload)
			}
		}
	}
	f.sink.mu.Unlock()
	// A replaced connection's late disconnect does not mark the node offline.
	if changed, _ := f.fleet.ObserveDisconnect(ctx, tk.NodeID, "conn_a", clock.Now()); changed {
		t.Fatal("stale conn disconnect applied")
	}
	n, _ := f.fleet.GetNode(ownerCtx, contracts.GetRequest{ID: tk.NodeID})
	if fr := NodeFreshness(n, clock.Now()); fr.Stale {
		t.Fatalf("fresh connected node reported stale: %+v", fr)
	}
	// Status reports update resources; no observation for > StaleAfter is stale.
	if err := f.fleet.ObserveStatus(ctx, tk.NodeID, contracts.NodeStatusReport{Resources: contracts.NodeResources{CPUCount: 8, ActiveRuns: 2}}); err != nil {
		t.Fatal(err)
	}
	n, _ = f.fleet.GetNode(ownerCtx, contracts.GetRequest{ID: tk.NodeID})
	if n.Resources.CPUCount != 8 || n.Resources.ObservedAt.IsZero() {
		t.Fatalf("resources: %+v", n.Resources)
	}
	clock.Advance(StaleAfter + time.Second)
	n, _ = f.fleet.GetNode(ownerCtx, contracts.GetRequest{ID: tk.NodeID})
	if fr := NodeFreshness(n, clock.Now()); !fr.Stale || !strings.Contains(fr.Reason, "no observation") {
		t.Fatalf("stale connected: %+v", fr)
	}
	lastSeen := clock.Now().Add(-10 * time.Second)
	if changed, err := f.fleet.ObserveDisconnect(ctx, tk.NodeID, "conn_c", lastSeen); !changed || err != nil {
		t.Fatalf("disconnect: %v %v", changed, err)
	}
	n, _ = f.fleet.GetNode(ownerCtx, contracts.GetRequest{ID: tk.NodeID})
	if n.Connection.State != contracts.ConnDisconnected || n.Connection.LastSeenAt == nil || !n.Connection.LastSeenAt.Equal(lastSeen) || n.Connection.ConnectedSince != nil {
		t.Fatalf("disconnected node: %+v", n.Connection)
	}
	fr := NodeFreshness(n, clock.Now())
	if !fr.Stale || !strings.Contains(fr.Reason, "last seen") || !fr.ObservedAt.Equal(lastSeen) {
		t.Fatalf("disconnected freshness: %+v", fr)
	}
	// Status events are rate limited; repeated identical status does not emit.
	before := len(f.sink.kinds(tk.NodeID))
	f.fleet.ObserveStatus(ctx, tk.NodeID, contracts.NodeStatusReport{Resources: contracts.NodeResources{CPUCount: 8}})
	if got := len(f.sink.kinds(tk.NodeID)); got != before {
		t.Fatalf("status event emitted within interval: %d -> %d", before, got)
	}
	clock.Advance(DefaultStatusEventInterval)
	f.fleet.ObserveStatus(ctx, tk.NodeID, contracts.NodeStatusReport{Resources: contracts.NodeResources{CPUCount: 8}})
	f.fleet.ObserveStatus(ctx, tk.NodeID, contracts.NodeStatusReport{Resources: contracts.NodeResources{CPUCount: 8}})
	if got := len(f.sink.kinds(tk.NodeID)); got != before+1 {
		t.Fatalf("status events %d -> %d, want one after the interval", before, got)
	}
	// A harness change emits immediately.
	f.fleet.ObserveStatus(ctx, tk.NodeID, contracts.NodeStatusReport{Harnesses: []contracts.HarnessInfo{{Harness: contracts.HarnessClaude, Version: "2.2", Available: true}}})
	if got := len(f.sink.kinds(tk.NodeID)); got != before+2 {
		t.Fatalf("harness change did not emit: %d", got)
	}
	// A status report from a disconnected node does not move last_seen forward.
	n, _ = f.fleet.GetNode(ownerCtx, contracts.GetRequest{ID: tk.NodeID})
	if !n.Connection.LastSeenAt.Equal(lastSeen) {
		t.Fatalf("late status claimed liveness: %v", n.Connection.LastSeenAt)
	}
	// Hub restart: connected rows become disconnected.
	f.fleet.ObserveHello(ctx, tk.NodeID, transport.ConnInfo{ConnID: "conn_d"}, hello)
	if c, err := f.fleet.ResetConnections(ctx); c != 1 || err != nil {
		t.Fatalf("reset: %d %v", c, err)
	}
	n, _ = f.fleet.GetNode(ownerCtx, contracts.GetRequest{ID: tk.NodeID})
	if n.Connection.State != contracts.ConnDisconnected {
		t.Fatalf("after reset: %s", n.Connection.State)
	}
}

// ---- end to end: enrollment over HTTPS, NodeLink, registry, revocation ----

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

type hubCore struct {
	mu        sync.Mutex
	committed map[uint64]bool
}

func (h *hubCore) welcome(context.Context, string, contracts.NodeHello) (transport.WelcomeInfo, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var m uint64
	for s := range h.committed {
		m = max(m, s)
	}
	return transport.WelcomeInfo{CommittedReportSeq: m}, nil
}

func (h *hubCore) report(_ context.Context, _ string, r contracts.NodeReport) error {
	if r.OriginSeq > 0 {
		h.mu.Lock()
		h.committed[r.OriginSeq] = true
		h.mu.Unlock()
	}
	return nil
}

type hubServer struct {
	ns  *transport.NodeServer
	srv *httptest.Server
}

func startHubServer(t *testing.T, f *fx, core *hubCore, addr string, logger *slog.Logger) *hubServer {
	t.Helper()
	if _, err := f.fleet.ResetConnections(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg, err := f.fleet.NodeServerConfig(HubHooks{Welcome: core.welcome, OnReport: core.report})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Auth, cfg.HubID, cfg.Logger = f.auth, "hub_e2e", logger
	ns, err := transport.NewNodeServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ns.DisconnectOnRevoke(f.auth)
	mux := http.NewServeMux()
	mux.Handle(contracts.PathNodeConnect, ns)
	f.enr.Register(mux, enrollment.HandlerOptions{Logger: logger})
	var l net.Listener
	deadline := time.Now().Add(5 * time.Second)
	for {
		l, err = net.Listen("tcp", addr)
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(mux)
	srv.Listener = l
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{f.cert.Certificate}}
	srv.StartTLS()
	hs := &hubServer{ns: ns, srv: srv}
	t.Cleanup(hs.stop)
	return hs
}

func (h *hubServer) stop() {
	if h.ns != nil {
		h.ns.Close()
		h.srv.Close()
		h.ns = nil
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestEndToEndEnrollConnectReconnectRevoke(t *testing.T) {
	// Reserve an address so the hub URL in tickets is stable across restarts.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	hubURL := "https://" + addr
	f := newFx(t, contracts.SystemClock{}, hubURL)
	logs := &syncBuf{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	core := &hubCore{committed: map[uint64]bool{}}
	hub := startHubServer(t, f, core, addr, logger)
	ctx := context.Background()

	// Owner creates a ticket; the "node" redeems it over pinned HTTPS and
	// saves its credential file.
	tk, err := f.fleet.CreateEnrollment(ownerCtx, contracts.EnrollmentRequest{Mutation: contracts.Mutation{OperationID: contracts.NewID("op")}, Name: "linux-box"})
	if err != nil {
		t.Fatal(err)
	}
	cred, err := enrollment.ExchangeEnrollment(ctx, tk.HubURL, tk.HubCertSHA256, contracts.NodeEnrollExchange{NodeID: tk.NodeID, Secret: tk.Secret, Hostname: "box", Platform: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	credPath := filepath.Join(t.TempDir(), "pyre", "node-credential.json")
	if err := enrollment.SaveNodeCredential(credPath, enrollment.NodeCredentialFile{HubURL: tk.HubURL, HubCertSHA256: tk.HubCertSHA256, NodeID: cred.NodeID, Token: cred.Token, EnrolledAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	nodeDBPath := filepath.Join(t.TempDir(), "node.db")

	startLink := func() (*transport.NodeLink, func()) {
		c, err := enrollment.LoadNodeCredential(credPath)
		if err != nil {
			t.Fatal(err)
		}
		tlsCfg, err := transport.PinnedTLSConfig(c.HubCertSHA256)
		if err != nil {
			t.Fatal(err)
		}
		db, err := sqlitex.Open(nodeDBPath)
		if err != nil {
			t.Fatal(err)
		}
		link, err := transport.NewNodeLink(transport.NodeLinkConfig{
			HubURL: c.HubURL, NodeID: c.NodeID, Token: c.Token, TLS: tlsCfg, DB: db, Logger: logger,
			Handler: transport.CommandHandlerFunc(func(context.Context, contracts.NodeCommand) (contracts.CommandResult, error) {
				return contracts.CommandResult{}, nil
			}),
			Hello: func(context.Context) (contracts.NodeHello, error) {
				return contracts.NodeHello{PyredVersion: "0.1.0", Hostname: "box", Platform: "linux", Arch: "amd64",
					Harnesses: []contracts.HarnessInfo{{Harness: contracts.HarnessCodex, Version: "0.160.0", Available: true}}}, nil
			},
			Backoff:     transport.BackoffConfig{Initial: 20 * time.Millisecond, Max: 200 * time.Millisecond},
			StableAfter: time.Hour,
		})
		if err != nil {
			t.Fatal(err)
		}
		rctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); link.Run(rctx) }()
		var once sync.Once
		stop := func() { once.Do(func() { cancel(); <-done; db.Close() }) }
		t.Cleanup(stop)
		return link, stop
	}

	nodeState := func() contracts.Node {
		n, err := f.fleet.GetNode(ownerCtx, contracts.GetRequest{ID: cred.NodeID})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Connect, restart the node service three times: one node, stable ID.
	for i := 0; i < 3; i++ {
		link, stop := startLink()
		waitFor(t, "connected", func() bool { return link.Connected() && nodeState().Connection.State == contracts.ConnConnected })
		if _, err := link.Enqueue(ctx, contracts.ReportEvent, map[string]int{"restart": i}); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "report acked", func() bool { n, _ := link.OutboxLen(ctx); return n == 0 })
		if err := link.SendStatus(ctx, contracts.NodeStatusReport{Resources: contracts.NodeResources{CPUCount: 16, ActiveRuns: i}}); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "status applied", func() bool { return nodeState().Resources.CPUCount == 16 })
		stop()
		waitFor(t, "disconnected", func() bool { return nodeState().Connection.State == contracts.ConnDisconnected })
	}
	n := nodeState()
	if n.Connection.LastSeenAt == nil || time.Since(*n.Connection.LastSeenAt) > 5*time.Second {
		t.Fatalf("last_seen inaccurate: %v", n.Connection.LastSeenAt)
	}
	if n.Versions["pyred"] != "0.1.0" || len(n.Harnesses) != 1 || n.Harnesses[0].Harness != contracts.HarnessCodex {
		t.Fatalf("hello data not recorded: %+v", n)
	}
	page, _ := f.fleet.ListNodes(ownerCtx, contracts.ListNodesRequest{IncludeRevoked: true})
	if len(page.Items) != 1 || page.Items[0].NodeID != cred.NodeID {
		t.Fatalf("nodes after reconnects: %d", len(page.Items))
	}
	if len(core.committed) != 3 {
		t.Fatalf("committed reports: %d", len(core.committed))
	}

	// Hub restart while the node is connected: registry resets, node comes back.
	link, _ := startLink()
	waitFor(t, "connected", link.Connected)
	hub.stop()
	waitFor(t, "node notices", func() bool { return !link.Connected() })
	hub = startHubServer(t, f, core, addr, logger)
	waitFor(t, "reconnected after hub restart", func() bool { return link.Connected() && nodeState().Connection.State == contracts.ConnConnected })

	// Owner revokes the node: live connection closes, node rejected.
	if _, err := f.fleet.RevokeNode(ownerCtx, contracts.RevokeRequest{Mutation: contracts.Mutation{OperationID: contracts.NewID("op")}, ID: cred.NodeID}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "revoked node rejected", func() bool {
		st := link.Status()
		return st.State == transport.LinkRejected && !hub.ns.Connected(cred.NodeID)
	})
	if nodeState().Connection.State != contracts.ConnRevoked {
		t.Fatalf("state after revoke: %s", nodeState().Connection.State)
	}
	time.Sleep(300 * time.Millisecond) // a few rejected reconnect attempts
	if hub.ns.Connected(cred.NodeID) || nodeState().Connection.State != contracts.ConnRevoked {
		t.Fatal("revoked node came back")
	}

	// Secrets never reach logs.
	all := logs.String()
	for _, s := range []string{tk.Secret, cred.Token} {
		if strings.Contains(all, s[4:]) {
			t.Fatalf("secret leaked into logs")
		}
	}
	var payload []byte
	_ = f.db.QueryRow(`SELECT response FROM operations LIMIT 1`).Scan(&payload)
	var anyMap map[string]any
	_ = json.Unmarshal(payload, &anyMap)
	if strings.Contains(string(payload), tk.Secret) {
		t.Fatal("secret stored in operations")
	}
}

// An enrollment ticket that has not been redeemed is listed as a node in
// state "never" (labelled pending) until it is redeemed, expires or is
// revoked; pagination merges it in node-ID order.
func TestPendingEnrollmentListedAsNever(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)}
	f := newFx(t, clock, "https://hub.test:7443")
	enrolled, _ := f.enroll(t, "server")
	mk := func(name string) contracts.EnrollmentTicket {
		tk, err := f.fleet.CreateEnrollment(ownerCtx, contracts.EnrollmentRequest{Mutation: contracts.Mutation{OperationID: contracts.NewID("op")}, Name: name, SSHAlias: name + "-ssh"})
		if err != nil {
			t.Fatal(err)
		}
		return tk
	}
	pending := mk("laptop")
	expiring := mk("laptop")
	list := func(limit int) map[string]contracts.Node {
		t.Helper()
		out := map[string]contracts.Node{}
		cur := ""
		for i := 0; i < 10; i++ {
			p, err := f.fleet.ListNodes(ownerCtx, contracts.ListNodesRequest{PageRequest: contracts.PageRequest{Cursor: cur, Limit: limit}})
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Items) > limit {
				t.Fatalf("page over limit: %d", len(p.Items))
			}
			for _, n := range p.Items {
				if _, dup := out[n.NodeID]; dup {
					t.Fatalf("node %s repeated", n.NodeID)
				}
				out[n.NodeID] = n
			}
			if p.NextCursor == "" {
				return out
			}
			cur = p.NextCursor
		}
		t.Fatal("pagination did not end")
		return nil
	}
	got := list(1)
	if len(got) != 3 {
		t.Fatalf("listed %d nodes, want 3: %v", len(got), got)
	}
	pn := got[pending.NodeID]
	if pn.Connection.State != contracts.ConnNever || pn.Name != "laptop" || pn.SSHAlias != "laptop-ssh" || pn.Labels[contracts.NodeLabelEnrollment] != "pending" || pn.Harnesses == nil {
		t.Fatalf("pending node: %+v", pn)
	}
	if got[enrolled.NodeID].Labels[contracts.NodeLabelEnrollment] != "" {
		t.Fatalf("redeemed node labelled pending: %+v", got[enrolled.NodeID])
	}
	if n, err := f.fleet.GetNode(ownerCtx, contracts.GetRequest{ID: pending.NodeID}); err != nil || n.Connection.State != contracts.ConnNever {
		t.Fatalf("get pending: %+v %v", n, err)
	}
	// Revoking a pending node invalidates its ticket.
	rn, err := f.fleet.RevokeNode(ownerCtx, contracts.RevokeRequest{Mutation: contracts.Mutation{OperationID: contracts.NewID("op")}, ID: expiring.NodeID})
	if err != nil || rn.Connection.State != contracts.ConnRevoked {
		t.Fatalf("revoke pending: %+v %v", rn, err)
	}
	if _, err := f.enr.RedeemNode(context.Background(), contracts.NodeEnrollExchange{NodeID: expiring.NodeID, Secret: expiring.Secret, Platform: "linux", Arch: "amd64"}); err == nil {
		t.Fatal("revoked ticket redeemed")
	}
	if got = list(50); len(got) != 2 {
		t.Fatalf("after revoke: %v", got)
	}
	// Redemption turns the pending entry into the real node (no duplicate).
	if _, err := f.enr.RedeemNode(context.Background(), contracts.NodeEnrollExchange{NodeID: pending.NodeID, Secret: pending.Secret, Hostname: "laptop.lan", Platform: "darwin", Arch: "arm64"}); err != nil {
		t.Fatal(err)
	}
	got = list(50)
	if len(got) != 2 || got[pending.NodeID].Platform != "darwin" || got[pending.NodeID].Labels[contracts.NodeLabelEnrollment] != "" {
		t.Fatalf("after redeem: %v", got)
	}
	// Expired tickets disappear.
	late := mk("late")
	clock.Advance(2 * time.Hour)
	if _, ok := list(50)[late.NodeID]; ok {
		t.Fatal("expired ticket still listed")
	}
	if _, err := f.fleet.GetNode(ownerCtx, contracts.GetRequest{ID: late.NodeID}); !contracts.IsCode(err, contracts.ErrNotFound) {
		t.Fatalf("expired get: %v", err)
	}
}

// A node whose clock runs ahead (or behind) must not make the hub's view
// fresher or older than it is: every timestamp the registry stores comes
// from the hub clock, a skewed node still turns stale after StaleAfter of
// silence, and its last_seen stays monotonic across reconnects (A01).
func TestNodeClockSkewDoesNotAffectFreshness(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)}
	f := newFx(t, clock, "https://hub.test:7443")
	tk, _ := f.enroll(t, "skewed")
	ctx := context.Background()
	hello := contracts.NodeHello{NodeID: tk.NodeID, Hostname: "skewed.lan", Platform: "linux", Arch: "amd64"}
	var lastSeen []time.Time
	seen := func() contracts.Node {
		n, err := f.fleet.GetNode(ownerCtx, contracts.GetRequest{ID: tk.NodeID})
		if err != nil {
			t.Fatal(err)
		}
		if n.Connection.LastSeenAt != nil {
			lastSeen = append(lastSeen, *n.Connection.LastSeenAt)
		}
		return n
	}
	for i, skew := range []time.Duration{10 * time.Minute, -10 * time.Minute} {
		clock.Advance(time.Second)
		hello.Resources = contracts.NodeResources{CPUCount: 4, ObservedAt: clock.Now().Add(skew)}
		if _, err := f.fleet.ObserveHello(ctx, tk.NodeID, transport.ConnInfo{ConnID: contracts.NewID("conn"), ConnectedAt: clock.Now()}, hello); err != nil {
			t.Fatal(err)
		}
		if n := seen(); !n.Resources.ObservedAt.Equal(clock.Now()) {
			t.Fatalf("skew %v: hello resources.observed_at %v, want hub time %v", skew, n.Resources.ObservedAt, clock.Now())
		}
		clock.Advance(5 * time.Second)
		nodeNow := clock.Now().Add(skew) // what the node's own clock says
		if err := f.fleet.ObserveStatus(ctx, tk.NodeID, contracts.NodeStatusReport{Resources: contracts.NodeResources{CPUCount: 4, ObservedAt: nodeNow}}); err != nil {
			t.Fatal(err)
		}
		n := seen()
		if !n.Resources.ObservedAt.Equal(clock.Now()) {
			t.Fatalf("skew %v: resources.observed_at %v, want hub time %v", skew, n.Resources.ObservedAt, clock.Now())
		}
		if !n.Connection.LastSeenAt.Equal(clock.Now()) {
			t.Fatalf("skew %v: last_seen %v, want %v", skew, n.Connection.LastSeenAt, clock.Now())
		}
		if fr := NodeFreshness(n, clock.Now()); fr.Stale || fr.ObservedAt.After(clock.Now()) {
			t.Fatalf("skew %v: fresh node: %+v", skew, fr)
		}
		// Silence for longer than StaleAfter: stale even though the node's
		// clock claimed an observation 10 minutes in the future.
		clock.Advance(StaleAfter + time.Second)
		if fr := NodeFreshness(seen(), clock.Now()); !fr.Stale {
			t.Fatalf("skew %v: silent node not stale: %+v", skew, fr)
		}
		// Disconnect: last_seen is the hub's last-frame time; offline is
		// never presented as an observation newer than that frame.
		frame := clock.Now().Add(-2 * time.Second)
		var connID string
		if err := f.db.QueryRow(`SELECT conn_id FROM fleet_nodes WHERE node_id=?`, tk.NodeID).Scan(&connID); err != nil {
			t.Fatal(err)
		}
		if changed, err := f.fleet.ObserveDisconnect(ctx, tk.NodeID, connID, frame); !changed || err != nil {
			t.Fatalf("disconnect %d: %v %v", i, changed, err)
		}
		n = seen()
		fr := NodeFreshness(n, clock.Now())
		if n.Connection.State != contracts.ConnDisconnected || !fr.Stale || !fr.ObservedAt.Equal(frame) || !strings.Contains(fr.Reason, "last seen") {
			t.Fatalf("skew %v: disconnected freshness %+v (conn %+v)", skew, fr, n.Connection)
		}
	}
	for i := 1; i < len(lastSeen); i++ {
		if lastSeen[i].Before(lastSeen[i-1]) {
			t.Fatalf("last_seen went backwards: %v", lastSeen)
		}
	}
}
