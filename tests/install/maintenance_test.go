package install_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// upgrade.sh sets a short maintenance window before it restarts services
// and clears it after a healthy restart; a longer window set by someone
// else is left alone.
func TestUpgradeSetsAndClearsItsMaintenanceWindow(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("install scripts target macOS and Linux")
	}
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	s := scripts(t)
	install, upgrade := filepath.Join(s, "install.sh"), filepath.Join(s, "upgrade.sh")
	common := []string{"--home", home, "--allow-unsigned"}
	run(t, true, install, append([]string{"--from", fakeRelease(t, dir, "v1")}, common...)...)
	maint := filepath.Join(home, ".local", "state", "pyre", "maintenance.json")

	out := run(t, true, upgrade, append([]string{"--from", fakeRelease(t, dir, "v2"), "--service", "node", "--skip-hub-check"}, common...)...)
	if !strings.Contains(out, "maintenance window until") || !strings.Contains(out, "maintenance window cleared") {
		t.Fatalf("want the window set before the restart and cleared after it:\n%s", out)
	}
	if _, err := os.Stat(maint); !os.IsNotExist(err) {
		t.Fatalf("maintenance.json left after a healthy upgrade: %v", err)
	}

	// The owner's own longer window survives an upgrade.
	long := `{"until":"2099-01-01T00:00:00Z","reason":"owner","set_at":"2026-10-22T18:00:00Z"}` + "\n"
	if err := os.MkdirAll(filepath.Dir(maint), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(maint, []byte(long), 0o644); err != nil {
		t.Fatal(err)
	}
	out = run(t, true, upgrade, append([]string{"--from", fakeRelease(t, dir, "v3"), "--service", "node", "--skip-hub-check"}, common...)...)
	if b, err := os.ReadFile(maint); err != nil || string(b) != long {
		t.Fatalf("the owner's window was changed: %q, %v\n%s", b, err, out)
	}
}
