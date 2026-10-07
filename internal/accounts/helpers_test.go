package accounts

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// Every test runs with a synthetic HOME so nothing can reach the user's
// real ~/.claude, ~/.codex, keychain wrapper defaults or rota snapshots.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "pyre-accounts-home-")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", home)
	for _, v := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY", "OPENAI_API_KEY"} {
		os.Unsetenv(v)
	}
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

// ---------- synthetic credentials ----------

func synthAccess(label string) string  { return "pyre-synthetic-" + label + "-access-initial" }
func synthRefresh(label string) string { return "pyre-synthetic-" + label + "-refresh-initial" }

func claudeUUID(label string) string {
	return "00000000-0000-4000-8000-0000000000" + strings.ToLower(label) + "0"
}
func codexAcct(label string) string { return "synthetic-acct-" + label }

func writeJSON(t *testing.T, p string, v any, mode os.FileMode) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, mode); err != nil {
		t.Fatal(err)
	}
}

// loginClaude simulates `claude auth login` inside a profile: a plaintext
// credential store plus oauthAccount in the profile's .claude.json.
func loginClaude(t *testing.T, dir, label string, expired bool) {
	t.Helper()
	exp := time.Now().Add(time.Hour)
	if expired {
		exp = time.Now().Add(-time.Hour)
	}
	writeJSON(t, filepath.Join(dir, ".credentials.json"), map[string]any{"claudeAiOauth": map[string]any{
		"accessToken": synthAccess(label), "refreshToken": synthRefresh(label), "expiresAt": exp.UnixMilli(),
		"scopes": []string{"user:inference", "user:profile"}, "subscriptionType": "max",
	}}, 0o600)
	setClaudeOAuthAccount(t, dir, claudeUUID(label), label)
}

func setClaudeOAuthAccount(t *testing.T, dir, uuid, label string) {
	t.Helper()
	p := filepath.Join(dir, ".claude.json")
	m := map[string]any{}
	if b, err := os.ReadFile(p); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	m["oauthAccount"] = map[string]any{"accountUuid": uuid, "emailAddress": "acct-" + label + "@synthetic.invalid"}
	writeJSON(t, p, m, 0o600)
}

func synthJWT(claims map[string]any) string {
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return enc(map[string]any{"alg": "none", "typ": "JWT"}) + "." + enc(claims) + ".sig"
}

// loginCodex simulates `codex login --device-auth` with the file store.
func loginCodex(t *testing.T, dir, label string, expired bool) {
	t.Helper()
	exp := time.Now().Add(time.Hour).Unix()
	if expired {
		exp = time.Now().Add(-time.Hour).Unix()
	}
	claims := map[string]any{"pyre_synthetic": true, "pyre_label": label, "exp": exp,
		"https://api.openai.com/auth": map[string]any{"chatgpt_plan_type": "pro", "chatgpt_account_id": codexAcct(label)}}
	access := map[string]any{}
	for k, v := range claims {
		access[k] = v
	}
	access["kind"] = "access"
	writeJSON(t, filepath.Join(dir, "auth.json"), map[string]any{
		"OPENAI_API_KEY": nil, "auth_mode": "chatgpt",
		"tokens": map[string]any{"id_token": synthJWT(claims), "access_token": synthJWT(access),
			"refresh_token": synthRefresh(label), "account_id": codexAcct(label)},
	}, 0o600)
}

// ---------- fake provider (port of spikes/accounts/fakeprovider.py) ----------

// The fake serves only under a random per-instance path prefix (base). Other
// test processes run concurrently (go test -p N) and some keep reconnecting
// to loopback ports of servers they stopped (node links, hub clients) with
// their own bearer tokens; when the kernel hands such a port to this
// httptest server, those requests must not count as a Pyre probe presenting
// a non-synthetic credential. Requests outside the prefix are strays: 404,
// counted separately, never alarms.
type fakeProvider struct {
	mu           sync.Mutex
	srv          *httptest.Server
	prefix       string            // "/fake-<random>"; every legitimate request starts with it
	base         string            // srv.URL + prefix
	strays       int               // requests outside prefix (another process on a reused port)
	usage        map[string]string // label -> usage JSON
	status       map[string]int    // label -> forced HTTP status
	retryAfter   string
	echoToken    bool // misbehaving server: echo the bearer in error bodies
	usageCalls   map[string]int
	refreshCalls int
	alarms       int // non-synthetic credential presented
	badHeaders   int
}

