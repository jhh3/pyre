package hub

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
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
	if err := h.notifyStuckMail(ctx); err != nil { // stuckmail.go (W-stuckmail)
		errs = append(errs, err)
	}
	h.deliverIssueNotices(ctx)                 // issues.go: triage notices not sent yet
	if err := h.resolveAsks(ctx); err != nil { // asks.go: deadlines, unsent ask mail (W-asks)
		errs = append(errs, err)
	}
	if err := h.sweepAttention(ctx); err != nil { // attention_sweep.go: stale failure/account items
		errs = append(errs, err)
	}
	if err := h.backfillMessageSummaries(ctx); err != nil { // attention_sweep.go: once per process
		errs = append(errs, err)
	}
	if !h.askMailAcked.Load() { // asks.go: mail of asks closed before close acknowledged it; once per process
		if done, err := h.ackClosedAskMail(ctx); err != nil {
			errs = append(errs, err)
		} else if done {
			h.askMailAcked.Store(true)
		}
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

// pruneHistory deletes operational records past their retention: finished
// node commands (Options.CommandRetention after completion; their large
// payloads and results are dropped after Options.CommandCompactAfter) and
// operation idempotency records (Options.OperationRetention). Unfinished
// commands are never touched. It runs at most once per
// Options.PruneInterval, in short batched transactions (retention.go).
func (h *Hub) pruneHistory(ctx context.Context) error {
	now := h.clock.Now()
	h.pruneMu.Lock()
	if !h.lastPrune.IsZero() && now.Sub(h.lastPrune) < h.opt.PruneInterval {
		h.pruneMu.Unlock()
		return nil
	}
	h.lastPrune = now
	h.pruneMu.Unlock()
	if _, err := h.pruneCommands(ctx, now.Add(-h.opt.CommandRetention)); err != nil {
		return err
	}
	if _, err := h.compactCommands(ctx, now.Add(-h.opt.CommandCompactAfter)); err != nil {
		return err
	}
	// Idempotency records are stamped with the wall clock (sqlitex).
	if _, err := sqlitex.PruneOperations(ctx, h.db, time.Now().Add(-h.opt.OperationRetention), sqlitex.PruneBatch); err != nil {
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
// It deletes at most a batch (sqlitex.Batched) per transaction.
const pruneReportDedupQuery = `DELETE FROM node_reports WHERE node_id=? AND origin_seq IN (SELECT origin_seq FROM node_reports WHERE node_id=? AND origin_seq < ? ORDER BY origin_seq LIMIT ?)`

func (h *Hub) pruneReportDedup(ctx context.Context) error {
	_, err := h.pruneReportDedupN(ctx)
	return err
}

func (h *Hub) pruneReportDedupN(ctx context.Context) (int64, error) {
	rows, err := h.db.QueryContext(ctx, `SELECT node_id, committed_report_seq FROM node_state WHERE committed_report_seq > ?`, reportDedupWindow)
	if err != nil {
		return 0, err
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
			return 0, err
		}
		cuts = append(cuts, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	var total int64
	for _, c := range cuts {
		n, err := sqlitex.Batched(ctx, h.db, sqlitex.PruneBatch, 0, func(tx *sql.Tx, limit int) (int64, error) {
			res, err := tx.ExecContext(ctx, pruneReportDedupQuery, c.node, c.node, c.seq-reportDedupWindow, limit)
			if err != nil {
				return 0, err
			}
			return res.RowsAffected()
		})
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
