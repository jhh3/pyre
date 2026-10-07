package claude

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestWarmLogin: the TUI is started with no prompt, hooks off and no MCP
// servers, in the profile, in a throwaway tmux server that is gone afterwards;
// it returns once the credential is fresh, and fails fast if Claude exits.
func TestWarmLogin(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux missing")
	}
	base, err := os.MkdirTemp("/tmp", "pyre-wl-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(base)
	profile := filepath.Join(base, "profile")
	os.MkdirAll(profile, 0o700)
	argsLog := filepath.Join(base, "args")
	// Fake claude: records argv and env, "refreshes" (writes a marker), stays up.
	fake := filepath.Join(base, "claude")
	os.WriteFile(fake, []byte(`#!/bin/sh
printf '%s\n' "$@" > '`+argsLog+`'
echo "CFG=$CLAUDE_CONFIG_DIR KEY=${ANTHROPIC_API_KEY:-unset} UPD=${DISABLE_AUTOUPDATER:-unset}" >> '`+argsLog+`'
sleep 0.3
touch "$CLAUDE_CONFIG_DIR/refreshed"
sleep 60
`), 0o700)
	fresh := func() bool { _, err := os.Stat(filepath.Join(profile, "refreshed")); return err == nil }
	t.Setenv("ANTHROPIC_API_KEY", "synthetic-not-a-key")
	o := WarmLoginOptions{Bin: fake, Dir: filepath.Join(base, "wl"), Unset: []string{"ANTHROPIC_API_KEY"}, Poll: 50 * time.Millisecond,
		Env: map[string]string{"DISABLE_AUTOUPDATER": "1"}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	t0 := time.Now()
	if err := WarmLogin(ctx, o, profile, fresh); err != nil {
		t.Fatalf("warm login: %v", err)
	}
	if time.Since(t0) > 10*time.Second {
		t.Fatal("did not return promptly after the refresh")
	}
	b, _ := os.ReadFile(argsLog)
	got := string(b)
	if !strings.Contains(got, `{"disableAllHooks":true}`) || !strings.Contains(got, "--strict-mcp-config") || strings.Contains(got, "-p\n") {
		t.Fatalf("argv/env %q", got)
	}
	// The owner's auto-update opt-out reaches the warm-up TUI as an
	// environment variable (never as a profile setting, which Claude
	// migrates into the linked settings.json).
	if !strings.Contains(got, "CFG="+profile) || !strings.Contains(got, "KEY=unset") || !strings.Contains(got, "UPD=1") {
		t.Fatalf("profile env / API key unset: %q", got)
	}
	if ents, _ := os.ReadDir(o.Dir); len(ents) != 0 {
		t.Fatalf("throwaway tmux dir left: %v", ents)
	}
	// Claude exits without refreshing: fails fast, not at the timeout.
	os.Remove(filepath.Join(profile, "refreshed"))
	os.WriteFile(fake, []byte("#!/bin/sh\nexit 3\n"), 0o700)
	t0 = time.Now()
	if err := WarmLogin(ctx, o, profile, fresh); err == nil || time.Since(t0) > 10*time.Second {
		t.Fatalf("exit without refresh: %v after %s", err, time.Since(t0))
	}
}
