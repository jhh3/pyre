package cli

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/client"
	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/enrollment"
	"github.com/jhh3/pyre/internal/httpapi"
)

func TestResolvePrecedence(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "client.json")
	if err := SaveConfig(cfgPath, ClientConfig{HubURL: "https://hub.synthetic:7788", HubCertSHA256: strings.Repeat("ab", 32), Token: "pyc_config-token-synthetic"}); err != nil {
		t.Fatal(err)
	}
	base := env{EnvConfig: cfgPath, "HOME": dir}
	resolve := func(e env, hubFlag string) *Connection {
		t.Helper()
		conn, err := Resolve(ConnOptions{HubFlag: hubFlag, Getenv: e.get})
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		return conn
	}

	// Config only.
	conn := resolve(base, "")
	if conn.Mode != "hub" || conn.HubURL != "https://hub.synthetic:7788" || !strings.HasPrefix(conn.Source, "config ") || conn.TokenSource != "config" {
		t.Fatalf("config: %+v", conn)
	}
	// Socket beats config; config token still usable locally.
	conn = resolve(base.with(EnvSocket, "/tmp/pyre-synthetic.sock"), "")
	if conn.Mode != "socket" || conn.Socket != "/tmp/pyre-synthetic.sock" || conn.TokenSource != "config" {
		t.Fatalf("socket: %+v", conn)
	}
	// PYRE_HUB beats socket. The config token is NOT sent to another hub.
	conn = resolve(base.with(EnvSocket, "/tmp/s.sock", EnvHub, "http://other.synthetic:1"), "")
	if conn.Mode != "hub" || conn.HubURL != "http://other.synthetic:1" || conn.Source != EnvHub || conn.TokenSource != "none" {
		t.Fatalf("PYRE_HUB: %+v", conn)
	}
	// ...unless it is the configured hub (trailing slash tolerated).
	conn = resolve(base.with(EnvHub, "https://hub.synthetic:7788/"), "")
	if conn.TokenSource != "config" {
		t.Fatalf("same hub should use config token: %+v", conn)
	}
	// --hub beats PYRE_HUB.
	conn = resolve(base.with(EnvHub, "http://other.synthetic:1"), "http://flag.synthetic:2")
	if conn.HubURL != "http://flag.synthetic:2" || conn.Source != "--hub" {
		t.Fatalf("--hub: %+v", conn)
	}
	// Token precedence: session token > PYRE_TOKEN > config.
	conn = resolve(base.with(EnvToken, "explicit"), "")
	if conn.TokenSource != EnvToken {
		t.Fatalf("PYRE_TOKEN: %+v", conn)
	}
	conn = resolve(base.with(EnvToken, "explicit", EnvSessionToken, "pys_session-synthetic"), "")
	if conn.TokenSource != EnvSessionToken {
		t.Fatalf("PYRE_SESSION_TOKEN: %+v", conn)
	}
	if strings.Contains(conn.Describe(), "pyc_") || strings.Contains(conn.Describe(), "pys_") {
		t.Fatal("Describe must never include a token")
	}
	// Nothing configured.
	if _, err := Resolve(ConnOptions{Getenv: env{EnvConfig: filepath.Join(dir, "missing.json")}.get}); !c.IsCode(err, c.ErrUnavailable) {
		t.Fatalf("missing config: %v", err)
	}
}

func TestConfigPermissions(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "client.json")
	if err := SaveConfig(p, ClientConfig{HubURL: "http://h.synthetic", Token: "t"}); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(p)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("config mode %v %v", st.Mode(), err)
	}
	if dst, _ := os.Stat(filepath.Dir(p)); dst.Mode().Perm() != 0o700 {
		t.Fatalf("config dir mode %v", dst.Mode())
	}
	if _, err := LoadConfig(p); err != nil {
		t.Fatalf("load 0600: %v", err)
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(p); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("0644 must be refused: %v", err)
	}
	// Resolution refuses to fall back to a loose config.
	if _, err := Resolve(ConnOptions{Getenv: env{EnvConfig: p}.get}); err == nil {
		t.Fatal("resolve must fail on a loose config")
	}
	os.Chmod(p, 0o600)
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(p, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(link); err == nil {
		t.Fatal("symlinked config must be refused")
	}
}

