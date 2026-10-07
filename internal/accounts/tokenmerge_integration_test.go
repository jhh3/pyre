package accounts

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/client"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/httpapi"
	"github.com/jhh3/pyre/internal/hub"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
	"github.com/jhh3/pyre/internal/usage"
)

// Isolated-hub integration (W-tokmerge): a real hub over HTTP, two nodes
// (accounts services with their own databases, profiles and node
// credentials) and synthetic tokens only. Node reports go to the hub's
// report handler; token fetches and the account directory go through the
// node's own HTTP client, as in pyred.

type hubReporter struct {
	h      *hub.Hub
	nodeID string
	mu     sync.Mutex
	seq    uint64
	errs   []string
}

func (r *hubReporter) Report(ctx context.Context, kind contracts.ReportKind, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.seq++
	seq := r.seq
	r.mu.Unlock()
	_, err = r.h.HandleReport(ctx, r.nodeID, contracts.NodeReport{OriginSeq: seq, Kind: kind, At: time.Now().UTC(), Payload: b})
	if err != nil {
		r.mu.Lock()
		r.errs = append(r.errs, string(kind)+": "+err.Error())
		r.mu.Unlock()
	}
	return err
}

type clientTokens struct{ cl *client.Client }

func (t clientTokens) FetchAccountToken(ctx context.Context, id string) (string, time.Time, error) {
	f, err := t.cl.FetchAccountToken(ctx, contracts.FetchAccountTokenRequest{AccountID: id})
	return f.Token, f.UpdatedAt, err
}

