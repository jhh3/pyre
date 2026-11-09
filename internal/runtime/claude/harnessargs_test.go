package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// The Aide session's extra arguments reach Claude Code in canonical "="
// form after Pyre's own, a session's own --append-system-prompt(-file)
// joins Pyre's guide in the single file Claude keeps, the prompt is pasted
// after startup (no plugin module here) instead of riding on argv, and the
// run records the development channels whose startup warning it may
// confirm.
func TestLaunchHarnessArgs(t *testing.T) {
	f := newFakeEnv(t, nil)
	work := filepath.Join(f.dir, "work")
	_ = os.MkdirAll(work, 0o755)
	argvFile := filepath.Join(f.dir, "aide.argv")
	stateDir := filepath.Join(f.dir, "runs", "run_aide")
	own := filepath.Join(f.dir, "aide.md")
	_ = os.WriteFile(own, []byte("Aide autonomy notes."), 0o600)
	h, err := f.a.Launch(context.Background(), contracts.ResolvedLaunch{
		RunID: "run_aide", SessionID: "ses_aide", Harness: contracts.HarnessClaude, Cwd: work,
		Args: []string{"--dangerously-load-development-channels=plugin:sample-bridge@sample",
			"--append-system-prompt-file=" + own,
			"--append-system-prompt=Session note: synthetic."},
		AgentGuide: "PYRE GUIDE", InitialPrompt: "synthetic first prompt",
		Env:      map[string]string{"PYRE_TEST_ROLE": "fakeclaude", "PYRE_SESSION_ID": "ses_aide", "FAKE_ARGV_FILE": argvFile, PermissionsEnv: "ask"},
		StateDir: stateDir, ProfileDir: filepath.Join(f.dir, "cfg"),
	})
	if err != nil {
		t.Fatal(err)
	}
	var raw []byte
	for i := 0; i < 100 && len(raw) == 0; i++ {
		raw, _ = os.ReadFile(argvFile)
		time.Sleep(50 * time.Millisecond)
	}
	if out := os.Getenv("PYRE_EVIDENCE_ARGV"); out != "" {
		_ = os.WriteFile(out, raw, 0o644)
	}
	// The fake writes one argument per line.
	sys := filepath.Join(stateDir, fileSysPrompt)
	if b, _ := os.ReadFile(sys); string(b) != "PYRE GUIDE\n\nAide autonomy notes.\n\nSession note: synthetic." {
		t.Fatalf("appended prompt not merged: %q", b)
	}
	argv := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if slices.ContainsFunc(argv, func(a string) bool {
		return strings.HasPrefix(a, "--append-system-prompt=") || strings.HasPrefix(a, "--append-system-prompt-file=") || strings.Contains(a, "synthetic")
	}) {
		t.Fatalf("prompt text or a second prompt flag passed: %q", argv)
	}
	if !slices.Contains(argv, "--dangerously-load-development-channels=plugin:sample-bridge@sample") || !slices.Contains(argv, sys) {
		t.Fatalf("missing channel or prompt file in %q", argv)
	}
	if !strings.HasPrefix(h.Data["initial_prompt"], "pasted") {
		t.Fatalf("initial prompt: %q", h.Data["initial_prompt"])
	}
	end := time.Now().Add(10 * time.Second)
	for !promptRan(filepath.Join(f.dir, "cfg"), "synthetic first prompt") {
		if time.Now().After(end) {
			t.Fatal("the pasted first prompt never ran")
		}
		time.Sleep(100 * time.Millisecond)
	}
	var meta runMeta
	b, _ := os.ReadFile(filepath.Join(stateDir, fileRun))
	if json.Unmarshal(b, &meta) != nil || !slices.Equal(meta.DevChannels, []string{"plugin:sample-bridge@sample"}) {
		t.Fatalf("meta dev channels = %+v", meta)
	}
}

func TestParseDevChannelsDialog(t *testing.T) {
	screen := strings.Join([]string{
		"╭──────────────────────────────────────────────╮",
		"│ WARNING: Loading development channels         │",
		"│ --dangerously-load-development-channels is for local channel development only. │",
		"│ Channels: plugin:sample-bridge@sample          │",
		"│ ❯ 1. I am using this for local development     │",
		"│   2. Exit                                     │",
		"╰──────────────────────────────────────────────╯",
	}, "\n")
	si := parseScreen(screen, "")
	if si.DialogKind != "devchannels" || !si.DevAccept {
		t.Fatalf("parsed %+v", si)
	}
	si = parseScreen(strings.Replace(screen, "❯ 1.", "  1.", 1), "")
	if si.DialogKind != "devchannels" || si.DevAccept {
		t.Fatalf("unselected parsed %+v", si)
	}
}
