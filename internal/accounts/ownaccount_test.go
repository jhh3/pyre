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

const synthOwnEmail = "synth-own@example.invalid"

// ownEnv: a node whose own Codex login is synthOwnEmail; the hub knows the
// matching account (no Pyre login anywhere) and another person's account.
func ownEnv(t *testing.T) *testEnv {
	t.Helper()
	e := newEnv(t, func(c *Config) {
		c.Fleet = ownFleet{views: []contracts.AccountView{
			{Account: contracts.Account{AccountID: "acct_synth-other", Provider: contracts.ProviderOpenAI, RotaRef: "synth-other@example.invalid", Kind: "subscription"}},
			{Account: contracts.Account{AccountID: "acct_synth-own", Provider: contracts.ProviderOpenAI, RotaRef: synthOwnEmail, Label: "own", Kind: "subscription"}},
			{Account: contracts.Account{AccountID: "acct_synth-anth", Provider: contracts.ProviderAnthropic, RotaRef: synthOwnEmail}},
		}}
	})
	home := e.svc.cfg.UserCodexDir
	_ = os.MkdirAll(home, 0o700)
	writeJSON(t, filepath.Join(home, "auth.json"), map[string]any{"auth_mode": "chatgpt", "tokens": map[string]any{
		"id_token":     synthJWT(map[string]any{"email": synthOwnEmail, "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "synth-acct-own"}}),
		"access_token": synthJWT(map[string]any{"kind": "access"}), "refresh_token": synthRefresh("own")}}, 0o600)
	return e
}

// Live smoke: `pyre start --harness codex --account <own-login account>`
// failed with "account not found" on the node (no local record). The node
// now runs it on its own login with the account named on the assignment.
func TestExplicitOwnLoginAccountRunsAmbient(t *testing.T) {
	e := ownEnv(t)
	ctx := context.Background()
	res, err := e.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessCodex, Policy: explicit("acct_synth-own"), RunID: "run_1", SessionID: "ses_1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.CredentialKind != contracts.CredAmbient || res.ProfileDir != "" || res.Assignment == nil || res.Assignment.AccountID != "acct_synth-own" {
		t.Fatalf("resolution %+v asg %+v", res, res.Assignment)
	}
	if !strings.Contains(res.Assignment.Reason, "explicit account") || !strings.Contains(res.Assignment.Reason, "own codex login") {
		t.Fatalf("reason %q", res.Assignment.Reason)
	}
	if len(e.rep.of(contracts.ReportAssignment)) != 1 {
		t.Fatal("assignment not reported to the hub")
	}
	if err := e.svc.CheckAssignment(ctx, "run_1"); err != nil {
		t.Fatal(err)
	}
	sel, err := e.svc.Select(ctx, contracts.AccountSelectParams{Provider: contracts.ProviderOpenAI, Policy: explicit("acct_synth-own")})
	if err != nil || sel.AccountID != "acct_synth-own" {
		t.Fatalf("dry run %+v %v", sel, err)
	}
}

// Another identity's account is refused clearly (never run on this login);
// a cross-provider or unknown account keeps its error.
func TestExplicitOtherIdentityRefused(t *testing.T) {
	e := ownEnv(t)
	ctx := context.Background()
	_, err := e.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessCodex, Policy: explicit("acct_synth-other"), RunID: "run_1"})
	ce := mustCode(t, err, contracts.ErrConflict)
	if !strings.Contains(ce.Message, "not this node's own codex login") || !strings.Contains(ce.Message, synthOwnEmail) {
		t.Fatalf("message %q", ce.Message)
	}
	_, err = e.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessCodex, Policy: explicit("acct_synth-anth"), RunID: "run_2"})
	mustCode(t, err, contracts.ErrInvalid)
	_, err = e.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessCodex, Policy: explicit("acct_synth-nowhere"), RunID: "run_3"})
	mustCode(t, err, contracts.ErrNotFound)
	if a, err := e.svc.assignmentForRun(ctx, e.svc.db, "run_1"); err == nil {
		t.Fatalf("refused run got an assignment %+v", a)
	}
}

// Automatic selection: a node without Pyre accounts runs on its own login and
// names the account; a node whose Pyre logins are all limited picks its own
// login the same way instead of waiting.
func TestAutomaticPicksOwnLoginAccount(t *testing.T) {
	e := ownEnv(t)
	ctx := context.Background()
	res, err := e.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessCodex, Policy: eligible, RunID: "run_1"})
	if err != nil || res.CredentialKind != contracts.CredAmbient || res.Assignment.AccountID != "acct_synth-own" {
		t.Fatalf("no-accounts node: %+v %v", res.Assignment, err)
	}

	e.addAccount("acct_synth-a", contracts.HarnessCodex, "A")
	e.observe("acct_synth-a", contracts.ProviderOpenAI, 100, time.Minute)
	putCooldown(ctx, e.svc.db, "acct_synth-a", e.clock.Now().Add(2*time.Hour), "usage_limit")
	res, err = e.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessCodex, Policy: eligible, RunID: "run_2"})
	if err != nil || res.CredentialKind != contracts.CredAmbient || res.Assignment.AccountID != "acct_synth-own" {
		t.Fatalf("limited node: %+v %v", res.Assignment, err)
	}
	if !strings.Contains(res.Assignment.Reason, "no Pyre login on this node is eligible") {
		t.Fatalf("reason %q", res.Assignment.Reason)
	}
}
