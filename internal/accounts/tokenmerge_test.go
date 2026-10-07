package accounts

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

type rotaTokenEnv struct {
	*rotaEnv
	toks *fakeTokens
	msgs *fakeMessages
}

func newRotaTokenEnv(t *testing.T) *rotaTokenEnv {
	toks := &fakeTokens{tok: map[string]string{}}
	msgs := newFakeMessages(t)
	r := newRotaEnv(t, func(c *Config) {
		c.Tokens = toks
		c.ClaudeMessagesURL = msgs.srv.URL + "/v1/messages"
		c.HTTPClient = msgs.srv.Client()
	})
	return &rotaTokenEnv{rotaEnv: r, toks: toks, msgs: msgs}
}

func (e *rotaTokenEnv) rotaAccount(id, label string) contracts.Account {
	return contracts.Account{AccountID: id, Provider: contracts.ProviderAnthropic, Label: label, Kind: "subscription",
		RotaRef: rotaName(label), ProviderRef: claudeUUID(label)}
}

func (e *rotaTokenEnv) setToken(id string, on bool) {
	e.toks.mu.Lock()
	defer e.toks.mu.Unlock()
	if on {
		e.toks.tok[id] = synthToken
	} else {
		delete(e.toks.tok, id)
	}
}

// A token added to a rota account converts its grant (same ID, own
// profile); a rota run in flight keeps its checkout and checks in at its
// end; new runs use the token; when the token goes away the account is
// back on its rota login.
func TestTokenAttachesToRotaAccountWithFallback(t *testing.T) {
	e := newRotaTokenEnv(t)
	ctx := context.Background()
	e.writeRotaClaude("P", "g1", time.Now().Add(time.Hour))
	info := e.addRotaAccount("acct_synth-p", contracts.HarnessClaude, "P", false)
	lock := filepath.Join(e.claudeAcc, rotaName("P")+".pyre-lock")
	e.observe("acct_synth-p", contracts.ProviderAnthropic, 10, time.Minute)
	rr, err := e.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: eligible, RunID: "run_rota", SessionID: "ses_rota"})
	if err != nil || rr.CredentialKind != CredRotaCheckout || !e.singleOwner(info.Dir, lock, "") {
		t.Fatalf("rota run: %+v %v", rr.CredentialKind, err)
	}

	// The owner adds the token for the same person.
	e.setToken("acct_synth-p", true)
	res, err := e.svc.Register(ctx, RegisterParams{Account: e.rotaAccount("acct_synth-p", "P"), Harness: contracts.HarnessClaude, Source: contracts.SourceToken})
	if err != nil {
		t.Fatal(err)
	}
	tokDir := filepath.Join(filepath.Dir(info.Dir), "claude-token")
	if res.Grant.GrantID != info.GrantID || res.Grant.Kind != contracts.CredOAuthToken || res.Grant.State != contracts.GrantValid || !res.Verified {
		t.Fatalf("converted grant: %+v", res)
	}
	if c, _ := Canonicalize(tokDir); res.Grant.ProfileDir != c && res.Grant.ProfileDir != tokDir {
		t.Fatalf("token profile %s, want %s", res.Grant.ProfileDir, tokDir)
	}
	if f, _ := getGrantFallback(ctx, e.svc.db, info.GrantID); f == nil || f.Kind != CredRotaCheckout || f.ProfileDir != info.Dir {
		t.Fatalf("fallback: %+v", f)
	}
	// The rota run in flight is untouched.
	if err := e.svc.CheckAssignment(ctx, "run_rota"); err != nil {
		t.Fatalf("in-flight rota run: %v", err)
	}
	// New runs: the token, in the credential-free token profile.
	tr, err := e.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: eligible, RunID: "run_tok", SessionID: "ses_tok"})
	if err != nil {
		t.Fatal(e.err(err))
	}
	if tr.CredentialKind != contracts.CredOAuthToken || tr.Env["CLAUDE_CODE_OAUTH_TOKEN"] != synthToken || tr.Env["CLAUDE_CONFIG_DIR"] != res.Grant.ProfileDir {
		t.Fatalf("token run: kind=%s dir=%s", tr.CredentialKind, tr.Env["CLAUDE_CONFIG_DIR"])
	}
	if _, err := os.Stat(filepath.Join(res.Grant.ProfileDir, ".credentials.json")); err == nil {
		t.Fatal("token profile holds a credential")
	}
	// The rota run ends: its checkout returns to rota although the token
	// run still uses the (converted) grant.
	e.svc.Release("run_rota")
	if e.singleOwner(info.Dir, lock, "") {
		t.Fatal("rota checkout not checked in while a token run is active")
	}
	e.svc.Release("run_tok")

	// The token is removed on the hub: the next selection falls back to
	// the rota login (checked out again).
	e.setToken("acct_synth-p", false)
	e.clock.Advance(tokenCacheFor + time.Minute)
	e.observe("acct_synth-p", contracts.ProviderAnthropic, 10, time.Minute)
	fr, err := e.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: eligible, RunID: "run_back", SessionID: "ses_back"})
	if err != nil {
		t.Fatal(e.err(err))
	}
	if fr.CredentialKind != CredRotaCheckout || fr.ProfileDir != info.Dir || !e.singleOwner(info.Dir, lock, "") {
		t.Fatalf("fallback run: kind=%s dir=%s", fr.CredentialKind, fr.ProfileDir)
	}
	if g, _ := e.svc.grantByID(ctx, e.svc.db, info.GrantID); g.Kind != CredRotaCheckout || g.ProfileDir != info.Dir {
		t.Fatalf("grant after revert: %+v", g)
	}
	if f, _ := getGrantFallback(ctx, e.svc.db, info.GrantID); f != nil {
		t.Fatal("fallback row kept after revert")
	}
	e.svc.Release("run_back")
	e.noTokenIn(e.base)
}

