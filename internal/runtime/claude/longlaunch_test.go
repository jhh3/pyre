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

// A Claude launch whose guide and initial prompt are far over tmux's 16 KiB
// client command limit starts (issue iss_…: "tmux
// new-session: command too long" with a 5 KB brief on laptop), on the real
// tmux of an isolated socket, and Claude gets both byte for byte, special
// characters included.
func TestLaunchLongPromptReachesClaude(t *testing.T) {
	f := newFakeEnv(t, nil)
	work := filepath.Join(f.dir, "work")
	_ = os.MkdirAll(work, 0o755)
	argvFile := filepath.Join(f.dir, "long.argv")
	special := "it's \"quoted\" $HOME ${PATH} $(id) `whoami` \\ ; echo hi; *glob? #x\nnext line\tünïcødé — 日本語 🔥 end;"
	repeat := func(n int) string {
		var b strings.Builder
		for b.Len() < n {
			b.WriteString(special)
		}
		return b.String()
	}
	guide, prompt := "SYNTHETIC GUIDE "+repeat(12<<10), "SYNTHETIC BRIEF "+repeat(20<<10)
	h, err := f.a.Launch(context.Background(), contracts.ResolvedLaunch{
		RunID: "run_long", SessionID: "ses_long", Harness: contracts.HarnessClaude, Cwd: work,
		AgentGuide: guide, InitialPrompt: prompt,
		Env:      map[string]string{"PYRE_TEST_ROLE": "fakeclaude", "PYRE_SESSION_ID": "ses_long", "FAKE_ARGV_FILE": argvFile},
		StateDir: filepath.Join(f.dir, "runs", "run_long"), ProfileDir: filepath.Join(f.dir, "cfg"),
	})
	if err != nil {
		t.Fatalf("launch with a %d-byte guide and a %d-byte prompt: %v", len(guide), len(prompt), err)
	}
	var raw []byte
	for i := 0; i < 100 && len(raw) == 0; i++ {
		raw, _ = os.ReadFile(argvFile)
		time.Sleep(50 * time.Millisecond)
	}
	// The fake writes its arguments joined by "\n".
	if !strings.Contains(string(raw), "\n--append-system-prompt\n"+guide+"\n") {
		t.Fatalf("the guide did not arrive intact (%d bytes of argv)", len(raw))
	}
	if !strings.HasSuffix(string(raw), "\n"+prompt) {
		t.Fatalf("the prompt did not arrive intact or last (%d bytes of argv)", len(raw))
	}
	if obs, _ := f.a.Observe(context.Background(), h, ""); obs.State == contracts.RunFailed || obs.State == contracts.RunExited {
		t.Fatalf("run %s: %s", obs.State, obs.StateReason)
	}
}
