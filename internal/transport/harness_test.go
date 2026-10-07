package transport

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
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)} }
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func openDB(t *testing.T, name string) *sql.DB {
	t.Helper()
	db, err := sqlitex.Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
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

// fakeHub is a stand-in for the hub's durable report/command store. Its
// state survives NodeServer restarts, like the hub database would.
type fakeHub struct {
	mu          sync.Mutex
	committed   map[uint64]contracts.NodeReport
	received    []uint64 // durable seqs received (including duplicates), per server generation reset by test
	statuses    int
	results     []contracts.CommandResult
	acks        []contracts.CommandAck
	commands    []contracts.NodeCommand
	resendAll   bool
	hellos      []contracts.NodeHello
	disconnects []DisconnectInfo
}

func newFakeHub() *fakeHub { return &fakeHub{committed: map[uint64]contracts.NodeReport{}} }

func (h *fakeHub) maxCommitted() uint64 {
	var m uint64
	for s := range h.committed {
		m = max(m, s)
	}
	return m
}

func (h *fakeHub) onHello(_ context.Context, _ string, hello contracts.NodeHello, _ ConnInfo) (WelcomeInfo, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hellos = append(h.hellos, hello)
	var pending []contracts.NodeCommand
	for _, c := range h.commands {
		if h.resendAll || c.Seq > hello.LastCommandSeq {
			pending = append(pending, c)
		}
	}
	return WelcomeInfo{CommittedReportSeq: h.maxCommitted(), PendingCommands: pending}, nil
}

func (h *fakeHub) onReport(_ context.Context, _ string, r contracts.NodeReport) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if r.OriginSeq == 0 {
		h.statuses++
		return nil
	}
	h.received = append(h.received, r.OriginSeq)
	if _, dup := h.committed[r.OriginSeq]; dup {
		return nil
	}
	h.committed[r.OriginSeq] = r
	if r.Kind == contracts.ReportCommandResult {
		var res contracts.CommandResult
		if err := json.Unmarshal(r.Payload, &res); err == nil {
			h.results = append(h.results, res)
		}
	}
	return nil
}

func (h *fakeHub) onAck(_ context.Context, _ string, a contracts.CommandAck) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.acks = append(h.acks, a)
	return nil
}

func (h *fakeHub) onDisconnect(_ context.Context, _ string, d DisconnectInfo) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.disconnects = append(h.disconnects, d)
}

func (h *fakeHub) snapshot(fn func(h *fakeHub)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	fn(h)
}

type env struct {
	t      *testing.T
	auth   *AuthStore
	cert   *HubCert
	hub    *fakeHub
	nodeID string
	token  string
	logs   *syncBuffer
	logger *slog.Logger
	addr   string
	ns     *NodeServer
	srv    *httptest.Server
	hbInt  time.Duration
}

func newEnv(t *testing.T) *env {
	t.Helper()
	auth, err := NewAuthStore(openDB(t, "hub.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := EnsureHubCert(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	nodeID := contracts.NewID(contracts.PrefixNode)
	tok, err := auth.Issue(context.Background(), contracts.PrincipalNode, nodeID, "test-node")
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	e := &env{t: t, auth: auth, cert: cert, hub: newFakeHub(), nodeID: nodeID, token: tok, logs: logs,
		logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})), hbInt: time.Second}
	t.Cleanup(e.stop)
	return e
}

func (e *env) start() {
	e.t.Helper()
	ns, err := NewNodeServer(NodeServerConfig{
		Auth: e.auth, HubID: "hub_test", HeartbeatInterval: e.hbInt, Logger: e.logger,
		OnHello: e.hub.onHello, OnReport: e.hub.onReport, OnCommandAck: e.hub.onAck, OnDisconnect: e.hub.onDisconnect,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle(contracts.PathNodeConnect, ns)
	addr := e.addr
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	var l net.Listener
	waitFor(e.t, "listen "+addr, func() bool {
		l, err = net.Listen("tcp", addr)
		return err == nil
	})
	srv := httptest.NewUnstartedServer(mux)
	srv.Listener = l
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{e.cert.Certificate}}
	srv.StartTLS()
	e.addr = l.Addr().String()
	e.ns, e.srv = ns, srv
}

func (e *env) stop() {
	if e.ns != nil {
		e.ns.Close()
		e.srv.Close()
		e.ns, e.srv = nil, nil
	}
}

func (e *env) url() string { return "https://" + e.addr }

type countingHandler struct {
	mu    sync.Mutex
	calls map[string]int
	fn    func(contracts.NodeCommand) (contracts.CommandResult, error)
}

func (c *countingHandler) HandleCommand(_ context.Context, cmd contracts.NodeCommand) (contracts.CommandResult, error) {
	c.mu.Lock()
	if c.calls == nil {
		c.calls = map[string]int{}
	}
	c.calls[cmd.CommandID]++
	c.mu.Unlock()
	if c.fn != nil {
		return c.fn(cmd)
	}
	return contracts.CommandResult{Outcome: contracts.OutcomeSucceeded, Result: json.RawMessage(`{"ok":true}`)}, nil
}

func (c *countingHandler) count(id string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[id]
}

func (e *env) newLink(db *sql.DB, h CommandHandler, mod func(*NodeLinkConfig)) *NodeLink {
	e.t.Helper()
	tlsCfg, err := PinnedTLSConfig(e.cert.Fingerprint)
	if err != nil {
		e.t.Fatal(err)
	}
	cfg := NodeLinkConfig{
		HubURL: e.url(), NodeID: e.nodeID, Token: e.token, TLS: tlsCfg, DB: db, Handler: h, Logger: e.logger,
		Hello: func(context.Context) (contracts.NodeHello, error) {
			return contracts.NodeHello{Hostname: "testhost", Platform: "linux", Arch: "amd64", PyredVersion: "test"}, nil
		},
		Backoff:     BackoffConfig{Initial: 20 * time.Millisecond, Max: 200 * time.Millisecond},
		StableAfter: time.Hour,
	}
	if mod != nil {
		mod(&cfg)
	}
	l, err := NewNodeLink(cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	return l
}

func runLink(t *testing.T, l *NodeLink) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := l.Run(ctx); err != nil {
			t.Errorf("link run: %v", err)
		}
	}()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
	t.Cleanup(stop)
	return stop
}
