package accounts

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Expired at rest: an idle rota account whose snapshot access token expired
// is "eligible, unverified". It is checked out, the harness refreshes its own
// login in the profile (fake refresher here), the usage probe decides, and
// the run exists only after a fresh eligible result. Rota's snapshot is never
// refreshed by Pyre.

// fakeRefresher simulates the harness refreshing its login in a profile.
type fakeRefresher struct {
	mu      sync.Mutex
	r       *rotaEnv
	calls   []string // profile dirs
	fail    bool     // the harness does not refresh
	locked  []bool   // the rota lock existed (single owner) at each call
	label   map[string]string
	lockFor map[string]string
}

func (f *fakeRefresher) RefreshLogin(_ context.Context, h contracts.Harness, dir string, fresh func() bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, dir)
	f.locked = append(f.locked, exists(f.lockFor[dir]))
	if f.fail {
		return fmt.Errorf("synthetic: refresh endpoint unreachable")
	}
	lab := f.label[dir]
	switch h {
	case contracts.HarnessClaude:
		f.r.simulateClaudeRefresh(dir, "", lab, "g2", f.r.clock.Now().Add(8*time.Hour))
	case contracts.HarnessCodex:
		var a map[string]any
		json.Unmarshal(mustRead(f.r.t, filepath.Join(dir, "auth.json")), &a)
		claims := map[string]any{"pyre_synthetic": true, "pyre_label": lab, "exp": f.r.clock.Now().Add(10 * 24 * time.Hour).Unix(), "kind": "access",
			"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": codexAcct(lab)}}
		t := a["tokens"].(map[string]any)
		t["access_token"], t["refresh_token"] = synthJWT(claims), "pyre-synthetic-"+lab+"-refresh-g2"
		a["last_refresh"] = f.r.clock.Now().Add(time.Minute).Format(time.RFC3339)
		writeJSON(f.r.t, filepath.Join(dir, "auth.json"), a, 0o600)
	}
	if !fresh() {
		return fmt.Errorf("synthetic refresh did not take")
	}
	return nil
}

func (f *fakeRefresher) n() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calls) }

func newRefreshEnv(t *testing.T) (*rotaEnv, *fakeRefresher) {
	fr := &fakeRefresher{label: map[string]string{}, lockFor: map[string]string{}}
	r := newRotaEnv(t, func(c *Config) { c.LoginRefresher = fr })
	fr.r = r
	return r, fr
}

func (r *rotaEnv) addExpiredClaude(fr *fakeRefresher, id, label string) (ProfileInfo, string) {
	snap := r.writeRotaClaude(label, "g1", time.Now().Add(-49*time.Hour))
	info := r.addRotaAccount(id, contracts.HarnessClaude, label, false)
	r.svc.cfg.Rota.Independent[id] = true
	fr.label[info.Dir] = label
	fr.lockFor[info.Dir] = filepath.Join(r.claudeAcc, rotaName(label)+".pyre-lock")
	return info, snap
}

