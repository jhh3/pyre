package sqlitex

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

func TestIdempotent(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Migrate(ctx, db, OperationsMigration); err != nil {
		t.Fatal(err)
	}
	p := contracts.Principal{Kind: contracts.PrincipalOwner, ID: "cli_x"}
	type req struct {
		contracts.Mutation
		V string
	}
	calls := 0
	run := func(r req) (string, bool, error) {
		var out string
		var replayed bool
		err := Tx(ctx, db, func(tx *sql.Tx) error {
			var e error
			replayed, e = Idempotent(ctx, tx, r.OperationID, "t.op", p, r, &out, func() (any, error) {
				calls++
				return "result-" + r.V, nil
			})
			return e
		})
		return out, replayed, err
	}
	out, rep, err := run(req{contracts.Mutation{OperationID: "op_1"}, "a"})
	if err != nil || rep || out != "result-a" {
		t.Fatalf("first: %v %v %q", err, rep, out)
	}
	out, rep, err = run(req{contracts.Mutation{OperationID: "op_1"}, "a"})
	if err != nil || !rep || out != "result-a" || calls != 1 {
		t.Fatalf("replay: %v %v %q calls=%d", err, rep, out, calls)
	}
	_, _, err = run(req{contracts.Mutation{OperationID: "op_1"}, "b"})
	if !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("want conflict, got %v", err)
	}
}

type keepErr struct{ err *contracts.Error }

func (k keepErr) Error() string    { return k.err.Error() }
func (k keepErr) Unwrap() error    { return k.err }
func (k keepErr) KeepWrites() bool { return true }

func TestIdempotentRejections(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Migrate(ctx, db, OperationsMigration); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE w (v TEXT)`); err != nil {
		t.Fatal(err)
	}
	rows := func() int {
		var n int
		db.QueryRow(`SELECT COUNT(*) FROM w`).Scan(&n)
		return n
	}
	p := contracts.Principal{Kind: contracts.PrincipalOwner, ID: "cli_x"}
	calls := 0
	run := func(opID string, fnErr error) (bool, error) {
		var replayed bool
		err := Tx(ctx, db, func(tx *sql.Tx) error {
			var e error
			replayed, e = Idempotent(ctx, tx, opID, "t.op", p, opID, nil, func() (any, error) {
				calls++
				if _, err := tx.Exec(`INSERT INTO w VALUES ('x')`); err != nil {
					return nil, err
				}
				return nil, fnErr
			})
			return e
		})
		return replayed, err
	}

	// A plain rejection is stored even though the caller's Tx sees an
	// error: Tx commits the stored rejection, fn's partial writes are
	// discarded, and the caller gets the bare *contracts.Error.
	rep, err := run("op_invalid", contracts.Errorf(contracts.ErrInvalid, "bad"))
	if _, ok := err.(*contracts.Error); !ok || !contracts.IsCode(err, contracts.ErrInvalid) || rep || rows() != 0 {
		t.Fatalf("first rejection: %T %v rep=%v rows=%d", err, err, rep, rows())
	}
	rep, err = run("op_invalid", nil)
	if !contracts.IsCode(err, contracts.ErrInvalid) || !rep || calls != 1 || rows() != 0 {
		t.Fatalf("replayed rejection: %v rep=%v calls=%d rows=%d", err, rep, calls, rows())
	}

	// Retryable errors are never stored: a retry re-executes.
	for _, code := range []contracts.ErrorCode{contracts.ErrCapacityExhausted, contracts.ErrUnavailable} {
		calls = 0
		op := "op_" + string(code)
		if _, err := run(op, contracts.Errorf(code, "later")); !contracts.IsCode(err, code) {
			t.Fatalf("%s: %v", code, err)
		}
		rep, err := run(op, nil)
		if err != nil || rep || calls != 2 {
			t.Fatalf("%s retry: %v rep=%v calls=%d", code, err, rep, calls)
		}
	}
	// So is a conflict explicitly marked retryable.
	calls = 0
	retryConflict := contracts.Errorf(contracts.ErrConflict, "not yet")
	retryConflict.Retryable = true
	run("op_retry_conflict", retryConflict)
	if _, err := run("op_retry_conflict", nil); err != nil || calls != 2 {
		t.Fatalf("retryable conflict retry: %v calls=%d", err, calls)
	}

	// A rejection that asks to keep its writes commits them with it.
	before := rows()
	if _, err := run("op_keep", keepErr{contracts.Errorf(contracts.ErrConflict, "kept")}); !contracts.IsCode(err, contracts.ErrConflict) || rows() != before+1 {
		t.Fatalf("keep writes: %v rows=%d", err, rows())
	}

	// A legacy stored retryable error is discarded and the operation re-run.
	if _, err := db.Exec(`INSERT INTO operations(operation_id,principal,op,body_digest,error,created_at) VALUES(?,?,?,?,?,?)`,
		"op_legacy", "owner:cli_x", "t.op", mustDigest(t, "op_legacy"), `{"code":"capacity_exhausted","message":"old","retryable":true}`, "2026-01-01T00:00:00.000000000Z"); err != nil {
		t.Fatal(err)
	}
	calls = 0
	if rep, err := run("op_legacy", nil); err != nil || rep || calls != 1 {
		t.Fatalf("legacy retryable: %v rep=%v calls=%d", err, rep, calls)
	}
}

func mustDigest(t *testing.T, v any) string {
	t.Helper()
	d, err := RequestDigest(v)
	if err != nil {
		t.Fatal(err)
	}
	return d
}
