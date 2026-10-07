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
