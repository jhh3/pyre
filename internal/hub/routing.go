package hub

import (
	"context"
	"database/sql"

	"github.com/jhh3/pyre/internal/contracts"
)

// Message routing hooks installed into the message store. They run inside
// the store's write transaction.

var hubPrincipal = contracts.Principal{Kind: contracts.PrincipalSystem, ID: "hub"}

// validateRecipient requires session recipients to exist.
func (h *Hub) validateRecipient(ctx context.Context, tx *sql.Tx, addr contracts.Address) error {
	if addr.Kind != contracts.AddrSession {
		return nil
	}
	_, err := h.getSession(ctx, tx, addr.ID)
	return err
}

// routeStored queues a deliver command to the node running the recipient's
// current run, or explains why the message waits / was rejected.
func (h *Hub) routeStored(ctx context.Context, tx *sql.Tx, v contracts.MessageView) error {
	sess, err := h.getSession(ctx, tx, v.Envelope.Recipient.ID)
	if err != nil {
		return err
	}
	var run *contracts.Run
	if sess.CurrentRunID != "" {
		r, err := h.getRun(ctx, tx, sess.CurrentRunID)
		if err != nil && !contracts.IsCode(err, contracts.ErrNotFound) {
			return err
		}
		if err == nil {
			run = &r
		}
	}
	return h.routeToRunTx(ctx, tx, v, run)
}

func (h *Hub) routeToRunTx(ctx context.Context, tx *sql.Tx, v contracts.MessageView, run *contracts.Run) error {
	id := v.Envelope.MessageID
	if v.Envelope.Mode == contracts.ModeSteer {
		switch {
		case run == nil || run.State.Terminal():
			_, err := h.msgs.SetRuntimeTx(ctx, tx, id, contracts.DeliveryRejected, "rejected: recipient has no live run to steer")
			return err
		case !contracts.HasCap(run.Capabilities, contracts.CapInputSteer):
			_, err := h.msgs.SetRuntimeTx(ctx, tx, id, contracts.DeliveryRejected, "unsupported: adapter does not advertise input.steer")
			return err
		}
		if connected, _ := h.nodeConnected(ctx, tx, run.NodeID); !connected {
			_, err := h.msgs.SetRuntimeTx(ctx, tx, id, contracts.DeliveryRejected, "rejected: recipient node is disconnected; steering is immediate-only")
			return err
		}
	} else {
		if run == nil || run.State.Terminal() {
			_, err := h.msgs.SetRuntimeTx(ctx, tx, id, contracts.DeliveryQueued, "recipient session has no live run; queued until a run starts")
			return err
		}
		if !contracts.HasCap(run.Capabilities, contracts.CapInputQueue) {
			_, err := h.msgs.SetRuntimeTx(ctx, tx, id, contracts.DeliveryRejected, "unsupported: adapter does not advertise input.queue")
			return err
		}
	}
	// Skip if a deliver command for this message is already outstanding on
	// that node.
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM commands WHERE ref_kind=? AND ref_id=? AND kind=? AND node_id=? AND state IN (?,?,?)`,
		string(contracts.KindMessage), id, string(contracts.CmdDeliver), run.NodeID, cmdQueued, cmdSent, cmdAcked).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	if _, err := h.enqueueTx(ctx, tx, run.NodeID, contracts.CmdDeliver, v.Envelope.Sender, contracts.DeliverCommand{
		Message: v.Envelope, SessionID: v.Envelope.Recipient.ID, BoundRunID: v.Status.BoundRunID,
	}, 0, contracts.ObjectRef{Kind: contracts.KindMessage, ID: id}); err != nil {
		return err
	}
	reason := "queued for delivery to run " + run.RunID + " on node " + run.NodeID
	if connected, _ := h.nodeConnected(ctx, tx, run.NodeID); !connected {
		reason = "recipient node " + run.NodeID + " disconnected; queued for delivery on reconnect"
	}
	_, err := h.msgs.SetRuntimeTx(ctx, tx, id, contracts.DeliveryQueued, reason)
	return err
}

// withdrawDelivery withdraws outstanding deliver commands for a cancelled or
// expired message.
func (h *Hub) withdrawDelivery(ctx context.Context, tx *sql.Tx, v contracts.MessageView) error {
	return h.cancelDeliveriesTx(ctx, tx, v.Envelope.MessageID, hubPrincipal)
}

// routeUnattemptedTx hands clearly unattempted messages (queued at the hub,
// never bound to a run) to a session's new current run. Messages bound to an
// earlier run, or already received by an earlier node, never move.
func (h *Hub) routeUnattemptedTx(ctx context.Context, tx *sql.Tx, sessionID string, run contracts.Run) error {
	msgs, err := h.msgs.UnattemptedForSessionTx(ctx, tx, sessionID)
	if err != nil {
		return err
	}
	for _, m := range msgs {
		// Outstanding commands to other nodes are superseded (never received).
		if err := h.supersedeDeliveriesTx(ctx, tx, m.Envelope.MessageID, run.NodeID); err != nil {
			return err
		}
		if m.Envelope.Mode == contracts.ModeSteer {
			if _, err := h.msgs.SetRuntimeTx(ctx, tx, m.Envelope.MessageID, contracts.DeliveryRejected, "rejected: the target run was replaced before steering was delivered"); err != nil {
				return err
			}
			continue
		}
		r := run
		if err := h.routeToRunTx(ctx, tx, m, &r); err != nil {
			return err
		}
	}
	return nil
}

// supersedeDeliveriesTx cancels deliver commands for a message that target a
// node other than keepNode and that the node never acknowledged.
func (h *Hub) supersedeDeliveriesTx(ctx context.Context, tx *sql.Tx, messageID, keepNode string) error {
	rows, err := tx.QueryContext(ctx, `SELECT command_id, node_id, state FROM commands WHERE ref_kind=? AND ref_id=? AND kind=? AND node_id<>? AND state IN (?,?)`,
		string(contracts.KindMessage), messageID, string(contracts.CmdDeliver), keepNode, cmdQueued, cmdSent)
	if err != nil {
		return err
	}
	type c struct{ id, node, state string }
	var cs []c
	for rows.Next() {
		var x c
		if err := rows.Scan(&x.id, &x.node, &x.state); err != nil {
			rows.Close()
			return err
		}
		cs = append(cs, x)
	}
	rows.Close()
	for _, x := range cs {
		if _, err := tx.ExecContext(ctx, `UPDATE commands SET state=? WHERE command_id=?`, cmdCancelled, x.id); err != nil {
			return err
		}
		if x.state == cmdSent {
			if _, err := h.enqueueTx(ctx, tx, x.node, contracts.CmdCancelDelivery, hubPrincipal, map[string]string{"message_id": messageID}, 0,
				contracts.ObjectRef{Kind: contracts.KindMessage, ID: messageID}); err != nil {
				return err
			}
		}
	}
	return nil
}
