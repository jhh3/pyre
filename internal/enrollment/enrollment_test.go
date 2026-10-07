package enrollment

import (
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
	"github.com/jhh3/pyre/internal/transport"
)

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

var owner = contracts.Principal{Kind: contracts.PrincipalOwner, ID: "cli_owner"}

type fixture struct {
	db    *sql.DB
	auth  *transport.AuthStore
	store *Store
	clock *fakeClock
	cert  *transport.HubCert
}

func newFixture(t *testing.T, rl RateLimitConfig) *fixture {
	t.Helper()
	db, err := sqlitex.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	clock := &fakeClock{t: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)}
	auth, err := transport.NewAuthStore(db, clock)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := transport.EnsureHubCert(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	st, err := NewStore(db, auth, clock, Options{HubURL: "https://hub.test:7443", HubCertSHA256: cert.Fingerprint, RateLimit: rl})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{db: db, auth: auth, store: st, clock: clock, cert: cert}
}

func (f *fixture) ticket(t *testing.T, ttl int) contracts.EnrollmentTicket {
	t.Helper()
	var tk contracts.EnrollmentTicket
	err := sqlitex.Tx(context.Background(), f.db, func(tx *sql.Tx) error {
		var err error
		tk, err = f.store.CreateTicketTx(context.Background(), tx, contracts.EnrollmentRequest{Name: "server", SSHAlias: "server", TTL: ttl}, owner)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func (f *fixture) pairing(t *testing.T, kind string) contracts.PairingTicket {
	t.Helper()
	var p contracts.PairingTicket
	err := sqlitex.Tx(context.Background(), f.db, func(tx *sql.Tx) error {
		var err error
		p, err = f.store.CreatePairingTx(context.Background(), tx, contracts.PairingRequest{ClientName: "pixel", Kind: kind}, owner)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestEnrollmentOneTimeAndExpiry(t *testing.T) {
	f := newFixture(t, RateLimitConfig{Disabled: true})
	ctx := context.Background()
	var hooked []NodeEnrollment
	f.store.SetNodeEnrolledHook(func(_ context.Context, tx *sql.Tx, e NodeEnrollment) error {
		if tx == nil {
			t.Error("hook without tx")
		}
		hooked = append(hooked, e)
		return nil
	})
	tk := f.ticket(t, 0)
	if !strings.HasPrefix(tk.Secret, transport.PrefixEnrollSecret) || tk.HubURL == "" || tk.HubCertSHA256 != f.cert.Fingerprint {
		t.Fatalf("ticket: %+v", tk)
	}
	var stored int
	f.db.QueryRow(`SELECT COUNT(*) FROM enroll_tickets WHERE secret_hash=?`, tk.Secret).Scan(&stored)
	if stored != 0 {
		t.Fatal("plaintext secret stored")
	}
	ex := contracts.NodeEnrollExchange{NodeID: tk.NodeID, Secret: tk.Secret, Hostname: "server", Platform: "linux", Arch: "amd64"}
	cred, err := f.store.RedeemNode(ctx, ex)
	if err != nil || cred.NodeID != tk.NodeID {
		t.Fatalf("redeem: %+v %v", cred, err)
	}
	p, err := f.auth.Authenticate(ctx, cred.Token)
	if err != nil || p.Kind != contracts.PrincipalNode || p.ID != tk.NodeID || p.Name != "server" {
		t.Fatalf("node token: %+v %v", p, err)
	}
	if len(hooked) != 1 || hooked[0].Platform != "linux" || hooked[0].SSHAlias != "server" {
		t.Fatalf("hook: %+v", hooked)
	}
	if _, err := f.store.RedeemNode(ctx, ex); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("second redeem: %v", err)
	}
	// Expiry.
	tk2 := f.ticket(t, 60)
	f.clock.Advance(61 * time.Second)
	if _, err := f.store.RedeemNode(ctx, contracts.NodeEnrollExchange{NodeID: tk2.NodeID, Secret: tk2.Secret}); !contracts.IsCode(err, contracts.ErrExpired) {
		t.Fatalf("expired: %v", err)
	}
	// Wrong secrets are uniform and eventually invalidate the ticket.
	tk3 := f.ticket(t, 0)
	wrong, _ := transport.NewSecret(transport.PrefixEnrollSecret)
	for i := 0; i < MaxFailedAttempts; i++ {
		if _, err := f.store.RedeemNode(ctx, contracts.NodeEnrollExchange{NodeID: tk3.NodeID, Secret: wrong}); !contracts.IsCode(err, contracts.ErrUnauthorized) {
			t.Fatalf("wrong secret: %v", err)
		}
	}
	if _, err := f.store.RedeemNode(ctx, contracts.NodeEnrollExchange{NodeID: tk3.NodeID, Secret: tk3.Secret}); !contracts.IsCode(err, contracts.ErrExpired) {
		t.Fatalf("invalidated ticket accepted: %v", err)
	}
	if _, err := f.store.RedeemNode(ctx, contracts.NodeEnrollExchange{NodeID: "node_unknown", Secret: wrong}); !contracts.IsCode(err, contracts.ErrUnauthorized) {
		t.Fatalf("unknown node: %v", err)
	}
	// A failing hook aborts redemption entirely (no credential minted).
	f.store.SetNodeEnrolledHook(func(context.Context, *sql.Tx, NodeEnrollment) error { return errors.New("boom") })
	tk4 := f.ticket(t, 0)
	if _, err := f.store.RedeemNode(ctx, contracts.NodeEnrollExchange{NodeID: tk4.NodeID, Secret: tk4.Secret}); err == nil {
		t.Fatal("hook failure ignored")
	}
	var n int
	f.db.QueryRow(`SELECT COUNT(*) FROM auth_tokens WHERE principal_id=?`, tk4.NodeID).Scan(&n)
	if n != 0 {
		t.Fatal("credential minted despite aborted redemption")
	}
	f.store.SetNodeEnrolledHook(nil)
	if _, err := f.store.RedeemNode(ctx, contracts.NodeEnrollExchange{NodeID: tk4.NodeID, Secret: tk4.Secret}); err != nil {
		t.Fatalf("redeem after aborted attempt: %v", err)
	}
}

func TestRotateTicketSecret(t *testing.T) {
	f := newFixture(t, RateLimitConfig{Disabled: true})
	ctx := context.Background()
	tk := f.ticket(t, 0)
	var rotated contracts.EnrollmentTicket
	if err := sqlitex.Tx(ctx, f.db, func(tx *sql.Tx) error {
		var err error
		rotated, err = f.store.RotateTicketSecretTx(ctx, tx, tk.NodeID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if rotated.Secret == tk.Secret || rotated.NodeID != tk.NodeID || !rotated.ExpiresAt.Equal(tk.ExpiresAt) {
		t.Fatalf("rotate: %+v", rotated)
	}
	if _, err := f.store.RedeemNode(ctx, contracts.NodeEnrollExchange{NodeID: tk.NodeID, Secret: tk.Secret}); !contracts.IsCode(err, contracts.ErrUnauthorized) {
		t.Fatalf("old secret still valid: %v", err)
	}
	if _, err := f.store.RedeemNode(ctx, contracts.NodeEnrollExchange{NodeID: tk.NodeID, Secret: rotated.Secret}); err != nil {
		t.Fatal(err)
	}
	err := sqlitex.Tx(ctx, f.db, func(tx *sql.Tx) error {
		_, err := f.store.RotateTicketSecretTx(ctx, tx, tk.NodeID)
		return err
	})
	if !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("rotate after redeem: %v", err)
	}
}

func TestPairingOneTimeExpiryAndURI(t *testing.T) {
	f := newFixture(t, RateLimitConfig{Disabled: true})
	ctx := context.Background()
	p := f.pairing(t, "android")
	u, err := ParsePairingURI(p.URI)
	if err != nil {
		t.Fatal(err)
	}
	if u.HubURL != p.HubURL || u.PinSHA256 != f.cert.Fingerprint || u.PairingID != p.PairingID || u.Code != p.Code {
		t.Fatalf("uri roundtrip: %+v vs %+v", u, p)
	}
	if !strings.HasPrefix(p.URI, "pyre://pair?hub=https%3A%2F%2Fhub.test%3A7443&pin="+f.cert.Fingerprint+"&id="+p.PairingID+"&code=") {
		t.Fatalf("uri format: %s", p.URI)
	}
	cred, err := f.store.RedeemPairing(ctx, contracts.PairingExchange{PairingID: p.PairingID, Code: p.Code, ClientName: "Alice's Pixel"})
	if err != nil {
		t.Fatal(err)
	}
	pr, err := f.auth.Authenticate(ctx, cred.Token)
	if err != nil || pr.Kind != contracts.PrincipalOwner || pr.ID != cred.ClientID {
		t.Fatalf("owner token: %+v %v", pr, err)
	}
	if _, err := f.store.RedeemPairing(ctx, contracts.PairingExchange{PairingID: p.PairingID, Code: p.Code}); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("reuse: %v", err)
	}
	c, err := f.store.GetClient(ctx, cred.ClientID)
	if err != nil || c.Kind != "android" || c.Name != "Alice's Pixel" || c.LastSeenAt == nil {
		t.Fatalf("client: %+v %v", c, err)
	}
	p2 := f.pairing(t, "")
	f.clock.Advance(DefaultPairingTTL + time.Second)
	if _, err := f.store.RedeemPairing(ctx, contracts.PairingExchange{PairingID: p2.PairingID, Code: p2.Code, Kind: "cli"}); !contracts.IsCode(err, contracts.ErrExpired) {
		t.Fatalf("expired pairing: %v", err)
	}
	for _, bad := range []string{"https://x", "pyre://pair?hub=http%3A%2F%2Fx&id=pair_1&code=pyp_x", "pyre://pair?hub=https%3A%2F%2Fx&id=pair_1"} {
		if _, err := ParsePairingURI(bad); err == nil {
			t.Fatalf("accepted bad uri %q", bad)
		}
	}

	// Revoke the client: its token stops working.
	var info contracts.ClientInfo
	if err := sqlitex.Tx(ctx, f.db, func(tx *sql.Tx) error {
		var err error
		info, err = f.store.RevokeClientTx(ctx, tx, cred.ClientID)
		return err
	}); err != nil || !info.Revoked {
		t.Fatalf("revoke: %+v %v", info, err)
	}
	if _, err := f.auth.Authenticate(ctx, cred.Token); !contracts.IsCode(err, contracts.ErrUnauthorized) {
		t.Fatalf("revoked client still authenticates: %v", err)
	}
	boot, err := f.store.BootstrapOwnerClient(ctx, "hub-console", "cli")
	if err != nil {
		t.Fatal(err)
	}
	page, err := f.store.ListClients(ctx, contracts.PageRequest{Limit: 1})
	if err != nil || len(page.Items) != 1 || page.NextCursor == "" {
		t.Fatalf("page1: %+v %v", page, err)
	}
	page2, err := f.store.ListClients(ctx, contracts.PageRequest{Limit: 1, Cursor: page.NextCursor})
	if err != nil || len(page2.Items) != 1 || page2.Items[0].ClientID != boot.ClientID || page2.NextCursor != "" {
		t.Fatalf("page2: %+v %v", page2, err)
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func startHub(t *testing.T, f *fixture, logs io.Writer) *httptest.Server {
	mux := http.NewServeMux()
	f.store.Register(mux, HandlerOptions{Logger: slog.New(slog.NewTextHandler(logs, nil))})
	srv := httptest.NewUnstartedServer(mux)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{f.cert.Certificate}}
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func TestExchangeHandlersOverPinnedTLS(t *testing.T) {
	f := newFixture(t, RateLimitConfig{PerIPBurst: 4, PerIPEvery: time.Hour})
	logs := &syncBuf{}
	srv := startHub(t, f, logs)
	ctx := context.Background()

	tk := f.ticket(t, 0)
	cred, err := ExchangeEnrollment(ctx, srv.URL, f.cert.Fingerprint, contracts.NodeEnrollExchange{NodeID: tk.NodeID, Secret: tk.Secret, Hostname: "mac", Platform: "darwin", Arch: "arm64"})
	if err != nil || cred.NodeID != tk.NodeID {
		t.Fatalf("exchange: %+v %v", cred, err)
	}
	if _, err := ExchangeEnrollment(ctx, srv.URL, f.cert.Fingerprint, contracts.NodeEnrollExchange{NodeID: tk.NodeID, Secret: tk.Secret}); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("replayed exchange: %v", err)
	}
	// Wrong pin refuses to talk to the hub at all.
	other, _ := transport.EnsureHubCert(t.TempDir(), nil)
	if _, err := ExchangeEnrollment(ctx, srv.URL, other.Fingerprint, contracts.NodeEnrollExchange{NodeID: tk.NodeID, Secret: tk.Secret}); !contracts.IsCode(err, contracts.ErrUnavailable) {
		t.Fatalf("wrong pin: %v", err)
	}

	p := f.pairing(t, "cli")
	u, _ := ParsePairingURI(p.URI)
	u.HubURL = srv.URL
	cc, err := ExchangePairing(ctx, u, "laptop", "")
	if err != nil || !strings.HasPrefix(cc.Token, transport.PrefixOwnerToken) {
		t.Fatalf("pair: %v", err)
	}
	// Per-IP rate limit (burst 4: three attempts used above, the pin failure never reached the handler).
	_, err = ExchangePairing(ctx, u, "laptop", "")
	if !contracts.IsCode(err, contracts.ErrCapacityExhausted) && !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("unexpected: %v", err)
	}
	_, err = ExchangePairing(ctx, u, "laptop", "")
	if !contracts.IsCode(err, contracts.ErrCapacityExhausted) {
		t.Fatalf("rate limit not enforced: %v", err)
	}
	for _, s := range []string{tk.Secret, cred.Token, p.Code, cc.Token} {
		if strings.Contains(logs.String(), s[4:]) {
			t.Fatalf("secret in logs:\n%s", logs.String())
		}
	}
	if !strings.Contains(logs.String(), "node enrolled") {
		t.Fatalf("expected audit log, got:\n%s", logs.String())
	}
}

func TestCredentialFilePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "node-credential.json")
	tok, _ := transport.NewSecret(transport.PrefixNodeToken)
	c := NodeCredentialFile{HubURL: "https://hub:7443", HubCertSHA256: strings.Repeat("a", 64), NodeID: "node_1", Token: tok, EnrolledAt: time.Now().UTC()}
	if err := SaveNodeCredential(path, c); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", st.Mode().Perm())
	}
	dst, _ := os.Stat(filepath.Dir(path))
	if dst.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %o", dst.Mode().Perm())
	}
	got, err := LoadNodeCredential(path)
	if err != nil || got.Token != tok || got.Version != CredentialFileVersion {
		t.Fatalf("load: %+v %v", got, err)
	}
	os.Chmod(path, 0o644)
	if _, err := LoadNodeCredential(path); err == nil || !strings.Contains(err.Error(), "chmod 600") || strings.Contains(err.Error(), tok) {
		t.Fatalf("world-readable accepted or leaked: %v", err)
	}
	os.Chmod(path, 0o600)
	link := filepath.Join(dir, "link.json")
	os.Symlink(path, link)
	if _, err := LoadNodeCredential(link); err == nil {
		t.Fatal("symlink accepted")
	}
	if err := SaveNodeCredential(path, NodeCredentialFile{NodeID: "node_1", Token: "nope", HubURL: "x"}); err == nil {
		t.Fatal("invalid credential saved")
	}
	ctok, _ := transport.NewSecret(transport.PrefixOwnerToken)
	cpath := filepath.Join(dir, "client.json")
	if err := SaveClientCredential(cpath, ClientCredentialFile{HubURL: "https://hub", ClientID: "cli_1", Token: ctok}); err != nil {
		t.Fatal(err)
	}
	if cc, err := LoadClientCredential(cpath); err != nil || cc.Token != ctok {
		t.Fatalf("client load: %v", err)
	}
}

type fakeRunner struct {
	calls  [][]string
	stdins []string
	failAt int
}

func (r *fakeRunner) Run(_ context.Context, argv []string, stdin io.Reader, stdout, _ io.Writer) error {
	b, _ := io.ReadAll(stdin)
	r.calls = append(r.calls, argv)
	r.stdins = append(r.stdins, string(b))
	if string(b) != "" {
		io.WriteString(stdout, "remote echoed "+strings.TrimSpace(string(b))+"\n")
	}
	if r.failAt > 0 && len(r.calls) == r.failAt {
		return errors.New("exit status 255")
	}
	return nil
}

func TestSSHBootstrapPlanAndExecute(t *testing.T) {
	sec, _ := transport.NewSecret(transport.PrefixEnrollSecret)
	pin := strings.Repeat("ab", 32)
	tk := contracts.EnrollmentTicket{NodeID: "node_01TEST", Secret: sec, HubURL: "https://hub.tail:7443", HubCertSHA256: pin}
	plan, err := SSHBootstrapPlan("server", "/tmp/build/pyred-linux-amd64", "", "", tk, "")
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"ssh", "server", `umask 077 && mkdir -p "$HOME"/.local/share/pyre/bin`},
		{"scp", "-q", "/tmp/build/pyred-linux-amd64", "server:.local/share/pyre/bin/.pyred.new"},
		{"ssh", "server", `chmod 700 "$HOME"/.local/share/pyre/bin/.pyred.new && mv -f "$HOME"/.local/share/pyre/bin/.pyred.new "$HOME"/.local/share/pyre/bin/pyred && "$HOME"/.local/share/pyre/bin/pyred version`},
		{"ssh", "server", `"$HOME"/.local/share/pyre/bin/pyred enroll --hub 'https://hub.tail:7443' --pin ` + pin + ` --node-id node_01TEST --credential-file "$HOME"/.local/share/pyre/node-credential.json --secret-stdin`},
	}
	if len(plan.Steps) != len(want) {
		t.Fatalf("steps = %d", len(plan.Steps))
	}
	for i, s := range plan.Steps {
		if strings.Join(s.Argv, "\x00") != strings.Join(want[i], "\x00") {
			t.Fatalf("step %d argv:\n got %q\nwant %q", i, s.Argv, want[i])
		}
		joined := strings.Join(s.Argv, " ")
		if strings.Contains(joined, "StrictHostKeyChecking") || strings.Contains(joined, "UserKnownHostsFile") || strings.Contains(joined, sec[4:]) {
			t.Fatalf("step %d unsafe: %s", i, joined)
		}
	}
	if plan.Steps[3].Stdin != sec+"\n" {
		t.Fatal("secret not passed on stdin")
	}

	var out bytes.Buffer
	r := &fakeRunner{}
	if err := Execute(context.Background(), plan, r, ExecuteOptions{DryRun: true, Out: &out}); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 0 || strings.Contains(out.String(), sec[4:]) || !strings.Contains(out.String(), "secret on stdin") {
		t.Fatalf("dry run: calls=%d out=%s", len(r.calls), out.String())
	}
	out.Reset()
	if err := Execute(context.Background(), plan, r, ExecuteOptions{Out: &out}); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 4 || r.stdins[3] != sec+"\n" || r.stdins[0] != "" {
		t.Fatalf("execute: %d calls", len(r.calls))
	}
	if strings.Contains(out.String(), sec[4:]) {
		t.Fatalf("secret echoed to output: %s", out.String())
	}
	r2 := &fakeRunner{failAt: 2}
	if err := Execute(context.Background(), plan, r2, ExecuteOptions{}); err == nil || len(r2.calls) != 2 {
		t.Fatalf("failure did not stop: %v calls=%d", err, len(r2.calls))
	}

	for _, bad := range []struct{ alias, dir, hub string }{
		{"-oProxyCommand=evil", "", ""}, {"server extra", "", ""}, {"server", "rel/path", ""}, {"server", "~/../etc", ""},
		{"server", "~/a b", ""}, {"server", "", "http://insecure"},
	} {
		if _, err := SSHBootstrapPlan(bad.alias, "/bin/pyred", bad.hub, "", tk, bad.dir); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	abs, err := SSHBootstrapPlan("mac-mini", "/bin/pyred", "", "", tk, "/opt/pyre/")
	if err != nil || abs.RemoteBinary != "/opt/pyre/bin/pyred" || abs.Steps[1].Argv[3] != "mac-mini:/opt/pyre/bin/.pyred.new" {
		t.Fatalf("abs dir: %+v %v", abs, err)
	}
}
