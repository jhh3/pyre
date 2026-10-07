package accounts

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

// fakeClaudeAuth writes a fake `claude` that answers `auth status --json`
// from <dir>/status.json, substituting $CLAUDE_CONFIG_DIR for @DIR@, and
// records its environment.
func fakeClaudeAuth(t *testing.T) (bin, statusFile, envFile string) {
	dir := t.TempDir()
	bin = filepath.Join(dir, "claude")
	statusFile = filepath.Join(dir, "status.json")
	envFile = filepath.Join(dir, "env.txt")
	script := "#!/bin/sh\n" +
		"[ \"$1 $2 $3\" = \"auth status --json\" ] || exit 9\n" +
		"env > '" + envFile + "'\n" +
		"[ -f '" + statusFile + "' ] || exit 2\n" +
		"sed \"s#@DIR@#$CLAUDE_CONFIG_DIR#g\" '" + statusFile + "'\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin, statusFile, envFile
}

func TestVerifyGrantUsesClaudeAuthStatus(t *testing.T) {
	bin, status, envFile := fakeClaudeAuth(t)
	e := newEnv(t, func(c *Config) { c.ClaudeBin = bin })
	ctx := context.Background()
	os.Setenv("ANTHROPIC_API_KEY", "synthetic-ambient-key-must-not-leak")
	defer os.Unsetenv("ANTHROPIC_API_KEY")

	write := func(s string) { os.WriteFile(status, []byte(s), 0o600) }
	write(`{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty","configDirectory":"@DIR@"}`)
	info := e.addAccount("acct_synth-as", contracts.HarnessClaude, "AS") // verifies
	envb, _ := os.ReadFile(envFile)
	if !strings.Contains(string(envb), "CLAUDE_CONFIG_DIR="+info.Dir) || strings.Contains(string(envb), "synthetic-ambient-key") {
		t.Fatalf("probe environment: %s", envb)
	}

	verify := func() (contracts.DeviceGrant, error) {
		return e.svc.VerifyGrant(ctx, "acct_synth-as", contracts.HarnessClaude)
	}
	// Harness reports a different config directory: binding not in effect.
	write(`{"loggedIn":true,"authMethod":"claude.ai","configDirectory":"/home/synth/.claude"}`)
	if g, err := verify(); err == nil || !strings.Contains(err.Error(), "binding not in effect") || g.State != contracts.GrantUnknown {
		t.Fatalf("config dir mismatch: %+v %v", g, err)
	}
	// API-key login is never a subscription.
	write(`{"loggedIn":true,"authMethod":"api_key","configDirectory":"@DIR@","apiKeySource":"ANTHROPIC_API_KEY"}`)
	if _, err := verify(); err == nil || !strings.Contains(err.Error(), "API-key") {
		t.Fatalf("api key: %v", err)
	}
	// Logged out according to the harness.
	write(`{"loggedIn":false,"authMethod":"none","configDirectory":"@DIR@"}`)
	if _, err := verify(); err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("logged out: %v", err)
	}
	// Probe unavailable (old Claude without the command): file signals decide.
	os.Remove(status)
	if g, err := verify(); err != nil || g.State != contracts.GrantValid {
		t.Fatalf("fallback: %+v %v", g, err)
	}
	// Agreement: valid.
	write(`{"loggedIn":true,"authMethod":"claude.ai","configDirectory":"@DIR@"}`)
	if g, err := verify(); err != nil || g.State != contracts.GrantValid {
		t.Fatalf("agree: %+v %v", g, err)
	}
}
