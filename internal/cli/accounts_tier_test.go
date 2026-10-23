package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

func TestAccountsTierCLI(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	ctx := c.WithPrincipal(context.Background(), testOwner)
	a, err := h.b.UpsertAccount(ctx, c.UpsertAccountRequest{Mutation: mut(""), Provider: c.ProviderAnthropic,
		Label: "synth-agent", RotaRef: "synth@example.invalid", PerMachineIndependent: true})
	if err != nil {
		t.Fatal(err)
	}
	// By label prefix, as rename resolves accounts; nothing else changes.
	r := run(t, e, nil, "--json", "accounts", "tier", "synth-ag", "reserve")
	expectCode(t, r, 0)
	got := mustJSON[c.Account](t, r)
	if got.AccountID != a.AccountID || got.Tier != c.TierReserve || got.Label != "synth-agent" || got.RotaRef != a.RotaRef || !got.PerMachineIndependent {
		t.Fatalf("tier: %+v", got)
	}
	r = run(t, e, nil, "accounts", "tier", a.AccountID, "Normal")
	expectCode(t, r, 0)
	if !strings.Contains(r.stdout, "anthropic account synth-agent ("+a.AccountID+") is now normal") {
		t.Fatalf("text: %s", r)
	}
	page, err := h.b.ListAccounts(ctx, c.ListAccountsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range page.Items {
		if v.Account.AccountID == a.AccountID && v.Account.Tier != "" {
			t.Fatalf("normal stored as %q", v.Account.Tier)
		}
	}
	// A label shared across providers needs --provider (or the ID).
	o, err := h.b.UpsertAccount(ctx, c.UpsertAccountRequest{Mutation: mut(""), Provider: c.ProviderOpenAI, Label: "synth-agent"})
	if err != nil {
		t.Fatal(err)
	}
	r = run(t, e, nil, "accounts", "tier", "synth-agent", "reserve")
	if r.code == 0 || !strings.Contains(r.stderr, "several providers") {
		t.Fatalf("ambiguous label across providers: %s", r)
	}
	r = run(t, e, nil, "--json", "accounts", "tier", "synth-agent", "reserve", "--provider", "openai")
	expectCode(t, r, 0)
	if got := mustJSON[c.Account](t, r); got.AccountID != o.AccountID || !got.Reserve() {
		t.Fatalf("--provider openai: %+v", got)
	}
	expectCode(t, run(t, e, nil, "accounts", "tier", "synth-agent", "reserve", "--provider", "nope"), ExitUsage)
	expectCode(t, run(t, e, nil, "accounts", "tier", a.AccountID), ExitUsage)
	expectCode(t, run(t, e, nil, "accounts", "tier", a.AccountID, "spare"), ExitUsage)
	expectCode(t, run(t, e, nil, "accounts", "tier", a.AccountID, ""), ExitUsage)
}

// `pyre accounts` says "reserve" in NOTES.
func TestAccountsListShowsReserve(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	as := []c.AccountView{
		{Account: c.Account{AccountID: "acct_synth-r", Provider: c.ProviderAnthropic, Label: "synth-r", Kind: "subscription", Tier: c.TierReserve}},
		{Account: c.Account{AccountID: "acct_synth-n", Provider: c.ProviderAnthropic, Label: "synth-n", Kind: "subscription"}},
	}
	var b bytes.Buffer
	printAccounts(&b, as, nil, now, nil)
	var r, n string
	for _, l := range strings.Split(b.String(), "\n") {
		switch {
		case strings.Contains(l, "synth-r"):
			r = l
		case strings.Contains(l, "synth-n"):
			n = l
		}
	}
	if !strings.Contains(r, "reserve") || strings.Contains(n, "reserve") {
		t.Fatalf("output:\n%s", b.String())
	}
}