func TestExpiredAtRestClaudeAutoSelected(t *testing.T) {
	r, fr := newRefreshEnv(t)
	r.svc.cfg.Rota.Independent = map[string]bool{}
	ctx := context.Background()
	info, snap := r.addExpiredClaude(fr, "acct_synth-e", "E")

	// Observation and dry-run selection never refresh a rota-held login.
	r.svc.ObserveDue(ctx, "")
	if o := latest(t, r.testEnv, "acct_synth-e"); o.Eligibility != contracts.EligibleUnknown || r.fake.calls("E") != 0 {
		t.Fatalf("expired snapshot must not be sent: %+v", o)
	}
	sel, err := r.svc.Select(ctx, contracts.AccountSelectParams{Provider: contracts.ProviderAnthropic})
	if err != nil || sel.AccountID != "acct_synth-e" || !strings.Contains(sel.Reason, "unverified") || fr.n() != 0 {
		t.Fatalf("dry run %+v %v (refresh calls %d)", sel, err, fr.n())
	}

	// Automatic selection picks it: checkout, harness refresh in the profile, probe, admit.
	res, err := r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: eligible, RunID: "run_1", SessionID: "ses_1"})
	if err != nil || res.ProfileDir != info.Dir {
		t.Fatalf("auto-selection of an expired-at-rest account: %+v %v", res.Assignment, err)
	}
	if fr.n() != 1 || !fr.locked[0] {
		t.Fatalf("refresh calls %d, under the rota lock %v", fr.n(), fr.locked)
	}
	if !strings.Contains(res.Assignment.Reason, "refreshed by the harness") || !strings.Contains(res.Assignment.Reason, "verified at checkout") {
		t.Fatalf("reason %q", res.Assignment.Reason)
	}
	if r.fake.calls("E") != 1 || r.fake.refreshes() != 0 {
		t.Fatalf("probe calls %d (want 1 with the refreshed token), Pyre refresh calls %d", r.fake.calls("E"), r.fake.refreshes())
	}
	// Write-back keeps rota healthy with the harness's rotated login.
	r.svc.SyncCheckouts(ctx)
	if s := readSnap(t, snap); s.ClaudeAiOauth["refreshToken"] != "pyre-synthetic-E-refresh-g2" || s.ClaudeAiOauth["refreshTokenExpiresAt"] == nil {
		t.Fatalf("write-back after the harness refresh: %v", s.ClaudeAiOauth["refreshToken"])
	}
	r.svc.Release("run_1")
	if exists(fr.lockFor[info.Dir]) {
		t.Fatal("not checked in")
	}
	// Verified-fresh accounts outrank unverified ones.
	r.writeRotaClaude("F", "g1", time.Now().Add(4*time.Hour))
	r.addRotaAccount("acct_synth-f", contracts.HarnessClaude, "F", false)
	r.svc.cfg.Rota.Independent["acct_synth-f"] = true
	r.addExpiredClaude(fr, "acct_synth-u", "U")
	r.fake.usage["F"] = `{"five_hour":{"utilization":95,"resets_at":"2026-10-05T03:00:00Z"}}`
	r.clock.Advance(2 * time.Minute)
	r.svc.ObserveDue(ctx, "")
	res, err = r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: eligible, RunID: "run_2"})
	if err != nil || res.Assignment.AccountID == "acct_synth-u" || fr.n() != 1 {
		t.Fatalf("fresh account must win over an unverified one: %+v %v (refresh calls %d)", res.Assignment, err, fr.n())
	}
}

// An expired-at-rest account that turns out to be at its limit: no run is
// started on it (its first prompt cannot be lost or replayed), the refreshed
// login goes back to rota, selection moves on or waits with the reset, and
// it is not refreshed again.
func TestExpiredAtRestExhaustedAfterRefresh(t *testing.T) {
	r, fr := newRefreshEnv(t)
	r.svc.cfg.Rota.Independent = map[string]bool{}
	ctx := context.Background()
	info, snap := r.addExpiredClaude(fr, "acct_synth-e", "E")
	r.fake.usage["E"] = `{"five_hour":{"utilization":100,"resets_at":"` + r.clock.Now().Add(2*time.Hour).Format(time.RFC3339) + `"}}`
	_, err := r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: eligible, RunID: "run_1"})
	e := mustCode(t, err, contracts.ErrCapacityExhausted)
	if e.Details["outcome"] != string(contracts.FallbackWaiting) || e.Details["earliest_reset"] == nil || !strings.Contains(e.Message, "verified at checkout") {
		t.Fatalf("exhausted after refresh: %+v", e)
	}
	if _, err := r.svc.assignmentForRun(ctx, r.svc.db, "run_1"); err == nil {
		t.Fatal("assignment kept for a run that must not start")
	}
	if exists(fr.lockFor[info.Dir]) || r.profileCred(info.Dir, "") != nil {
		t.Fatal("not checked back in")
	}
	if s := readSnap(t, snap); s.ClaudeAiOauth["refreshToken"] != "pyre-synthetic-E-refresh-g2" {
		t.Fatal("refreshed login not written back to rota at check-in")
	}
	// With a second (unverified, healthy) account, the same request moves on.
	pb, _ := r.addExpiredClaude(fr, "acct_synth-b", "B")
	res, err := r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: eligible, RunID: "run_2"})
	if err != nil || res.ProfileDir != pb.Dir {
		t.Fatalf("next account: %+v %v", res.Assignment, err)
	}
	if fr.n() != 2 {
		t.Fatalf("refresh calls %d (E once, B once)", fr.n())
	}
}

