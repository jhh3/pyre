package security

import (
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/artifacts"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/diagnostics"
	"github.com/jhh3/pyre/internal/enrollment"
	"github.com/jhh3/pyre/internal/httpapi"
	"github.com/jhh3/pyre/internal/projects"
	"github.com/jhh3/pyre/internal/runtime/mock"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
	"github.com/jhh3/pyre/internal/topology"
	"github.com/jhh3/pyre/internal/transport"
)

// base lifts the mock one embedding level down so the real stores win.
type base struct{ contracts.API }

type (
	topologyStore  = topology.Store
	projectsStore  = projects.Store
	artifactsStore = artifacts.Store
)

// composite is the security target until the WP05 hub exists: real
// fleet/enrollment (topology), projects and artifacts over one SQLite file;
// sessions/messages/approvals from the deterministic mock (whose
// authorization semantics are pinned by internal/conformance).
type composite struct {
	base
	*topologyStore
	*projectsStore
	*artifactsStore
}

var _ contracts.API = (*composite)(nil)

// sink appends events to a table inside the caller's transaction so the
// payloads are part of the scanned database.
type sink struct{}

func (sink) AppendTx(ctx context.Context, tx *sql.Tx, ev contracts.Event) (contracts.Event, error) {
	b, err := json.Marshal(ev)
	if err != nil {
		return ev, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO sec_events(kind, body) VALUES(?,?)`, string(ev.Kind), b)
	if err != nil {
		return ev, err
	}
	ev.Cursor, _ = res.LastInsertId()
	return ev, nil
}

type fixture struct {
	t        *testing.T
	dir      string
	dbPath   string
	db       *sql.DB
	auth     *transport.AuthStore
	enr      *enrollment.Store
	fleet    *topology.Store
	proj     *projects.Store
	arts     *artifacts.Store
	mock     *mock.Backend
	ns       *transport.NodeServer
	logs     *diagnostics.Ring
	srv      *httptest.Server // management API (TLS)
	preview  *httptest.Server // separate preview origin (TLS)
	pin      string
	owner    contracts.ClientCredential
	clock    *fakeClock
	secrets  []string // every secret minted during the test
	secretMu sync.Mutex
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

type fixtureOpts struct {
	rate     enrollment.RateLimitConfig
	artOpts  artifacts.Options
	hbPeriod time.Duration
}

func newFixture(t *testing.T, o fixtureOpts) *fixture {
	t.Helper()
	ctx := context.Background()
	f := &fixture{t: t, dir: t.TempDir(), logs: diagnostics.NewRing(20000, 8<<20, nil), clock: &fakeClock{t: time.Now().UTC()}}
	f.dbPath = filepath.Join(f.dir, "hub.db")
	db, err := sqlitex.Open(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	f.db = db
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS sec_events (cursor INTEGER PRIMARY KEY AUTOINCREMENT, kind TEXT NOT NULL, body BLOB NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	if f.auth, err = transport.NewAuthStore(db, f.clock); err != nil {
		t.Fatal(err)
	}
	if f.enr, err = enrollment.NewStore(db, f.auth, f.clock, enrollment.Options{HubURL: "https://hub.invalid", RateLimit: o.rate}); err != nil {
		t.Fatal(err)
	}
	if f.fleet, err = topology.NewStore(db, f.auth, f.enr, sink{}, f.clock); err != nil {
		t.Fatal(err)
	}
	if f.proj, err = projects.New(ctx, db, sink{}, projects.Options{}); err != nil {
		t.Fatal(err)
	}
	o.artOpts.DataDir = filepath.Join(f.dir, "artifacts")
	o.artOpts.Projects = f.proj
	if f.arts, err = artifacts.New(ctx, db, sink{}, o.artOpts); err != nil {
		t.Fatal(err)
	}
	f.mock = mock.New(mock.Options{Clock: mock.NewFakeClock(mock.FixtureEpoch)})

	if o.hbPeriod == 0 {
		o.hbPeriod = 200 * time.Millisecond
	}
	cfg, err := f.fleet.NodeServerConfig(topology.HubHooks{
		Welcome: func(context.Context, string, contracts.NodeHello) (transport.WelcomeInfo, error) {
			return transport.WelcomeInfo{}, nil
		},
		OnReport: func(context.Context, string, contracts.NodeReport) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Auth, cfg.HubID, cfg.Logger, cfg.HeartbeatInterval = f.auth, "hub_security", logger, o.hbPeriod
	if f.ns, err = transport.NewNodeServer(cfg); err != nil {
		t.Fatal(err)
	}
	f.ns.DisconnectOnRevoke(f.auth)

	extra := map[string]http.Handler{
		contracts.PathNodeConnect:    f.ns,
		contracts.PathEnrollExchange: f.enr.EnrollExchangeHandler(enrollment.HandlerOptions{Logger: logger}),
		contracts.PathPairExchange:   f.enr.PairExchangeHandler(enrollment.HandlerOptions{Logger: logger}),
	}
	api := &composite{base{f.mock}, f.fleet, f.proj, f.arts}
	f.srv = httptest.NewUnstartedServer(httpapi.NewServer(api, f.auth, httpapi.Options{Extra: extra, Logger: logger, StreamWait: time.Second}))
	f.srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	f.srv.StartTLS()
	f.pin = transport.CertFingerprint(f.srv.Certificate().Raw)

	pmux := http.NewServeMux()
	pmux.Handle("/preview/", artifacts.PreviewHandler(f.arts, f.auth))
	f.preview = httptest.NewUnstartedServer(pmux)
	f.preview.Config.ErrorLog = log.New(io.Discard, "", 0)
	f.preview.StartTLS()

	t.Cleanup(func() {
		f.ns.Close()
		f.srv.Close()
		f.preview.Close()
	})
	if f.owner, err = f.enr.BootstrapOwnerClient(ctx, "security-owner", "cli"); err != nil {
		t.Fatal(err)
	}
	f.remember(f.owner.Token)
	return f
}

func (f *fixture) remember(secrets ...string) {
	f.secretMu.Lock()
	defer f.secretMu.Unlock()
	f.secrets = append(f.secrets, secrets...)
}

// scanner returns a scanner that knows every secret minted in this test.
func (f *fixture) scanner() *diagnostics.Scanner {
	f.secretMu.Lock()
	defer f.secretMu.Unlock()
	s := diagnostics.NewScanner().AddKnown("minted", f.secrets...)
	s.IgnorePII = true
	return s
}

// httpClient trusts the fixture servers' certificates.
func (f *fixture) httpClient() *http.Client {
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, Timeout: 10 * time.Second} //nolint:gosec // test servers
}

type rpcResult struct {
	status int
	header http.Header
	body   []byte
	err    *contracts.Error
}

// rpc posts a raw JSON body to /v1/<op> with an optional bearer token.
func (f *fixture) rpc(token, op string, body any) rpcResult {
	f.t.Helper()
	var raw []byte
	switch b := body.(type) {
	case nil:
		raw = []byte("{}")
	case string:
		raw = []byte(b)
	case []byte:
		raw = b
	default:
		var err error
		if raw, err = json.Marshal(b); err != nil {
			f.t.Fatal(err)
		}
	}
	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+contracts.RPCPath(op), bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(contracts.ProtocolHeader, strconv.Itoa(contracts.ProtocolVersion))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return f.do(req)
}

func (f *fixture) do(req *http.Request) rpcResult {
	f.t.Helper()
	resp, err := f.httpClient().Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	r := rpcResult{status: resp.StatusCode, header: resp.Header, body: b}
	var eb contracts.ErrorBody
	if resp.StatusCode >= 300 && json.Unmarshal(b, &eb) == nil {
		r.err = eb.Error
	}
	return r
}

// post sends an unauthenticated JSON POST to a public path.
func (f *fixture) post(path string, body any) rpcResult {
	f.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		f.t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	return f.do(req)
}

func mustJSON(t *testing.T, b []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
}

func (r rpcResult) code() contracts.ErrorCode {
	if r.err == nil {
		return ""
	}
	return r.err.Code
}

func mockApproval() mock.ApprovalSpec { return mock.ApprovalSpec{TTL: time.Minute} }

func op() contracts.Mutation {
	return contracts.Mutation{OperationID: contracts.NewID(contracts.PrefixOperation)}
}

var ownerP = contracts.Principal{Kind: contracts.PrincipalOwner, ID: "cli_security-direct"}

func ownerCtx() context.Context { return contracts.WithPrincipal(context.Background(), ownerP) }

// enrollNode creates and redeems an enrollment through the real stores.
func (f *fixture) enrollNode(name string) (nodeID, token string) {
	f.t.Helper()
	ctx := contracts.WithPrincipal(context.Background(), contracts.Principal{Kind: contracts.PrincipalOwner, ID: f.owner.ClientID})
	tk, err := f.fleet.CreateEnrollment(ctx, contracts.EnrollmentRequest{Mutation: op(), Name: name})
	if err != nil {
		f.t.Fatal(err)
	}
	f.remember(tk.Secret)
	cred, err := f.enr.RedeemNode(context.Background(), contracts.NodeEnrollExchange{NodeID: tk.NodeID, Secret: tk.Secret, Hostname: name, Platform: "linux", Arch: "amd64"})
	if err != nil {
		f.t.Fatal(err)
	}
	f.remember(cred.Token)
	return cred.NodeID, cred.Token
}

// startSession launches a mock session (owner) and mints its session token.
func (f *fixture) startSession(title string) (contracts.SessionView, string) {
	f.t.Helper()
	res, err := f.mock.StartSession(ownerCtx(), contracts.StartSessionRequest{Mutation: op(), NodeID: "node_synth-mac", Harness: contracts.HarnessMock, Cwd: "/tmp/pyre-security", Title: title})
	if err != nil {
		f.t.Fatal(err)
	}
	if res.Session.Run != nil {
		_ = f.mock.SetRunState(res.Session.Run.RunID, contracts.RunWaitingInput, "")
	}
	tok, err := f.auth.IssueSessionToken(context.Background(), res.Session.Session.SessionID)
	if err != nil {
		f.t.Fatal(err)
	}
	f.remember(tok)
	return res.Session, tok
}

// assertNoLeaks scans logs, the hub database (+WAL/SHM), the artifact store
// and any extra blobs for secrets minted during the test.
func (f *fixture) assertNoLeaks(extra map[string][]byte) {
	f.t.Helper()
	s := f.scanner()
	var findings []diagnostics.Finding
	findings = append(findings, diagnostics.Secrets(s.ScanString("hub-logs", f.logs.String()))...)
	// Checkpoint so the main file is scanned with current pages too; the
	// WAL is scanned either way.
	_, _ = f.db.Exec(`PRAGMA wal_checkpoint(PASSIVE)`)
	dbf, err := s.ScanSQLite(f.dbPath)
	if err != nil {
		f.t.Fatal(err)
	}
	findings = append(findings, diagnostics.Secrets(dbf)...)
	af, err := s.ScanDir(filepath.Join(f.dir, "artifacts"))
	if err != nil {
		f.t.Fatal(err)
	}
	findings = append(findings, diagnostics.Secrets(af)...)
	for name, b := range extra {
		findings = append(findings, diagnostics.Secrets(s.ScanBytes(name, b))...)
	}
	if len(findings) > 0 {
		var lines []string
		for _, x := range findings {
			lines = append(lines, x.String())
		}
		f.t.Fatalf("secret material found:\n%s", strings.Join(lines, "\n"))
	}
	if st := f.logs.Stats(); st.TotalLines == 0 {
		f.t.Fatal("no log lines captured; the leak scan would be vacuous")
	}
}
