package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/jhh3/pyre/integrations/hive"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/daemon"
)

// TestOnRunEndedQueuesCodexRollout: an ended Codex run's rollout (found in
// its bound profile's CODEX_HOME) is queued for Hive capture; other harnesses
// and runs without a rollout are not.
func TestOnRunEndedQueuesCodexRollout(t *testing.T) {
	const thread = "019a0000-aaaa-7bbb-8ccc-0000000000aa"
	prof := t.TempDir()
	p := filepath.Join(prof, "sessions", "2026", "10", "05", "rollout-2026-10-05T08-00-00-"+thread+".jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	in := &integrations{log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		capturer: hive.NewCapturer(hive.New(hive.EnvFrom(nil), hive.Options{}), 8, nil)}
	codexRun := func(id string) daemon.RunEnded {
		return daemon.RunEnded{ProfileDir: prof, Run: contracts.Run{RunID: "run_x", Harness: contracts.HarnessCodex, Cwd: prof,
			NativeRef: &contracts.NativeRef{Kind: "codex_thread", ID: id}}}
	}
	in.onRunEnded(daemon.RunEnded{ProfileDir: prof, Run: contracts.Run{RunID: "run_c", Harness: contracts.HarnessClaude,
		NativeRef: &contracts.NativeRef{Kind: "claude_session", ID: thread}}})
	in.onRunEnded(codexRun("019a0000-aaaa-7bbb-8ccc-0000000000bb")) // no rollout
	if in.capturer.Pending() != 0 {
		t.Fatal("non-codex run or missing rollout queued")
	}
	in.onRunEnded(codexRun(thread))
	if in.capturer.Pending() != 1 {
		t.Fatal("codex rollout not queued")
	}
}