// A rejected token (401) puts the account back on its rota login, and a
// sync does not re-attach the same token.
func TestRejectedTokenFallsBackAndStays(t *testing.T) {
	e := newRotaTokenEnv(t)
	ctx := context.Background()
	e.writeRotaClaude("Q", "g1", time.Now().Add(time.Hour))
	info := e.addRotaAccount("acct_synth-q", contracts.HarnessClaude, "Q", false)
	e.setToken("acct_synth-q", true)
	if _, err := e.svc.Register(ctx, RegisterParams{Account: e.rotaAccount("acct_synth-q", "Q"), Harness: contracts.HarnessClaude, Source: contracts.SourceToken}); err != nil {
		t.Fatal(err)
	}
	e.msgs.set(func(f *fakeMessages) { f.status = 401 })
	e.clock.Advance(2 * time.Minute)
	e.svc.ObserveNow(ctx, contracts.ProviderAnthropic)
	g, _ := e.svc.grantByID(ctx, e.svc.db, info.GrantID)
	if g.Kind != CredRotaCheckout || g.ProfileDir != info.Dir {
		t.Fatalf("after 401: %+v", g)
	}
	if o, _ := getObservation(ctx, e.svc.db, "acct_synth-q"); o != nil && o.Eligibility == contracts.EligibleUnusable {
		t.Fatalf("token rejection still judges the rota login: %+v", o)
	}
	// The hub still lists the (same) token: no re-attach.
	dir := &fakeDir{accts: []contracts.Account{e.rotaAccount("acct_synth-q", "Q")},
		toks: []contracts.AccountTokenStatus{{AccountID: "acct_synth-q", Provider: contracts.ProviderAnthropic, Set: true, UpdatedAt: time.Unix(1_790_000_000, 0)}}}
	if r := e.svc.SyncTokenAccounts(ctx, dir, nil); len(r.Registered) != 0 {
		t.Fatalf("rejected token re-attached: %+v", r)
	}
	// A new token (newer UpdatedAt) is attached.
	e.msgs.set(func(f *fakeMessages) { f.status = 200 })
	dir.toks[0].UpdatedAt = time.Unix(1_790_000_000, 0).Add(time.Hour)
	if r := e.svc.SyncTokenAccounts(ctx, dir, nil); len(r.Registered) != 1 {
		t.Fatalf("new token not attached: %+v", r)
	}
	if g, _ := e.svc.grantByID(ctx, e.svc.db, info.GrantID); g.Kind != contracts.CredOAuthToken || g.State != contracts.GrantValid {
		t.Fatalf("re-attached: %+v", g)
	}
}

