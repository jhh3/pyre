package accounts

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

func latest(t *testing.T, e *testEnv, id string) contracts.AccountObservation {
	t.Helper()
	o, err := getObservation(context.Background(), e.svc.db, id)
	if err != nil || o == nil {
		t.Fatalf("no observation for %s (%v)", id, err)
	}
	return *o
}

// The fake provider's alarm is evidence only for requests addressed to it:
// a stray request from another test process that reconnects to a reused
// loopback port (the cause of a TestUsageProbeNeverRefreshes "alarms 1"
// flake under go test -p 4) is a stray, while a non-synthetic credential on
// the fake's own endpoint still alarms.
func TestFakeProviderSeparatesStraysFromAlarms(t *testing.T) {
	f := newFakeProvider(t)
	get := func(url, bearer string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		req.Header.Set("Authorization", "Bearer "+bearer)
		resp, err := f.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	get(f.srv.URL+"/v1/nodes/link", "synthetic-other-process-node-token")
	get(f.srv.URL+"/api/oauth/usage", "synthetic-other-process-token")
	if alarms, _, strays := f.counters(); alarms != 0 || strays != 2 {
		t.Fatalf("strays must not alarm: alarms %d strays %d", alarms, strays)
	}
	get(f.base+"/api/oauth/usage", "not-a-pyre-synthetic-token")
	if alarms, _, _ := f.counters(); alarms != 1 {
		t.Fatalf("non-synthetic bearer on the fake's endpoint: alarms %d, want 1", alarms)
	}
}

// A13: the usage probe reads the profile's token and NEVER refreshes; an
// expired token yields unknown and no request at all.
func TestUsageProbeNeverRefreshes(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	pa := e.addAccount("acct_synth-a", contracts.HarnessClaude, "A")
	e.addAccount("acct_synth-b", contracts.HarnessClaude, "B")
	px := e.addAccount("acct_synth-x", contracts.HarnessCodex, "X")
	loginClaude(t, pa.Dir, "A", true) // expired access token
	loginCodex(t, px.Dir, "X", true)  // expired codex access token
	e.fake.usage["B"] = `{"five_hour":{"utilization":42,"resets_at":"2026-10-05T03:00:00Z"},"seven_day":{"utilization":7,"resets_at":"2026-10-09T00:00:00Z"}}`

	obs := e.svc.ObserveDue(ctx, "")
	if len(obs) != 3 {
		t.Fatalf("observations %d", len(obs))
	}
	if e.fake.calls("A") != 0 {
		t.Fatal("expired Claude token was sent to the usage endpoint")
	}
	if e.reader.count(px.Dir) != 0 {
		t.Fatal("expired Codex home was probed")
	}
	if alarms, _, _ := e.fake.counters(); e.fake.refreshes() != 0 || alarms != 0 {
		t.Fatalf("refresh calls %d, alarms %d", e.fake.refreshes(), alarms)
	}
	oa := latest(t, e, "acct_synth-a")
	if oa.Eligibility != contracts.EligibleUnknown || !oa.Stale || !strings.Contains(oa.Detail, "never refreshes") {
		t.Fatalf("expired => unknown/stale, got %+v", oa)
	}
	ob := latest(t, e, "acct_synth-b")
	if ob.Eligibility != contracts.EligibleYes || ob.Stale || ob.Confidence != "high" || len(ob.Windows) != 2 ||
		ob.Windows[0].Name != "five_hour" || ob.Windows[0].UsedPercent != 42 || ob.Windows[0].ResetAt == nil {
		t.Fatalf("B observation %+v", ob)
	}
	if _, bad, _ := e.fake.counters(); e.fake.calls("B") != 1 || bad != 0 {
		t.Fatalf("B calls %d bad headers %d", e.fake.calls("B"), bad)
	}
	// 401 on a harness-owned grant: unknown, back off, still no refresh.
	e.fake.status["B"] = 401
	e.clock.Advance(2 * time.Minute)
	e.svc.ObserveDue(ctx, contracts.ProviderAnthropic)
	if ob := latest(t, e, "acct_synth-b"); ob.Eligibility != contracts.EligibleUnknown || e.fake.refreshes() != 0 {
		t.Fatalf("401 => unknown without refresh: %+v", ob)
	}
	g, _ := e.svc.getGrant(ctx, e.svc.db, "acct_synth-b", contracts.HarnessClaude)
	if g.State == contracts.GrantDead {
		t.Fatal("a 401 on a refreshable profile is not a dead login")
	}
	if len(e.rep.of(contracts.ReportAccountObs)) < 3 {
		t.Fatal("observations not reported")
	}
}

func TestCodexUsageViaAdapter(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	px := e.addAccount("acct_synth-x", contracts.HarnessCodex, "X")
	e.reader.resp[px.Dir] = `{"rateLimits":{"limitId":"codex","rateLimitReachedType":null,"primary":{"usedPercent":77,"windowDurationMins":300,"resetsAt":1791190000},"secondary":{"usedPercent":22,"windowDurationMins":10080,"resetsAt":1791270000}}}`
	e.svc.ObserveDue(ctx, contracts.ProviderOpenAI)
	if e.reader.count(px.Dir) != 1 {
		t.Fatal("codex reader not called with the canonical home")
	}
	o := latest(t, e, "acct_synth-x")
	if o.Source != "codex:app-server" || o.Eligibility != contracts.EligibleYes || o.Windows[0].Name != "five_hour" || o.Windows[0].UsedPercent != 77 {
		t.Fatalf("%+v", o)
	}
	g, _ := e.svc.getGrant(ctx, e.svc.db, "acct_synth-x", contracts.HarnessCodex)
	if l, _ := getLease(ctx, e.svc.db, g.GrantID); l != nil {
		t.Fatal("probe leaked its lease")
	}
	// A foreign process holder on the home: the probe skips (one process per home).
	acquireLease(ctx, e.svc.db, g.GrantID, "run:other", e.clock.Now())
	e.clock.Advance(10 * time.Minute)
	e.svc.ObserveDue(ctx, contracts.ProviderOpenAI)
	if e.reader.count(px.Dir) != 1 {
		t.Fatal("probe ran a second process on a leased home")
	}
}

func TestUsagePollingBoundedAndRetryAfter(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addAccount("acct_synth-a", contracts.HarnessClaude, "A")
	e.svc.ObserveDue(ctx, "")
	e.svc.ObserveDue(ctx, "")
	e.svc.ObserveDue(ctx, "")
	if c := e.fake.calls("A"); c != 1 {
		t.Fatalf("polled %d times within one interval", c)
	}
	e.clock.Advance(e.cfg.PollInterval + e.cfg.PollInterval/5 + time.Second)
	e.svc.ObserveDue(ctx, "")
	if c := e.fake.calls("A"); c != 2 {
		t.Fatalf("calls %d after interval", c)
	}
	// 429 with Retry-After: an hour of silence, last good sample kept.
	e.fake.status["A"] = 429
	e.fake.retryAfter = "3600"
	e.clock.Advance(2 * time.Minute)
	e.svc.ObserveDue(ctx, "")
	if c := e.fake.calls("A"); c != 3 {
		t.Fatalf("calls %d", c)
	}
	ps, _ := getPoll(ctx, e.svc.db, "acct_synth-a")
	if ps.NextAt.Sub(e.clock.Now()) < time.Hour || ps.Failures != 1 {
		t.Fatalf("Retry-After not honored: next in %s, failures %d", ps.NextAt.Sub(e.clock.Now()), ps.Failures)
	}
	for i := 0; i < 5; i++ {
		e.clock.Advance(10 * time.Minute)
		e.svc.ObserveDue(ctx, "")
	}
	if c := e.fake.calls("A"); c != 3 {
		t.Fatalf("polled during Retry-After window: %d", c)
	}
	// Minimum interval outside test mode is 5 minutes.
	cfg := e.cfg
	cfg.TestMode = false
	cfg.PollInterval = time.Second
	s2, err := New(ctx, cfg)
	if err != nil || s2.cfg.PollInterval != 5*time.Minute {
		t.Fatalf("poll interval floor: %v %v", s2.cfg.PollInterval, err)
	}
}

func TestSetupTokenRejectionIsDead(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := contracts.Account{AccountID: "acct_synth-t", Provider: contracts.ProviderAnthropic, Kind: "subscription", ProviderRef: claudeUUID("T")}
	info, err := e.svc.CreateProfile(ctx, a, contracts.HarnessClaude)
	if err != nil {
		t.Fatal(err)
	}
	// Model a token-fd grant (owner-provisioned setup token in the profile).
	g, _ := e.svc.getGrant(ctx, e.svc.db, a.AccountID, contracts.HarnessClaude)
	e.svc.db.Exec(`UPDATE accounts_grant SET kind=?, state='valid' WHERE grant_id=?`, string(contracts.CredClaudeTokenFD), g.GrantID)
	os.WriteFile(filepath.Join(info.Dir, "oauth-token"), []byte(synthAccess("T")), 0o600)
	e.fake.status["T"] = 401
	e.svc.ObserveDue(ctx, "")
	g, _ = e.svc.getGrant(ctx, e.svc.db, a.AccountID, contracts.HarnessClaude)
	if g.State != contracts.GrantDead {
		t.Fatalf("setup-token 401 => dead, got %s", g.State)
	}
	e.clock.Advance(24 * time.Hour)
	e.svc.ObserveDue(ctx, "")
	if c := e.fake.calls("T"); c != 1 {
		t.Fatalf("dead token retried: %d calls", c)
	}
}

// writeSecurityShim writes a fake macOS `security` for keychain tests. It
// serves synthetic items from kc/<service>, logs only the verb and service,
// and exits 51 when kc/locked exists.
func writeSecurityShim(t *testing.T, dir string) (shim, kc, logf string) {
	t.Helper()
	kc = filepath.Join(dir, "kc")
	os.MkdirAll(kc, 0o700)
	logf = filepath.Join(dir, "security.log")
	shim = filepath.Join(dir, "security")
	script := fmt.Sprintf(`#!/bin/sh
svc=""; prev=""
for a in "$@"; do [ "$prev" = "-s" ] && svc="$a"; prev="$a"; done
echo "$1 $svc" >> '%s'
[ -f '%s/locked' ] && exit 51
if [ -f '%s/'"$svc" ]; then cat '%s/'"$svc"; exit 0; fi
echo "security: The specified item could not be found in the keychain." >&2
exit 44
`, logf, kc, kc, kc)
	if err := os.WriteFile(shim, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return shim, kc, logf
}

func TestKeychainProfileStore(t *testing.T) {
	var shim, kc, logf string
	e := newEnv(t, func(c *Config) {
		shim, kc, logf = writeSecurityShim(t, t.TempDir())
		c.GOOS = "darwin"
		c.SecurityPath = shim
	})
	ctx := context.Background()
	a := contracts.Account{AccountID: "acct_synth-k", Provider: contracts.ProviderAnthropic, Kind: "subscription", ProviderRef: claudeUUID("K")}
	info, err := e.svc.CreateProfile(ctx, a, contracts.HarnessClaude)
	if err != nil {
		t.Fatal(err)
	}
	// Login landed in the profile's keychain item (no plaintext file).
	item := map[string]any{"claudeAiOauth": map[string]any{"accessToken": synthAccess("K"), "refreshToken": synthRefresh("K"),
		"expiresAt": time.Now().Add(time.Hour).UnixMilli()}}
	b, _ := json.Marshal(item)
	os.WriteFile(filepath.Join(kc, info.KeychainService), b, 0o600)
	setClaudeOAuthAccount(t, info.Dir, claudeUUID("K"), "K")
	if _, err := e.svc.VerifyGrant(ctx, a.AccountID, contracts.HarnessClaude); err != nil {
		t.Fatal(err)
	}
	e.svc.ObserveDue(ctx, "")
	if e.fake.calls("K") != 1 {
		t.Fatal("keychain token not used for the probe")
	}
	lb, _ := os.ReadFile(logf)
	if !strings.Contains(string(lb), "find-generic-password "+info.KeychainService) {
		t.Fatalf("shim log %q; want lookup of %s", lb, info.KeychainService)
	}
	if strings.Contains(string(lb), "add-generic-password") || strings.Contains(string(lb), "delete") {
		t.Fatal("Pyre must never write or delete keychain items")
	}
	// A locked keychain is actionable "unknown", never a dead login, and
	// unlocking does not bump the generation (live runs stay valid).
	g0, _ := e.svc.getGrant(ctx, e.svc.db, a.AccountID, contracts.HarnessClaude)
	os.WriteFile(filepath.Join(kc, "locked"), nil, 0o600)
	if _, err := e.svc.VerifyGrant(ctx, a.AccountID, contracts.HarnessClaude); err == nil || !strings.Contains(err.Error(), "unlock") {
		t.Fatalf("locked keychain: %v", err)
	}
	g, _ := e.svc.getGrant(ctx, e.svc.db, a.AccountID, contracts.HarnessClaude)
	if g.State == contracts.GrantDead {
		t.Fatal("locked keychain treated as dead login")
	}
	// A blanked keychain item (Claude logged the profile out) is dead.
	os.Remove(filepath.Join(kc, "locked"))
	if g1, err := e.svc.VerifyGrant(ctx, a.AccountID, contracts.HarnessClaude); err != nil || g1.Generation != g0.Generation {
		t.Fatalf("unlock changed generation %d -> %d (%v)", g0.Generation, g1.Generation, err)
	}
	os.WriteFile(filepath.Join(kc, info.KeychainService), []byte(`{}`), 0o600)
	e.svc.VerifyGrant(ctx, a.AccountID, contracts.HarnessClaude)
	g, _ = e.svc.getGrant(ctx, e.svc.db, a.AccountID, contracts.HarnessClaude)
	if g.State != contracts.GrantDead {
		t.Fatalf("blanked keychain item => dead, got %s", g.State)
	}
}

func TestParseUsage(t *testing.T) {
	ws, err := ParseClaudeUsage([]byte(`{"five_hour":{"utilization":3,"resets_at":"2026-10-04T20:10:00.123+00:00"},
		"seven_day":{"utilization":42,"resets_at":"2026-10-06T02:00:00Z"},"seven_day_opus":{"utilization":71,"resets_at":null},
		"extra_usage":{"is_enabled":true,"used_credits":340142}}`))
	if err != nil || len(ws) != 3 || ws[0].Name != "five_hour" || ws[1].Name != "seven_day" || ws[2].Name != "seven_day_opus" || ws[0].ResetAt == nil || ws[2].ResetAt != nil {
		t.Fatalf("%+v %v", ws, err)
	}
	ws, _ = ParseClaudeUsage([]byte(`{"limits":[{"kind":"session","percent":3,"resets_at":"2026-10-04T20:10:00Z"},
		{"kind":"weekly_all","percent":42},{"kind":"weekly_scoped","percent":71,"scope":{"model":{"display_name":"Fable"}}}]}`))
	if len(ws) != 3 || ws[0].Name != "five_hour" || ws[1].Name != "seven_day" || ws[2].Name != "seven_day_fable" || ws[2].UsedPercent != 71 {
		t.Fatalf("limits form %+v", ws)
	}
	rl, err := ParseCodexRateLimits([]byte(`{"rateLimitsByLimitId":{"codex":{"primary":{"usedPercent":16,"windowDurationMins":10080,"resetsAt":1791270000}},
		"codex_spark":{"limitName":"GPT-5.3-Codex-Spark","rateLimitReachedType":"rate_limit_reached","primary":{"usedPercent":100,"windowDurationMins":10080,"resetsAt":1791370000}}}}`))
	cw := rl.Windows
	if err != nil || !rl.Reached || len(cw) != 2 || cw[0].Name != "seven_day" || cw[1].Name != "seven_day_gpt_5_3_codex_spark" || cw[1].ResetAt.Unix() != 1791370000 ||
		rl.Reason != "codex_spark:rate_limit_reached" || rl.ResetAt.Unix() != 1791370000 {
		t.Fatalf("%+v %v", rl, err)
	}
}

// TestParseCodexRateLimitReached: Codex 0.160.0 reports a blocked bucket with
// rateLimitReachedType (there is no "limitReached" field). Synthetic payloads
// shaped like the generated RateLimitSnapshot schema.
func TestParseCodexRateLimitReached(t *testing.T) {
	cases := []struct {
		name, body string
		reached    bool
		reason     string
	}{
		{"not reached (null)", `{"ordinaryUsageAllowed":null,"rateLimits":{"limitId":"codex","primary":{"usedPercent":40,"windowDurationMins":300,"resetsAt":1791190000},"rateLimitReachedType":null,"spendControlReached":null}}`, false, ""},
		{"reached below 100%", `{"rateLimits":{"limitId":"codex","primary":{"usedPercent":63,"windowDurationMins":300,"resetsAt":1791190000},"secondary":{"usedPercent":22,"windowDurationMins":10080,"resetsAt":1791270000},"rateLimitReachedType":"rate_limit_reached"}}`, true, "codex:rate_limit_reached"},
		{"workspace credits", `{"rateLimitsByLimitId":{"codex":{"limitId":"codex","primary":{"usedPercent":5,"windowDurationMins":300},"rateLimitReachedType":"workspace_member_credits_depleted"}}}`, true, "codex:workspace_member_credits_depleted"},
		{"spend control", `{"rateLimits":{"limitId":"codex","primary":{"usedPercent":5,"windowDurationMins":300},"spendControlReached":true}}`, true, "codex:spend_control_reached"},
		{"ordinary usage denied", `{"ordinaryUsageAllowed":false,"rateLimits":{"limitId":"codex","primary":{"usedPercent":5,"windowDurationMins":300}}}`, true, "ordinaryUsageAllowed=false"},
		{"legacy limitReached ignored", `{"rateLimits":{"limitId":"codex","limitReached":true,"primary":{"usedPercent":5,"windowDurationMins":300}}}`, false, ""},
	}
	for _, c := range cases {
		rl, err := ParseCodexRateLimits([]byte(c.body))
		if err != nil || rl.Reached != c.reached || rl.Reason != c.reason {
			t.Errorf("%s: got %+v err=%v; want reached=%v reason=%q", c.name, rl, err, c.reached, c.reason)
		}
	}
}

// TestCodexProbeLimitReached: a reached bucket below 100% still makes the
// observation exhausted with the bucket's reset time.
func TestCodexProbeLimitReached(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	px := e.addAccount("acct_synth-x", contracts.HarnessCodex, "X")
	e.reader.resp[px.Dir] = `{"rateLimits":{"limitId":"codex","rateLimitReachedType":"rate_limit_reached","primary":{"usedPercent":63,"windowDurationMins":300,"resetsAt":1791190000},"secondary":{"usedPercent":22,"windowDurationMins":10080,"resetsAt":1791270000}}}`
	e.svc.ObserveDue(ctx, contracts.ProviderOpenAI)
	o := latest(t, e, "acct_synth-x")
	if o.Eligibility != contracts.EligibleExhausted || o.ResetAt == nil || o.ResetAt.Unix() != 1791270000 || !strings.Contains(o.Detail, "codex:rate_limit_reached") {
		t.Fatalf("%+v", o)
	}
}

// TestCodexSchemaBindings: the decode structs name exactly the fields the
// drift guard checks (and never the non-existent "limitReached").
func TestCodexSchemaBindings(t *testing.T) {
	b := CodexSchemaBindings()
	snap := strings.Join(b["RateLimitSnapshot"], ",")
	if !strings.Contains(snap, "rateLimitReachedType") || strings.Contains(snap, "limitReached,") || strings.HasSuffix(snap, "limitReached") {
		t.Fatalf("RateLimitSnapshot bindings: %s", snap)
	}
	if len(b["RateLimitWindow"]) != 3 || len(b["GetAccountRateLimitsResponse"]) != 3 {
		t.Fatalf("%v", b)
	}
}
