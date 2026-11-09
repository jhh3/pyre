package claude

import (
	"context"
	"encoding/json"
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
// characters included. Neither is on the command line, where `pkill -f`
// patterns matched briefs (issue iss_…): the guide
// is a 0600 --append-system-prompt-file, the prompt goes through the plugin.
func TestLaunchLongPromptReachesClaude(t *testing.T) {
	f := newFakeEnv(t, nil)
	work := filepath.Join(f.dir, "work")
	_ = os.MkdirAll(work, 0o755)
	argvFile, profile := filepath.Join(f.dir, "long.argv"), filepath.Join(f.dir, "cfg")
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
		Env:      map[string]string{"PYRE_TEST_ROLE": "fakeclaude", "PYRE_SESSION_ID": "ses_long", "FAKE_ARGV_FILE": argvFile, "FAKE_PLUGIN_HOST": "1"},
		StateDir: filepath.Join(f.dir, "runs", "run_long"), ProfileDir: profile,
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
	if len(raw) > 4<<10 || strings.Contains(string(raw), "SYNTHETIC") {
		t.Fatalf("prompt text on the command line (%d bytes of argv)", len(raw))
	}
	sys := filepath.Join(f.dir, "runs", "run_long", fileSysPrompt)
	if !strings.Contains(string(raw), "\n--append-system-prompt-file\n"+sys+"\n") {
		t.Fatalf("no --append-system-prompt-file %s: %q", sys, raw)
	}
	if b, err := os.ReadFile(sys); err != nil || string(b) != guide {
		t.Fatalf("the guide did not arrive intact (%d bytes, %v)", len(b), err)
	}
	if st, err := os.Stat(sys); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("system prompt file mode: %v %v", st.Mode(), err)
	}
	if h.Data["initial_prompt"] == "" || strings.HasPrefix(h.Data["initial_prompt"], "not") {
		t.Fatalf("initial prompt: %q", h.Data["initial_prompt"])
	}
	// The fake's transcript stores the prompt JSON-encoded.
	want, _ := json.Marshal(prompt)
	end := time.Now().Add(15 * time.Second)
	for !promptRan(profile, string(want[1:len(want)-1])) {
		if time.Now().After(end) {
			t.Fatal("the prompt did not reach Claude intact")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if obs, _ := f.a.Observe(context.Background(), h, ""); obs.State == contracts.RunFailed || obs.State == contracts.RunExited {
		t.Fatalf("run %s: %s", obs.State, obs.StateReason)
	}
}
