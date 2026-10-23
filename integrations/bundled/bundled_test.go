package bundled

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, p string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), mode); err != nil {
		t.Fatal(err)
	}
}

func TestPathFrom(t *testing.T) {
	prefix := t.TempDir()
	exe := filepath.Join(prefix, "v1", "bin", "pyred")
	write(t, exe, 0o755)
	if err := os.Symlink("v1", filepath.Join(prefix, "current")); err != nil {
		t.Fatal(err)
	}
	viaCurrent := filepath.Join(prefix, "current", "bin", "pyred")
	outfit := filepath.Join(prefix, "tools", "outfit", "bin", "outfit")
	write(t, outfit, 0o755)
	write(t, filepath.Join(prefix, "tools", "rota", "bin", "rota"), 0o644) // not executable

	if got := PathFrom(exe, "outfit"); got != "" {
		t.Fatalf("without the marker: got %q, want none", got)
	}
	write(t, filepath.Join(prefix, "tools", Marker), 0o644)
	want, _ := filepath.EvalSymlinks(outfit)
	if got := PathFrom(viaCurrent, "outfit"); got != want {
		t.Fatalf("outfit: got %q, want %q", got, want)
	}
	for _, tool := range []string{"rota", "hive", "", "../outfit"} {
		if got := PathFrom(exe, tool); got != "" {
			t.Fatalf("%q: got %q, want none", tool, got)
		}
	}
}

// install.ps1 -WithTools: <prefix>\tools\outfit\bin\outfit.exe, found
// without an exec bit (Windows has none).
func TestPathFromWindows(t *testing.T) {
	old := goos
	goos = "windows"
	t.Cleanup(func() { goos = old })
	// PathFrom resolves symlinks (macOS's temp dir is under /var ->
	// /private/var), so compare against the resolved prefix.
	prefix, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(prefix, "v1", "bin", "pyred.exe")
	write(t, exe, 0o644)
	write(t, filepath.Join(prefix, "tools", Marker), 0o644)
	if got := PathFrom(exe, "outfit"); got != "" {
		t.Fatalf("no outfit.exe yet: got %q", got)
	}
	outfit := filepath.Join(prefix, "tools", "outfit", "bin", "outfit.exe")
	write(t, outfit, 0o644)
	if got := PathFrom(exe, "outfit"); got != outfit {
		t.Fatalf("outfit: got %q, want %q", got, outfit)
	}
}
