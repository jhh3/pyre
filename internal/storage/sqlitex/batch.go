package sqlitex

import (
	"context"
	"database/sql"
	"time"
)

// PruneBatch is the row limit of one retention transaction. Retention
// deletes and compactions run as many short write transactions instead of
// one long one, so a backlog (first run after an upgrade, a hub that was
// down) never holds the write lock long enough to fail other writers with
// SQLITE_BUSY (W-dbhygiene).
const PruneBatch = 500

// batchPause separates batches so waiting writers get the lock (SQLite's
// busy handler is not fair: a loop that commits and begins again at once
// can keep it).
var batchPause = 5 * time.Millisecond

// DefaultMaxBatches bounds one Batched call; the rest waits for the next
// maintenance pass.
const DefaultMaxBatches = 200

// Batched runs step in its own write transaction until it affects fewer
// than limit rows, ctx ends, or maxBatches transactions ran (0: default).
// It pauses briefly between transactions and returns the total affected.
func Batched(ctx context.Context, db *sql.DB, limit, maxBatches int, step func(tx *sql.Tx, limit int) (int64, error)) (int64, error) {
	if limit <= 0 {
		limit = PruneBatch
	}
	if maxBatches <= 0 {
		maxBatches = DefaultMaxBatches
	}
	var total int64
	for i := 0; i < maxBatches; i++ {
		if i > 0 {
			t := time.NewTimer(batchPause)
			select {
			case <-ctx.Done():
				t.Stop()
				return total, ctx.Err()
			case <-t.C:
			}
		}
		var n int64
		err := Tx(ctx, db, func(tx *sql.Tx) error {
			var err error
			n, err = step(tx, limit)
			return err
		})
		if err != nil {
			return total, err
		}
		total += n
		if n < int64(limit) {
			break
		}
	}
	return total, nil
}
