package svcutil_test

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/projects/svcutil"
	"github.com/jhh3/pyre/internal/projects/svcutil/svctest"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

func TestMutateCommitSemantics(t *testing.T) {
	db := svctest.OpenDB(t)
	if err := sqlitex.Migrate(t.Context(), db, sqlitex.OperationsMigration); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE t (v TEXT)`); err != nil {
		t.Fatal(err)
	}
	ctx := svctest.Owner()
	count := func() int {
		var n int
		db.QueryRow(`SELECT COUNT(*) FROM t`).Scan(&n)
		return n
	}
	write := func(tx *sql.Tx) error { _, err := tx.Exec(`INSERT INTO t VALUES ('x')`); return err }

	// Plain contract error: fn's writes rolled back, the rejection stored
	// and replayed (a retry does not re-execute).
	m := svctest.Op()
	err := svcutil.Mutate(ctx, db, m, "t.op", "req", nil, func(tx *sql.Tx, _ contracts.Principal) (any, error) {
		write(tx)
		return nil, contracts.Errorf(contracts.ErrInvalid, "nope")
	})
	if _, ok := err.(*contracts.Error); !ok || !contracts.IsCode(err, contracts.ErrInvalid) || count() != 0 {
		t.Fatalf("rejection: %T %v %d", err, err, count())
	}
	reran := false
	err = svcutil.Mutate(ctx, db, m, "t.op", "req", nil, func(tx *sql.Tx, _ contracts.Principal) (any, error) {
		reran = true
		return "ok", write(tx)
	})
	if !contracts.IsCode(err, contracts.ErrInvalid) || reran || count() != 0 {
		t.Fatalf("replayed rejection: %v reran=%v %d", err, reran, count())
	}

	// Retryable contract error: rolled back, not stored (retry re-executes).
	m = svctest.Op()
	err = svcutil.Mutate(ctx, db, m, "t.op", "req", nil, func(tx *sql.Tx, _ contracts.Principal) (any, error) {
		write(tx)
		return nil, contracts.Errorf(contracts.ErrCapacityExhausted, "full")
	})
	if !contracts.IsCode(err, contracts.ErrCapacityExhausted) || count() != 0 {
		t.Fatalf("retryable: %v %d", err, count())
	}
	var out string
	if err := svcutil.Mutate(ctx, db, m, "t.op", "req", &out, func(tx *sql.Tx, _ contracts.Principal) (any, error) {
		return "ok", write(tx)
	}); err != nil || out != "ok" || count() != 1 {
		t.Fatalf("retry after retryable error: %v %q %d", err, out, count())
	}

	// CommitWith: writes kept, error stored and replayed without re-running.
	m2 := svctest.Op()
	runs := 0
	fn := func(tx *sql.Tx, _ contracts.Principal) (any, error) {
		runs++
		write(tx)
		return nil, svcutil.CommitWith(contracts.Errorf(contracts.ErrConflict, "kept").WithDetail("k", "v"))
	}
	err = svcutil.Mutate(ctx, db, m2, "t.op", "req2", nil, fn)
	e, ok := err.(*contracts.Error)
	if !ok || e.Code != contracts.ErrConflict || count() != 2 {
		t.Fatalf("commit-with: %T %v %d", err, err, count())
	}
	err = svcutil.Mutate(ctx, db, m2, "t.op", "req2", nil, fn)
	if !contracts.IsCode(err, contracts.ErrConflict) || runs != 1 || count() != 2 {
		t.Fatalf("replay: %v runs=%d count=%d", err, runs, count())
	}

	err = svcutil.Mutate(t.Context(), db, svctest.Op(), "t.op", "r", nil, func(*sql.Tx, contracts.Principal) (any, error) { return nil, nil })
	if !contracts.IsCode(err, contracts.ErrUnauthorized) {
		t.Fatalf("no principal: %v", err)
	}
}

func TestHelpers(t *testing.T) {
	if got := svcutil.TruncateUTF8("héllo", 2); got != "h" {
		t.Fatalf("truncate: %q", got)
	}
	if got := svcutil.LikeEscape(`50%_\`); got != `%50\%\_\\%` {
		t.Fatalf("like: %q", got)
	}
	c := svcutil.EncodeCursor("prj_X")
	if k, err := svcutil.DecodeCursor(c); err != nil || k != "prj_X" {
		t.Fatalf("cursor: %v %q", err, k)
	}
	if _, err := svcutil.DecodeCursor("!!"); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("bad cursor: %v", err)
	}
	if svcutil.Limit(0) != 50 || svcutil.Limit(1000) != 200 {
		t.Fatal("limit clamp")
	}
	if f, _ := svcutil.ProjectFilter("p", nil); f != "" {
		t.Fatal("nil filter")
	}
	empty := ""
	if f, _ := svcutil.ProjectFilter("p", &empty); !strings.Contains(f, "IS NULL") {
		t.Fatal("standalone filter")
	}
}
