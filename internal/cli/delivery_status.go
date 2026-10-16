package cli

import (
	"context"
	"strings"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/messaging/msgstatus"
)

// W-stuckmail: the derived delivery status of a message (MessageView.
// Delivery). Hubs fill it on send and get; against an older hub (or the
// mock) the client derives it from the message and the recipient session.

// recipientCache remembers the recipient session view briefly, so a
// polling wait does not fetch it on every poll.
type recipientCache struct {
	id   string
	at   time.Time
	view *c.SessionView
}

const recipientCacheTTL = 5 * time.Second

// withDelivery fills m.Delivery when the hub did not. Best effort: a failed
// session lookup derives without the recipient ("state unavailable").
func (o *Ops) withDelivery(ctx context.Context, m *c.MessageView, cache *recipientCache) {
	if m.Delivery != nil || m.Envelope.MessageID == "" {
		return
	}
	var rcpt *c.SessionView
	if msgstatus.NeedsRecipient(*m) {
		id := m.Envelope.Recipient.ID
		if cache != nil && cache.id == id && time.Since(cache.at) < recipientCacheTTL {
			rcpt = cache.view
		} else {
			if sv, err := o.API.GetSession(ctx, c.GetRequest{ID: id}); err == nil {
				rcpt = &sv
			}
			if cache != nil {
				*cache = recipientCache{id: id, at: time.Now(), view: rcpt}
			}
		}
	}
	d := msgstatus.Derive(*m, rcpt, time.Now())
	m.Delivery = &d
}

// stillPhrase words a status line for "your message is ..." after a wait.
func stillPhrase(s string) string {
	switch {
	case strings.HasPrefix(s, "queued:"), strings.HasPrefix(s, "held:"), strings.HasPrefix(s, "pending:"):
		return "still " + s
	case strings.HasPrefix(s, "delivering:"):
		return "still being delivered:" + strings.TrimPrefix(s, "delivering:")
	case strings.HasPrefix(s, "delivered"), strings.HasPrefix(s, "read"), strings.HasPrefix(s, "acknowledged"):
		return s + "; no reply yet"
	}
	return s
}

// sendNote is send_message's extra text block: the delivery status, so the
// agent sees at once whether its message is getting through.
func sendNote(r any) string {
	m, ok := r.(c.MessageView)
	if !ok || m.Delivery == nil {
		return ""
	}
	d := m.Delivery
	s := "delivery: " + d.Summary
	switch {
	case d.Final:
		s += "\nThis message will not get through unless someone acts; consider messaging another session or the owner."
	case d.State == c.SummaryQueued && (d.Cause == c.CauseNodeDisconnected || d.Cause == c.CauseRecipientFailed || d.Cause == c.CauseRecipientUnknown):
		s += "\nIt stays queued; a reply may take a while."
	}
	return s
}

// deliveryLine is the one-line status ("" when unknown).
func deliveryLine(m c.MessageView) string {
	if m.Delivery == nil {
		return ""
	}
	return m.Delivery.Summary
}
