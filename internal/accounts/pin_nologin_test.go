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

// server, 2026-11-05: three Codex starts named account "personal"
// (non-strict). server has no Pyre login of it and its own ~/.codex/auth.json
// is API-key mode (no own ChatGPT login), so every launch failed with "has
// no Pyre login on this node and this node has no own codex login" although
// "code" on server had room. A non-strict pin now falls back to the best
// eligible account here; a strict pin still refuses, and its dry run says
// so as an outcome so placement skips the node before a run is created.
func TestNonStrictPinWithNoLoginHereFallsBack(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, func(c *Config) {
		c.Fleet = ownFleet{views: []contracts.AccountView{
			{Account: contracts.Account{AccountID: "acct_synth-elsewhere", Provider: contracts.ProviderOpenAI, RotaRef: "synth-elsewhere@example.invalid", Label: "elsewhere", Kind: "subscription"}},
		}}
	})
	home := e.svc.cfg.UserCodexDir
	_ = os.MkdirAll(home, 0o700)
	writeJSON(t, filepath.Join(home, "auth.json"), map[string]any{"auth_mode": "apikey", "OPENAI_API_KEY": "sk-synthetic-not-a-key"}, 0o600)
	if e.svc.OwnLogin(contracts.HarnessCodex) != nil {
		t.Fatal("API-key auth.json read as an own login")
	}
	e.addAccount("acct_synth-code", contracts.HarnessCodex, "code")
	e.observe("acct_synth-code", contracts.ProviderOpenAI, 10, time.Minute)

	res, err := e.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessCodex, Policy: nonStrict("acct_synth-elsewhere"), RunID: "run_1", SessionID: "ses_1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Assignment == nil || res.Assignment.AccountID != "acct_synth-code" || res.CredentialKind == contracts.CredAmbient {
		t.Fatalf("bound %+v (%s), want the eligible account", res.Assignment, res.CredentialKind)
	}
	if r := res.Assignment.Reason; !strings.Contains(r, "is unavailable") || !strings.Contains(r, "has no Pyre login on this node") {
		t.Fatalf("reason %q", r)
	}
	sel, err := e.svc.Select(ctx, contracts.AccountSelectParams{Provider: contracts.ProviderOpenAI, Policy: nonStrict("acct_synth-elsewhere")})
	if err != nil || sel.AccountID != "acct_synth-code" || sel.Outcome != "" {
		t.Fatalf("dry run %+v %v", sel, err)
	}

	_, err = e.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessCodex, Policy: explicit("acct_synth-elsewhere"), RunID: "run_2"})
	mustCode(t, err, contracts.ErrUnavailable)
	if _, err := e.svc.assignmentForRun(ctx, e.svc.db, "run_2"); err == nil {
		t.Fatal("a strict pin with no login here got an assignment")
	}
	sel, err = e.svc.Select(ctx, contracts.AccountSelectParams{Provider: contracts.ProviderOpenAI, Policy: explicit("acct_synth-elsewhere")})
	if err != nil || sel.AccountID != "" || sel.Outcome != contracts.FallbackIntervention || !strings.Contains(sel.Reason, "no own codex login") {
		t.Fatalf("strict dry run %+v %v", sel, err)
	}
}

// The same for a node whose own login is another identity: a non-strict
// pin falls back; cross-provider stays an error.
func TestNonStrictPinOtherIdentityFallsBack(t *testing.T) {
	ctx := context.Background()
	e := ownEnv(t)
	e.addAccount("acct_synth-code", contracts.HarnessCodex, "code")
	e.observe("acct_synth-code", contracts.ProviderOpenAI, 10, time.Minute)
	res, err := e.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessCodex, Policy: nonStrict("acct_synth-other"), RunID: "run_1", SessionID: "ses_1"})
	if err != nil || res.Assignment == nil || res.Assignment.AccountID != "acct_synth-code" {
		t.Fatalf("bound %+v %v", res.Assignment, err)
	}
	_, err = e.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessCodex, Policy: nonStrict("acct_synth-anth"), RunID: "run_2"})
	mustCode(t, err, contracts.ErrInvalid)
}
