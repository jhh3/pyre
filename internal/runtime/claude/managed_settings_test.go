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

// A managed settings overlay (environment generation) is merged into the
// run's single --settings file: Pyre's hooks stay first in every event, the
// managed hooks follow, permissions are unioned, other keys are taken over.
func TestManagedSettingsMergedIntoRunSettings(t *testing.T) {
	dir := t.TempDir()
	overlay := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(overlay, []byte(`{
  "env": {"SYNTHETIC_MANAGED": "p3"},
  "cleanupPeriodDays": 45,
  "permissions": {"allow": ["Bash(ls:*)", "mcp__pyre__inbox"], "deny": ["Bash(rm:*)"], "defaultMode": "default"},
  "hooks": {
    "SessionStart": [{"hooks": [{"type": "command", "command": "echo synthetic-managed-start"}]}],
    "ConfigChange": [{"hooks": [{"type": "command", "command": "echo synthetic-managed-config"}]}]
  }
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := writeRunSettings(dir, []string{"pyred", "hook", "claude"}, time.Minute, []string{"mcp__pyre__inbox"}, overlay)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	var s struct {
		Env               map[string]string `json:"env"`
		CleanupPeriodDays int               `json:"cleanupPeriodDays"`
		Permissions       struct {
			Allow       []string `json:"allow"`
			Deny        []string `json:"deny"`
			DefaultMode string   `json:"defaultMode"`
		} `json:"permissions"`
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	if s.Env["SYNTHETIC_MANAGED"] != "p3" || s.CleanupPeriodDays != 45 {
		t.Fatalf("managed values missing: %s", b)
	}
	if strings.Join(s.Permissions.Allow, ",") != "mcp__pyre__inbox,Bash(ls:*)" || strings.Join(s.Permissions.Deny, ",") != "Bash(rm:*)" || s.Permissions.DefaultMode != "default" {
		t.Fatalf("permissions = %+v", s.Permissions)
	}
	ss := s.Hooks["SessionStart"]
	if len(ss) != 2 || !strings.Contains(ss[0].Hooks[0].Command, "hook claude SessionStart") || ss[1].Hooks[0].Command != "echo synthetic-managed-start" {
		t.Fatalf("SessionStart groups = %+v", ss)
	}
	if cc := s.Hooks["ConfigChange"]; len(cc) != 1 || cc[0].Hooks[0].Command != "echo synthetic-managed-config" {
		t.Fatalf("ConfigChange = %+v", cc)
	}
	for _, ev := range HookEvents {
		if len(s.Hooks[ev]) == 0 || !strings.Contains(s.Hooks[ev][0].Hooks[0].Command, "hook claude "+ev) {
			t.Fatalf("Pyre hook for %s lost: %+v", ev, s.Hooks[ev])
		}
	}
}

func TestManagedSettingsRefusals(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"disable-hooks": `{"disableAllHooks": true}`,
		"not-object":    `[1,2]`,
		"bad-hooks":     `{"hooks": "x"}`,
		"bad-hook-list": `{"hooks": {"Stop": {"x": 1}}}`,
	} {
		ov := filepath.Join(t.TempDir(), name+".json")
		_ = os.WriteFile(ov, []byte(body), 0o600)
		if _, err := writeRunSettings(dir, []string{"pyred", "hook", "claude"}, time.Minute, nil, ov); err == nil {
			t.Errorf("%s: merged, want refusal", name)
		}
	}
	if _, err := writeRunSettings(dir, []string{"pyred", "hook", "claude"}, time.Minute, nil, filepath.Join(dir, "missing.json")); err == nil {
		t.Error("missing overlay accepted")
	}
}

// The adapter takes the generation's --settings from ResolvedLaunch.Args,
// merges it, and passes exactly one --settings (its own file) to Claude.
func TestLaunchMergesManagedSettingsArg(t *testing.T) {
	f := newFakeEnv(t, nil)
	overlay := filepath.Join(f.dir, "gen-settings.json")
	if err := os.WriteFile(overlay, []byte(`{"env":{"SYNTHETIC_MANAGED":"yes"},"hooks":{"Stop":[{"hooks":[{"type":"command","command":"true"}]}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(f.dir, "work")
	_ = os.MkdirAll(work, 0o755)
	launch := func(runID string, args []string) (contracts.RunHandle, error) {
		return f.a.Launch(context.Background(), contracts.ResolvedLaunch{
			RunID: runID, SessionID: "ses_t", Harness: contracts.HarnessClaude, Cwd: work, Args: args,
			Env: map[string]string{"PYRE_TEST_ROLE": "fakeclaude", "PYRE_SESSION_ID": "ses_t"}, StateDir: filepath.Join(f.dir, "runs", runID),
			ProfileDir: filepath.Join(f.dir, "cfg"),
		})
	}
	h, err := launch("run_managed", []string{"--settings=" + overlay, "--mcp-config=/nonexistent/synthetic.json"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(h.StateDir, fileHooks))
	var rs struct {
		Hooks map[string][]any `json:"hooks"`
	}
	_ = json.Unmarshal(b, &rs)
	if !strings.Contains(string(b), "SYNTHETIC_MANAGED") || len(rs.Hooks["Stop"]) != 2 || len(rs.Hooks["SessionStart"]) != 1 {
		t.Fatalf("run settings did not merge the overlay: %s", b)
	}
	m, _ := os.ReadFile(filepath.Join(h.StateDir, fileRun))
	if !strings.Contains(string(m), overlay) {
		t.Fatalf("run.json does not record the managed settings: %s", m)
	}
	if _, err := launch("run_two", []string{"--settings=" + overlay, "--settings=" + overlay}); err == nil {
		t.Fatal("two managed --settings accepted")
	}
	if _, err := launch("run_rel", []string{"--settings=relative.json"}); err == nil {
		t.Fatal("relative managed --settings accepted")
	}
}
