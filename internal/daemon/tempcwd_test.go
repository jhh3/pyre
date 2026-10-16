package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

// A resumed session's scratch working dir under a temp root is recreated;
// missing dirs elsewhere are not.
func TestRecreateTempDir(t *testing.T) {
	base := t.TempDir()
	tmp := filepath.Join(base, "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	old := tempRoots
	tempRoots = []string{tmp}
	t.Cleanup(func() { tempRoots = old })

	scratch := filepath.Join(tmp, "claude-1000", "proj", "scratchpad", "monitor")
	if !recreateTempDir(scratch) {
		t.Fatal("temp scratch dir not recreated")
	}
	if fi, err := os.Stat(scratch); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("recreated dir: %v %v", fi, err)
	}
	if recreateTempDir(scratch) {
		t.Fatal("an existing dir is not recreated")
	}
	if recreateTempDir(filepath.Join(base, "code", "repo")) || recreateTempDir(tmp) {
		t.Fatal("only missing dirs strictly inside a temp root are recreated")
	}
}
