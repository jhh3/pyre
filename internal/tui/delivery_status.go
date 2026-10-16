package tui

import (
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/messaging/msgstatus"
)

// deliverySummary is the message's derived delivery status (W-stuckmail):
// the hub's when it sent one, else derived from the listed recipient
// session (message lists do not carry it).
func (m *Model) deliverySummary(mv contracts.MessageView) contracts.DeliverySummary {
	if mv.Delivery != nil {
		return *mv.Delivery
	}
	var rcpt *contracts.SessionView
	if msgstatus.NeedsRecipient(mv) {
		if sv, ok := m.session(mv.Envelope.Recipient.ID); ok {
			rcpt = &sv
		}
	}
	return msgstatus.Derive(mv, rcpt, m.now())
}
