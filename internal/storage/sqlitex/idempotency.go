package sqlitex

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// OperationsMigration creates the shared operation-idempotency table. Every
// hub domain that handles mutations includes it in its Migrate call (it is
// idempotent and namespaced "ops").
var OperationsMigration = Migration{Namespace: "ops", Version: 1, Name: "operations", SQL: `
CREATE TABLE IF NOT EXISTS operations (
	operation_id TEXT NOT NULL,
	principal TEXT NOT NULL,
	op TEXT NOT NULL,
	body_digest TEXT NOT NULL,
	response BLOB,
	error BLOB,
	created_at TEXT NOT NULL,
	PRIMARY KEY (operation_id)
);`}

// RequestDigest canonically hashes a request (JSON of the value with the
// Mutation.OperationID field included; same JSON => same digest).
func RequestDigest(req any) (string, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// StoredRejection is returned by Idempotent when fn's contract error was
// stored as the operation's result. The transaction must be COMMITTED so a
// retry replays the rejection: Tx does this (commits, then returns Err), as
// do svcutil.Mutate and persistence.Mutate. It unwraps to Err, so
// contracts.AsError / IsCode see the rejection.
type StoredRejection struct{ Err *contracts.Error }

func (s *StoredRejection) Error() string { return s.Err.Error() }
func (s *StoredRejection) Unwrap() error { return s.Err }

// WriteKeeper is implemented by errors whose fn writes must be kept along
// with the stored rejection (for example a document conflict that preserved
// the caller's proposal). Without it fn's writes are rolled back to a
// savepoint before the rejection is stored.
type WriteKeeper interface{ KeepWrites() bool }

func keepsWrites(err error) bool {
	var k WriteKeeper
	return errors.As(err, &k) && k.KeepWrites()
}

// storable reports whether a contract error is a durable answer for the
// operation. Retryable errors (capacity_exhausted, unavailable, or any error
// marked retryable) and internal errors are not: a retry must re-execute.
func storable(ce *contracts.Error) bool {
	return ce.Code != contracts.ErrInternal && ce.Code != contracts.ErrUnavailable && !ce.Retryable
}

// Idempotent executes fn at most once per operation ID inside tx.
//
//   - First call: fn runs inside a savepoint. A result is stored and decoded
//     into out. A non-retryable *contracts.Error is stored, fn's writes are
//     rolled back to the savepoint (unless the error implements WriteKeeper)
//     and a *StoredRejection is returned: the caller must commit.
//   - Retry with identical request: the stored result is decoded into out (or
//     the stored *contracts.Error returned) and replayed=true; fn does not run.
//   - Same operation ID with a different request (or different principal/op):
//     ErrConflict.
//   - Retryable contract errors, internal errors and non-contract errors are
//     returned unchanged and not stored (fn's writes are rolled back to the
//     savepoint unless the error implements WriteKeeper); a retry re-executes.
//     A stored retryable error left by an older version is discarded and the
//     operation re-executed.
func Idempotent(ctx context.Context, tx *sql.Tx, opID, op string, principal contracts.Principal, req any, out any, fn func() (any, error)) (replayed bool, err error) {
	if opID == "" {
		return false, contracts.Errorf(contracts.ErrInvalid, "operation_id is required")
	}
	digest, err := RequestDigest(req)
	if err != nil {
		return false, err
	}
	who := string(principal.Kind) + ":" + principal.ID
	var gotOp, gotWho, gotDigest string
	var resp, errb []byte
	row := tx.QueryRowContext(ctx, `SELECT op, principal, body_digest, response, error FROM operations WHERE operation_id=?`, opID)
	switch scanErr := row.Scan(&gotOp, &gotWho, &gotDigest, &resp, &errb); {
	case scanErr == nil:
		if gotOp != op || gotWho != who || gotDigest != digest {
			return false, contracts.Errorf(contracts.ErrConflict, "operation_id reused with a different request body").WithRef(contracts.KindOperation, opID)
		}
		if len(errb) > 0 {
			var ce contracts.Error
			if err := json.Unmarshal(errb, &ce); err != nil {
				return true, err
			}
			if storable(&ce) {
				return true, &ce
			}
			// Legacy row holding a retryable error: re-execute.
			if _, err := tx.ExecContext(ctx, `DELETE FROM operations WHERE operation_id=?`, opID); err != nil {
				return false, err
			}
			break
		}
		if out != nil && len(resp) > 0 {
			if err := json.Unmarshal(resp, out); err != nil {
				return true, err
			}
		}
		return true, nil
	case !errors.Is(scanErr, sql.ErrNoRows):
		return false, scanErr
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT pyre_idem`); err != nil {
		return false, err
	}
	result, fnErr := fn()
	if fnErr != nil {
		if !keepsWrites(fnErr) {
			if _, err := tx.ExecContext(ctx, `ROLLBACK TO pyre_idem`); err != nil {
				return false, err
			}
		}
		if _, err := tx.ExecContext(ctx, `RELEASE pyre_idem`); err != nil {
			return false, err
		}
		var ce *contracts.Error
		if !errors.As(fnErr, &ce) || !storable(ce) {
			return false, fnErr // not stored; a retry re-executes
		}
		eb, _ := json.Marshal(ce)
		if _, err := tx.ExecContext(ctx, `INSERT INTO operations(operation_id,principal,op,body_digest,error,created_at) VALUES(?,?,?,?,?,?)`,
			opID, who, op, digest, eb, FormatTime(time.Now())); err != nil {
			return false, err
		}
		return false, &StoredRejection{Err: ce}
	}
	if _, err := tx.ExecContext(ctx, `RELEASE pyre_idem`); err != nil {
		return false, err
	}
	rb, err := json.Marshal(result)
	if err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO operations(operation_id,principal,op,body_digest,response,created_at) VALUES(?,?,?,?,?,?)`,
		opID, who, op, digest, rb, FormatTime(time.Now())); err != nil {
		return false, err
	}
	if out != nil {
		if err := json.Unmarshal(rb, out); err != nil {
			return false, err
		}
	}
	return false, nil
}

// PruneOperations deletes operation records created before cutoff, in
// batches of at most batch rows, and returns how many were deleted. A
// retry of a pruned operation executes again, so callers keep records far
// longer than any client retries.
func PruneOperations(ctx context.Context, db *sql.DB, cutoff time.Time, batch int) (int64, error) {
	if batch <= 0 {
		batch = 5000
	}
	var total int64
	for i := 0; i < 100; i++ {
		res, err := db.ExecContext(ctx, `DELETE FROM operations WHERE rowid IN (SELECT rowid FROM operations WHERE created_at < ? LIMIT ?)`, FormatTime(cutoff), batch)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
		if n < int64(batch) {
			break
		}
	}
	return total, nil
}