func TestTokenMergeIntegrationTwoNodes(t *testing.T) {
	ctx := context.Background()
	hubDir := t.TempDir()
	db, err := sqlitex.Open(filepath.Join(hubDir, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	h, err := hub.New(hub.Config{DB: db, DataDir: hubDir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	nodeA, nodeB := contracts.NewID(contracts.PrefixNode), contracts.NewID(contracts.PrefixNode)
	owner := contracts.Principal{Kind: contracts.PrincipalOwner, ID: "own_synth", Name: "synthetic owner"}
	auth := httpapi.NewStaticTokens(map[string]contracts.Principal{
		"synthetic-owner-cred": owner,
		"synthetic-node-a":     {Kind: contracts.PrincipalNode, ID: nodeA},
		"synthetic-node-b":     {Kind: contracts.PrincipalNode, ID: nodeB},
	})
	srv := httptest.NewServer(httpapi.NewServer(h, auth, httpapi.Options{StreamWait: time.Second}))
	t.Cleanup(srv.Close)
	opts := client.Options{RetryBase: time.Millisecond, MaxAttempts: 2}
	own := client.New(srv.URL, "synthetic-owner-cred", opts)
	clA, clB := client.New(srv.URL, "synthetic-node-a", opts), client.New(srv.URL, "synthetic-node-b", opts)

	msgs := newFakeMessages(t)
	node := func(id string, cl *client.Client) (*rotaEnv, *hubReporter) {
		rep := &hubReporter{h: h, nodeID: id}
		e := newRotaEnv(t, func(c *Config) {
			c.NodeID, c.Reporter, c.Tokens = id, rep, clientTokens{cl}
			c.ClaudeMessagesURL, c.HTTPClient = msgs.srv.URL+"/v1/messages", msgs.srv.Client()
		})
		return e, rep
	}
	a, repA := node(nodeA, clA)

	// The live state before the upgrade: the person's rota account (grant on
	// node A) and a token-only duplicate an older add-token created, used
	// by a session running on node A.
	person, err := own.UpsertAccount(ctx, contracts.UpsertAccountRequest{Mutation: contracts.Mutation{OperationID: contracts.NewID(contracts.PrefixOperation)},
		Provider: contracts.ProviderAnthropic, Label: "synth-person-rota", RotaRef: rotaName("P"), ProviderRef: claudeUUID("P")})
	if err != nil {
		t.Fatal(err)
	}
	a.writeRotaClaude("P", "g1", time.Now().Add(time.Hour))
	if _, err := a.svc.Register(ctx, RegisterParams{Account: person, Harness: contracts.HarnessClaude, Source: "rota"}); err != nil {
		t.Fatal(err)
	}
	dup, err := own.UpsertAccount(ctx, contracts.UpsertAccountRequest{Mutation: contracts.Mutation{OperationID: contracts.NewID(contracts.PrefixOperation)},
		Provider: contracts.ProviderAnthropic, Label: "synth-person", PerMachineIndependent: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := own.SetAccountToken(ctx, contracts.SetAccountTokenRequest{Mutation: contracts.Mutation{OperationID: contracts.NewID(contracts.PrefixOperation)},
		AccountID: dup.AccountID, Token: synthToken}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.svc.Register(ctx, RegisterParams{Account: dup, Harness: contracts.HarnessClaude, Source: contracts.SourceToken}); err != nil {
		t.Fatal(err)
	}
	running, err := a.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: explicit(dup.AccountID), RunID: "run_synth-old", SessionID: "ses_synth-old"})
	if err != nil || running.CredentialKind != contracts.CredOAuthToken {
		t.Fatalf("run on the duplicate: %v", a.err(err))
	}
	label := "synth-person"
	if _, err := own.UpdateAccount(ctx, contracts.UpdateAccountRequest{Mutation: contracts.Mutation{OperationID: contracts.NewID(contracts.PrefixOperation)},
		AccountID: person.AccountID, Label: &label}); err != nil {
		t.Fatal(err)
	}
	if n := countLabel(t, own, label); n != 2 {
		t.Fatalf("setup: %d accounts labelled %s", n, label)
	}

	// The upgraded hub folds the duplicate on its own (maintenance pass).
	if err := h.RunMaintenance(ctx); err != nil {
		t.Fatal(err)
	}
	if n := countLabel(t, own, label); n != 1 {
		t.Fatalf("after fold: %d accounts", n)
	}

	// Node A follows: alias, survivor on the token (converted rota grant).
	ra := a.svc.SyncTokenAccounts(ctx, clA, nil)
	if len(ra.Folded) != 1 || len(ra.Registered) != 1 || ra.Registered[0] != person.AccountID || len(ra.Errors) != 0 {
		t.Fatalf("node A sync: %+v", ra)
	}
	// Node B, enrolled after the token was added, never ran add-token.
	b, repB := node(nodeB, clB)
	rb := b.svc.SyncTokenAccounts(ctx, clB, nil)
	if len(rb.Registered) != 1 || rb.Registered[0] != person.AccountID || len(rb.Errors) != 0 {
		t.Fatalf("node B sync: %+v", rb)
	}
	for _, e := range []*rotaEnv{a, b} {
		r, err := e.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: eligible, RunID: contracts.NewID(contracts.PrefixRun), SessionID: "ses_synth-new"})
		if err != nil || r.Assignment.AccountID != person.AccountID || r.CredentialKind != contracts.CredOAuthToken || r.Env["CLAUDE_CODE_OAUTH_TOKEN"] != synthToken {
			t.Fatalf("%s run: %+v %v", e.cfg.NodeID, r.Assignment, e.err(err))
		}
	}
	// The run that started on the duplicate ends on node A.
	a.svc.Release("run_synth-old")
	a.svc.SyncTokenAccounts(ctx, clA, nil)

	// The hub sees one account: token grants on both nodes, the in-flight
	// assignment repointed and released, the old account never re-created.
	page, err := own.ListAccounts(ctx, contracts.ListAccountsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var v *contracts.AccountView
	for i := range page.Items {
		switch page.Items[i].Account.AccountID {
		case person.AccountID:
			v = &page.Items[i]
		case dup.AccountID:
			t.Fatal("folded account listed again")
		}
	}
	if v == nil {
		t.Fatal("survivor missing")
	}
	nodes := map[string]contracts.CredentialKind{}
	for _, g := range v.Grants {
		nodes[g.NodeID] = g.Kind
	}
	if len(v.Grants) != 2 || nodes[nodeA] != contracts.CredOAuthToken || nodes[nodeB] != contracts.CredOAuthToken {
		t.Fatalf("grants: %+v", v.Grants)
	}
	if v.Account.RotaRef != rotaName("P") || v.Account.PerMachineIndependent {
		t.Fatalf("survivor: %+v", v.Account)
	}
	for _, asg := range v.Assignments {
		if asg.RunID == "run_synth-old" {
			t.Fatalf("old run's assignment still active: %+v", asg)
		}
	}
	// One row for the identity: "token · 2 machines" (no node list here).
	cards := usage.Cards(page.Items, nil, time.Now())
	n := 0
	for _, cd := range cards {
		if cd.Account != nil && cd.Account.AccountID == person.AccountID {
			n++
			if cd.Kind() != usage.KindToken {
				t.Fatalf("card kind %q", cd.Kind())
			}
		}
	}
	if n != 1 {
		t.Fatalf("%d cards for one identity", n)
	}
	if len(repA.errs)+len(repB.errs) != 0 {
		t.Fatalf("hub rejected reports: %v %v", repA.errs, repB.errs)
	}
	// The token is nowhere on disk (hub DB/WAL/key dir aside from ciphertext, node state).
	for _, dir := range []string{hubDir, a.base, b.base} {
		_ = filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
			if err != nil || fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
				return nil
			}
			if bs, err := os.ReadFile(p); err == nil && strings.Contains(string(bs), synthToken) {
				t.Errorf("token in %s", p)
			}
			return nil
		})
	}
}

func countLabel(t *testing.T, cl *client.Client, label string) int {
	t.Helper()
	page, err := cl.ListAccounts(context.Background(), contracts.ListAccountsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, v := range page.Items {
		if strings.EqualFold(v.Account.Label, label) {
			n++
		}
	}
	return n
}
