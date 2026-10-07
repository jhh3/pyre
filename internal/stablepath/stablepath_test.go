package stablepath

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOfPrefersCurrentLink(t *testing.T) {
	root := t.TempDir()
	ver := filepath.Join(root, "v0.1.0-rc9", "bin")
	if err := os.MkdirAll(ver, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(ver, "pyred")
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := Of(bin); got != bin {
		t.Fatalf("no current link: %s", got)
	}
	if err := os.Symlink("v0.1.0-rc9", filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	if got, want := Of(bin), filepath.Join(root, "current", "bin", "pyred"); got != want {
		t.Fatalf("got %s want %s", got, want)
	}
	other := filepath.Join(t.TempDir(), "pyred")
	_ = os.WriteFile(other, []byte("y"), 0o755)
	if got := Of(other); got != other {
		t.Fatalf("unrelated path changed: %s", got)
	}
}
