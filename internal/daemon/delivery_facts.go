package daemon

import (
	"context"

	"github.com/jhh3/pyre/internal/client"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/messaging/delivery"
)

// hubFacts is the delivery engine's pre-paste gate (W-delivery2): one
// single-attempt hub lookup of the message as the recipient session (a plain
// get is side-effect free; reads are recorded only by messages.mark_read,
// ack and reply). Any failure falls back to the local fact.
type hubFacts struct{ d *Daemon }

var _ delivery.MessageFacts = hubFacts{}

func (f hubFacts) Settled(ctx context.Context, sessionID, messageID string) (contracts.RuntimeDeliveryState, string, bool) {
	d := f.d
	if d.hubUnreachable() || sessionID == "" {
		return "", "", false
	}
	tok, ok, err := d.st.tokenForSession(ctx, sessionID)
	if err != nil || !ok {
		return "", "", false
	}
	up, err := d.upstream(tok)
	if err != nil {
		return "", "", false
	}
	pctx := contracts.WithPrincipal(client.WithMaxAttempts(ctx, 1), contracts.Principal{Kind: contracts.PrincipalSession, ID: sessionID})
	v, err := up.GetMessage(pctx, contracts.GetRequest{ID: messageID})
	if err != nil {
		return "", "", false
	}
	switch v.Status.Runtime {
	case contracts.DeliveryAccepted, contracts.DeliveryCancelled, contracts.DeliveryExpired:
		detail := v.Status.RuntimeReason
		if detail == "" {
			detail = "the hub records it as " + string(v.Status.Runtime)
		}
		return v.Status.Runtime, "hub: " + detail, true
	}
	return "", "", false
}
