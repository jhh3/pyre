package sqlitex

import (
	"context"
	"path/filepath"
	"testing"
)

func TestMigrateIdempotentAndDowngradeGuard(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "t.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m1 := Migration{Namespace: "a", Version: 1, Name: "init", SQL: `CREATE TABLE x(id TEXT PRIMARY KEY); CREATE TABLE y(id TEXT);`}
	m2 := Migration{Namespace: "a", Version: 2, Name: "col", SQL: `ALTER TABLE x ADD COLUMN v INTEGER`}
	if err := Migrate(ctx, db, m1, m2); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, db, m1, m2); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if err := Migrate(ctx, db, m1); err == nil {
		t.Fatal("expected downgrade refusal")
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO x(id,v) VALUES('a',1)`); err != nil {
		t.Fatal(err)
	}
	if err := Backup(ctx, db, filepath.Join(t.TempDir(), "b.db")); err != nil {
		t.Fatal(err)
	}
}

func TestDurabilityPragmas(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var sync int
	if err := db.QueryRow(`PRAGMA synchronous`).Scan(&sync); err != nil || sync != 2 {
		t.Fatalf("synchronous = %d (want 2=FULL): %v", sync, err)
	}
	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q: %v", mode, err)
	}
}
