package hub

import (
	"context"
	"database/sql"

	"github.com/jhh3/pyre/internal/contracts"
)

// Read = delivered (W-delivery). When the recipient session itself reads a
// message through the Pyre API (MCP get_message / inbox / wait_reply, or the
// same CLI commands with the session's token), acknowledges it, or replies
// to it, the agent has the message: the hub records acceptance with
// contracts.EvidencePyreRead and withdraws the paste delivery from the node,
// so the message is never pasted afterwards (incident 2026-10-07: a backlog
// the agent had read and acked over 25 minutes was pasted wholesale when
// pyred restarted).
//
// Only the session principal itself counts (never the owner reading or
// acking on its behalf, and never a read of another session's mail). Only a
// pending delivery (queued at the hub or held by the node, no attempt in
// flight) changes; an attempt already in flight finishes on its own.

var _ contracts.MessageReadAPI = (*Hub)(nil)

// readPayload is the cancel_delivery payload of a read withdrawal. Nodes
// that predate it decode only message_id and cancel the queued message.
type readPayload struct {
	MessageID string `json:"message_id"`
	Read      string `json:"read"`             // evidence category
	RunID     string `json:"run_id,omitempty"` // run the acceptance is bound to
	Detail    string `json:"detail,omitempty"`
}

// MarkMessagesRead implements contracts.OpMarkMessagesRead.
func (h *Hub) MarkMessagesRead(ctx context.Context, req contracts.MarkMessagesReadRequest) (contracts.MarkMessagesReadResponse, error) {
	out := contracts.MarkMessagesReadResponse{Recorded: []string{}}
	p, err := principal(ctx)
	if err != nil {
		return out, err
	}
	if len(req.MessageIDs) > 500 {
		return out, contracts.Errorf(contracts.ErrInvalid, "at most 500 message ids per call")
	}
	via := req.Via
	if via == "" {
		via = "read"
	}
	rec, err := h.recipientRead(ctx, p, via, req.MessageIDs...)
	if rec != nil {
		out.Recorded = rec
	}
	return out, err
}

// recipientRead records a read by p of each message addressed to p's
// session and returns the ids whose pending delivery it resolved.
func (h *Hub) recipientRead(ctx context.Context, p contracts.Principal, via string, ids ...string) ([]string, error) {
	if p.Kind != contracts.PrincipalSession || p.OnBehalfOf != nil || len(ids) == 0 {
		return nil, nil
	}
	recorded := []string{}
	err := h.tx(ctx, func(tx *sql.Tx) error {
		recorded = recorded[:0]
		var runID string
		if s, err := h.getSession(ctx, tx, p.ID); err == nil {
			runID = s.CurrentRunID
		}
		for _, id := range ids {
			v, err := h.msgs.GetTx(ctx, tx, id)
			if err != nil {
				continue
			}
			if v.Envelope.Recipient.Kind != contracts.AddrSession || v.Envelope.Recipient.ID != p.ID {
				continue
			}
			detail := "read by the recipient session (" + via + "); not pasted"
			nv, ok, err := h.msgs.RecordReadTx(ctx, tx, id, runID, contracts.EvidencePyreRead, detail)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			pl := readPayload{MessageID: id, Read: string(contracts.EvidencePyreRead), RunID: nv.Status.BoundRunID, Detail: detail}
			if err := h.withdrawDeliveriesTx(ctx, tx, id, hubPrincipal, pl); err != nil {
				return err
			}
			recorded = append(recorded, id)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return recorded, nil
}
