package hub

import (
	"context"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/messaging/store"
)

// Hello negotiates the protocol. A client outside [MinProtocolVersion,
// ProtocolVersion] gets ErrProtocolMismatch with the supported range.
func (h *Hub) Hello(ctx context.Context, req contracts.HelloRequest) (contracts.HelloResponse, error) {
	if req.Protocol < contracts.MinProtocolVersion || req.Protocol > contracts.ProtocolVersion {
		return contracts.HelloResponse{}, contracts.Errorf(contracts.ErrProtocolMismatch,
			"client protocol %d not supported; hub supports %d..%d", req.Protocol, contracts.MinProtocolVersion, contracts.ProtocolVersion).
			WithDetail("min_protocol", contracts.MinProtocolVersion).WithDetail("protocol", contracts.ProtocolVersion)
	}
	p, _ := contracts.PrincipalFrom(ctx)
	return contracts.HelloResponse{
		Protocol:    contracts.ProtocolVersion,
		MinProtocol: contracts.MinProtocolVersion,
		HubID:       h.hubID,
		HubVersion:  h.opt.HubVersion,
		ServerTime:  h.now(),
		Principal:   p,
		Features:    h.features(),
	}, nil
}

func (h *Hub) features() []string {
	f := []string{"system", "sessions", "runs", "leases", "approvals", "messages", "events", "accounts", "node_actions", "connections",
		contracts.FeatureSessionsGetMany, contracts.FeatureAsks, contracts.FeatureAskHold, contracts.FeatureGrants, contracts.FeatureIssues}
	add := func(ok bool, name string) {
		if ok {
			f = append(f, name)
		}
	}
	add(h.cfg.Fleet != nil, "fleet")
	add(h.cfg.Projects != nil, "projects")
	add(h.cfg.Documents != nil, "documents")
	add(h.cfg.Documents != nil, contracts.FeatureProjectPage)
	add(h.cfg.Documents != nil, contracts.FeatureProjectPageStatus)
	add(h.cfg.Artifacts != nil, "artifacts")
	add(h.cfg.Reviews != nil, "reviews")
	add(h.cfg.Notes != nil, "notes")
	add(h.cfg.Attention != nil, "attention")
	add(h.cfg.Terminal != nil, contracts.TerminalFit) // the relay forwards fit requests and size frames
	return f
}

// WhoAmI returns the authenticated principal.
func (h *Hub) WhoAmI(ctx context.Context) (contracts.Principal, error) {
	return principal(ctx)
}

// ListEvents pages the durable event log.
func (h *Hub) ListEvents(ctx context.Context, req contracts.ListEventsRequest) (contracts.ListEventsResponse, error) {
	if _, err := principal(ctx); err != nil {
		return contracts.ListEventsResponse{}, err
	}
	return h.events.ListEvents(ctx, req)
}

// ---- messages (delegated to the store; routing hooks in routing.go) ----

func (h *Hub) SendMessage(ctx context.Context, req contracts.SendMessageRequest) (contracts.MessageView, error) {
	mv, err := h.msgs.SendMessage(ctx, req)
	if err == nil && req.InReplyTo != "" {
		// Replying proves the recipient has the original (W-delivery).
		if p, ok := contracts.PrincipalFrom(ctx); ok {
			_, _ = h.recipientRead(ctx, p, "reply", req.InReplyTo)
		}
	}
	if err == nil {
		h.withDelivery(ctx, &mv) // W-stuckmail
		if p, ok := contracts.PrincipalFrom(ctx); ok && mv.Envelope.Recipient.Kind == contracts.AddrSession {
			h.ownerActive(p, mv.Envelope.Recipient.ID) // owner activity: a message (or reply) to the session
			h.answerChatMessage(ctx, p, mv.Envelope)   // asks_chat.go: the owner's chat answers its open asks
			// Messaging it engages with it: its mail to him so far is read (the
			// deriver answers its items from the event).
			h.ownerEngaged(p, mv.Envelope.Recipient.ID, "You messaged the session.")
		}
	}
	return mv, err
}

// GetMessage: the owner viewing a session's message to them answers that
// session's message flag and acknowledges that message (pyre msg show, the
// phone opening a message; listing does not count).
func (h *Hub) GetMessage(ctx context.Context, req contracts.GetRequest) (contracts.MessageView, error) {
	mv, err := h.msgs.GetMessage(ctx, req)
	if err == nil {
		env := mv.Envelope
		if p, ok := contracts.PrincipalFrom(ctx); ok && env.Recipient.Kind == contracts.AddrOwner && env.Sender.Kind == contracts.PrincipalSession {
			// Viewing it is reading that message (only that one).
			h.ownerEngagedMail(p, env.Sender.ID, "Read.", store.OwnerMail{MessageID: env.MessageID, SkipAsks: true}, readKinds...)
		}
		h.withDelivery(ctx, &mv) // W-stuckmail
	}
	return mv, err
}

func (h *Hub) ListMessages(ctx context.Context, req contracts.ListMessagesRequest) (contracts.Page[contracts.MessageView], error) {
	return h.msgs.ListMessages(ctx, req)
}

// AckMessage: the recipient session acknowledging a message also resolves
// its pending paste delivery (W-delivery: acked mail is never pasted).
func (h *Hub) AckMessage(ctx context.Context, req contracts.AckMessageRequest) (contracts.MessageView, error) {
	mv, err := h.msgs.AckMessage(ctx, req)
	if err == nil {
		if p, ok := contracts.PrincipalFrom(ctx); ok && p.Kind == contracts.PrincipalSession {
			if rec, rerr := h.recipientRead(ctx, p, "ack", req.MessageID); rerr == nil && len(rec) > 0 {
				if nv, gerr := h.msgs.GetMessage(ctx, contracts.GetRequest{ID: req.MessageID}); gerr == nil {
					mv = nv
				}
			}
		}
	}
	return mv, err
}

func (h *Hub) ReissueMessage(ctx context.Context, req contracts.ReissueMessageRequest) (contracts.MessageView, error) {
	return h.msgs.ReissueMessage(ctx, req)
}

func (h *Hub) ResolveDelivery(ctx context.Context, req contracts.ResolveDeliveryRequest) (contracts.MessageView, error) {
	return h.msgs.ResolveDelivery(ctx, req)
}

func (h *Hub) CancelMessage(ctx context.Context, req contracts.CancelMessageRequest) (contracts.MessageView, error) {
	return h.msgs.CancelMessage(ctx, req)
}
