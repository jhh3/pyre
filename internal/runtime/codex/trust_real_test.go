package codex

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/tmux"
)

// TestRealCodexTUITrust launches a run in a fresh folder under a trusted
// directory with the real codex and reports whether the native TUI shows
// Codex's "Trust this folder?" screen. Skipped unless PYRE_REAL_HARNESS=1.
// PYRE_REAL_CODEX_HOME: isolated CODEX_HOME ("" = ambient, never written).
// PYRE_REAL_TRUST_DIR: the --trust-dir; PYRE_REAL_CODEX_CWD: fresh folder.
func TestRealCodexTUITrust(t *testing.T) {
	if os.Getenv("PYRE_REAL_HARNESS") != "1" {
		t.Skip("set PYRE_REAL_HARNESS=1 to run against the real codex")
	}
	cwd, trustDir := os.Getenv("PYRE_REAL_CODEX_CWD"), os.Getenv("PYRE_REAL_TRUST_DIR")
	if cwd == "" || trustDir == "" {
		t.Fatal("PYRE_REAL_CODEX_CWD and PYRE_REAL_TRUST_DIR required")
	}
	// PYRE_TRUST_MODE: "profile" (default; config write + watcher), "watch"
	// (answer the prompt only, as for an ambient home), "off" (cwd not
	// trusted: the prompt must stay).
	mode := os.Getenv("PYRE_TRUST_MODE")
	if mode == "" {
		mode = "profile"
	}
	if mode == "watch" {
		skipProfileTrust = true
		defer func() { skipProfileTrust = false }()
	}
	home := os.Getenv("PYRE_REAL_CODEX_HOME")
	codex := os.Getenv("PYRE_REAL_CODEX")
	if codex == "" {
		codex, _ = exec.LookPath("codex")
	}
	dir, _ := os.MkdirTemp("", "pqt")
	defer os.RemoveAll(dir)
	if home == "" && mode != "off" {
		t.Fatal("ambient home: only PYRE_TRUST_MODE=off (Pyre must not get the owner's config.toml written in a test)")
	}
	o := Options{NodeID: "node_real", TmuxSocket: filepath.Join(dir, "tmux", "t.sock"), StateRoot: filepath.Join(dir, "st"),
		CodexBin: codex, ApprovalPolicy: "never", Sandbox: "danger-full-access",
		TrustWorkspace: func(c string) bool { return mode != "off" && (c == trustDir || strings.HasPrefix(c, trustDir+"/")) }}
	a, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	defer a.srv.KillServer(ctx)
	l := contracts.ResolvedLaunch{RunID: "run_trust1", SessionID: "ses_trust", Harness: contracts.HarnessCodex, Cwd: cwd,
		StateDir: filepath.Join(dir, "runs", "run_trust1"), ProfileDir: home, Unset: []string{"OPENAI_API_KEY", "CODEX_API_KEY"}}
	h, err := a.Launch(ctx, l)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Stop(ctx, h)
	var screen string
	trustSeen := false
	for i := 0; i < 40; i++ {
		time.Sleep(500 * time.Millisecond)
		screen, _ = a.srv.CapturePane(ctx, h.RunID, tmux.CaptureOptions{})
		if isCodexTrustScreen(screen) {
			trustSeen = true
		}
	}
	t.Logf("mode=%s trust screen seen=%v final screen:\n%s", mode, trustSeen, screen)
	switch {
	case mode == "off" && !isCodexTrustScreen(screen):
		t.Fatal("untrusted cwd: expected the trust prompt to stay")
	case mode == "profile" && trustSeen:
		t.Fatal("profile home: trust screen appeared")
	case mode != "off" && !codexConversationScreen(screen):
		t.Fatal("TUI did not reach the conversation")
	}
}
