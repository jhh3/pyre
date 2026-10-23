package claude

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// A session's environment file variables (merged into ResolvedLaunch.Env by
// the node) reach the Claude process through the tmux launch, and an empty
// auth key in the file (ResolvedLaunch.Unset) removes the inherited one.
func TestLaunchAppliesSessionEnv(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "synthetic-ambient-key")
	f := newFakeEnv(t, nil)
	work := filepath.Join(f.dir, "work")
	_ = os.MkdirAll(work, 0o755)
	out := filepath.Join(f.dir, "env.out")
	sess := map[string]string{"AGENT_BRIDGE_TOKEN": "synthetic-bridge-token", "AGENT_INTAKE_URL": "http://127.0.0.1:8091"}
	env := map[string]string{"PYRE_TEST_ROLE": "fakeclaude", "PYRE_SESSION_ID": "ses_env", "FAKE_ENV_FILE": out,
		"FAKE_ENV_KEYS": "AGENT_BRIDGE_TOKEN,AGENT_INTAKE_URL,ANTHROPIC_API_KEY"}
	for k, v := range sess {
		env[k] = v
	}
	if _, err := f.a.Launch(context.Background(), contracts.ResolvedLaunch{
		RunID: "run_env", SessionID: "ses_env", Harness: contracts.HarnessClaude, Cwd: work,
		Env: env, SessionEnv: sess, Unset: []string{"ANTHROPIC_API_KEY"},
		StateDir: filepath.Join(f.dir, "runs", "run_env"), ProfileDir: filepath.Join(f.dir, "cfg"),
	}); err != nil {
		t.Fatal(err)
	}
	var b []byte
	for i := 0; i < 100; i++ {
		if b, _ = os.ReadFile(out); len(b) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	got := string(b)
	for _, want := range []string{"AGENT_BRIDGE_TOKEN=synthetic-bridge-token", "AGENT_INTAKE_URL=http://127.0.0.1:8091", "ANTHROPIC_API_KEY unset"} {
		if !strings.Contains(got, want) {
			t.Fatalf("claude env %q, want %q", got, want)
		}
	}
}
