package sqlitex

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openT(t *testing.T) *sql.DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE t (x INTEGER)`); err != nil {
		t.Fatal(err)
	}
	return db
}

// W-dbhygiene: a write transaction opened while the same goroutine already
// holds one on the same database could only wait for busy_timeout (the
// 2026-10-08 hub outage). It fails at once with ErrNestedTx instead.
func TestNestedTxFailsFast(t *testing.T) {
	db := openT(t)
	ctx := context.Background()
	t0 := time.Now()
	err := Tx(ctx, db, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO t VALUES (1)`); err != nil {
			return err
		}
		return Tx(ctx, db, func(*sql.Tx) error { return nil })
	})
	if !errors.Is(err, ErrNestedTx) {
		t.Fatalf("nested Tx: %v, want ErrNestedTx", err)
	}
	if d := time.Since(t0); d > time.Second {
		t.Fatalf("nested Tx took %v", d)
	}
	// The outer transaction rolled back and the lock is free again.
	if err := Tx(ctx, db, func(tx *sql.Tx) error { _, err := tx.Exec(`INSERT INTO t VALUES (2)`); return err }); err != nil {
		t.Fatal(err)
	}
	// Another database in the same goroutine is fine.
	other := openT(t)
	if err := Tx(ctx, db, func(*sql.Tx) error {
		return Tx(ctx, other, func(tx *sql.Tx) error { _, err := tx.Exec(`INSERT INTO t VALUES (3)`); return err })
	}); err != nil {
		t.Fatal(err)
	}
	// So is a transaction in another goroutine (it waits for the lock).
	done := make(chan error, 1)
	if err := Tx(ctx, db, func(*sql.Tx) error {
		go func() { done <- Tx(ctx, db, func(*sql.Tx) error { return nil }) }()
		time.Sleep(20 * time.Millisecond)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestSlowTxLogged(t *testing.T) {
	db := openT(t)
	var buf bytes.Buffer
	SetLogger(slog.New(slog.NewTextHandler(&buf, nil)))
	SetSlowTxThreshold(20 * time.Millisecond)
	t.Cleanup(func() { SetLogger(nil); SetSlowTxThreshold(0) })
	if err := Tx(context.Background(), db, func(*sql.Tx) error { time.Sleep(40 * time.Millisecond); return nil }); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "write transaction held the lock") || !strings.Contains(out, "TestSlowTxLogged") {
		t.Fatalf("slow tx log: %q", out)
	}
}

func TestBatchedShortTransactions(t *testing.T) {
	db := openT(t)
	ctx := context.Background()
	if err := Tx(ctx, db, func(tx *sql.Tx) error {
		for i := 0; i < 1234; i++ {
			if _, err := tx.Exec(`INSERT INTO t VALUES (?)`, i); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	n, err := Batched(ctx, db, 500, 0, func(tx *sql.Tx, limit int) (int64, error) {
		calls++
		res, err := tx.Exec(`DELETE FROM t WHERE rowid IN (SELECT rowid FROM t LIMIT ?)`, limit)
		if err != nil {
			return 0, err
		}
		return res.RowsAffected()
	})
	if err != nil || n != 1234 || calls != 3 {
		t.Fatalf("Batched: n=%d calls=%d err=%v", n, calls, err)
	}
}
