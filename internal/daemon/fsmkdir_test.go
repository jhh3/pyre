package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

func TestMkdirUnderHome(t *testing.T) {
	home := t.TempDir()
	outside := t.TempDir()
	res, err := mkdirUnderHome(home, "~/a/b")
	if err != nil || !res.Created || res.Path != filepath.Join(home, "a", "b") {
		t.Fatalf("create: %+v %v", res, err)
	}
	if res, err = mkdirUnderHome(home, filepath.Join(home, "a", "b")); err != nil || res.Created {
		t.Fatalf("existing: %+v %v", res, err)
	}
	for _, p := range []string{filepath.Join(outside, "x"), "~/../escape", "relative/x", ""} {
		if _, err := mkdirUnderHome(home, p); err == nil {
			t.Errorf("%q accepted", p)
		}
	}
	if err := os.Symlink(outside, filepath.Join(home, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := mkdirUnderHome(home, "~/link/new"); !contracts.IsCode(err, contracts.ErrForbidden) {
		t.Fatalf("through a symlink out of home: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "new")); err == nil {
		t.Fatal("created outside home")
	}
}
