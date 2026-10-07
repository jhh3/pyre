package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

func TestReadTokenFrom(t *testing.T) {
	tok, err := readTokenFrom(strings.NewReader("  sk-ant-oat01-pyre-synthetic-x\n"))
	if err != nil || tok != "sk-ant-oat01-pyre-synthetic-x" {
		t.Fatalf("%q %v", tok, err)
	}
	if _, err := readTokenFrom(strings.NewReader("a\nb\n")); err == nil {
		t.Fatal("two lines accepted")
	}
}

func TestTokenStatusLine(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	v := now.Add(-3 * time.Minute)
	st := c.AccountTokenStatus{Set: true, AddedAt: now.Add(-24 * time.Hour), VerifiedAt: &v}
	if got := tokenStatusLine(st, now); !strings.HasPrefix(got, "token set · added Oct 5 · last verified") {
		t.Fatalf("%q", got)
	}
	st.Rejected, st.Detail = true, "token revoked or expired"
	if got := tokenStatusLine(st, now); !strings.Contains(got, "rejected: token revoked") {
		t.Fatalf("%q", got)
	}
	if got := tokenStatusLine(c.AccountTokenStatus{}, now); got != "no token" {
		t.Fatalf("%q", got)
	}
}

// add-token attaches to the person's existing account (one account per
// identity) and refuses an ambiguous label.
func TestAddTokenAttachesToSameLabelAccount(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	ctx := c.WithPrincipal(context.Background(), testOwner)
	rota, err := h.b.UpsertAccount(ctx, c.UpsertAccountRequest{Mutation: mut(""), Provider: c.ProviderAnthropic,
		Label: "synth-person", RotaRef: "synth-person@example.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	tok := "sk-ant-oat01-SYNTHETIC-cli-attach\n"
	r := run(t, e, &App{Stdin: strings.NewReader(tok), IsTerminal: func() bool { return false }}, "--json", "accounts", "add-token", "Synth-Person", "--provider", "anthropic")
	expectCode(t, r, 0)
	res := mustJSON[AddTokenResult](t, r)
	if res.Account.AccountID != rota.AccountID || res.Created || !res.Attached || !res.Status.Set {
		t.Fatalf("attach: %+v", res)
	}
	if res.Account.PerMachineIndependent {
		t.Fatal("rota account marked independent by add-token")
	}
	page, _ := h.b.ListAccounts(ctx, c.ListAccountsRequest{PageRequest: c.PageRequest{Limit: 500}})
	n := 0
	for _, v := range page.Items {
		if strings.EqualFold(v.Account.Label, "synth-person") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("want one synth-person account, got %d", n)
	}
	// Again: the same account, now an update.
	r = run(t, e, &App{Stdin: strings.NewReader(tok), IsTerminal: func() bool { return false }}, "--json", "accounts", "add-token", "synth-person", "--provider", "anthropic")
	expectCode(t, r, 0)
	if again := mustJSON[AddTokenResult](t, r); again.Account.AccountID != rota.AccountID || again.Created || again.Attached {
		t.Fatalf("update: %+v", again)
	}
	// A new label creates a token account.
	r = run(t, e, &App{Stdin: strings.NewReader(tok), IsTerminal: func() bool { return false }}, "--json", "accounts", "add-token", "synth-new", "--provider", "anthropic")
	expectCode(t, r, 0)
	if nw := mustJSON[AddTokenResult](t, r); !nw.Created || nw.Account.AccountID == rota.AccountID {
		t.Fatalf("create: %+v", nw)
	}
	// Two accounts with one label: refused, naming both.
	other, _ := h.b.UpsertAccount(ctx, c.UpsertAccountRequest{Mutation: mut(""), Provider: c.ProviderAnthropic, Label: "synth-person", RotaRef: "other@example.invalid"})
	r = run(t, e, &App{Stdin: strings.NewReader(tok), IsTerminal: func() bool { return false }}, "accounts", "add-token", "synth-person", "--provider", "anthropic")
	if r.code == 0 || !strings.Contains(r.stderr, rota.AccountID) || !strings.Contains(r.stderr, other.AccountID) {
		t.Fatalf("ambiguous: %s", r)
	}
	if strings.Contains(r.stdout+r.stderr, "SYNTHETIC-cli-attach") {
		t.Fatal("token echoed")
	}
}
