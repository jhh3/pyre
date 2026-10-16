package hub

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jhh3/pyre/internal/persistence"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// Retention of operational history (W-dbhygiene).
//
// Production (2026-10-08): hub.db had grown to 5.5GB, commands 2.9GB in 159k
// rows (transcript reads for previews kept 7 days with their results).
// Finished commands were pruned hourly by one DELETE whose filter had no
// index: a full scan of the table, through the overflow pages of the large
// payload column stored before state, under the write lock. Now:
//
//   - finished commands (done, expired, cancelled) are found through the
//     partial index commands_finished and deleted after CommandRetention
//     (7 days) in transactions of at most sqlitex.PruneBatch rows;
//   - after CommandCompactAfter (1 day) a finished command's large payload
//     (> compactMinBytes) is replaced by "{}" and its large result dropped,
//     walking the same index from a persisted watermark so each row is
//     visited once;
//   - operation records are pruned through operations_created.

// retentionMigrations add the indexes retention needs. A separate
// namespace keeps the core hub migration registry untouched. Creating them
// scans the tables once (at the first start of this version).
var retentionMigrations = []sqlitex.Migration{{Namespace: "hub_retention", Version: 1, Name: "retention_indexes", SQL: `
CREATE INDEX IF NOT EXISTS commands_finished ON commands(COALESCE(NULLIF(completed_at,''), issued_at)) WHERE state IN ('done','expired','cancelled');
CREATE INDEX IF NOT EXISTS operations_created ON operations(created_at);
`}}

// finishedAt is the indexed completion time expression; finishedWhere must
// match the partial index's WHERE literally for SQLite to use it.
const (
	finishedAt    = `COALESCE(NULLIF(completed_at,''), issued_at)`
	finishedWhere = `state IN ('done','expired','cancelled')`
)

// compactMinBytes: payloads and results at most this large are kept.
const compactMinBytes = 1024

// compactWatermarkKey (hub_meta) is the completion time through which
// finished commands were compacted.
const compactWatermarkKey = "commands_compacted_through"

// RetentionStats reports one retention pass.
type RetentionStats struct {
	CommandsDeleted   int64
	CommandsCompacted int64
	OperationsDeleted int64
	EventsDeleted     int64
	AuditDeleted      int64
	ReportDedup       int64
}

// pruneCommands deletes finished commands completed before cut.
func (h *Hub) pruneCommands(ctx context.Context, cut time.Time) (int64, error) {
	c := persistence.Time(cut)
	return sqlitex.Batched(ctx, h.db, sqlitex.PruneBatch, 0, func(tx *sql.Tx, limit int) (int64, error) {
		res, err := tx.ExecContext(ctx, `DELETE FROM commands WHERE rowid IN (SELECT rowid FROM commands INDEXED BY commands_finished
			WHERE `+finishedWhere+` AND `+finishedAt+` < ? ORDER BY `+finishedAt+` LIMIT ?)`, c, limit)
		if err != nil {
			return 0, err
		}
		return res.RowsAffected()
	})
}

// compactCommands drops large payloads/results of finished commands
// completed before cut, from the persisted watermark on.
func (h *Hub) compactCommands(ctx context.Context, cut time.Time) (int64, error) {
	c := persistence.Time(cut)
	return sqlitex.Batched(ctx, h.db, sqlitex.PruneBatch, 0, func(tx *sql.Tx, limit int) (int64, error) {
		var from string
		err := tx.QueryRowContext(ctx, `SELECT value FROM hub_meta WHERE key=?`, compactWatermarkKey).Scan(&from)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		rows, err := tx.QueryContext(ctx, `SELECT rowid, `+finishedAt+` FROM commands INDEXED BY commands_finished
			WHERE `+finishedWhere+` AND `+finishedAt+` >= ? AND `+finishedAt+` < ? ORDER BY `+finishedAt+` LIMIT ?`, from, c, limit)
		if err != nil {
			return 0, err
		}
		var ids []int64
		last := from
		for rows.Next() {
			var id int64
			var at string
			if err := rows.Scan(&id, &at); err != nil {
				rows.Close()
				return 0, err
			}
			ids = append(ids, id)
			last = at
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return 0, err
		}
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `UPDATE commands SET
				payload = CASE WHEN length(CAST(payload AS BLOB)) > ? THEN '{}' ELSE payload END,
				result = CASE WHEN length(CAST(result AS BLOB)) > ? THEN NULL ELSE result END
				WHERE rowid=?`, compactMinBytes, compactMinBytes, id); err != nil {
				return 0, err
			}
		}
		if last != from {
			// Rows completing at exactly `last` are revisited next time
			// (>=): harmless, and nothing at that instant is skipped.
			if _, err := tx.ExecContext(ctx, `INSERT INTO hub_meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, compactWatermarkKey, last); err != nil {
				return 0, err
			}
		}
		if len(ids) == limit && last == from {
			// A full batch at one instant: advance past it to make progress.
			if _, err := tx.ExecContext(ctx, `INSERT INTO hub_meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, compactWatermarkKey, last+"\x00"); err != nil {
				return 0, err
			}
		}
		return int64(len(ids)), nil
	})
}

// RunRetention runs every retention pass now, regardless of PruneInterval
// (pyre-hub compact; maintenance runs it hourly through pruneHistory).
func (h *Hub) RunRetention(ctx context.Context) (RetentionStats, error) {
	var st RetentionStats
	var errs []error
	var err error
	now := h.clock.Now()
	if st.CommandsDeleted, err = h.pruneCommands(ctx, now.Add(-h.opt.CommandRetention)); err != nil {
		errs = append(errs, err)
	}
	if st.CommandsCompacted, err = h.compactCommands(ctx, now.Add(-h.opt.CommandCompactAfter)); err != nil {
		errs = append(errs, err)
	}
	// Idempotency records are stamped with the wall clock (sqlitex).
	if st.OperationsDeleted, err = sqlitex.PruneOperations(ctx, h.db, time.Now().Add(-h.opt.OperationRetention), sqlitex.PruneBatch); err != nil {
		errs = append(errs, err)
	}
	if st.EventsDeleted, err = h.events.Prune(ctx); err != nil {
		errs = append(errs, err)
	}
	if st.AuditDeleted, err = h.pruneAuditN(ctx); err != nil { // W-audit
		errs = append(errs, err)
	}
	if st.ReportDedup, err = h.pruneReportDedupN(ctx); err != nil {
		errs = append(errs, err)
	}
	return st, errors.Join(errs...)
}
