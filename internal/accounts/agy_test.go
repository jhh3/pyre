package accounts

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

// agy has no isolated profiles: every run is the node's own login, under
// any default account policy, and selection never balances it across
// accounts. Its identity is the id_token's email (decoded locally, never a
// token), so a registered google account with that rota_ref is named on
// the assignment.
func TestAgyRunsOnTheOwnLogin(t *testing.T) {
	email := "synth-agy@example.invalid"
	agyDir := ""
	e := newEnv(t, func(c *Config) {
		agyDir = filepath.Join(filepath.Dir(c.UserCodexDir), ".gemini", "antigravity-cli")
		c.UserAgyDir = agyDir
		c.DefaultAccount = DefaultAccountSubscription // would refuse an unmanaged login for claude/codex
		c.Fleet = ownFleet{views: []contracts.AccountView{
			{Account: contracts.Account{AccountID: "acct_synth-agy", Provider: contracts.ProviderGoogle, RotaRef: email, Label: "agy"}},
			{Account: contracts.Account{AccountID: "acct_synth-agy-other", Provider: contracts.ProviderGoogle, RotaRef: "other@example.invalid"}},
		}}
	})
	ctx := context.Background()

	// No sign-in yet: still ambient, no account named.
	res, err := e.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessAgy, Policy: contracts.AccountPolicy{Mode: contracts.AccountEligible}, RunID: "run_a0", SessionID: "ses_a"})
	if err != nil || res.CredentialKind != contracts.CredAmbient || res.ProfileDir != "" || res.Assignment == nil || res.Assignment.AccountID != "" {
		t.Fatalf("unsigned: %+v %v", res, err)
	}

	_ = os.MkdirAll(agyDir, 0o700)
	writeJSON(t, filepath.Join(agyDir, "antigravity-oauth-token"), map[string]any{
		"token":    map[string]any{"access_token": "pyre-synthetic-access", "refresh_token": "pyre-synthetic-refresh", "token_type": "Bearer"},
		"id_token": synthJWT(map[string]any{"email": email, "sub": "synth-sub-1"}), "auth_method": "synthetic"}, 0o600)
	l := e.svc.OwnLogin(contracts.HarnessAgy)
	if l == nil || l.Email != email || l.ProviderRef != "synth-sub-1" {
		t.Fatalf("own login %+v", l)
	}
	if b, _ := json.Marshal(l); strings.Contains(string(b), "pyre-synthetic") {
		t.Fatalf("identity carries token material: %s", b)
	}

	res, err = e.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessAgy, Policy: contracts.AccountPolicy{Mode: contracts.AccountEligible}, RunID: "run_a1", SessionID: "ses_a"})
	if err != nil || res.CredentialKind != contracts.CredAmbient || res.Assignment.AccountID != "acct_synth-agy" ||
		!strings.Contains(res.Assignment.Reason, "does not manage agy logins") {
		t.Fatalf("eligible: %+v %v", res.Assignment, err)
	}
	// Explicit: the own login's account runs; another identity is refused.
	if res, err := e.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessAgy, Policy: explicit("acct_synth-agy"), RunID: "run_a2"}); err != nil || res.Assignment.AccountID != "acct_synth-agy" {
		t.Fatalf("explicit own: %+v %v", res, err)
	}
	if _, err := e.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessAgy, Policy: explicit("acct_synth-agy-other"), RunID: "run_a3"}); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("explicit other identity: %v", err)
	}
	// The dry run (placement preflight) agrees.
	sel, err := e.svc.Select(ctx, contracts.AccountSelectParams{Provider: contracts.ProviderGoogle})
	if err != nil || sel.AccountID != "acct_synth-agy" || sel.Outcome != "" {
		t.Fatalf("select: %+v %v", sel, err)
	}
	if UnsetFor(contracts.HarnessAgy) != nil {
		t.Fatal("agy launches unset nothing")
	}
}