// A harness that cannot refresh: refused for automatic selection, backed off
// (no refresh on every selection), explicit still launches.
func TestExpiredAtRestRefreshFails(t *testing.T) {
	r, fr := newRefreshEnv(t)
	r.svc.cfg.Rota.Independent = map[string]bool{}
	ctx := context.Background()
	info, _ := r.addExpiredClaude(fr, "acct_synth-e", "E")
	fr.fail = true
	_, err := r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: eligible, RunID: "run_1"})
	if e := mustCode(t, err, contracts.ErrCapacityExhausted); !strings.Contains(e.Message, "did not refresh") {
		t.Fatalf("refresh failure: %v", err)
	}
	if exists(fr.lockFor[info.Dir]) {
		t.Fatal("left checked out")
	}
	if _, err := r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: eligible, RunID: "run_2"}); err == nil || fr.n() != 1 {
		t.Fatalf("retried the refresh at once: %v calls %d", err, fr.n())
	}
	if _, err := r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: explicit("acct_synth-e"), RunID: "run_3"}); err != nil || fr.n() != 1 {
		t.Fatalf("explicit --account must still launch during the backoff (the harness refreshes on use): %v (refresh calls %d)", err, fr.n())
	}
	r.svc.Release("run_3")
	r.clock.Advance(refreshFailBackoff + time.Minute)
	fr.fail = false
	if _, err := r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: eligible, RunID: "run_4"}); err != nil || fr.n() != 2 {
		t.Fatalf("after the backoff: %v (refresh calls %d)", err, fr.n())
	}
}

// Codex: an expired snapshot is refreshed by the app-server in the checked-out
// home (account/read refreshToken), then rate limits are read there.
func TestExpiredAtRestCodex(t *testing.T) {
	r, fr := newRefreshEnv(t)
	ctx := context.Background()
	snap := r.writeRotaCodex("X")
	loginCodex(t, filepath.Dir(snap), "X", true)
	info := r.addRotaAccount("acct_synth-x", contracts.HarnessCodex, "X", true)
	fr.label[info.Dir] = "X"
	fr.lockFor[info.Dir] = codexLock(snap)
	r.svc.ObserveDue(ctx, "")
	if r.reader.count(info.Dir) != 0 || fr.n() != 0 {
		t.Fatal("rota-held codex login probed or refreshed")
	}
	res, err := r.svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessCodex, Policy: eligible, RunID: "run_1"})
	if err != nil || res.Env["CODEX_HOME"] != info.Dir || !strings.Contains(res.Assignment.Reason, "refreshed by the harness") {
		t.Fatalf("%+v %v", res.Assignment, err)
	}
	if fr.n() != 1 || !fr.locked[0] || r.reader.count(info.Dir) != 1 {
		t.Fatalf("refresh %d (locked %v), probe %d", fr.n(), fr.locked, r.reader.count(info.Dir))
	}
	r.svc.Release("run_1")
	if !strings.Contains(string(mustRead(t, snap)), "pyre-synthetic-X-refresh-g2") {
		t.Fatal("codex refresh not written back")
	}
}