func TestLoginWritesConfigAndIsUsed(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h).with(EnvHub, "", EnvToken, "")
	cfg := e[EnvConfig]

	// Token from stdin keeps it out of argv.
	r := run(t, e, &App{Stdin: strings.NewReader(ownerToken + "\n")}, "--json", "--hub", h.srv.URL, "login", "--token", "-")
	expectCode(t, r, 0)
	pr := mustJSON[PairResult](t, r)
	if pr.ConfigPath != cfg || pr.ClientID != testOwner.ID {
		t.Fatalf("login result: %+v", pr)
	}
	if strings.Contains(r.stdout, ownerToken) {
		t.Fatal("login output must not echo the token")
	}
	if st, _ := os.Stat(cfg); st.Mode().Perm() != 0o600 {
		t.Fatalf("config perms %v", st.Mode())
	}
	r = run(t, e, nil, "--json", "whoami")
	expectCode(t, r, 0)
	who := mustJSON[WhoAmIResult](t, r)
	if who.Principal.ID != testOwner.ID || who.TokenSource != "config" || who.Endpoint != h.srv.URL {
		t.Fatalf("whoami via config: %+v", who)
	}
	// Refuses to overwrite without --force; a bad token fails verification.
	expectCode(t, run(t, e, &App{Stdin: strings.NewReader(ownerToken + "\n")}, "--hub", h.srv.URL, "login", "--token", "-"), ExitConflict)
	if run(t, e, &App{Stdin: strings.NewReader("wrong\n")}, "--hub", h.srv.URL, "login", "--token", "-", "--force").code == 0 {
		t.Fatal("login with a bad token must fail verification")
	}
	// A token on the command line is refused (visible in ps) and not echoed.
	r = run(t, e, nil, "--hub", h.srv.URL, "login", "--token", ownerToken, "--force")
	expectCode(t, r, ExitUsage)
	if strings.Contains(r.stdout+r.stderr, ownerToken) {
		t.Fatal("refused argv token echoed")
	}
}

// TestLoginFromOwnerCredentialFile takes pyre-hub init's owner-credential.json
// on stdin: token, hub URL and pin come from the file.
func TestLoginFromOwnerCredentialFile(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h).with(EnvHub, "", EnvToken, "")
	cred, _ := json.Marshal(map[string]string{"hub_url": h.srv.URL, "client_id": testOwner.ID, "token": ownerToken})
	r := run(t, e, &App{Stdin: strings.NewReader(string(cred) + "\n")}, "--json", "login", "--token", "-")
	expectCode(t, r, 0)
	if pr := mustJSON[PairResult](t, r); pr.HubURL != h.srv.URL || pr.ClientID != testOwner.ID {
		t.Fatalf("login result: %+v", pr)
	}
	if strings.Contains(r.stdout+r.stderr, ownerToken) {
		t.Fatal("login output must not echo the token")
	}
	expectCode(t, run(t, e, &App{Stdin: strings.NewReader("{not json")}, "login", "--token", "-", "--force"), ExitUsage)
}

