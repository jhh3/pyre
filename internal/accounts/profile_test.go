package accounts

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

func TestCanonicalPathHandling(t *testing.T) {
	// /tmp vs /private/tmp (macOS) and symlinked roots resolve to one string;
	// the Claude keychain item name depends on the exact string.
	if runtime.GOOS == "darwin" {
		c, err := Canonicalize("/tmp")
		if err != nil || c != "/private/tmp" {
			t.Fatalf("Canonicalize(/tmp) = %q, %v; want /private/tmp", c, err)
		}
		if ClaudeKeychainService("/tmp/x") == ClaudeKeychainService("/private/tmp/x") {
			t.Fatal("keychain names must differ for different strings (that is why Pyre canonicalizes)")
		}
	}
	base := t.TempDir()
	real := filepath.Join(base, "real-root")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link-root")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, func(c *Config) { c.ProfileRoot = link + "//" }) // symlink + doubled slash
	info, err := e.svc.CreateProfile(context.Background(), contracts.Account{AccountID: "acct_synth-a", Provider: contracts.ProviderAnthropic}, contracts.HarnessClaude)
	if err != nil {
		t.Fatal(err)
	}
	canonReal, _ := filepath.EvalSymlinks(real)
	want := filepath.Join(canonReal, "acct_synth-a", "claude")
	if info.Dir != want {
		t.Fatalf("profile dir %q, want canonical %q", info.Dir, want)
	}
	if info.KeychainService != ClaudeKeychainService(want) {
		t.Fatalf("keychain service computed from non-canonical path")
	}
	// Same answer through the other spelling.
	viaLink, _ := Canonicalize(filepath.Join(link, "acct_synth-a", "claude"))
	if viaLink != info.Dir || ClaudeKeychainService(viaLink) != info.KeychainService {
		t.Fatal("canonical forms differ")
	}
	for _, d := range []string{canonReal, filepath.Join(canonReal, "acct_synth-a"), info.Dir} {
		fi, err := os.Stat(d)
		if err != nil || fi.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode %v err %v; want 0700", d, fi.Mode().Perm(), err)
		}
	}
	g, err := e.svc.getGrant(context.Background(), e.svc.db, "acct_synth-a", contracts.HarnessClaude)
	if err != nil || g.ProfileDir != want {
		t.Fatalf("grant profile dir %q (%v)", g.ProfileDir, err)
	}
	if !strings.Contains(info.LoginCommand, "CLAUDE_CONFIG_DIR="+want) {
		t.Fatalf("login command not canonical: %s", info.LoginCommand)
	}
}

func TestCreateProfileClaudeComposition(t *testing.T) {
	e := newEnv(t)
	before := fileState(t, filepath.Dir(e.userClaude))
	a := contracts.Account{AccountID: "acct_synth-a", Provider: contracts.ProviderAnthropic, Label: "A"}
	info, err := e.svc.CreateProfile(context.Background(), a, contracts.HarnessClaude)
	if err != nil {
		t.Fatal(err)
	}
	want := "CLAUDE_CONFIG_DIR=" + shellQuote(info.Dir) + " claude auth login --claudeai"
	if info.LoginCommand != want {
		t.Fatalf("login command %q want %q", info.LoginCommand, want)
	}
	// No credential store was created, copied or linked.
	for _, n := range []string{".credentials.json", "auth.json", "oauth-token"} {
		if _, err := os.Lstat(filepath.Join(info.Dir, n)); !os.IsNotExist(err) {
			t.Fatalf("%s must not exist in a fresh profile (err=%v)", n, err)
		}
	}
	// Composed from the user's config without write-through: directories are
	// real profile dirs whose entries link to the originals; files the
	// harness may rewrite are per-launch copies.
	uc := canonicalOrClean(e.userClaude)
	for _, n := range []string{"skills", "agents"} {
		if fi, err := os.Lstat(filepath.Join(info.Dir, n)); err != nil || !fi.IsDir() {
			t.Fatalf("%s must be a real profile dir: %v %v", n, fi, err)
		}
	}
	if tg, err := os.Readlink(filepath.Join(info.Dir, "skills", "demo")); err != nil || tg != filepath.Join(uc, "skills", "demo") {
		t.Fatalf("skills/demo link = %q (%v)", tg, err)
	}
	for _, n := range []string{"settings.json", "CLAUDE.md"} {
		fi, err := os.Lstat(filepath.Join(info.Dir, n))
		if err != nil || !fi.Mode().IsRegular() || !bytes.Equal(mustRead(t, filepath.Join(info.Dir, n)), mustRead(t, filepath.Join(uc, n))) {
			t.Fatalf("%s must be a regular per-launch copy: %v %v", n, fi, err)
		}
		if !contains(info.Copied, n) {
			t.Fatalf("%s not reported as copied: %v", n, info.Copied)
		}
	}
	for _, n := range []string{"commands", "plugins", "output-styles"} {
		if !contains(info.Skipped, n) {
			t.Fatalf("%s absent in user config should be skipped, got %v", n, info.Skipped)
		}
		if _, err := os.Lstat(filepath.Join(e.userClaude, n)); !os.IsNotExist(err) {
			t.Fatalf("composition must not create %s in the user's config", n)
		}
	}
	// Shared transcript stores live in Pyre's shared store.
	for _, n := range []string{"projects", "history.jsonl", "file-history", "todos"} {
		tg, err := os.Readlink(filepath.Join(info.Dir, n))
		if err != nil || !strings.HasPrefix(tg, canonicalOrClean(e.cfg.SharedRoot)) {
			t.Fatalf("%s should link into the shared store, got %q (%v)", n, tg, err)
		}
	}
	// .claude.json is a regular file with the user's mcpServers merged.
	fi, err := os.Lstat(filepath.Join(info.Dir, ".claude.json"))
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		t.Fatalf(".claude.json must be a 0600 regular file: %v %v", fi, err)
	}
	var cj map[string]json.RawMessage
	b, _ := os.ReadFile(filepath.Join(info.Dir, ".claude.json"))
	json.Unmarshal(b, &cj)
	if !strings.Contains(string(cj["mcpServers"]), "synthetic-mcp-server") {
		t.Fatalf("user mcpServers not merged: %s", b)
	}
	if _, ok := cj["oauthAccount"]; ok {
		t.Fatal("the user's oauthAccount must not be copied into a profile")
	}
	// Idempotent; the profile login's oauthAccount survives a re-run.
	loginClaude(t, info.Dir, "A", false)
	info2, err := e.svc.CreateProfile(context.Background(), a, contracts.HarnessClaude)
	if err != nil || info2.Dir != info.Dir || info2.GrantID != info.GrantID || len(info2.Conflicts) != 0 {
		t.Fatalf("re-run: %+v %v", info2, err)
	}
	if id, err := claudeIdentity(info.Dir); err != nil || id != claudeUUID("A") {
		t.Fatalf("profile identity lost on re-run: %q %v", id, err)
	}
	// A conflicting entry is reported and left alone.
	os.Remove(filepath.Join(info.Dir, "agents"))
	os.WriteFile(filepath.Join(info.Dir, "agents"), []byte("mine"), 0o600)
	info3, _ := e.svc.CreateProfile(context.Background(), a, contracts.HarnessClaude)
	if len(info3.Conflicts) != 1 {
		t.Fatalf("want one conflict, got %v", info3.Conflicts)
	}
	if b, _ := os.ReadFile(filepath.Join(info.Dir, "agents")); string(b) != "mine" {
		t.Fatal("conflicting entry was modified")
	}
	sameState(t, "user ~/.claude and ~/.claude.json", before, fileState(t, filepath.Dir(e.userClaude)))
}

