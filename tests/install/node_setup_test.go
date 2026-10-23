package install_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Node-setup regressions found while adding office-box2: install.sh value
// flags in --flag=value form, the Claude bypass acknowledgement, and the
// outfit-core command the Outfit plugin runs.

func nodeUnit(t *testing.T, home string) string {
	t.Helper()
	p := filepath.Join(home, ".config", "systemd", "user", "pyred.service")
	if runtime.GOOS == "darwin" {
		p = filepath.Join(home, "Library", "LaunchAgents", "dev.jhh3.pyre.pyred.plist")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("node unit: %v", err)
	}
	return string(b)
}

func readSettings(t *testing.T, home string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]any{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("settings.json is not JSON: %v\n%s", err, b)
	}
	return m
}

func TestInstallAcceptsEqualsFormAndRecordsBypass(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("install scripts target macOS and Linux")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 merges settings.json")
	}
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The owner's existing settings survive the merge.
	existing := `{"model": "opus", "enabledPlugins": {"outfit@synthetic": true}, "env": {"SYNTHETIC": "1"}}`
	settings := filepath.Join(home, ".claude", "settings.json")
	if err := os.WriteFile(settings, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	install := filepath.Join(scripts(t), "install.sh")
	out := run(t, true, install, "--from", fakeRelease(t, dir, "v1"), "--home", home, "--allow-unsigned", "--service", "node",
		"--pyred-arg=--trust-dir=/synthetic/trust", "--pyred-arg", "--permissions", "--pyred-arg", "bypass", "--name=synth-office2")
	unit := nodeUnit(t, home)
	for _, want := range []string{"--trust-dir=/synthetic/trust", "--permissions", "bypass", "synth-office2"} {
		if !strings.Contains(unit, want) {
			t.Fatalf("unit lacks %q:\n%s\n%s", want, unit, out)
		}
	}
	m := readSettings(t, home)
	if m["skipDangerousModePermissionPrompt"] != true || m["model"] != "opus" || m["enabledPlugins"] == nil || m["env"] == nil {
		t.Fatalf("settings after merge: %v\n%s", m, out)
	}
	if fi, _ := os.Stat(settings); fi.Mode().Perm() != 0o600 {
		t.Fatalf("settings mode changed to %v", fi.Mode().Perm())
	}
	// Re-running is a no-op note.
	out = run(t, true, install, "--from", fakeRelease(t, dir, "v1"), "--home", home, "--allow-unsigned", "--service", "node", "--permissions", "bypass")
	if !strings.Contains(out, "already accepted") {
		t.Fatalf("second run: %s", out)
	}

	// Invalid JSON is never overwritten; no settings file is created from nothing without bypass.
	home2 := filepath.Join(dir, "home2")
	os.MkdirAll(filepath.Join(home2, ".claude"), 0o755)
	os.WriteFile(filepath.Join(home2, ".claude", "settings.json"), []byte("{not json"), 0o600)
	out = run(t, true, install, "--from", fakeRelease(t, dir, "v1"), "--home", home2, "--allow-unsigned", "--service", "node", "--pyred-arg=--permissions=bypass")
	if b, _ := os.ReadFile(filepath.Join(home2, ".claude", "settings.json")); string(b) != "{not json" || !strings.Contains(out, "left unchanged") {
		t.Fatalf("invalid settings touched: %q\n%s", b, out)
	}
	home3 := filepath.Join(dir, "home3")
	os.MkdirAll(home3, 0o755)
	run(t, true, install, "--from", fakeRelease(t, dir, "v1"), "--home", home3, "--allow-unsigned", "--service", "node", "--permissions=ask")
	if _, err := os.Stat(filepath.Join(home3, ".claude", "settings.json")); !os.IsNotExist(err) {
		t.Fatal("settings.json written without bypass")
	}
	// Bypass with no settings yet creates a private one.
	home4 := filepath.Join(dir, "home4")
	os.MkdirAll(home4, 0o755)
	run(t, true, install, "--from", fakeRelease(t, dir, "v1"), "--home", home4, "--allow-unsigned", "--service", "node", "--permissions", "bypass")
	if m := readSettings(t, home4); m["skipDangerousModePermissionPrompt"] != true {
		t.Fatalf("fresh settings: %v", m)
	}
	run(t, false, install, "--from", fakeRelease(t, dir, "v1"), "--home", home4, "--allow-unsigned", "--permissions", "sometimes")
}

// --with-tools links outfit-core (the Outfit plugin's MCP server and hook
// command) to the bundled outfit, unless one is installed; uninstall removes it.
func TestInstallWithToolsLinksOutfitCore(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("install scripts target macOS and Linux")
	}
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	os.MkdirAll(home, 0o755)
	tools := filepath.Join(dir, "pyre-tools-v1-test")
	for _, tool := range []string{"outfit", "hive", "rota"} {
		os.MkdirAll(filepath.Join(tools, tool, "bin"), 0o755)
		if err := os.WriteFile(filepath.Join(tools, tool, "bin", tool), []byte("#!/bin/sh\necho synthetic-"+tool+" \"$@\"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(tools, "VERSION"), []byte("v1\n"), 0o644)
	s := scripts(t)
	out := run(t, true, filepath.Join(s, "install.sh"), "--from", fakeRelease(t, dir, "v1"), "--home", home, "--allow-unsigned", "--tools-from="+tools)
	shim := filepath.Join(home, ".local", "bin", "outfit-core")
	if l, _ := os.Readlink(shim); l != "../share/pyre/tools/outfit/bin/outfit" {
		t.Fatalf("outfit-core shim = %q\n%s", l, out)
	}
	if got, err := exec.Command(shim, "mcp").Output(); err != nil || strings.TrimSpace(string(got)) != "synthetic-outfit mcp" {
		t.Fatalf("outfit-core runs %q (%v)", got, err)
	}
	run(t, true, filepath.Join(s, "uninstall.sh"), "--home", home)
	if _, err := os.Lstat(shim); !os.IsNotExist(err) {
		t.Fatal("uninstall left the outfit-core shim")
	}

	// An existing outfit-core is left alone.
	home2 := filepath.Join(dir, "home2")
	os.MkdirAll(filepath.Join(home2, ".local", "bin"), 0o755)
	own := filepath.Join(home2, ".local", "bin", "outfit-core")
	os.WriteFile(own, []byte("#!/bin/sh\necho own\n"), 0o755)
	out = run(t, true, filepath.Join(s, "install.sh"), "--from", fakeRelease(t, dir, "v1"), "--home", home2, "--allow-unsigned", "--tools-from", tools)
	if b, _ := os.ReadFile(own); string(b) != "#!/bin/sh\necho own\n" || !strings.Contains(out, "left untouched") {
		t.Fatalf("existing outfit-core replaced:\n%s", out)
	}
}
