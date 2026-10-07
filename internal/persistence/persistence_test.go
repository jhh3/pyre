package persistence

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

func TestMutateStoresOnlyNonRetryableErrors(t *testing.T) {
	ctx := context.Background()
	db, err := sqlitex.Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := sqlitex.Migrate(ctx, db, sqlitex.OperationsMigration, sqlitex.Migration{Namespace: "t", Version: 1, Name: "t", SQL: `CREATE TABLE t(v TEXT)`}); err != nil {
		t.Fatal(err)
	}
	p := contracts.Principal{Kind: contracts.PrincipalOwner, ID: "cli_t"}
	type req struct {
		contracts.Mutation
		V string
	}
	count := func() int {
		var n int
		db.QueryRow(`SELECT COUNT(*) FROM t`).Scan(&n)
		return n
	}
	// Conflict: partial writes discarded, error stored and replayed.
	calls := 0
	fn := func(tx *sql.Tx) (any, error) {
		calls++
		tx.Exec(`INSERT INTO t VALUES('partial')`)
		return nil, contracts.Errorf(contracts.ErrConflict, "held")
	}
	var out string
	if _, err := Mutate(ctx, db, "op_1", "x", p, req{contracts.Mutation{OperationID: "op_1"}, "a"}, &out, fn); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatal(err)
	}
	rep, err := Mutate(ctx, db, "op_1", "x", p, req{contracts.Mutation{OperationID: "op_1"}, "a"}, &out, fn)
	if !contracts.IsCode(err, contracts.ErrConflict) || !rep || calls != 1 || count() != 0 {
		t.Fatalf("replay: %v rep=%v calls=%d rows=%d", err, rep, calls, count())
	}
	// Retryable: not stored, so the retry runs again.
	calls = 0
	busy := true
	fn2 := func(tx *sql.Tx) (any, error) {
		calls++
		if busy {
			return nil, contracts.Errorf(contracts.ErrCapacityExhausted, "full")
		}
		_, err := tx.Exec(`INSERT INTO t VALUES('ok')`)
		return "done", err
	}
	if _, err := Mutate(ctx, db, "op_2", "x", p, req{contracts.Mutation{OperationID: "op_2"}, "b"}, &out, fn2); !contracts.IsCode(err, contracts.ErrCapacityExhausted) {
		t.Fatal(err)
	}
	busy = false
	if _, err := Mutate(ctx, db, "op_2", "x", p, req{contracts.Mutation{OperationID: "op_2"}, "b"}, &out, fn2); err != nil || out != "done" || calls != 2 || count() != 1 {
		t.Fatalf("retry: %v %q calls=%d rows=%d", err, out, calls, count())
	}
}

func TestCursorRoundTripAndClamp(t *testing.T) {
	c := PageCursor{K: "2026", I: "ses_1", N: 7}
	got, err := DecodeCursor(EncodeCursor(c))
	if err != nil || got != c {
		t.Fatalf("%v %+v", err, got)
	}
	if _, err := DecodeCursor("!!!"); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatal("bad cursor accepted")
	}
	if ClampLimit(0, 50, 500) != 50 || ClampLimit(9999, 50, 500) != 500 || ClampLimit(3, 50, 500) != 3 {
		t.Fatal("clamp")
	}
	if WaitSeconds(120) != 60*time.Second || WaitSeconds(-1) != 0 {
		t.Fatal("wait clamp")
	}
}

func TestBroadcasterNoLostWakeup(t *testing.T) {
	var b Broadcaster
	c := b.C()
	b.Broadcast()
	select {
	case <-c:
	default:
		t.Fatal("not woken")
	}
	c2 := b.C()
	select {
	case <-c2:
		t.Fatal("stale wake")
	default:
	}
}
