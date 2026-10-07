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

func TestOwnerAcceptedBypass(t *testing.T) {
	home := t.TempDir()
	if OwnerAcceptedBypass(home) {
		t.Fatal("no settings: not accepted")
	}
	os.MkdirAll(filepath.Join(home, ".claude"), 0o700)
	os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(`{"skipDangerousModePermissionPrompt":false}`), 0o600)
	if OwnerAcceptedBypass(home) {
		t.Fatal("false value accepted")
	}
	os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"bypassPermissionsModeAccepted":true}`), 0o600)
	if !OwnerAcceptedBypass(home) {
		t.Fatal("legacy ~/.claude.json acceptance ignored")
	}
	os.Remove(filepath.Join(home, ".claude.json"))
	os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(`{"skipDangerousModePermissionPrompt":true}`), 0o600)
	if !OwnerAcceptedBypass(home) {
		t.Fatal("settings.json acceptance ignored")
	}
}

func TestLaunchPermissionsBypass(t *testing.T) {
	home := t.TempDir()
	f := newFakeEnv(t, func(o *Options) { o.OwnerHome = home; o.PermissionMode = "default" })
	work := filepath.Join(f.dir, "work")
	_ = os.MkdirAll(work, 0o755)
	launch := func(runID, perm string) (contracts.RunHandle, string, error) {
		argvFile := filepath.Join(f.dir, runID+".argv")
		env := map[string]string{"PYRE_TEST_ROLE": "fakeclaude", "PYRE_SESSION_ID": "ses_t", "FAKE_ARGV_FILE": argvFile}
		if perm != "" {
			env[PermissionsEnv] = perm
		}
		h, err := f.a.Launch(context.Background(), contracts.ResolvedLaunch{
			RunID: runID, SessionID: "ses_t", Harness: contracts.HarnessClaude, Cwd: work,
			Env: env, StateDir: filepath.Join(f.dir, "runs", runID), ProfileDir: filepath.Join(f.dir, "cfg"),
		})
		if err != nil {
			return h, "", err
		}
		var argv []byte
		for i := 0; i < 100; i++ {
			if argv, _ = os.ReadFile(argvFile); len(argv) > 0 {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		return h, string(argv), nil
	}
	// Not accepted by the owner: refused before anything starts.
	if _, _, err := launch("run_noaccept", "bypass"); err == nil || !strings.Contains(err.Error(), "skipDangerousModePermissionPrompt") {
		t.Fatalf("bypass without the owner's acceptance: %v", err)
	}
	os.MkdirAll(filepath.Join(home, ".claude"), 0o700)
	os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(`{"skipDangerousModePermissionPrompt":true}`), 0o600)

	h, argv, err := launch("run_bypass", "bypass")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(argv, "--dangerously-skip-permissions") || strings.Contains(argv, "--permission-mode") {
		t.Fatalf("bypass argv: %q", argv)
	}
	if h.Data["permissions"] != "bypass" {
		t.Fatalf("handle data: %v", h.Data)
	}
	var rs map[string]any
	b, _ := os.ReadFile(filepath.Join(h.StateDir, fileHooks))
	_ = json.Unmarshal(b, &rs)
	if rs[settingSkipBypassPrompt] != true || rs["hooks"] == nil {
		t.Fatalf("run settings must carry the acceptance and keep the hooks: %s", b)
	}

	h, argv, err = launch("run_ask", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(argv, "--dangerously-skip-permissions") || !strings.Contains(argv, "--permission-mode\ndefault") {
		t.Fatalf("ask argv: %q", argv)
	}
	b, _ = os.ReadFile(filepath.Join(h.StateDir, fileHooks))
	if strings.Contains(string(b), settingSkipBypassPrompt) {
		t.Fatal("ask run got the bypass acceptance")
	}
}
