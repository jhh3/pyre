package accounts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// TestRealRotaCheckoutMac is the owner-directed real verification (WP13
// rota checkout, step 8). It runs ONLY with PYRE_REAL_ROTA_CHECKOUT=1 and
// PYRE_REAL_HOME=<home> on macOS. It picks ONE non-active, healthy rota
// Claude account whose access token is valid for >30 min (so no refresh is
// needed), checks it out into a scratch profile, runs one tiny `claude -p`,
// verifies the local identity claim, checks it back in with write-back,
// probes usage read-only with the snapshot token, and runs `rota list`
// (verified local-only in rota's source). Output is redacted: the account is
// "acct-A"; no tokens, names or emails are printed.
func TestRealRotaCheckoutMac(t *testing.T) {
	if os.Getenv("PYRE_REAL_ROTA_CHECKOUT") != "1" {
		t.Skip("owner-directed real check; set PYRE_REAL_ROTA_CHECKOUT=1 and PYRE_REAL_HOME")
	}
	home := os.Getenv("PYRE_REAL_HOME")
	if home == "" || !filepath.IsAbs(home) {
		t.Fatal("PYRE_REAL_HOME must be the real home directory")
	}
	t.Setenv("HOME", home) // TestMain isolates HOME; the login keychain needs the real one
	ctx := context.Background()
	scratch := t.TempDir()
	accDir := filepath.Join(home, ".claude", "accounts")
	liveJSON := filepath.Join(home, ".claude.json")
	logf := func(f string, a ...any) { t.Logf(f, a...) }

	// security wrapper: logs verb + service only; payloads pass via stdin.
	secLog := filepath.Join(scratch, "security-calls.log")
	wrapper := filepath.Join(scratch, "security")
	os.WriteFile(wrapper, []byte(fmt.Sprintf(`#!/bin/sh
LOG='%s'
if [ "$1" = "-i" ]; then
  line=$(cat)
  printf '%%s\n' "-i $(printf '%%s' "$line" | awk '{print $1}') $(printf '%%s' "$line" | sed -n 's/.*-s "\([^"]*\)".*/\1/p')" >> "$LOG"
  printf '%%s\n' "$line" | /usr/bin/security -i
  exit $?
fi
svc=""; prev=""
for a in "$@"; do [ "$prev" = "-s" ] && svc="$a"; prev="$a"; done
echo "$1 $svc" >> "$LOG"
exec /usr/bin/security "$@"
`, secLog)), 0o700)

	// Live default identity (non-secret).
	liveBefore, err := readLiveUUID(liveJSON)
	if err != nil {
		t.Fatal(err)
	}

	// Pick ONE eligible account without printing names.
	names, _ := filepath.Glob(filepath.Join(accDir, "*.json"))
	sort.Strings(names)
	var chosen, uuid string
	var chosenSnap *claudeSnapshot
	for _, p := range names {
		base := filepath.Base(p)
		if strings.HasPrefix(base, ".") {
			continue
		}
		sn, err := readClaudeSnapshot(p)
		if err != nil || sn.dead || sn.refreshToken.empty() || sn.accountUUID == "" || sn.accountUUID == liveBefore {
			continue
		}
		if _, err := os.Stat(strings.TrimSuffix(p, ".json") + ".pyre-lock"); err == nil {
			continue
		}
		if time.UnixMilli(sn.expiresAt).Before(time.Now().Add(30 * time.Minute)) {
			continue // would need a refresh; not for a minimal check
		}
		if rt, ok := sn.oauth["refreshTokenExpiresAt"]; ok {
			var ms int64
			json.Unmarshal(rt, &ms)
			if ms > 0 && time.UnixMilli(ms).Before(time.Now().Add(7*24*time.Hour)) {
				continue
			}
		}
		chosen, uuid, chosenSnap = strings.TrimSuffix(base, ".json"), sn.accountUUID, sn
		break
	}
	if chosen == "" {
		t.Skip("no non-active rota Claude account with a valid access token; nothing done")
	}
	snapPath := filepath.Join(accDir, chosen+".json")
	snapBefore := mustRead(t, snapPath)
	snapStat, _ := os.Stat(snapPath)
	logf("selected acct-A (non-active, not dead, access token valid >30m); live default identity differs: %v", uuid != liveBefore)

	db, err := sqlitex.Open(filepath.Join(scratch, "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	logs := &lockedBuf{}
	svc, err := New(ctx, Config{
		NodeID: "node_verify_mac", DB: db, ProfileRoot: filepath.Join(scratch, "profiles"), SharedRoot: filepath.Join(scratch, "shared"),
		UserClaudeDir: filepath.Join(home, ".claude"), UserClaudeJSON: liveJSON, UserCodexDir: filepath.Join(home, ".codex"),
		SecurityPath: wrapper, Logger: newTextLogger(logs),
		Rota: RotaConfig{Enabled: true, ClaudeAccountsDir: accDir, CodexHome: filepath.Join(home, ".codex"),
			Independent: map[string]bool{"acct_verify-a": true}, User: os.Getenv("USER")},
	})
	if err != nil {
		t.Fatal(err)
	}
	acct := contracts.Account{AccountID: "acct_verify-a", Provider: contracts.ProviderAnthropic, Label: "acct-A", Kind: "subscription",
		RotaRef: chosen, ProviderRef: uuid}
	info, err := svc.CreateRotaProfile(ctx, acct, contracts.HarnessClaude)
	if err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(accDir, chosen+".pyre-lock")
	// The account is selected explicitly (no usage observation needed).
	res, err := svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: explicit("acct_verify-a"), RunID: "run_verify_1", SessionID: "ses_verify"})
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	released := false
	defer func() {
		if !released {
			svc.Release("run_verify_1") // never leave the account checked out
		}
	}()
	_, lerr := os.Stat(lock)
	logf("checkout: profile keychain item written=%v, plaintext file=%v, lock present=%v, env CLAUDE_CONFIG_DIR=<scratch profile>",
		svcHasItem(ctx, svc, info.Dir), fileExists(filepath.Join(info.Dir, ".credentials.json")), lerr == nil)

	// One tiny prompt through the profile with a clean environment.
	env := []string{"HOME=" + home, "USER=" + os.Getenv("USER"), "PATH=" + os.Getenv("PATH"), "TERM=dumb", "LANG=en_US.UTF-8"}
	for k, v := range res.Env {
		env = append(env, k+"="+v)
	}
	claude, err := exec.LookPath("claude")
	if err != nil {
		t.Fatal("claude not on PATH")
	}
	run := func(args ...string) (string, string, error) {
		cctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(cctx, claude, args...)
		cmd.Env, cmd.Dir = env, scratch
		var o, e bytes.Buffer
		cmd.Stdout, cmd.Stderr, cmd.Stdin = &o, &e, nil
		err := cmd.Run()
		return o.String(), e.String(), err
	}
	out, stderr, err := run("-p", "reply with ok")
	logf("claude -p 'reply with ok': exit=%v reply=%q stderr=%q", err, truncate(strings.TrimSpace(out), 80), truncate(redact(stderr), 200))
	if err != nil {
		t.Errorf("claude -p failed")
	}
	st, _, err := run("auth", "status", "--json")
	var status map[string]any
	json.Unmarshal([]byte(st), &status)
	var snapAcct struct {
		Email   string `json:"emailAddress"`
		OrgUUID string `json:"organizationUuid"`
	}
	json.Unmarshal(chosenSnap.raw["oauthAccount"], &snapAcct)
	emailMatch := status["email"] == snapAcct.Email && snapAcct.Email != ""
	orgMatch := snapAcct.OrgUUID == "" || status["orgId"] == snapAcct.OrgUUID
	logf("identity (local claim, claude auth status --json): loggedIn=%v authMethod=%v email matches acct-A snapshot=%v org matches=%v (err=%v)",
		status["loggedIn"], status["authMethod"], emailMatch, orgMatch, err)
	if !emailMatch {
		t.Errorf("profile identity does not match the chosen account")
	}

	// Check in: final write-back, delete the profile credential, unlock.
	if _, err := svc.ReleaseRun(ctx, "run_verify_1"); err != nil {
		t.Fatal(err)
	}
	released = true
	_, lerr = os.Stat(lock)
	snapAfter := mustRead(t, snapPath)
	sn2, err := readClaudeSnapshot(snapPath)
	changed := !bytes.Equal(snapBefore, snapAfter)
	logf("check-in: lock removed=%v, profile keychain item removed=%v, plaintext file present=%v, snapshot changed=%v (refreshed in profile=%v), snapshot parses=%v, mode=%v (was %v)",
		lerr != nil, !svcHasItem(ctx, svc, info.Dir), fileExists(filepath.Join(info.Dir, ".credentials.json")), changed,
		err == nil && sn2.refreshToken.reveal() != chosenSnap.refreshToken.reveal(), err == nil, statMode(snapPath), snapStat.Mode().Perm())
	if lerr == nil || svcHasItem(ctx, svc, info.Dir) {
		t.Errorf("not fully checked in")
	}
	cs, _ := svc.Checkouts(ctx)
	if len(cs) == 1 {
		logf("checkout record: state=%s detail=%q", cs[0].State, cs[0].Detail)
	}

	// rota's copy still works: one read-only usage GET with the snapshot
	// token (never refreshes), then `rota list` (local-only).
	svc.cfg.ClaudeUsageURL = "https://api.anthropic.com/api/oauth/usage"
	svc.ObserveDue(ctx, contracts.ProviderAnthropic)
	if o, _ := getObservation(ctx, db, "acct_verify-a"); o != nil {
		logf("post-check-in usage probe with the rota snapshot token: eligibility=%s stale=%v windows=%d detail=%q", o.Eligibility, o.Stale, len(o.Windows), o.Detail)
	}
	rotaBin, err := exec.LookPath("rota")
	if err == nil {
		cmd := exec.Command(rotaBin, "list")
		cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "USER=" + os.Getenv("USER")}
		lo, rerr := cmd.Output()
		health := "?"
		for _, line := range strings.Split(string(lo), "\n") {
			if strings.Contains(line, chosen) {
				clean := stripANSI(line)
				health = strings.TrimSpace(clean[strings.Index(clean, chosen)+len(chosen):])
			}
		}
		health = strings.ReplaceAll(health, chosen, "acct-A")
		logf("rota list (local-only; no refresh): acct-A -> %q (err=%v); needs re-auth=%v", health, rerr,
			strings.Contains(health, "re-auth") || strings.Contains(health, "expired"))
	}
	liveAfter, _ := readLiveUUID(liveJSON)
	logf("live default identity unchanged=%v", liveAfter == liveBefore)
	calls, _ := os.ReadFile(secLog)
	var services []string
	for _, l := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
		services = append(services, strings.ReplaceAll(l, ClaudeKeychainService(info.Dir), "<profile item>"))
	}
	logf("security calls (verb + service): %v", services)
	for _, l := range strings.Split(string(calls), "\n") {
		if strings.HasSuffix(strings.TrimSpace(l), " Claude Code-credentials") {
			t.Errorf("touched the default keychain item")
		}
	}
	if strings.Contains(logs.String(), "sk-ant-") || strings.Contains(logs.String(), chosenSnap.refreshToken.reveal()) {
		t.Errorf("service log contains a token")
	}
}

func newTextLogger(w *lockedBuf) *slog.Logger { return slog.New(slog.NewTextHandler(w, nil)) }

func readLiveUUID(p string) (string, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	var c struct {
		OAuthAccount struct {
			AccountUUID string `json:"accountUuid"`
		} `json:"oauthAccount"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return "", err
	}
	return c.OAuthAccount.AccountUUID, nil
}

func svcHasItem(ctx context.Context, s *Service, dir string) bool {
	_, code, err := s.runSecurity(ctx, "find-generic-password", "-s", ClaudeKeychainService(dir))
	return err == nil && code == 0
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func statMode(p string) os.FileMode {
	fi, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return fi.Mode().Perm()
}

func stripANSI(s string) string {
	var b strings.Builder
	esc := false
	for _, r := range s {
		switch {
		case r == 0x1b:
			esc = true
		case esc && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'):
			esc = false
		case !esc:
			b.WriteRune(r)
		}
	}
	return b.String()
}