// TestPairOverPinnedTLS pairs with a self-signed hub (pin in the link),
// writes the 0600 config, and uses it (with the pin) for later commands.
func TestPairOverPinnedTLS(t *testing.T) {
	h := newHub(t)
	auth := httpapi.NewStaticTokens(nil)
	const pairedToken = "pyc_synthetic-paired-client-token"
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+c.PathPairExchange, func(w http.ResponseWriter, r *http.Request) {
		var ex c.PairingExchange
		if err := json.NewDecoder(r.Body).Decode(&ex); err != nil || ex.PairingID != "pair_synthetic" || ex.Code != "pyp_synthetic-code" || ex.Kind != "cli" || ex.ClientName != "laptop" {
			httpapi.WriteError(w, c.Errorf(c.ErrUnauthorized, "invalid pairing code"))
			return
		}
		auth.Add(pairedToken, c.Principal{Kind: c.PrincipalOwner, ID: "cli_paired"})
		json.NewEncoder(w).Encode(c.ClientCredential{ClientID: "cli_paired", Token: pairedToken})
	})
	mux.Handle("/", httpapi.NewServer(h.b, auth, httpapi.Options{}))
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	pin := client.CertFingerprint(srv.Certificate().Raw)
	link := enrollment.PairingURI{HubURL: srv.URL, PinSHA256: pin, PairingID: "pair_synthetic", Code: "pyp_synthetic-code"}.String()

	e := newEnv(t, nil)
	// The link is a one-time secret: refused as an argument, read from stdin.
	r := run(t, e, nil, "--json", "pair", link, "--name", "laptop")
	expectCode(t, r, ExitUsage)
	if strings.Contains(r.stdout+r.stderr, "pyp_synthetic-code") {
		t.Fatal("refused argv link echoed")
	}
	stdin := func() *App { return &App{Stdin: strings.NewReader(link + "\n")} }
	r = run(t, e, stdin(), "--json", "pair", "--name", "laptop")
	expectCode(t, r, 0)
	if pr := mustJSON[PairResult](t, r); pr.ClientID != "cli_paired" || pr.HubCertSHA256 != pin {
		t.Fatalf("pair result: %+v", pr)
	}
	if strings.Contains(r.stdout+r.stderr, pairedToken) {
		t.Fatal("pair must never print the credential")
	}
	cfg, err := LoadConfig(e[EnvConfig])
	if err != nil || cfg.Token != pairedToken || cfg.HubCertSHA256 != pin {
		t.Fatalf("config: %+v %v", cfg, err)
	}
	r = run(t, e, nil, "--json", "whoami")
	expectCode(t, r, 0)
	if who := mustJSON[WhoAmIResult](t, r); who.Principal.ID != "cli_paired" || who.TokenSource != "config" {
		t.Fatalf("whoami after pair: %+v", who)
	}
	// A wrong pin is refused (TLS identity, not just the token).
	bad := e.with(EnvHubPin, strings.Repeat("00", 32), EnvHub, srv.URL)
	if r := run(t, bad, nil, "whoami"); r.code == 0 {
		t.Fatalf("a mismatched pin must fail: %s", r)
	}
	// Pairing again without --force is refused; a reused code fails.
	expectCode(t, run(t, e, stdin(), "pair", "-", "--name", "laptop"), ExitConflict)
	if r := run(t, e, stdin(), "pair", "--name", "other", "--force"); r.code == 0 {
		t.Fatalf("bad exchange must fail: %s", r)
	}
}

func TestPairRejectsBadLinks(t *testing.T) {
	e := newEnv(t, nil)
	for _, link := range []string{"https://not-a-pair-link", "pyre://pair?hub=http%3A%2F%2Fplain&id=pair_x&code=pyp_x", ""} {
		expectCode(t, run(t, e, &App{Stdin: strings.NewReader(link)}, "pair"), ExitUsage)
	}
}

// TestSocketConnection serves the API on a unix socket (as the node daemon
// does) and checks PYRE_SOCKET + PYRE_SESSION_TOKEN are used.
func TestSocketConnection(t *testing.T) {
	h := newHub(t)
	dir, err := os.MkdirTemp("", "pyre-sock") // short path: sun_path is ~104 bytes on macOS
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	auth := httpapi.NewStaticTokens(map[string]c.Principal{sessBToken: {Kind: c.PrincipalSession, ID: "ses_synth-b"}})
	srv := &http.Server{Handler: httpapi.NewServer(h.b, auth, httpapi.Options{StreamWait: time.Second}), ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	e := newEnv(t, nil).with(EnvSocket, sock, EnvSessionToken, sessBToken, EnvToken, "ignored-because-session-token-wins", EnvSessionID, "ses_synth-b")
	r := run(t, e, nil, "--json", "whoami")
	expectCode(t, r, 0)
	who := mustJSON[WhoAmIResult](t, r)
	if who.Mode != "socket" || who.Endpoint != sock || who.TokenSource != EnvSessionToken || who.Principal.ID != "ses_synth-b" || who.Self.SessionID != "ses_synth-b" {
		t.Fatalf("socket whoami: %+v", who)
	}
	// A real operation over the socket.
	r = run(t, e, nil, "--json", "read", "self", "--tail", "3")
	expectCode(t, r, 0)
	mustJSON[c.ReadSessionResponse](t, r)
	// PYRE_HUB still wins over the socket.
	r = run(t, e.with(EnvHub, h.srv.URL, EnvSessionToken, "", EnvToken, ownerToken), nil, "--json", "whoami")
	expectCode(t, r, 0)
	if who := mustJSON[WhoAmIResult](t, r); who.Mode != "hub" {
		t.Fatalf("PYRE_HUB must beat PYRE_SOCKET: %+v", who)
	}
}