// fakeDir is the hub's account directory as a node sees it.
type fakeDir struct {
	accts   []contracts.Account
	toks    []contracts.AccountTokenStatus
	aliases map[string]string
}

func (d *fakeDir) ListAccounts(_ context.Context, _ contracts.ListAccountsRequest) (contracts.Page[contracts.AccountView], error) {
	var p contracts.Page[contracts.AccountView]
	for _, a := range d.accts {
		p.Items = append(p.Items, contracts.AccountView{Account: a})
	}
	return p, nil
}

func (d *fakeDir) ListAccountTokens(_ context.Context, req contracts.ListAccountTokensRequest) ([]contracts.AccountTokenStatus, error) {
	id := req.AccountID
	if to, ok := d.aliases[id]; ok {
		id = to
	}
	out := []contracts.AccountTokenStatus{}
	for _, s := range d.toks {
		if id == "" || s.AccountID == id {
			out = append(out, s)
		}
	}
	return out, nil
}

// A node that never ran add-token gets every token account the hub lists
// (a new node, or the harness installed later), and nothing when the
// harness is missing or the token is restricted to other nodes.
func TestSyncRegistersTokenAccountsOnNewNode(t *testing.T) {
	e := newTokenEnv(t)
	ctx := context.Background()
	e.toks.tok["acct_synth-new"] = synthToken
	e.toks.tok["acct_synth-elsewhere"] = synthToken
	dir := &fakeDir{
		accts: []contracts.Account{
			{AccountID: "acct_synth-new", Provider: contracts.ProviderAnthropic, Label: "synth-new", Kind: "subscription"},
			{AccountID: "acct_synth-elsewhere", Provider: contracts.ProviderAnthropic, Label: "synth-else", Kind: "subscription"},
		},
		toks: []contracts.AccountTokenStatus{
			{AccountID: "acct_synth-new", Provider: contracts.ProviderAnthropic, Set: true},
			{AccountID: "acct_synth-elsewhere", Provider: contracts.ProviderAnthropic, Set: true, Nodes: []string{"node_other"}},
		},
	}
	noClaude := func(h contracts.Harness) bool { return h != contracts.HarnessClaude }
	if r := e.svc.SyncTokenAccounts(ctx, dir, noClaude); len(r.Registered) != 0 {
		t.Fatalf("registered without the harness: %+v", r)
	}
	r := e.svc.SyncTokenAccounts(ctx, dir, nil)
	if len(r.Registered) != 1 || r.Registered[0] != "acct_synth-new" || len(r.Errors) != 0 {
		t.Fatalf("sync: %+v", r)
	}
	g, err := e.svc.getGrant(ctx, e.svc.db, "acct_synth-new", contracts.HarnessClaude)
	if err != nil || g.Kind != contracts.CredOAuthToken || g.State != contracts.GrantValid {
		t.Fatalf("grant: %+v %v", g, err)
	}
	// Idempotent: nothing more to do.
	if r := e.svc.SyncTokenAccounts(ctx, dir, nil); len(r.Registered) != 0 {
		t.Fatalf("second sync: %+v", r)
	}
	// Selection works on the new node.
	if res, err := e.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: eligible, RunID: "run_new", SessionID: "ses_new"}); err != nil || res.Assignment.AccountID != "acct_synth-new" {
		t.Fatalf("resolve: %v", e.err(err))
	}
	e.noTokenAnywhere()
}

