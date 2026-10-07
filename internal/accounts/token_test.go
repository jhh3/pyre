package accounts

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/usage"
)

// Synthetic token: obviously fake, shaped like a setup token.
const synthToken = "sk-ant-oat01-pyre-synthetic-token-0123456789abcdef"

// fakeTokens is the hub side of accounts.token.fetch.
type fakeTokens struct {
	mu    sync.Mutex
	tok   map[string]string
	err   error
	calls int
}

func (f *fakeTokens) FetchAccountToken(_ context.Context, id string) (string, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return "", time.Time{}, f.err
	}
	t, ok := f.tok[id]
	if !ok {
		return "", time.Time{}, contracts.Errorf(contracts.ErrNotFound, "account has no token").WithRef(contracts.KindAccount, id)
	}
	return t, time.Unix(1_790_000_000, 0), nil
}

// fakeMessages serves POST /v1/messages with unified rate-limit headers.
type fakeMessages struct {
	mu        sync.Mutex
	srv       *httptest.Server
	status    int
	h5, h7    string // utilization 0..1
	unified   string // anthropic-ratelimit-unified-status
	calls     int
	bad       []string // problems seen in requests
	lastModel string
}

func newFakeMessages(t *testing.T) *fakeMessages {
	f := &fakeMessages{status: 200, h5: "0.12", h7: "0.40", unified: "allowed"}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls++
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
			f.bad = append(f.bad, "path "+r.Method+" "+r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer "+synthToken {
			f.bad = append(f.bad, "authorization header")
		}
		if r.Header.Get("anthropic-version") != "2023-06-01" || r.Header.Get("anthropic-beta") != "oauth-2025-04-20" {
			f.bad = append(f.bad, "version/beta headers")
		}
		var body struct {
			Model     string `json:"model"`
			MaxTokens int    `json:"max_tokens"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		f.lastModel = body.Model
		if body.MaxTokens != 1 {
			f.bad = append(f.bad, "max_tokens "+strconv.Itoa(body.MaxTokens))
		}
		reset5 := strconv.FormatInt(time.Now().Add(2*time.Hour).Unix(), 10)
		reset7 := strconv.FormatInt(time.Now().Add(72*time.Hour).Unix(), 10)
		hd := w.Header()
		hd.Set("anthropic-ratelimit-unified-5h-utilization", f.h5)
		hd.Set("anthropic-ratelimit-unified-5h-reset", reset5)
		hd.Set("anthropic-ratelimit-unified-5h-status", "allowed")
		hd.Set("anthropic-ratelimit-unified-7d-utilization", f.h7)
		hd.Set("anthropic-ratelimit-unified-7d-reset", reset7)
		hd.Set("anthropic-ratelimit-unified-7d-status", "allowed")
		hd.Set("anthropic-ratelimit-unified-overage-utilization", "0.0")
		hd.Set("anthropic-ratelimit-unified-status", f.unified)
		hd.Set("anthropic-ratelimit-unified-reset", reset5)
		hd.Set("anthropic-ratelimit-unified-representative-claim", "five_hour")
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(`{"type":"message","usage":{"output_tokens":1}}`))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeMessages) set(fn func(*fakeMessages)) { f.mu.Lock(); fn(f); f.mu.Unlock() }
func (f *fakeMessages) n() int                     { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

type tokenEnv struct {
	*testEnv
	toks *fakeTokens
	msgs *fakeMessages
}

func newTokenEnv(t *testing.T) *tokenEnv {
	toks := &fakeTokens{tok: map[string]string{}}
	msgs := newFakeMessages(t)
	e := newEnv(t, func(c *Config) {
		c.Tokens = toks
		c.ClaudeMessagesURL = msgs.srv.URL + "/v1/messages"
		c.HTTPClient = msgs.srv.Client()
	})
	return &tokenEnv{testEnv: e, toks: toks, msgs: msgs}
}

func (e *tokenEnv) addToken(id, label string, h contracts.Harness) RegisterResult {
	e.t.Helper()
	e.toks.mu.Lock()
	e.toks.tok[id] = synthToken
	e.toks.mu.Unlock()
	a := contracts.Account{AccountID: id, Provider: contracts.ProviderFor(h), Label: label, Kind: "subscription"}
	res, err := e.svc.Register(context.Background(), RegisterParams{Account: a, Harness: h, Source: contracts.SourceToken})
	if err != nil {
		e.t.Fatalf("register token: %v", err)
	}
	return res
}

// noTokenAnywhere: the synthetic token is absent from every file under the
// node's state (DB, WAL, profiles), the logs, the reports and the errors.
func (e *tokenEnv) noTokenAnywhere() {
	e.t.Helper()
	needle := []byte(synthToken)
	_ = filepath.WalkDir(e.base, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if b, err := os.ReadFile(p); err == nil && bytes.Contains(b, needle) {
			e.t.Errorf("token found in %s", p)
		}
		return nil
	})
	if strings.Contains(e.logs.String(), synthToken) {
		e.t.Error("token found in logs")
	}
	b := e.rep.all()
	if bytes.Contains(b, needle) {
		e.t.Error("token found in reports")
	}
	for _, s := range e.errs {
		if strings.Contains(s, synthToken) {
			e.t.Error("token found in an error")
		}
	}
}

func TestParseUnifiedHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.01")
	h.Set("Anthropic-Ratelimit-Unified-5h-Reset", "1791323400")
	h.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.02")
	h.Set("Anthropic-Ratelimit-Unified-7d-Reset", "1791622800")
	h.Set("Anthropic-Ratelimit-Unified-7d_opus-Utilization", "0.5")
	h.Set("Anthropic-Ratelimit-Unified-Overage-Utilization", "0.0")
	h.Set("Anthropic-Ratelimit-Unified-Status", "allowed")
	h.Set("Anthropic-Ratelimit-Unified-Reset", "1791323400")
	h.Set("Anthropic-Ratelimit-Unified-Representative-Claim", "five_hour")
	u := ParseUnifiedHeaders(h)
	if u.Rejected || u.Status != "allowed" || u.Claim != "five_hour" || u.Reset == nil || u.Reset.Unix() != 1791323400 {
		t.Fatalf("summary: %+v", u)
	}
	if len(u.Windows) != 3 {
		t.Fatalf("windows: %+v", u.Windows)
	}
	if w := u.Windows[0]; w.Name != usage.FiveHour || w.UsedPercent != 1 || w.ResetAt.Unix() != 1791323400 {
		t.Fatalf("5h: %+v", w)
	}
	if w := u.Windows[1]; w.Name != usage.Weekly || w.UsedPercent != 2 {
		t.Fatalf("7d: %+v", w)
	}
	if w := u.Windows[2]; w.Name != "seven_day_opus" || w.UsedPercent != 50 {
		t.Fatalf("scoped: %+v", w)
	}
	h.Set("Anthropic-Ratelimit-Unified-5h-Status", "rejected")
	h.Set("Anthropic-Ratelimit-Unified-Overage-Utilization", "0.3")
	u = ParseUnifiedHeaders(h)
	if !u.Rejected || len(u.Windows) != 4 {
		t.Fatalf("rejected window / extra usage: %+v", u)
	}
	if u := ParseUnifiedHeaders(http.Header{}); len(u.Windows) != 0 || u.Rejected {
		t.Fatalf("empty: %+v", u)
	}
}

func TestTokenAccountLifecycle(t *testing.T) {
	e := newTokenEnv(t)
	ctx := context.Background()
	res := e.addToken("acct_tok1", "synth-token", contracts.HarnessClaude)
	if !res.Verified || res.Grant.Kind != contracts.CredOAuthToken || res.Grant.State != contracts.GrantValid || res.Grant.RefreshOwner != "none" {
		t.Fatalf("register: %+v", res)
	}
	if e.msgs.n() != 1 || e.msgs.lastModel != TokenProbeModel || len(e.msgs.bad) > 0 {
		t.Fatalf("probe calls=%d model=%s bad=%v", e.msgs.n(), e.msgs.lastModel, e.msgs.bad)
	}
	o, _ := getObservation(ctx, e.svc.db, "acct_tok1")
	if o == nil || o.Eligibility != contracts.EligibleYes || o.Source != contracts.UsageSourceMessagesProbe {
		t.Fatalf("observation: %+v", o)
	}
	if f, _, w, _ := usage.Used(o.Windows); f != 12 || w != 40 {
		t.Fatalf("windows %+v", o.Windows)
	}
	// The profile is isolated and holds no credential.
	dir := res.Grant.ProfileDir
	for _, n := range []string{".credentials.json", "auth.json", "oauth-token"} {
		if _, err := os.Stat(filepath.Join(dir, n)); err == nil {
			t.Fatalf("profile holds %s", n)
		}
	}
	// Launch wiring: the token only in Env (for the private env file),
	// with the isolated profile; API keys removed.
	r, err := e.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: contracts.AccountPolicy{Mode: contracts.AccountEligible}, RunID: "run_tok1", SessionID: "ses_tok1"})
	if err != nil {
		t.Fatal(e.err(err))
	}
	if r.Env["CLAUDE_CODE_OAUTH_TOKEN"] != synthToken || r.Env["CLAUDE_CONFIG_DIR"] != dir || r.CredentialKind != contracts.CredOAuthToken {
		t.Fatalf("env keys / kind wrong: kind=%s config=%q", r.CredentialKind, r.Env["CLAUDE_CONFIG_DIR"])
	}
	if contains(r.Unset, "CLAUDE_CODE_OAUTH_TOKEN") || !contains(r.Unset, "ANTHROPIC_API_KEY") {
		t.Fatalf("unset: %v", r.Unset)
	}
	if !strings.Contains(r.Assignment.Reason, "only eligible account") {
		t.Fatalf("reason: %s", r.Assignment.Reason)
	}
	// Concurrent use: a second run binds at once (no lease, no limit).
	r2, err := e.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: contracts.AccountPolicy{Mode: contracts.AccountEligible}, RunID: "run_tok2", SessionID: "ses_tok2"})
	if err != nil || r2.Assignment.AccountID != "acct_tok1" {
		t.Fatalf("second run: %v", e.err(err))
	}
	e.noTokenAnywhere()
}

func TestTokenProbeSchedule(t *testing.T) {
	e := newTokenEnv(t)
	ctx := context.Background()
	e.addToken("acct_tok1", "synth-token", contracts.HarnessClaude)
	base := e.msgs.n()
	// Idle: re-read once its reading is TokenIdleProbeEvery old, so it
	// does not show as unknown.
	e.clock.Advance(time.Hour)
	e.svc.ObserveDue(ctx, "")
	if e.msgs.n() != base+1 {
		t.Fatalf("idle account probes = %d, want 1", e.msgs.n()-base)
	}
	base = e.msgs.n()
	e.clock.Advance(time.Hour)
	// The display still counts the aged reading as ready ("checked on use").
	o, _ := getObservation(ctx, e.svc.db, "acct_tok1")
	g, _ := e.svc.getGrant(ctx, e.svc.db, "acct_tok1", contracts.HarnessClaude)
	if as := usage.Assess(contracts.Account{AccountID: "acct_tok1"}, o, &g.DeviceGrant, e.clock.Now()); as.State != usage.Healthy || as.Label() != "ready · checked on use" {
		t.Fatalf("assessment %+v %q", as, as.Label())
	}
	// Selection with an old reading probes first (once).
	if _, err := e.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, RunID: "run_s1", SessionID: "ses_s1"}); err != nil {
		t.Fatal(e.err(err))
	}
	if e.msgs.n() != base+1 {
		t.Fatalf("selection probe calls=%d", e.msgs.n()-base)
	}
	// In use: probed again only after 10 minutes.
	e.clock.Advance(5 * time.Minute)
	e.svc.ObserveDue(ctx, "")
	if e.msgs.n() != base+1 {
		t.Fatal("probed before 10 minutes")
	}
	e.clock.Advance(6 * time.Minute)
	e.svc.ObserveDue(ctx, "")
	if e.msgs.n() != base+2 {
		t.Fatalf("in-use account not probed after 10 minutes (calls=%d)", e.msgs.n()-base)
	}
	// On demand: at most once a minute.
	e.svc.ObserveNow(ctx, "")
	if e.msgs.n() != base+2 {
		t.Fatal("forced probe within a minute")
	}
	e.clock.Advance(61 * time.Second)
	e.svc.ObserveNow(ctx, "")
	if e.msgs.n() != base+3 {
		t.Fatal("forced probe did not run")
	}
}

func TestTokenRejectedIsNeedsReauthAndNeverSelected(t *testing.T) {
	e := newTokenEnv(t)
	ctx := context.Background()
	e.msgs.set(func(f *fakeMessages) { f.status = 401 })
	res := e.addToken("acct_tok1", "synth-token", contracts.HarnessClaude)
	if res.Verified {
		t.Fatal("401 verified")
	}
	g, _ := e.svc.getGrant(ctx, e.svc.db, "acct_tok1", contracts.HarnessClaude)
	if g.State != contracts.GrantDead || !strings.Contains(g.Detail, "claude setup-token") || !strings.Contains(g.Detail, "pyre accounts add-token") {
		t.Fatalf("grant %s %q", g.State, g.Detail)
	}
	o, _ := getObservation(ctx, e.svc.db, "acct_tok1")
	if as := usage.Assess(contracts.Account{AccountID: "acct_tok1"}, o, &g.DeviceGrant, e.clock.Now()); as.State != usage.NeedsReauth {
		t.Fatalf("assessment %+v", as)
	}
	// Never selected: with the auto default the node runs on its own login
	// instead (a login that needs re-auth counts as no account; reauth.go).
	rx, err := e.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, RunID: "run_x", SessionID: "ses_x"})
	if err != nil || rx.Assignment.AccountID == "acct_tok1" || rx.CredentialKind != contracts.CredAmbient {
		t.Fatalf("dead token selected or launch failed: %v %+v", err, rx.Assignment)
	}
	calls := e.msgs.n()
	e.clock.Advance(time.Hour)
	e.svc.ObserveNow(ctx, "")
	if e.msgs.n() != calls {
		t.Fatal("dead token probed again")
	}
	// add-token again (new token on the hub) revives it.
	e.msgs.set(func(f *fakeMessages) { f.status = 200 })
	res = e.addToken("acct_tok1", "synth-token", contracts.HarnessClaude)
	if !res.Verified || res.Grant.State != contracts.GrantValid || res.Grant.Generation <= g.Generation {
		t.Fatalf("revive: %+v", res.Grant)
	}
	e.noTokenAnywhere()
}

func TestTokenAtLimitUntilReset(t *testing.T) {
	e := newTokenEnv(t)
	ctx := context.Background()
	e.msgs.set(func(f *fakeMessages) { f.status, f.h5, f.unified = 429, "1.0", "rejected" })
	e.addToken("acct_tok1", "synth-token", contracts.HarnessClaude)
	o, _ := getObservation(ctx, e.svc.db, "acct_tok1")
	if o == nil || o.Eligibility != contracts.EligibleExhausted || o.ResetAt == nil || !o.ResetAt.After(time.Now()) {
		t.Fatalf("observation %+v", o)
	}
	_, err := e.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, RunID: "run_x", SessionID: "ses_x"})
	if ce := mustCode(t, err, contracts.ErrCapacityExhausted); ce.Details["outcome"] != string(contracts.FallbackWaiting) {
		t.Fatalf("outcome %v", ce.Details)
	}
	// 200 with unified status rejected is at limit too.
	e.msgs.set(func(f *fakeMessages) { f.status, f.h5, f.unified = 200, "0.5", "rejected" })
	e.clock.Advance(2 * time.Minute)
	e.svc.ObserveNow(ctx, "")
	if o, _ := getObservation(ctx, e.svc.db, "acct_tok1"); o.Eligibility != contracts.EligibleExhausted {
		t.Fatalf("unified rejected: %+v", o)
	}
}

func TestTokenRemovedOnHubRevokesGrant(t *testing.T) {
	e := newTokenEnv(t)
	ctx := context.Background()
	e.addToken("acct_tok1", "synth-token", contracts.HarnessClaude)
	e.toks.mu.Lock()
	delete(e.toks.tok, "acct_tok1")
	e.toks.mu.Unlock()
	e.clock.Advance(11 * time.Minute) // cached copy expires
	if rx, err := e.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, RunID: "run_x", SessionID: "ses_x"}); err != nil ||
		rx.Assignment.AccountID == "acct_tok1" || rx.CredentialKind != contracts.CredAmbient {
		t.Fatalf("removed token selected or launch failed: %v %+v", err, rx.Assignment)
	}
	g, _ := e.svc.getGrant(ctx, e.svc.db, "acct_tok1", contracts.HarnessClaude)
	if g.State != contracts.GrantRevoked {
		t.Fatalf("grant %s", g.State)
	}
}

func TestTokenHubOutageUsesMemoryCopy(t *testing.T) {
	e := newTokenEnv(t)
	ctx := context.Background()
	e.addToken("acct_tok1", "synth-token", contracts.HarnessClaude)
	e.toks.mu.Lock()
	e.toks.err = contracts.Errorf(contracts.ErrUnavailable, "hub unreachable")
	e.toks.mu.Unlock()
	e.clock.Advance(11 * time.Minute)
	r, err := e.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, RunID: "run_x", SessionID: "ses_x"})
	if err != nil || r.Env["CLAUDE_CODE_OAUTH_TOKEN"] != synthToken {
		t.Fatalf("outage: %v", e.err(err))
	}
	e.noTokenAnywhere()
}

func TestOpenAITokenRegistersButIsNotSelected(t *testing.T) {
	e := newTokenEnv(t)
	ctx := context.Background()
	res := e.addToken("acct_cx1", "synth-codex", contracts.HarnessCodex)
	if res.Grant.Kind != contracts.CredOAuthToken || res.Verified || !strings.Contains(res.Detail, "not observed yet") {
		t.Fatalf("register: %+v", res)
	}
	if TokenEnvVar(contracts.HarnessCodex) != "CODEX_ACCESS_TOKEN" {
		t.Fatal("codex token variable")
	}
	_, err := e.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessCodex, Policy: contracts.AccountPolicy{Mode: contracts.AccountExplicit, AccountID: "acct_cx1"}, RunID: "run_x", SessionID: "ses_x"})
	if err == nil || !strings.Contains(err.Error(), "not verified yet") {
		t.Fatalf("codex token selected: %v", err)
	}
	if e.msgs.n() != 0 {
		t.Fatal("codex token probed the anthropic endpoint")
	}
}

func TestPreferTokensOverSamePersonsRota(t *testing.T) {
	mk := func(id, label, rota string, kind contracts.CredentialKind) *candidate {
		return &candidate{acct: contracts.Account{AccountID: id, Label: label, RotaRef: rota}, grant: grantRow{DeviceGrant: contracts.DeviceGrant{Kind: kind}}}
	}
	tok := mk("acct_t", "Alice1", "", contracts.CredOAuthToken)
	same := mk("acct_r", "alice1", "alice1@synthetic.invalid", CredRotaCheckout)
	sameByRota := mk("acct_r2", "personal", "alice1@synthetic.invalid", CredRotaCheckout)
	other := mk("acct_o", "code", "code@synthetic.invalid", CredRotaCheckout)
	out := preferTokens([]*candidate{tok, same, other}, []*candidate{tok, same, other}, []*candidate{sameByRota})
	if len(out[0]) != 2 || out[0][0] != tok || out[0][1] != other || len(out[1]) != 0 {
		t.Fatalf("prefer: %v", out)
	}
	// Without an eligible token account nothing is dropped.
	out = preferTokens([]*candidate{same, other}, []*candidate{same, other})
	if len(out[0]) != 2 {
		t.Fatal("dropped without a token account")
	}
}
