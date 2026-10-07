package transport

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

func TestAuthStoreTokens(t *testing.T) {
	db := openDB(t, "auth.db")
	clock := newFakeClock()
	a, err := NewAuthStore(db, clock)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	cases := []struct {
		kind   contracts.PrincipalKind
		prefix string
	}{{contracts.PrincipalOwner, PrefixOwnerToken}, {contracts.PrincipalNode, PrefixNodeToken}, {contracts.PrincipalSession, PrefixSessionToken}}
	for _, c := range cases {
		tok, err := a.Issue(ctx, c.kind, "id_"+string(c.kind), "name")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(tok, c.prefix) || len(tok) < 40 {
			t.Fatalf("token %s lacks prefix/entropy", c.prefix)
		}
		p, err := a.Authenticate(ctx, tok)
		if err != nil || p.Kind != c.kind || p.ID != "id_"+string(c.kind) {
			t.Fatalf("authenticate %s: %+v %v", c.kind, p, err)
		}
		// A token is only valid with its own prefix.
		other := PrefixOwnerToken + strings.TrimPrefix(tok, c.prefix)
		if c.kind != contracts.PrincipalOwner {
			if _, err := a.Authenticate(ctx, other); !contracts.IsCode(err, contracts.ErrUnauthorized) {
				t.Fatalf("prefix swap accepted: %v", err)
			}
		}
		// Only hashes are stored.
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM auth_tokens WHERE token_hash=? OR token_hash=?`, tok, HashSecret(tok)).Scan(&n); err != nil {
			t.Fatal(err)
		}
		var plain int
		_ = db.QueryRow(`SELECT COUNT(*) FROM auth_tokens WHERE token_hash=? OR principal_id=? OR name=?`, tok, tok, tok).Scan(&plain)
		if n != 1 || plain != 0 {
			t.Fatalf("storage: hash rows=%d plaintext rows=%d", n, plain)
		}
	}
	if _, err := a.Issue(ctx, contracts.PrincipalSystem, "x", ""); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("system principal token: %v", err)
	}
	for _, bad := range []string{"", "garbage", PrefixNodeToken + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"} {
		if _, err := a.Authenticate(ctx, bad); !contracts.IsCode(err, contracts.ErrUnauthorized) {
			t.Fatalf("bad token %q: %v", bad, err)
		}
	}

	// Session tokens revoke per session.
	s1, _ := a.IssueSessionToken(ctx, "ses_1")
	s2, _ := a.IssueSessionToken(ctx, "ses_1")
	s3, _ := a.IssueSessionToken(ctx, "ses_2")
	var notified []contracts.Principal
	a.OnRevoke(func(p contracts.Principal) { notified = append(notified, p) })
	if err := a.RevokeSessionTokens(ctx, "ses_1"); err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{s1, s2} {
		if _, err := a.Authenticate(ctx, tok); !contracts.IsCode(err, contracts.ErrUnauthorized) {
			t.Fatalf("revoked session token accepted: %v", err)
		}
	}
	if _, err := a.Authenticate(ctx, s3); err != nil {
		t.Fatalf("other session revoked: %v", err)
	}
	if len(notified) != 1 || notified[0].ID != "ses_1" {
		t.Fatalf("revocation listeners: %+v", notified)
	}
	lu, err := a.LastUsed(ctx, contracts.PrincipalSession, "ses_2")
	if err != nil || lu == nil || !lu.Equal(clock.Now()) {
		t.Fatalf("last used = %v %v", lu, err)
	}
}

func TestTicketsRedeemOnceAndExpire(t *testing.T) {
	clock := newFakeClock()
	a, err := NewAuthStore(openDB(t, "auth.db"), clock)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ref := contracts.ObjectRef{Kind: contracts.KindRun, ID: "run_1"}
	tk, exp, err := a.IssueTicket(ctx, "terminal", ref, 30*time.Second)
	if err != nil || !exp.Equal(clock.Now().Add(30*time.Second)) || !strings.HasPrefix(tk, PrefixTicket) {
		t.Fatalf("issue: %v %v", exp, err)
	}
	if _, err := a.RedeemTicket(ctx, tk, "preview"); !contracts.IsCode(err, contracts.ErrUnauthorized) {
		t.Fatalf("wrong purpose: %v", err)
	}
	got, err := a.RedeemTicket(ctx, tk, "terminal")
	if err != nil || got != ref {
		t.Fatalf("redeem: %+v %v", got, err)
	}
	if _, err := a.RedeemTicket(ctx, tk, "terminal"); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("second redeem: %v", err)
	}
	tk2, _, _ := a.IssueTicket(ctx, "terminal", ref, time.Second)
	clock.Advance(2 * time.Second)
	if _, err := a.RedeemTicket(ctx, tk2, "terminal"); !contracts.IsCode(err, contracts.ErrExpired) {
		t.Fatalf("expired: %v", err)
	}
	if _, err := a.RedeemTicket(ctx, "pyt_unknownunknownunknown", "terminal"); !contracts.IsCode(err, contracts.ErrUnauthorized) {
		t.Fatalf("unknown: %v", err)
	}
}

func TestRedact(t *testing.T) {
	tok, _ := NewSecret(PrefixNodeToken)
	sec, _ := NewSecret(PrefixEnrollSecret)
	in := "failed with " + tok + " and Authorization: Bearer abc.def and secret=" + sec
	out := Redact(in)
	if strings.Contains(out, tok[4:]) || strings.Contains(out, sec[4:]) || strings.Contains(out, "abc.def") {
		t.Fatalf("not redacted: %s", out)
	}
	if !strings.Contains(out, "pyn_[REDACTED]") || !strings.Contains(out, "pye_[REDACTED]") {
		t.Fatalf("markers missing: %s", out)
	}
	buf := &syncBuffer{}
	l := NewRedactingLogger(slog.NewJSONHandler(buf, nil))
	l.With("cred", tok).Info("msg "+tok, "token", tok, "err", contracts.Errorf(contracts.ErrInternal, "x %s", tok), slog.Group("g", "t", tok))
	if strings.Contains(buf.String(), tok[4:]) {
		t.Fatalf("logger leaked: %s", buf.String())
	}
}

func TestHubCertAndPinning(t *testing.T) {
	dir := t.TempDir()
	c1, err := EnsureHubCert(dir, []string{"hub.local", "10.0.0.5"})
	if err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(filepath.Join(dir, HubKeyFile))
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %o", st.Mode().Perm())
	}
	c2, err := EnsureHubCert(dir, []string{"other"})
	if err != nil || c2.Fingerprint != c1.Fingerprint {
		t.Fatalf("cert not reused: %v", err)
	}
	if len(c1.Leaf.DNSNames) != 1 || len(c1.Leaf.IPAddresses) != 1 {
		t.Fatalf("SANs: %v %v", c1.Leaf.DNSNames, c1.Leaf.IPAddresses)
	}
	if fp, err := NormalizeFingerprint("SHA256:" + strings.ToUpper(c1.Fingerprint)); err != nil || fp != c1.Fingerprint {
		t.Fatalf("normalize: %v", err)
	}
	if _, err := NormalizeFingerprint("abcd"); err == nil {
		t.Fatal("short fingerprint accepted")
	}

	// Loose key permissions are refused.
	if err := os.Chmod(filepath.Join(dir, HubKeyFile), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureHubCert(dir, nil); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("loose key accepted: %v", err)
	}
	os.Chmod(filepath.Join(dir, HubKeyFile), 0o600)
	// A lone cert is refused rather than silently replaced.
	lone := t.TempDir()
	EnsureHubCert(lone, nil)
	os.Remove(filepath.Join(lone, HubKeyFile))
	if _, err := EnsureHubCert(lone, nil); err == nil {
		t.Fatal("missing key silently regenerated")
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	srv.TLS = c1.ServerTLSConfig()
	srv.StartTLS()
	defer srv.Close()
	get := func(cfg *tls.Config) error {
		cl := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}
		resp, err := cl.Get(srv.URL)
		if err == nil {
			resp.Body.Close()
		}
		return err
	}
	good, _ := PinnedTLSConfig(c1.Fingerprint)
	if err := get(good); err != nil {
		t.Fatalf("pinned: %v", err)
	}
	other, _ := EnsureHubCert(t.TempDir(), nil)
	bad, _ := PinnedTLSConfig(other.Fingerprint)
	if err := get(bad); err == nil || !strings.Contains(err.Error(), "does not match pinned") {
		t.Fatalf("wrong pin accepted: %v", err)
	}
	ca, _ := ClientTLSConfig("")
	if err := get(ca); err == nil {
		t.Fatal("self-signed cert accepted by CA-validated mode")
	}
}

func TestBackoffBoundsAndGrowth(t *testing.T) {
	b := NewBackoff(BackoffConfig{Initial: 100 * time.Millisecond, Max: 2 * time.Second, Jitter: 0.5})
	prevCeil := time.Duration(0)
	for i := 0; i < 200; i++ {
		ceil := b.ceiling(b.Attempt())
		d := b.Next()
		if d > 2*time.Second || d < ceil/2 || d > ceil {
			t.Fatalf("attempt %d: delay %v outside [%v,%v]", i, d, ceil/2, ceil)
		}
		if ceil < prevCeil {
			t.Fatalf("ceiling shrank")
		}
		prevCeil = ceil
	}
	if prevCeil != 2*time.Second {
		t.Fatalf("never reached max: %v", prevCeil)
	}
	b.Reset()
	if d := b.Next(); d > 100*time.Millisecond {
		t.Fatalf("reset: %v", d)
	}
	if d := b.MaxDelay(); d < time.Second || d > 2*time.Second {
		t.Fatalf("max delay %v", d)
	}
}

// Transient reconnects cap at 10 s by default (quick repeated hub outages
// must not push the node to ~40-60 s gaps); rejections back off to 60 s.
func TestBackoffDefaultCaps(t *testing.T) {
	b := NewBackoff(BackoffConfig{})
	var maxSeen time.Duration
	for i := 0; i < 40; i++ {
		maxSeen = max(maxSeen, b.Next())
	}
	if maxSeen > 10*time.Second || maxSeen < 5*time.Second {
		t.Fatalf("transient cap: max delay %v", maxSeen)
	}
	var rej time.Duration
	for i := 0; i < 20; i++ {
		rej = max(rej, b.MaxDelay())
	}
	if rej <= 10*time.Second || rej > 60*time.Second {
		t.Fatalf("rejection cap: %v", rej)
	}
	l, err := NewNodeLink(NodeLinkConfig{HubURL: "https://hub.synth.invalid", NodeID: "node_x", Token: "pyn_synthetic-token-for-test",
		DB: openDB(t, "n.db"), Handler: &countingHandler{}})
	if err != nil {
		t.Fatal(err)
	}
	if l.cfg.StableAfter != 10*time.Second {
		t.Fatalf("default StableAfter %v", l.cfg.StableAfter)
	}
}
