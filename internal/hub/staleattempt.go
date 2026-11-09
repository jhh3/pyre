package hub

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/messaging/store"
)

// Stale attempts (incident 2026-10-23, iss_…): a
// message must never stay "attempting" forever. A node bounds every attempt
// to seconds and reports its outcome durably, so an attempt that has
// reported nothing for store.StaleAttemptAfter while its run is observed
// idle (its connected node's latest report says waiting_input: the attempt
// started no turn) lost its outcome, e.g. the node stopped mid-attempt.
// Nodes report run state changes in order with receipts, so a turn the
// attempt started would show as running. The hub returns it to
// node_received (the node retries it or reports the outcome; facts still
// win) and keeps the binding, so a reissue needs accept_duplicate_risk.

const staleAttemptBatch = 200

// requeueStaleAttempts runs in the maintenance pass.
func (h *Hub) requeueStaleAttempts(ctx context.Context) error {
	cut := h.now().Add(-store.StaleAttemptAfter)
	rows, err := h.db.QueryContext(ctx, `SELECT message_id FROM messages WHERE runtime=? AND recipient_kind=? AND bound_run_id IS NOT NULL ORDER BY seq LIMIT ?`,
		string(contracts.DeliveryAttempting), string(contracts.AddrSession), staleAttemptBatch)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := h.requeueStaleAttempt(ctx, id, cut); err != nil {
			return err
		}
	}
	return nil
}

// requeueStaleAttempt applies the rule to one message (also checked right
// before a reissue, so the rule does not wait for the next pass).
func (h *Hub) requeueStaleAttempt(ctx context.Context, messageID string, cut time.Time) (bool, error) {
	var ok bool
	err := h.tx(ctx, func(tx *sql.Tx) error {
		v, err := h.msgs.GetTx(ctx, tx, messageID)
		if err != nil {
			return err
		}
		last := v.Status.UpdatedAt
		if v.Status.Runtime != contracts.DeliveryAttempting || v.Status.BoundRunID == "" || last.After(cut) {
			return nil
		}
		r, err := h.getRun(ctx, tx, v.Status.BoundRunID)
		if err != nil {
			return nil // run unknown here: leave it to the node
		}
		h.decorateRun(ctx, tx, &r) // a disconnected node's run is stale, state unknown
		if r.State != contracts.RunWaitingInput || r.Freshness.Stale {
			return nil // busy, or not observed: its outcome may still come
		}
		reason := fmt.Sprintf("no outcome reported since %s while run %s is observed idle (waiting for input), so the attempt started no turn; returned to the queue: the node retries it or reports its outcome (reissue needs accept_duplicate_risk)",
			last.UTC().Format(time.RFC3339), r.RunID)
		_, ok, err = h.msgs.RequeueStaleAttemptTx(ctx, tx, messageID, cut, reason)
		return err
	})
	return ok, err
}
