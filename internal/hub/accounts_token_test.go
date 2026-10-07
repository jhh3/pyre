package hub

import (
	"bytes"
	"context"
	"database/sql"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

const synthHubToken = "sk-ant-oat01-pyre-synthetic-hub-token-0123456789"

func newTokenHub(t *testing.T) (*Hub, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := sqlitex.Open(filepath.Join(dir, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(Config{DB: db, DataDir: dir, Clock: &fakeClock{t: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close(); db.Close() })
	return h, dir
}

func tokOp() contracts.Mutation {
	return contracts.Mutation{OperationID: contracts.NewID(contracts.PrefixOperation)}
}

func TestAccountTokenStoreRoundTrip(t *testing.T) {
	h, dir := newTokenHub(t)
	ctx := context.Background()
	a, err := h.UpsertAccount(as(owner), contracts.UpsertAccountRequest{Mutation: tokOp(), Provider: contracts.ProviderAnthropic, Label: "synth-token"})
	if err != nil {
		t.Fatal(err)
	}
	nodeA, nodeB := contracts.NewID(contracts.PrefixNode), contracts.NewID(contracts.PrefixNode)
	sess := contracts.Principal{Kind: contracts.PrincipalSession, ID: "ses_synth-a"}

	// Shape and authorization.
	if _, err := h.SetAccountToken(as(owner), contracts.SetAccountTokenRequest{Mutation: tokOp(), AccountID: a.AccountID, Token: "not-a-setup-token"}); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("bad shape: %v", err)
	} else if strings.Contains(err.Error(), "not-a-setup-token") {
		t.Fatal("error echoes the token")
	}
	if _, err := h.SetAccountToken(as(sess), contracts.SetAccountTokenRequest{Mutation: tokOp(), AccountID: a.AccountID, Token: synthHubToken}); !contracts.IsCode(err, contracts.ErrForbidden) {
		t.Fatalf("non-owner: %v", err)
	}
	st, err := h.SetAccountToken(as(owner), contracts.SetAccountTokenRequest{Mutation: tokOp(), AccountID: a.AccountID, Token: synthHubToken})
	if err != nil || !st.Set || st.AddedAt.IsZero() || st.VerifiedAt != nil {
		t.Fatalf("set: %+v %v", st, err)
	}
	if acct, _ := h.getAccount(ctx, h.db, a.AccountID); !acct.PerMachineIndependent {
		t.Fatal("token account not marked independent")
	}

	// Key file private; ciphertext only in the DB.
	fi, err := os.Stat(filepath.Join(dir, TokenKeyFile))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", fi, err)
	}

	// Any node (no node list) may fetch; owners and sessions may not.
	got, err := h.FetchAccountToken(as(contracts.Principal{Kind: contracts.PrincipalNode, ID: nodeA}), contracts.FetchAccountTokenRequest{AccountID: a.AccountID})
	if err != nil || got.Token != synthHubToken {
		t.Fatalf("node fetch: %v", err)
	}
	if _, err := h.FetchAccountToken(as(owner), contracts.FetchAccountTokenRequest{AccountID: a.AccountID}); err == nil {
		t.Fatal("owner fetched a token")
	}
	if _, err := h.FetchAccountToken(as(sess), contracts.FetchAccountTokenRequest{AccountID: a.AccountID}); err == nil {
		t.Fatal("session fetched a token")
	}
	// A node list restricts fetches.
	if _, err := h.SetAccountToken(as(owner), contracts.SetAccountTokenRequest{Mutation: tokOp(), AccountID: a.AccountID, Token: synthHubToken, Nodes: []string{nodeA}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.FetchAccountToken(as(contracts.Principal{Kind: contracts.PrincipalNode, ID: nodeB}), contracts.FetchAccountTokenRequest{AccountID: a.AccountID}); !contracts.IsCode(err, contracts.ErrForbidden) {
		t.Fatalf("unlisted node: %v", err)
	}

	// Status: list never carries the token; a probe observation verifies it.
	obs := contracts.AccountObservation{AccountID: a.AccountID, Provider: contracts.ProviderAnthropic, ReportingNode: nodeA,
		ObservedAt: h.now().Add(time.Minute), Source: contracts.UsageSourceMessagesProbe, Confidence: "high", Eligibility: contracts.EligibleYes}
	if err := sqlitex.Tx(ctx, h.db, func(tx *sql.Tx) error { return h.ingestObservationTx(ctx, tx, nodeA, obs, 1) }); err != nil {
		t.Fatal(err)
	}
	list, err := h.ListAccountTokens(as(owner), contracts.ListAccountTokensRequest{})
	if err != nil || len(list) != 1 || list[0].VerifiedAt == nil || list[0].Rejected || len(list[0].Nodes) != 1 {
		t.Fatalf("list: %+v %v", list, err)
	}
	obs.ObservedAt, obs.Eligibility, obs.Detail = obs.ObservedAt.Add(time.Minute), contracts.EligibleUnusable, "token revoked or expired"
	if err := sqlitex.Tx(ctx, h.db, func(tx *sql.Tx) error { return h.ingestObservationTx(ctx, tx, nodeA, obs, 2) }); err != nil {
		t.Fatal(err)
	}
	if list, _ := h.ListAccountTokens(as(owner), contracts.ListAccountTokensRequest{}); !list[0].Rejected {
		t.Fatalf("rejected not shown: %+v", list[0])
	}

	// The persisted state never
	// holds the token (DB, WAL, operations, events).
	scanNoToken(t, dir, TokenKeyFile)

	// Remove: fetch says not found.
	if _, err := h.RemoveAccountToken(as(owner), contracts.RemoveAccountTokenRequest{Mutation: tokOp(), AccountID: a.AccountID}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.FetchAccountToken(as(contracts.Principal{Kind: contracts.PrincipalNode, ID: nodeA}), contracts.FetchAccountTokenRequest{AccountID: a.AccountID}); !contracts.IsCode(err, contracts.ErrNotFound) {
		t.Fatalf("after remove: %v", err)
	}
}

func TestAccountTokenKeyRequiredToDecrypt(t *testing.T) {
	h, dir := newTokenHub(t)
	a, _ := h.UpsertAccount(as(owner), contracts.UpsertAccountRequest{Mutation: tokOp(), Provider: contracts.ProviderAnthropic, Label: "synth-token"})
	if _, err := h.SetAccountToken(as(owner), contracts.SetAccountTokenRequest{Mutation: tokOp(), AccountID: a.AccountID, Token: synthHubToken}); err != nil {
		t.Fatal(err)
	}
	// A different hub key (e.g. a backup restored elsewhere) cannot open it.
	if err := os.Remove(filepath.Join(dir, TokenKeyFile)); err != nil {
		t.Fatal(err)
	}
	h.tokens.id = nil
	_, err := h.FetchAccountToken(as(contracts.Principal{Kind: contracts.PrincipalNode, ID: contracts.NewID(contracts.PrefixNode)}), contracts.FetchAccountTokenRequest{AccountID: a.AccountID})
	if err == nil || !strings.Contains(err.Error(), "add-token") {
		t.Fatalf("foreign key: %v", err)
	}
}

// scanNoToken fails if the synthetic token appears in any file under dir
// (except the named key file, which holds no token either).
func scanNoToken(t *testing.T, dir string, skip ...string) {
	t.Helper()
	n := 0
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		n++
		b, _ := os.ReadFile(p)
		if bytes.Contains(b, []byte(synthHubToken)) || bytes.Contains(b, []byte("pyre-synthetic-hub-token")) {
			t.Errorf("token found in %s", p)
		}
		return nil
	})
	if n < 2 {
		t.Fatalf("scanned only %d files", n)
	}
}

// A restarted hub must read back the key file it wrote (with its comment line).
func TestAccountTokenKeySurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	v1 := &tokenVault{dir: dir}
	id1, err := v1.identity()
	if err != nil {
		t.Fatal(err)
	}
	v2 := &tokenVault{dir: dir}
	id2, err := v2.identity()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if id1.String() != id2.String() {
		t.Fatal("reloaded a different identity")
	}
}
