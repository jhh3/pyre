package claude

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Runs get Options.TmpDir as CLAUDE_CODE_TMPDIR (created), unless the launch
// environment already sets one.
func TestLaunchSetsClaudeTmpDir(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "state", "tmp", "claude")
	f := newFakeEnv(t, func(o *Options) { o.TmpDir = tmp })
	work := filepath.Join(f.dir, "work")
	_ = os.MkdirAll(work, 0o755)
	launch := func(runID string, extra map[string]string) string {
		t.Helper()
		out := filepath.Join(f.dir, runID+".tmpdir")
		env := map[string]string{"PYRE_TEST_ROLE": "fakeclaude", "PYRE_SESSION_ID": "ses_t", "FAKE_TMPDIR_FILE": out}
		for k, v := range extra {
			env[k] = v
		}
		if _, err := f.a.Launch(context.Background(), contracts.ResolvedLaunch{
			RunID: runID, SessionID: "ses_t", Harness: contracts.HarnessClaude, Cwd: work,
			Env: env, StateDir: filepath.Join(f.dir, "runs", runID), ProfileDir: filepath.Join(f.dir, "cfg"),
		}); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 100; i++ {
			if _, err := os.Stat(out); err == nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		b, err := os.ReadFile(out)
		if err != nil {
			t.Fatalf("%s: fake never reported its env: %v", runID, err)
		}
		return string(b)
	}
	if got := launch("run_tmp_default", nil); got != tmp {
		t.Fatalf("CLAUDE_CODE_TMPDIR = %q, want %q", got, tmp)
	}
	if st, err := os.Stat(tmp); err != nil || !st.IsDir() || st.Mode().Perm() != 0o700 {
		t.Fatalf("temp dir not created 0700: %v %v", st, err)
	}
	own := filepath.Join(f.dir, "own-tmp")
	if got := launch("run_tmp_own", map[string]string{TmpDirEnv: own}); got != own {
		t.Fatalf("launch env override: CLAUDE_CODE_TMPDIR = %q, want %q", got, own)
	}
}
