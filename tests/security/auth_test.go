//go:build !windows

package security

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/transport"
)

// TestTokenAuthRejectsBadCredentials: unknown, malformed, wrong-kind,
// ticket-as-bearer and revoked credentials all get the same 401 without
// echoing the presented value; a valid token works; and nothing secret
// reaches logs, the database or responses.
func TestTokenAuthRejectsBadCredentials(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	if r := f.rpc(f.owner.Token, contracts.OpWhoAmI, nil); r.status != 200 || !strings.Contains(string(r.body), f.owner.ClientID) {
		t.Fatalf("valid owner token: %d %s", r.status, r.body)
	}
	ticket, _, err := f.auth.IssueTicket(context.Background(), "preview", contracts.ObjectRef{Kind: contracts.KindArtifactVersion, ID: "ver_x"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	revokedTok, err := f.auth.Issue(context.Background(), contracts.PrincipalOwner, "cli_to-revoke", "revoked")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.auth.RevokePrincipal(context.Background(), contracts.PrincipalOwner, "cli_to-revoke"); err != nil {
		t.Fatal(err)
	}
	unknown, _ := transport.NewSecret(transport.PrefixOwnerToken)
	wrongKind := "pyn_" + f.owner.Token[4:] // right bytes, wrong prefix
	f.remember(ticket, revokedTok, unknown, wrongKind)

	var bodies []byte
	for name, tok := range map[string]string{
		"missing": "", "unknown": unknown, "malformed": "not-a-token", "wrong-kind-prefix": wrongKind,
		"ticket-as-bearer": ticket, "revoked": revokedTok, "owner-token-with-suffix": f.owner.Token + "x",
	} {
		r := f.rpc(tok, contracts.OpWhoAmI, nil)
		if r.status != http.StatusUnauthorized || r.code() != contracts.ErrUnauthorized {
			t.Errorf("%s: status %d code %s", name, r.status, r.code())
		}
		if tok != "" && strings.Contains(string(r.body), tok) {
			t.Errorf("%s: response echoes the credential", name)
		}
		if name != "missing" && r.err != nil && r.err.Message != "invalid or revoked credential" {
			t.Errorf("%s: non-uniform message %q", name, r.err.Message)
		}
		bodies = append(bodies, r.body...)
	}
	// Revocation applies to live use, not only new logins.
	if _, err := f.auth.RevokePrincipal(context.Background(), contracts.PrincipalOwner, f.owner.ClientID); err != nil {
		t.Fatal(err)
	}
	if r := f.rpc(f.owner.Token, contracts.OpWhoAmI, nil); r.status != http.StatusUnauthorized {
		t.Fatalf("revoked owner token still accepted: %d", r.status)
	}
	f.assertNoLeaks(map[string][]byte{"responses": bodies})
}

// TestTicketsExpireAndRedeemOnce: one-time purpose-bound tickets (terminal
// attach, preview links) expire, redeem exactly once, and a wrong purpose
// neither works nor consumes the ticket. (Owner/node/session tokens have no
// expiry by design; they are revocable — see README.)
func TestTicketsExpireAndRedeemOnce(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	ctx := context.Background()
	ref := contracts.ObjectRef{Kind: contracts.KindRun, ID: "run_synthetic"}
	tk, _, err := f.auth.IssueTicket(ctx, "terminal", ref, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	f.remember(tk)
	if _, err := f.auth.RedeemTicket(ctx, tk, "preview"); !contracts.IsCode(err, contracts.ErrUnauthorized) {
		t.Fatalf("wrong purpose = %v", err)
	}
	got, err := f.auth.RedeemTicket(ctx, tk, "terminal")
	if err != nil || got != ref {
		t.Fatalf("redeem = %v %v (a wrong-purpose attempt must not consume it)", got, err)
	}
	if _, err := f.auth.RedeemTicket(ctx, tk, "terminal"); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("second redeem = %v, want conflict", err)
	}
	tk2, _, err := f.auth.IssueTicket(ctx, "terminal", ref, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	f.remember(tk2)
	f.clock.Advance(31 * time.Second)
	if _, err := f.auth.RedeemTicket(ctx, tk2, "terminal"); !contracts.IsCode(err, contracts.ErrExpired) {
		t.Fatalf("expired ticket = %v, want expired", err)
	}
	// TTLs are capped.
	_, exp, err := f.auth.IssueTicket(ctx, "terminal", ref, 1000*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if exp.Sub(f.clock.Now()) > transport.MaxTicketTTL {
		t.Fatalf("ticket TTL not capped: %v", exp.Sub(f.clock.Now()))
	}
}

// TestNoPlaintextSecretsAtRest: after a full credential lifecycle (owner
// bootstrap, enrollment + redeem, pairing + redeem, session tokens,
// tickets, idempotent replay of nodes.enroll which rotates the secret,
// revocation), no minted secret appears in the SQLite file, WAL or SHM, in
// any log line, or in any stored event payload.
func TestNoPlaintextSecretsAtRest(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	ctx := context.Background()
	nodeID, nodeTok := f.enrollNode("at-rest-node")
	_ = nodeID
	_, sessTok := f.startSession("at-rest")
	_ = sessTok
	// Pairing through the HTTP exchange (as a client would).
	pr := f.rpc(f.owner.Token, contracts.OpCreatePairing, contracts.PairingRequest{Mutation: op(), ClientName: "phone", Kind: "android"})
	if pr.status != 200 {
		t.Fatalf("pair: %d %s", pr.status, pr.body)
	}
	var pt contracts.PairingTicket
	mustJSON(t, pr.body, &pt)
	f.remember(pt.Code, pt.URI)
	ex := f.post(contracts.PathPairExchange, contracts.PairingExchange{PairingID: pt.PairingID, Code: pt.Code, ClientName: "phone", Kind: "android"})
	if ex.status != 200 {
		t.Fatalf("pair exchange: %d %s", ex.status, ex.body)
	}
	var cc contracts.ClientCredential
	mustJSON(t, ex.body, &cc)
	f.remember(cc.Token)
	// Replay nodes.enroll with the same operation id: the hub rotates the
	// secret rather than storing plaintext for replay.
	m := op()
	e1 := f.rpc(f.owner.Token, contracts.OpCreateEnrollment, contracts.EnrollmentRequest{Mutation: m, Name: "replayed"})
	e2 := f.rpc(f.owner.Token, contracts.OpCreateEnrollment, contracts.EnrollmentRequest{Mutation: m, Name: "replayed"})
	var t1, t2 contracts.EnrollmentTicket
	mustJSON(t, e1.body, &t1)
	mustJSON(t, e2.body, &t2)
	f.remember(t1.Secret, t2.Secret)
	if t1.NodeID != t2.NodeID || t1.Secret == t2.Secret {
		t.Fatalf("replayed enrollment: same node %v, rotated secret %v", t1.NodeID == t2.NodeID, t1.Secret != t2.Secret)
	}
	if _, err := f.enr.RedeemNode(ctx, contracts.NodeEnrollExchange{NodeID: t1.NodeID, Secret: t1.Secret}); !contracts.IsCode(err, contracts.ErrUnauthorized) {
		t.Fatalf("superseded enrollment secret = %v, want unauthorized", err)
	}
	for i := 0; i < 5; i++ {
		tk, _, err := f.auth.IssueTicket(ctx, "preview", contracts.ObjectRef{Kind: contracts.KindArtifactVersion, ID: "ver_x"}, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		f.remember(tk)
	}
	// Use every credential once so last_used paths and access logs run.
	for _, tok := range []string{nodeTok, sessTok, cc.Token, f.owner.Token} {
		f.rpc(tok, contracts.OpWhoAmI, nil)
	}
	if _, err := f.auth.RevokePrincipal(ctx, contracts.PrincipalNode, nodeID); err != nil {
		t.Fatal(err)
	}
	f.assertNoLeaks(nil)
}

// TestConstantTimeComparePresence is a static guard: secret verification
// must go through crypto/subtle, and the hub must look tokens up by hash
// (never compare plaintext with ==).
func TestConstantTimeComparePresence(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..", "..")
	type want struct {
		file, fn, call string
	}
	for _, w := range []want{
		{"internal/transport/tokens.go", "SecretMatches", "ConstantTimeCompare"},
		{"internal/transport/authstore.go", "Authenticate", "SecretMatches"},
		{"internal/transport/authstore.go", "RedeemTicket", "SecretMatches"},
		{"internal/transport/tls.go", "PinnedTLSConfig", "ConstantTimeCompare"},
		{"internal/enrollment/store.go", "RedeemNode", "SecretMatches"},
		{"internal/enrollment/store.go", "RedeemPairing", "SecretMatches"},
		{"internal/httpapi/auth.go", "Authenticate", "ConstantTimeCompare"},
		{"internal/client/client.go", "PinnedTLSConfig", "ConstantTimeCompare"},
	} {
		fs := token.NewFileSet()
		af, err := parser.ParseFile(fs, filepath.Join(root, w.file), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		found, calls := false, false
		ast.Inspect(af, func(n ast.Node) bool {
			fd, ok := n.(*ast.FuncDecl)
			if !ok || fd.Name.Name != w.fn {
				return true
			}
			found = true
			ast.Inspect(fd, func(n ast.Node) bool {
				if ce, ok := n.(*ast.CallExpr); ok {
					switch fn := ce.Fun.(type) {
					case *ast.SelectorExpr:
						if fn.Sel.Name == w.call {
							calls = true
						}
					case *ast.Ident:
						if fn.Name == w.call {
							calls = true
						}
					}
				}
				return true
			})
			return false
		})
		if !found || !calls {
			t.Errorf("%s: %s found=%v, calls %s=%v", w.file, w.fn, found, w.call, calls)
		}
	}
}