func newFakeProvider(t *testing.T) *fakeProvider {
	f := &fakeProvider{usage: map[string]string{}, status: map[string]int{}, usageCalls: map[string]int{},
		prefix: "/fake-" + strings.ToLower(rand.Text())}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	f.base = f.srv.URL + f.prefix
	t.Cleanup(f.srv.Close)
	return f
}

func labelOf(tok string) string {
	if strings.HasPrefix(tok, "pyre-synthetic-") {
		return strings.Split(tok, "-")[2]
	}
	return ""
}

func (f *fakeProvider) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path, ok := strings.CutPrefix(r.URL.Path, f.prefix)
	if !ok || !strings.HasPrefix(path, "/") {
		f.strays++
		http.NotFound(w, r)
		return
	}
	if strings.HasSuffix(path, "/oauth/token") {
		// Any refresh attempt is a test failure: probes never refresh.
		f.refreshCalls++
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
		return
	}
	auth := r.Header.Get("Authorization")
	tok := strings.TrimPrefix(auth, "Bearer ")
	if !strings.HasPrefix(tok, "pyre-synthetic-") {
		f.alarms++
		http.Error(w, `{"error":"non-synthetic"}`, http.StatusUnauthorized)
		return
	}
	if path != "/api/oauth/usage" || r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("anthropic-beta") != "oauth-2025-04-20" {
		f.badHeaders++
	}
	lab := labelOf(tok)
	f.usageCalls[lab]++
	if st := f.status[lab]; st != 0 {
		if st == http.StatusTooManyRequests && f.retryAfter != "" {
			w.Header().Set("Retry-After", f.retryAfter)
		}
		body := `{"error":{"type":"authentication_error","message":"denied"}}`
		if f.echoToken {
			body = `{"error":"bad token ` + tok + `","authorization":"Bearer ` + tok + `"}`
		}
		w.WriteHeader(st)
		w.Write([]byte(body))
		return
	}
	body := f.usage[lab]
	if body == "" {
		body = `{"five_hour":{"utilization":10,"resets_at":"2026-10-05T03:00:00Z"},"seven_day":{"utilization":20,"resets_at":"2026-10-09T00:00:00Z"}}`
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(body))
}

func (f *fakeProvider) calls(label string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.usageCalls[label]
}

func (f *fakeProvider) refreshes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refreshCalls
}

// counters returns the alarm, bad-header and stray counts under the lock
// (a stray can arrive at any time from another process).
func (f *fakeProvider) counters() (alarms, badHeaders, strays int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.alarms, f.badHeaders, f.strays
}

// ---------- fake Codex adapter reader ----------

type fakeCodexReader struct {
	mu    sync.Mutex
	resp  map[string]string // canonical home -> raw result
	errs  map[string]error  // canonical home -> error (wins over resp)
	calls map[string]int
}

func (r *fakeCodexReader) ReadRateLimits(_ context.Context, home string) (json.RawMessage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls[home]++
	if err := r.errs[home]; err != nil {
		return nil, err
	}
	if v, ok := r.resp[home]; ok {
		return json.RawMessage(v), nil
	}
	return json.RawMessage(`{"rateLimits":{"limitId":"codex","primary":{"usedPercent":11,"windowDurationMins":300,"resetsAt":1791190000},"secondary":{"usedPercent":22,"windowDurationMins":10080,"resetsAt":1791270000}}}`), nil
}

func (r *fakeCodexReader) count(home string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[home]
}

// ---------- fake daemon run controller ----------

type fakeController struct {
	mu            sync.Mutex
	svc           *Service
	runs          map[string]RunInfo
	order         []string // "quiesce:<run>" / "start:<account>"
	quiesceErr    error
	notExited     bool
	turnInFlight  bool
	continuations []Continuation
	startErr      map[string]error
}

func (c *fakeController) CurrentRun(_ context.Context, sessionID string) (RunInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.runs[sessionID]
	if !ok {
		return r, contracts.Errorf(contracts.ErrNotFound, "no run")
	}
	return r, nil
}

func (c *fakeController) Quiesce(_ context.Context, sessionID, runID string) (QuiesceResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.order = append(c.order, "quiesce:"+runID)
	if c.quiesceErr != nil {
		return QuiesceResult{}, c.quiesceErr
	}
	return QuiesceResult{Exited: !c.notExited, TurnInFlight: c.turnInFlight}, nil
}

