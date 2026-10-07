package accounts

import (
	"bytes"
	"context"
	"encoding/json"
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

// TestRealRotaExpiredRefreshLinux is an owner-directed real experiment on a
// Linux node (server). It runs ONLY with PYRE_REAL_ROTA_REFRESH=1 and
// PYRE_REAL_HOME=<home>. It picks ONE non-active, unlocked rota Claude
// account whose access token has EXPIRED at rest (login good for >7 days),
// checks it out into a scratch profile (independent: no hub), runs one
// harness command inside the profile (PYRE_REAL_REFRESH_CMD, default
// "auth status --json"; it spends no turn), reports whether the harness
// refreshed the credential there, checks the account back in with
// write-back, and probes usage with the snapshot token (read-only). Output
// is redacted: the account is "acct-E"; no tokens, names or emails.
func TestRealRotaExpiredRefreshLinux(t *testing.T) {
	if os.Getenv("PYRE_REAL_ROTA_REFRESH") != "1" {
		t.Skip("owner-directed real experiment; set PYRE_REAL_ROTA_REFRESH=1 and PYRE_REAL_HOME")
	}
	home := os.Getenv("PYRE_REAL_HOME")
	if home == "" || !filepath.IsAbs(home) {
		t.Fatal("PYRE_REAL_HOME must be the real home directory")
	}
	t.Setenv("HOME", home)
	ctx := context.Background()
	scratch := t.TempDir()
	accDir := filepath.Join(home, ".claude", "accounts")
	liveBefore, err := readLiveUUID(filepath.Join(home, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	names, _ := filepath.Glob(filepath.Join(accDir, "*.json"))
	sort.Strings(names)
	var chosen, uuid string
	var snap0 *claudeSnapshot
	for _, p := range names {
		base := filepath.Base(p)
		if strings.HasPrefix(base, ".") {
			continue
		}
		sn, err := readClaudeSnapshot(p)
		if err != nil || sn.dead || sn.refreshToken.empty() || sn.accountUUID == "" || sn.accountUUID == liveBefore {
			continue
		}
		if fileExists(strings.TrimSuffix(p, ".json") + ".pyre-lock") {
			continue
		}
		if time.UnixMilli(sn.expiresAt).After(time.Now()) {
			continue // only accounts expired at rest
		}
		if rt, ok := sn.oauth["refreshTokenExpiresAt"]; ok {
			var ms int64
			json.Unmarshal(rt, &ms)
			if ms > 0 && time.UnixMilli(ms).Before(time.Now().Add(7*24*time.Hour)) {
				continue
			}
		}
		chosen, uuid, snap0 = strings.TrimSuffix(base, ".json"), sn.accountUUID, sn
		break
	}
	if chosen == "" {
		t.Skip("no non-active, unlocked rota Claude account expired at rest; nothing done")
	}
	snapPath := filepath.Join(accDir, chosen+".json")
	t.Logf("selected acct-E: access token expired %s ago at rest", time.Since(time.UnixMilli(snap0.expiresAt)).Round(time.Minute))

	db, err := sqlitex.Open(filepath.Join(scratch, "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	logs := &lockedBuf{}
	svc, err := New(ctx, Config{
		NodeID: "node_verify_linux", DB: db, ProfileRoot: filepath.Join(scratch, "profiles"), SharedRoot: filepath.Join(scratch, "shared"),
		UserClaudeDir: filepath.Join(home, ".claude"), UserClaudeJSON: filepath.Join(home, ".claude.json"), UserCodexDir: filepath.Join(home, ".codex"),
		GOOS: "linux", Logger: newTextLogger(logs),
		Rota: RotaConfig{Enabled: true, ClaudeAccountsDir: accDir, CodexHome: filepath.Join(home, ".codex"),
			Independent: map[string]bool{"acct_verify-e": true}, User: os.Getenv("USER")},
	})
	if err != nil {
		t.Fatal(err)
	}
	acct := contracts.Account{AccountID: "acct_verify-e", Provider: contracts.ProviderAnthropic, Label: "acct-E", Kind: "subscription",
		RotaRef: chosen, ProviderRef: uuid}
	info, err := svc.CreateRotaProfile(ctx, acct, contracts.HarnessClaude)
	if err != nil {
		t.Fatal(err)
	}
	res, err := svc.ResolveFor(ctx, ResolveRequest{Harness: contracts.HarnessClaude, Policy: explicit("acct_verify-e"), RunID: "run_verify_e", SessionID: "ses_verify_e"})
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	released := false
	defer func() {
		if !released {
			svc.Release("run_verify_e")
		}
	}()
	expOf := func() time.Time {
		_, si, _ := svc.claudeProfileOAuth(ctx, info.Dir)
		return si.ExpiresAt
	}
	before := expOf()
	env := []string{"HOME=" + home, "USER=" + os.Getenv("USER"), "PATH=" + os.Getenv("PATH"), "TERM=dumb", "LANG=en_US.UTF-8"}
	for k, v := range res.Env {
		env = append(env, k+"="+v)
	}
	claude, err := exec.LookPath("claude")
	if err != nil {
		t.Fatal("claude not on PATH")
	}
	args := strings.Fields(os.Getenv("PYRE_REAL_REFRESH_CMD"))
	if len(args) == 0 {
		args = []string{"auth", "status", "--json"}
	}
	var o, e bytes.Buffer
	t0 := time.Now()
	if args[0] == "tui" {
		// The interactive TUI with no prompt, in an isolated tmux server,
		// until the credential changes (or 45 s), then killed.
		sock := filepath.Join(scratch, "tmux.sock")
		tm := func(a ...string) error {
			c := exec.Command("tmux", append([]string{"-S", sock, "-f", "/dev/null"}, a...)...)
			return c.Run()
		}
		envArgs := []string{}
		for _, kv := range env {
			envArgs = append(envArgs, "-e", kv)
		}
		err = tm(append(append([]string{"new-session", "-d", "-s", "exp", "-x", "160", "-y", "45", "-c", scratch}, envArgs...), claude)...)
		for time.Since(t0) < 45*time.Second && !expOf().After(time.Now()) {
			time.Sleep(500 * time.Millisecond)
		}
		if b, cerr := exec.Command("tmux", "-S", sock, "capture-pane", "-p", "-t", "exp").Output(); cerr == nil {
			lines := strings.Split(strings.TrimSpace(string(b)), "\n")
			for _, l := range lines[max(0, len(lines)-6):] {
				t.Logf("tui screen: %s", truncate(strings.TrimSpace(redact(l)), 100))
			}
		}
		_ = tm("kill-server")
		time.Sleep(time.Second)
	} else {
		cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		cmd := exec.CommandContext(cctx, claude, args...)
		cmd.Env, cmd.Dir = env, scratch
		cmd.Stdout, cmd.Stderr = &o, &e
		err = cmd.Run()
		cancel()
	}
	var status map[string]any
	json.Unmarshal(o.Bytes(), &status)
	after := expOf()
	t.Logf("claude %s: exit=%v in %s loggedIn=%v authMethod=%v; profile access token expiry before=%s after=%s refreshed=%v",
		args[0], err, time.Since(t0).Round(time.Millisecond), status["loggedIn"], status["authMethod"],
		before.Format(time.RFC3339), after.Format(time.RFC3339), after.After(time.Now()))

	if _, err := svc.ReleaseRun(ctx, "run_verify_e"); err != nil {
		t.Fatal(err)
	}
	released = true
	sn2, err := readClaudeSnapshot(snapPath)
	t.Logf("check-in: lock removed=%v, profile credential removed=%v, snapshot parses=%v, snapshot access token valid now=%v, mode=%v",
		!fileExists(filepath.Join(accDir, chosen+".pyre-lock")), !fileExists(filepath.Join(info.Dir, ".credentials.json")),
		err == nil, err == nil && time.UnixMilli(sn2.expiresAt).After(time.Now()), statMode(snapPath))
	cs, _ := svc.Checkouts(ctx)
	if len(cs) == 1 {
		t.Logf("checkout record: state=%s detail=%q", cs[0].State, cs[0].Detail)
	}
	svc.cfg.ClaudeUsageURL = "https://api.anthropic.com/api/oauth/usage"
	svc.ObserveDue(ctx, contracts.ProviderAnthropic)
	if ob, _ := getObservation(ctx, db, "acct_verify-e"); ob != nil {
		t.Logf("post-check-in usage probe with the snapshot token: eligibility=%s windows=%d detail=%q", ob.Eligibility, len(ob.Windows), ob.Detail)
	}
	if strings.Contains(logs.String(), snap0.refreshToken.reveal()) || strings.Contains(logs.String(), "sk-ant-") {
		t.Errorf("service log contains a token")
	}
}
