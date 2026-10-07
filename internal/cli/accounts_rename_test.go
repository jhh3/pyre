package cli

import (
	"context"
	"strings"
	"testing"

	c "github.com/jhh3/pyre/internal/contracts"
)

func TestAccountsRenameCLI(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	ctx := c.WithPrincipal(context.Background(), testOwner)
	a, err := h.b.UpsertAccount(ctx, c.UpsertAccountRequest{Mutation: mut(""), Provider: c.ProviderAnthropic,
		Label: "synth-laptop", RotaRef: "synth@example.invalid", PerMachineIndependent: true})
	if err != nil {
		t.Fatal(err)
	}
	// By label prefix; only the label changes (rota binding and flags kept).
	r := run(t, e, nil, "--json", "accounts", "rename", "synth-lap", "synth")
	expectCode(t, r, 0)
	got := mustJSON[c.Account](t, r)
	if got.AccountID != a.AccountID || got.Label != "synth" || got.RotaRef != a.RotaRef || !got.PerMachineIndependent {
		t.Fatalf("rename: %+v", got)
	}
	r = run(t, e, nil, "accounts", "rename", a.AccountID, "synthetic one")
	expectCode(t, r, 0)
	if !strings.Contains(r.stdout, `is now "synthetic one"`) {
		t.Fatalf("text: %s", r)
	}
	expectCode(t, run(t, e, nil, "accounts", "rename", a.AccountID), ExitUsage)
	expectCode(t, run(t, e, nil, "accounts", "rename", a.AccountID, "  "), ExitUsage)
}

func TestDefaultAccountLabel(t *testing.T) {
	for in, want := range map[string]string{"dev@example.invalid": "dev", "plain": "plain", " me@x ": "me", "@odd": "@odd"} {
		if got := DefaultAccountLabel(in); got != want {
			t.Errorf("%q: %q want %q", in, got, want)
		}
	}
}