// The hub folded a token-only account into the person's rota account: the
// node follows (alias, old grant retired after its running session), and
// the survivor runs on the token.
func TestSyncFollowsHubFold(t *testing.T) {
	e := newRotaTokenEnv(t)
	ctx := context.Background()
	e.writeRotaClaude("R", "g1", time.Now().Add(time.Hour))
	info := e.addRotaAccount("acct_synth-r", contracts.HarnessClaude, "R", false)
	// The duplicate token-only account, registered by an older CLI.
	e.setToken("acct_synth-rtok", true)
	if _, err := e.svc.Register(ctx, RegisterParams{Account: contracts.Account{AccountID: "acct_synth-rtok", Provider: contracts.ProviderAnthropic, Label: "R", Kind: "subscription"},
		Harness: contracts.HarnessClaude, Source: contracts.SourceToken}); err != nil {
		t.Fatal(err)
	}
	run, err := e.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: explicit("acct_synth-rtok"), RunID: "run_old", SessionID: "ses_old"})
	if err != nil || run.CredentialKind != contracts.CredOAuthToken {
		t.Fatalf("run on the duplicate: %v", e.err(err))
	}
	// The hub folds it: the token now belongs to acct_synth-r.
	e.setToken("acct_synth-r", true)
	dir := &fakeDir{accts: []contracts.Account{e.rotaAccount("acct_synth-r", "R")},
		toks:    []contracts.AccountTokenStatus{{AccountID: "acct_synth-r", Provider: contracts.ProviderAnthropic, Set: true}},
		aliases: map[string]string{"acct_synth-rtok": "acct_synth-r"}}
	r := e.svc.SyncTokenAccounts(ctx, dir, nil)
	if len(r.Folded) != 1 || r.Folded[0] != "acct_synth-rtok->acct_synth-r" || len(r.Registered) != 1 || r.Registered[0] != "acct_synth-r" {
		t.Fatalf("sync: %+v", r)
	}
	// The running session on the old ID keeps working (its process already
	// has the token; nothing stops it and its account stays until it ends);
	// the old grant is no longer selected; the survivor runs on the token.
	if g, _ := e.svc.getGrant(ctx, e.svc.db, "acct_synth-rtok", contracts.HarnessClaude); g.State != contracts.GrantRevoked {
		t.Fatalf("old grant still selectable: %+v", g)
	}
	if _, err := e.svc.Account(ctx, "acct_synth-rtok"); err != nil {
		t.Fatalf("old account removed while its run is active: %v", err)
	}
	nr, err := e.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: eligible, RunID: "run_new", SessionID: "ses_new"})
	if err != nil || nr.Assignment.AccountID != "acct_synth-r" || nr.CredentialKind != contracts.CredOAuthToken {
		t.Fatalf("new run: %+v %v", nr.Assignment, e.err(err))
	}
	if nr.Assignment.GrantID != info.GrantID {
		t.Fatalf("survivor grant %s, want the converted %s", nr.Assignment.GrantID, info.GrantID)
	}
	// An explicit policy naming the old ID selects the survivor.
	xr, err := e.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: explicit("acct_synth-rtok"), RunID: "run_x", SessionID: "ses_x"})
	if err != nil || xr.Assignment.AccountID != "acct_synth-r" {
		t.Fatalf("explicit old ID: %v", e.err(err))
	}
	// The old run ends: the old account disappears from this node.
	e.svc.Release("run_old")
	e.svc.SyncTokenAccounts(ctx, dir, nil)
	if _, err := e.svc.Account(ctx, "acct_synth-rtok"); !contracts.IsCode(err, contracts.ErrNotFound) {
		t.Fatalf("old account still here: %v", err)
	}
	if gs, _ := e.svc.listGrants(ctx, e.svc.db, "acct_synth-rtok"); len(gs) != 0 {
		t.Fatalf("old grants: %+v", gs)
	}
}

// noTokenIn fails if the synthetic token is in any file under dir.
func (e *rotaTokenEnv) noTokenIn(dir string) {
	e.t.Helper()
	_ = filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		if b, err := os.ReadFile(p); err == nil && strings.Contains(string(b), synthToken) {
			e.t.Errorf("token in %s", p)
		}
		return nil
	})
}
