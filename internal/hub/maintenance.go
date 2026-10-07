package hub

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/persistence"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// RunMaintenance performs one pass of background upkeep: message expiry,
// approval expiry, event retention, node report dedup pruning, retention of
// finished commands and idempotency records, and handing queued commands
// to connected nodes. Start runs it periodically.
func (h *Hub) RunMaintenance(ctx context.Context) error {
	var errs []error
	if _, err := h.msgs.ExpireDue(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := h.tx(ctx, func(tx *sql.Tx) error {
		_, err := h.expireApprovalsTx(ctx, tx)
		return err
	}); err != nil {
		errs = append(errs, err)
	}
	if _, err := h.events.Prune(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := h.pruneReportDedup(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := h.pruneHistory(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := h.pruneChildReports(ctx); err != nil { // W-childnotify
		errs = append(errs, err)
	}
	if _, _, err := h.FoldTokenAccounts(ctx); err != nil { // accounts_merge.go
		errs = append(errs, err)
	}
	rows, err := h.db.QueryContext(ctx, `SELECT DISTINCT node_id FROM commands WHERE state=?`, cmdQueued)
	if err != nil {
		errs = append(errs, err)
	} else {
		var nodes []string
		for rows.Next() {
			var n string
			if rows.Scan(&n) == nil {
				nodes = append(nodes, n)
			}
		}
		rows.Close()
		for _, n := range nodes {
			h.flushNode(ctx, n)
		}
	}
	return errors.Join(errs...)
}

// PendingCommands returns the queued-but-unacknowledged commands for a node
// (diagnostics and tests).
func (h *Hub) PendingCommands(ctx context.Context, nodeID string) ([]contracts.NodeCommand, error) {
	rows, err := h.db.QueryContext(ctx, `SELECT `+cmdCols+` FROM commands WHERE node_id=? AND state IN (?,?) ORDER BY seq`, nodeID, cmdQueued, cmdSent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []contracts.NodeCommand
	for rows.Next() {
		r, err := scanCommand(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r.NodeCommand)
	}
	return out, rows.Err()
}

// pruneBatch bounds one DELETE so a large backlog (first run after an
// upgrade) never holds the write lock for long.
const pruneBatch = 5000

// pruneHistory deletes operational records past their retention: finished
// node commands (Options.CommandRetention after completion) and operation
// idempotency records (Options.OperationRetention). Unfinished commands
// are never touched. It runs at most once per Options.PruneInterval.
func (h *Hub) pruneHistory(ctx context.Context) error {
	now := h.clock.Now()
	h.pruneMu.Lock()
	if !h.lastPrune.IsZero() && now.Sub(h.lastPrune) < h.opt.PruneInterval {
		h.pruneMu.Unlock()
		return nil
	}
	h.lastPrune = now
	h.pruneMu.Unlock()
	cmdCut := persistence.Time(now.Add(-h.opt.CommandRetention))
	for i := 0; i < 100; i++ {
		res, err := h.db.ExecContext(ctx, `DELETE FROM commands WHERE rowid IN (SELECT rowid FROM commands WHERE state IN (?,?,?)
			AND COALESCE(NULLIF(completed_at,''), issued_at) < ? LIMIT ?)`, cmdDone, cmdExpired, cmdCancelled, cmdCut, pruneBatch)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n < pruneBatch {
			break
		}
	}
	// Idempotency records are stamped with the wall clock (sqlitex).
	if _, err := sqlitex.PruneOperations(ctx, h.db, time.Now().Add(-h.opt.OperationRetention), pruneBatch); err != nil {
		return err
	}
	return h.pruneAudit(ctx) // W-audit
}

// reportDedupWindow is how far below a node's committed report sequence
// dedup rows are kept; older ones can never be resent.
const reportDedupWindow = 100000

// pruneReportDedupQuery deletes one node's old dedup rows through the
// (node_id, origin_seq) primary key. The previous single statement with a
// correlated subquery scanned every node_reports row on each maintenance
// pass (11 ms at 48k rows in the A20 soak, linear in history).
const pruneReportDedupQuery = `DELETE FROM node_reports WHERE node_id=? AND origin_seq < ?`

func (h *Hub) pruneReportDedup(ctx context.Context) error {
	rows, err := h.db.QueryContext(ctx, `SELECT node_id, committed_report_seq FROM node_state WHERE committed_report_seq > ?`, reportDedupWindow)
	if err != nil {
		return err
	}
	type cut struct {
		node string
		seq  int64
	}
	var cuts []cut
	for rows.Next() {
		var c cut
		if err := rows.Scan(&c.node, &c.seq); err != nil {
			rows.Close()
			return err
		}
		cuts = append(cuts, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, c := range cuts {
		if _, err := h.db.ExecContext(ctx, pruneReportDedupQuery, c.node, c.seq-reportDedupWindow); err != nil {
			return err
		}
	}
	return nil
}
