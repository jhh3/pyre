//go:build !windows

package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/hub/assemble"
	"github.com/jhh3/pyre/internal/persistence"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// W-dbhygiene: compact refuses while the data directory is locked (a hub
// serving it), and otherwise runs retention to completion and VACUUMs.
func TestCompact(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hub")
	if code, _, errs := runCmd(t, "init", "--data", dir, "--hub-url", "https://hub.synth.invalid:7443"); code != 0 {
		t.Fatalf("init: %s", errs)
	}
	// Synthetic bloat: finished transcript reads 8 days old.
	db, err := sqlitex.Open(filepath.Join(dir, assemble.DBFile))
	if err != nil {
		t.Fatal(err)
	}
	old := persistence.Time(time.Now().Add(-8 * 24 * time.Hour))
	big := strings.Repeat("t", 64<<10)
	for i := 0; i < 300; i++ {
		if _, err := db.Exec(`INSERT INTO commands(command_id,node_id,seq,kind,actor,issued_at,payload,state,completed_at,result) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			"cmd_synthcompact"+string(rune('a'+i%26))+strings.Repeat("x", i/26), "nod_synth", i+1, "read_transcript", "owner:owner", old, "{}", "done", old, big); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	unlock, err := lockDataDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if code, _, errs := runCmd(t, "compact", "--data", dir); code != 1 || !strings.Contains(errs, "in use") {
		t.Fatalf("compact with the lock held: %d %s", code, errs)
	}
	unlock()

	code, out, errs := runCmd(t, "compact", "--data", dir, "--dry-run")
	if code != 0 || !strings.Contains(out, "read_transcript/done") {
		t.Fatalf("dry run: %d %s %s", code, out, errs)
	}
	code, out, errs = runCmd(t, "compact", "--data", dir)
	if code != 0 || !strings.Contains(out, "300 finished commands deleted") || !strings.Contains(out, "->") {
		t.Fatalf("compact: %d %s %s", code, out, errs)
	}
	db, err = sqlitex.Open(filepath.Join(dir, assemble.DBFile))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM commands`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("commands left: %d %v", n, err)
	}
}
