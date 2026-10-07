package accounts

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

type ownFleet struct{ views []contracts.AccountView }

func (f ownFleet) ListAccounts(context.Context, contracts.ListAccountsRequest) (contracts.Page[contracts.AccountView], error) {
	return contracts.Page[contracts.AccountView]{Items: f.views}, nil
}

// The machine's own Codex login is identified read-only (email and account
// ref from the id_token, never a token), and the rate limits a Pyre run on
// it receives become an observation of the matching hub account.
func TestOwnLoginIdentityAndRateLimits(t *testing.T) {
	email := "synth-owner@example.invalid"
	e := newEnv(t, func(c *Config) {
		c.Fleet = ownFleet{views: []contracts.AccountView{
			{Account: contracts.Account{AccountID: "acct_synth-other", Provider: contracts.ProviderOpenAI, RotaRef: "other@example.invalid"}},
			{Account: contracts.Account{AccountID: "acct_synth-own", Provider: contracts.ProviderOpenAI, RotaRef: email}},
		}}
	})
	home := e.svc.cfg.UserCodexDir
	_ = os.MkdirAll(home, 0o700)
	writeJSON(t, filepath.Join(home, "auth.json"), map[string]any{"auth_mode": "chatgpt", "tokens": map[string]any{
		"id_token":     synthJWT(map[string]any{"email": email, "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "synth-acct-own", "chatgpt_plan_type": "pro"}}),
		"access_token": synthJWT(map[string]any{"kind": "access"}), "refresh_token": synthRefresh("own")}}, 0o600)
	before, _ := os.ReadFile(filepath.Join(home, "auth.json"))

	l := e.svc.OwnLogin(contracts.HarnessCodex)
	if l == nil || l.Email != email || l.ProviderRef != "synth-acct-own" || l.Plan != "pro" {
		t.Fatalf("own login %+v", l)
	}
	b, _ := json.Marshal(l)
	if strings.Contains(string(b), "pyre-synthetic") || strings.Contains(string(b), "refresh") {
		t.Fatalf("identity carries token material: %s", b)
	}

	raw := json.RawMessage(`{"rateLimits":{"limitId":"codex","primary":{"usedPercent":12,"windowDurationMins":300,"resetsAt":1791190000},"secondary":{"usedPercent":34,"windowDurationMins":10080,"resetsAt":1791270000}}}`)
	ctx := context.Background()
	e.svc.ObserveOwnRateLimits(ctx, contracts.HarnessCodex, raw)
	e.svc.ObserveOwnRateLimits(ctx, contracts.HarnessCodex, raw) // throttled
	reps := e.rep.of(contracts.ReportAccountObs)
	if len(reps) != 1 {
		t.Fatalf("observations reported: %d", len(reps))
	}
	var o contracts.AccountObservation
	_ = json.Unmarshal(reps[0], &o)
	if o.AccountID != "acct_synth-own" || o.ReportingNode != e.svc.cfg.NodeID || len(o.Windows) != 2 || o.Eligibility != contracts.EligibleYes {
		t.Fatalf("observation %+v", o)
	}
	e.clock.Advance(2 * time.Minute)
	e.svc.ObserveOwnRateLimits(ctx, contracts.HarnessCodex, raw)
	if len(e.rep.of(contracts.ReportAccountObs)) != 2 {
		t.Fatal("not observed again after the interval")
	}
	if after, _ := os.ReadFile(filepath.Join(home, "auth.json")); string(after) != string(before) {
		t.Fatal("own auth.json changed")
	}
}

// TestRealOwnLogin reads this machine's own Codex login identity
// (read-only) and compares it with PYRE_REAL_OWN_EMAIL; it prints no
// identity. Skipped unless PYRE_REAL_HARNESS=1 and PYRE_REAL_CODEX_HOME
// (TestMain points HOME at a temp dir).
func TestRealOwnLogin(t *testing.T) {
	dir := os.Getenv("PYRE_REAL_CODEX_HOME")
	if os.Getenv("PYRE_REAL_HARNESS") != "1" || dir == "" {
		t.Skip("set PYRE_REAL_HARNESS=1 and PYRE_REAL_CODEX_HOME")
	}
	before, _ := os.Stat(filepath.Join(dir, "auth.json"))
	l := codexOwnLogin(dir)
	after, _ := os.Stat(filepath.Join(dir, "auth.json"))
	if l == nil {
		t.Fatal("no own Codex login identity")
	}
	t.Logf("identity found: email=%v provider_ref=%v plan=%q matches PYRE_REAL_OWN_EMAIL=%v; auth.json mtime unchanged=%v",
		l.Email != "", l.ProviderRef != "", l.Plan, strings.EqualFold(l.Email, os.Getenv("PYRE_REAL_OWN_EMAIL")),
		before != nil && after != nil && before.ModTime().Equal(after.ModTime()))
}
