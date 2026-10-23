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
// form after Pyre's own, the prompt stays last (a variadic channel list
// cannot swallow it), a session's own --append-system-prompt joins Pyre's
// guide in the single flag Claude keeps, and the run records the
// development channels whose startup warning it may confirm.
func TestLaunchHarnessArgs(t *testing.T) {
	f := newFakeEnv(t, nil)
	work := filepath.Join(f.dir, "work")
	_ = os.MkdirAll(work, 0o755)
	argvFile := filepath.Join(f.dir, "aide.argv")
	stateDir := filepath.Join(f.dir, "runs", "run_aide")
	_, err := f.a.Launch(context.Background(), contracts.ResolvedLaunch{
		RunID: "run_aide", SessionID: "ses_aide", Harness: contracts.HarnessClaude, Cwd: work,
		Args: []string{"--dangerously-load-development-channels=plugin:sample-bridge@sample",
			"--append-system-prompt-file=/Users/synthetic/Documents/sample/autonomy/aide.md",
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
	if !strings.Contains(string(raw), "\n--append-system-prompt\nPYRE GUIDE\n\nSession note: synthetic.\n") {
		t.Fatalf("appended prompt not merged: %q", raw)
	}
	argv := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if slices.ContainsFunc(argv, func(a string) bool { return strings.HasPrefix(a, "--append-system-prompt=") }) {
		t.Fatalf("second --append-system-prompt passed: %q", argv)
	}
	for _, want := range []string{"--dangerously-load-development-channels=plugin:sample-bridge@sample",
		"--append-system-prompt-file=/Users/synthetic/Documents/sample/autonomy/aide.md"} {
		if !slices.Contains(argv, want) {
			t.Fatalf("missing %s in %q", want, argv)
		}
	}
	if argv[len(argv)-1] != "synthetic first prompt" {
		t.Fatalf("prompt not last: %q", argv)
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