func TestCreateProfileCodexComposition(t *testing.T) {
	e := newEnv(t)
	before := fileState(t, e.userCodex)
	a := contracts.Account{AccountID: "acct_synth-x", Provider: contracts.ProviderOpenAI}
	info, err := e.svc.CreateProfile(context.Background(), a, contracts.HarnessCodex)
	if err != nil {
		t.Fatal(err)
	}
	want := "CODEX_HOME=" + shellQuote(info.Dir) + ` codex -c 'cli_auth_credentials_store="file"' login --device-auth`
	if info.LoginCommand != want {
		t.Fatalf("login command %q want %q", info.LoginCommand, want)
	}
	if _, err := os.Lstat(filepath.Join(info.Dir, "auth.json")); !os.IsNotExist(err) {
		t.Fatal("auth.json must never be copied or linked into a profile")
	}
	cfg, err := os.ReadFile(filepath.Join(info.Dir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(cfg)
	if !strings.Contains(s, "cli_auth_credentials_store = \"file\"") || strings.Contains(s, "\"keyring\"") {
		t.Fatalf("config.toml must force the file store:\n%s", s)
	}
	if !strings.Contains(s, "model = \"synthetic\"") || !strings.Contains(s, "[features]") {
		t.Fatalf("user config not adopted:\n%s", s)
	}
	if strings.Index(s, "cli_auth_credentials_store = \"file\"") > strings.Index(s, "[features]") {
		t.Fatal("managed key must be in the TOML root table")
	}
	for _, n := range []string{"skills", "prompts"} {
		if fi, err := os.Lstat(filepath.Join(info.Dir, n)); err != nil || !fi.IsDir() {
			t.Fatalf("%s must be a real profile dir: %v %v", n, fi, err)
		}
	}
	if fi, err := os.Lstat(filepath.Join(info.Dir, "AGENTS.md")); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("AGENTS.md must be a per-launch copy: %v %v", fi, err)
	}
	for _, n := range []string{"sessions", "archived_sessions", "session_index.jsonl"} {
		if _, err := os.Readlink(filepath.Join(info.Dir, n)); err != nil {
			t.Fatalf("%s should be a shared-store link: %v", n, err)
		}
	}
	g, _ := e.svc.getGrant(context.Background(), e.svc.db, a.AccountID, contracts.HarnessCodex)
	if g.Kind != contracts.CredCodexHomeFile || g.MaxProcs != 1 || g.RefreshOwner != "harness" || g.State != contracts.GrantUnknown {
		t.Fatalf("codex grant %+v", g)
	}
	sameState(t, "user ~/.codex", before, fileState(t, e.userCodex))
}

func TestCreateProfileRefusals(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.svc.CreateProfile(ctx, contracts.Account{AccountID: "acct_x", Provider: contracts.ProviderOpenAI}, contracts.HarnessClaude); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("cross-provider profile must be refused: %v", err)
	}
	if _, err := e.svc.CreateProfile(ctx, contracts.Account{AccountID: "acct_x", Provider: contracts.ProviderAnthropic, Kind: "api"}, contracts.HarnessClaude); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("API billing account must be refused: %v", err)
	}
	if _, err := e.svc.CreateProfile(ctx, contracts.Account{AccountID: "../evil", Provider: contracts.ProviderAnthropic}, contracts.HarnessClaude); err == nil {
		t.Fatal("path traversal account id accepted")
	}
	e2 := newEnv(t, func(c *Config) { c.ProfileRoot = filepath.Join(c.UserClaudeDir, "profiles") })
	if _, err := e2.svc.CreateProfile(ctx, contracts.Account{AccountID: "acct_x", Provider: contracts.ProviderAnthropic}, contracts.HarnessClaude); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("profile root inside ~/.claude must be refused: %v", err)
	}
}
