package claudeplugin

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWriteModulePlugin(t *testing.T) {
	dir := t.TempDir()
	run := filepath.Join(dir, "run")
	if _, err := Write(filepath.Join(dir, "p"), Spec{Module: &Module{RunDir: run, HookArgv: []string{"/opt/pyre/pyred", "hook", "claude"}}}); err != nil {
		t.Fatal(err)
	}
	hj, _ := os.ReadFile(filepath.Join(dir, "p", "hooks", "hooks.json"))
	if !strings.Contains(string(hj), `"./register.ts"`) {
		t.Fatalf("hooks.json: %s", hj)
	}
	src, _ := os.ReadFile(filepath.Join(dir, "p", "hooks", "register.ts"))
	for _, want := range []string{`const RUN_DIR: string = "` + run + `"`, `["/opt/pyre/pyred","hook","claude"]`, "'PyrePluginClaim'", "$.prompt.submit({ text: msg.text })"} {
		if !strings.Contains(string(src), want) {
			t.Errorf("module lacks %q", want)
		}
	}
	if strings.Contains(string(src), "__") {
		t.Errorf("unreplaced placeholder in module")
	}
	if _, err := os.Stat(filepath.Join(dir, "p", "monitors")); err == nil {
		t.Errorf("module-only plugin has a monitor")
	}
}

func TestFormatLineSentTime(t *testing.T) {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skip(err)
	}
	sent := time.Date(2026, 10, 7, 15, 5, 0, 0, loc)
	line := formatLineAt([]Message{{ID: "msg_1", Sender: "session:planner", SentAt: sent}}, sent.Add(8*time.Hour+3*time.Minute), loc)
	if !strings.Contains(line, "(msg_1 from session:planner, sent 3:05 PM PT (8h ago))") {
		t.Fatalf("line: %s", line)
	}
}

// TestModuleUnderClaudePluginTest runs the generated module under Claude
// Code's own plugin test kit (no login, no session): PYRE_CLAUDE_PLUGIN_TEST=1.
func TestModuleUnderClaudePluginTest(t *testing.T) {
	if os.Getenv("PYRE_CLAUDE_PLUGIN_TEST") != "1" {
		t.Skip("set PYRE_CLAUDE_PLUGIN_TEST=1 (needs the claude binary)")
	}
	bin := os.Getenv("PYRE_CLAUDE_BIN")
	if bin == "" {
		bin = "claude"
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "pyre-session")
	if _, err := Write(p, Spec{Module: &Module{RunDir: "/synthetic/pyre-run", HookArgv: []string{"/synthetic/pyred", "hook", "claude"}}}); err != nil {
		t.Fatal(err)
	}
	ts, err := os.ReadFile("testdata/module.test.ts")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(p, "tests"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "tests", "module.test.ts"), ts, 0o600); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "CLAUDE_CONFIG_DIR="+filepath.Join(dir, "cfg"))
	for _, args := range [][]string{{"plugin", "validate", p}, {"plugin", "test", p}} {
		cmd := exec.Command(bin, args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		t.Logf("claude %s:\n%s", strings.Join(args, " "), out)
		if err != nil {
			t.Fatalf("claude %s: %v", strings.Join(args, " "), err)
		}
	}
}