func (c *fakeController) StartContinuation(ctx context.Context, k Continuation) (ContinuationResult, error) {
	c.mu.Lock()
	c.order = append(c.order, "start:"+k.AccountID)
	c.continuations = append(c.continuations, k)
	err := c.startErr[k.AccountID]
	c.mu.Unlock()
	if err != nil {
		return ContinuationResult{}, err
	}
	// Like the daemon: bind the successor through the resolver.
	newRun := contracts.NewID(contracts.PrefixRun)
	res := ContinuationResult{NewRunID: newRun}
	sess := k.SessionID
	if k.Kind == contracts.ContinuationHandoff {
		res.NewSessionID = contracts.NewID(contracts.PrefixSession)
		sess = res.NewSessionID
	}
	if _, err := c.svc.ResolveFor(ctx, ResolveRequest{Harness: k.Harness, RunID: newRun, SessionID: sess,
		Policy: contracts.AccountPolicy{Mode: contracts.AccountExplicit, AccountID: k.AccountID}}); err != nil {
		return ContinuationResult{}, err
	}
	c.mu.Lock()
	c.runs[sess] = RunInfo{SessionID: sess, RunID: newRun, Harness: k.Harness, Capabilities: c.runs[k.SessionID].Capabilities}
	c.mu.Unlock()
	return res, nil
}

// ---------- recording reporter ----------

type report struct {
	Kind    contracts.ReportKind
	Payload []byte
}

type recReporter struct {
	mu      sync.Mutex
	reports []report
}

func (r *recReporter) Report(_ context.Context, kind contracts.ReportKind, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reports = append(r.reports, report{kind, b})
	return nil
}

func (r *recReporter) of(kind contracts.ReportKind) [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out [][]byte
	for _, x := range r.reports {
		if x.Kind == kind {
			out = append(out, x.Payload)
		}
	}
	return out
}

func (r *recReporter) all() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	var b bytes.Buffer
	for _, x := range r.reports {
		b.Write(x.Payload)
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// ---------- clock ----------

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// ---------- environment ----------

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type testEnv struct {
	t      *testing.T
	base   string
	dbPath string
	svc    *Service
	cfg    Config
	fake   *fakeProvider
	reader *fakeCodexReader
	ctrl   *fakeController
	rep    *recReporter
	logs   *lockedBuf
	clock  *fakeClock

	userClaude, userClaudeJSON, userCodex string
	errs                                  []string // every error string seen by the test
}

// newEnv builds an isolated node: synthetic user config (with a synthetic
// DEFAULT login that must never change), a node DB, fake provider, fake
// Codex reader and fake daemon controller.
func newEnv(t *testing.T, mod ...func(*Config)) *testEnv {
	t.Helper()
	base := t.TempDir()
	e := &testEnv{t: t, base: base, fake: newFakeProvider(t), rep: &recReporter{}, logs: &lockedBuf{},
		reader: &fakeCodexReader{resp: map[string]string{}, calls: map[string]int{}},
		clock:  &fakeClock{t: time.Now().UTC().Truncate(time.Second)}}
	home := filepath.Join(base, "home")
	e.userClaude = filepath.Join(home, ".claude")
	e.userClaudeJSON = filepath.Join(home, ".claude.json")
	e.userCodex = filepath.Join(home, ".codex")
	for _, d := range []string{e.userClaude, filepath.Join(e.userClaude, "skills", "demo"), filepath.Join(e.userClaude, "agents"),
		e.userCodex, filepath.Join(e.userCodex, "skills"), filepath.Join(e.userCodex, "prompts")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	loginClaude(t, e.userClaude, "DEFAULT", false) // writes ~/.claude/.credentials.json and ~/.claude/.claude.json
	os.Remove(filepath.Join(e.userClaude, ".claude.json"))
	writeJSON(t, e.userClaudeJSON, map[string]any{
		"oauthAccount": map[string]any{"accountUuid": claudeUUID("DEFAULT")},
		"mcpServers":   map[string]any{"synthetic-mcp": map[string]any{"command": "synthetic-mcp-server", "args": []string{"--x"}}},
	}, 0o600)
	os.WriteFile(filepath.Join(e.userClaude, "settings.json"), []byte(`{"theme":"dark"}`), 0o600)
	os.WriteFile(filepath.Join(e.userClaude, "CLAUDE.md"), []byte("# synthetic user memory\n"), 0o600)
	os.WriteFile(filepath.Join(e.userClaude, "skills", "demo", "SKILL.md"), []byte("synthetic skill\n"), 0o600)
	loginCodex(t, e.userCodex, "DEFAULT", false)
	os.WriteFile(filepath.Join(e.userCodex, "config.toml"), []byte("model = \"synthetic\"\ncli_auth_credentials_store = \"keyring\"\n\n[features]\nfoo = true\n"), 0o600)
	os.WriteFile(filepath.Join(e.userCodex, "AGENTS.md"), []byte("synthetic agents\n"), 0o600)

	e.dbPath = filepath.Join(base, "node.db")
	db, err := sqlitex.Open(e.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	e.cfg = Config{
		NodeID: "node_synth", DB: db,
		ProfileRoot: filepath.Join(base, "state", "profiles"), SharedRoot: filepath.Join(base, "state", "shared"),
		UserClaudeDir: e.userClaude, UserClaudeJSON: e.userClaudeJSON, UserCodexDir: e.userCodex,
		ClaudeUsageURL: e.fake.base + "/api/oauth/usage", HTTPClient: e.fake.srv.Client(),
		PollInterval: time.Minute, FreshFor: 15 * time.Minute, CodexReader: e.reader,
		GOOS: "linux", SecurityPath: filepath.Join(base, "no-security"),
		Reporter: e.rep, Logger: slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now: e.clock.Now, TestMode: true,
	}
	for _, m := range mod {
		m(&e.cfg)
	}
	e.ctrl = &fakeController{runs: map[string]RunInfo{}, startErr: map[string]error{}}
	e.cfg.Controller = e.ctrl
	e.svc, err = New(context.Background(), e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.ctrl.svc = e.svc
	return e
}

func (e *testEnv) err(err error) error {
	if err != nil {
		e.errs = append(e.errs, err.Error())
		if ce := contracts.AsError(err); ce != nil {
			b, _ := json.Marshal(ce)
			e.errs = append(e.errs, string(b))
		}
	}
	return err
}

// addAccount registers an account, prepares its profile, performs a
// synthetic login and verifies it. ProviderRef matches the login unless ref
// overrides it.
func (e *testEnv) addAccount(id string, h contracts.Harness, label string, ref ...string) ProfileInfo {
	e.t.Helper()
	a := contracts.Account{AccountID: id, Provider: contracts.ProviderFor(h), Label: label, Kind: "subscription"}
	switch h {
	case contracts.HarnessClaude:
		a.ProviderRef = claudeUUID(label)
	case contracts.HarnessCodex:
		a.ProviderRef = codexAcct(label)
	}
	if len(ref) > 0 {
		a.ProviderRef = ref[0]
	}
	info, err := e.svc.CreateProfile(context.Background(), a, h)
	if err != nil {
		e.t.Fatalf("CreateProfile %s: %v", id, err)
	}
	switch h {
	case contracts.HarnessClaude:
		loginClaude(e.t, info.Dir, label, false)
	case contracts.HarnessCodex:
		loginCodex(e.t, info.Dir, label, false)
	}
	if len(ref) == 0 {
		if _, err := e.svc.VerifyGrant(context.Background(), id, h); err != nil {
			e.t.Fatalf("VerifyGrant %s: %v", id, err)
		}
	}
	return info
}

// observe stores a fresh synthetic observation directly.
func (e *testEnv) observe(id string, p contracts.Provider, used float64, age time.Duration) {
	e.t.Helper()
	o := contracts.AccountObservation{AccountID: id, Provider: p, ReportingNode: "node_synth", ObservedAt: e.clock.Now().Add(-age),
		Source: "mock", Confidence: "high", Eligibility: contracts.EligibleYes,
		Windows: []contracts.UsageWindow{{Name: "five_hour", UsedPercent: used}}}
	if err := e.svc.IngestObservation(context.Background(), o); err != nil {
		e.t.Fatal(err)
	}
}

// fileState fingerprints a tree (content hash, mode, mtime, symlink target)
// so tests can prove the user's originals were never modified.
func fileState(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return nil
		}
		v := fmt.Sprintf("%v|%d", fi.Mode(), fi.ModTime().UnixNano())
		if fi.Mode()&fs.ModeSymlink != 0 {
			tg, _ := os.Readlink(p)
			v += "|->" + tg
		} else if fi.Mode().IsRegular() {
			b, _ := os.ReadFile(p)
			s := sha256.Sum256(b)
			v += "|" + hex.EncodeToString(s[:])
		}
		out[p] = v
		return nil
	})
	return out
}

func sameState(t *testing.T, what string, a, b map[string]string) {
	t.Helper()
	var diffs []string
	for k, v := range a {
		if b[k] != v {
			diffs = append(diffs, k)
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			diffs = append(diffs, "+"+k)
		}
	}
	sort.Strings(diffs)
	if len(diffs) > 0 {
		t.Fatalf("%s changed: %v", what, diffs)
	}
}

func mustCode(t *testing.T, err error, code contracts.ErrorCode) *contracts.Error {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s error, got nil", code)
	}
	if !contracts.IsCode(err, code) {
		t.Fatalf("expected %s, got %v", code, err)
	}
	return contracts.AsError(err)
}
