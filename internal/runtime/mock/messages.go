package mock

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

// Body markers that steer the mock's simulated runtime outcome.
const (
	MarkerIndeterminate = "[mock:indeterminate]" // simulate a crash between submit and receipt
	MarkerReject        = "[mock:reject]"        // simulate a runtime refusal
	MarkerApproval      = "[mock:approval]"      // adapter: the accepted input raises a permission request
	MarkerQuestion      = "[mock:question]"      // adapter: the accepted input raises a question
)

const maxMessageBytes = 64 << 10

func senderOf(p c.Principal) c.Principal {
	return c.Principal{Kind: p.Kind, ID: p.ID, Name: p.Name, OnBehalfOf: p.OnBehalfOf}
}

// sameContent compares the parts of an envelope a sender controls.
func sameContent(a, b c.MessageEnvelope) bool {
	return samePrincipal(a.Sender, b.Sender) && a.Recipient == b.Recipient && eqStrPtr(a.ProjectID, b.ProjectID) &&
		a.ThreadID == b.ThreadID && a.InReplyTo == b.InReplyTo && a.Reissues == b.Reissues && a.Mode == b.Mode &&
		a.Body == b.Body && reflect.DeepEqual(nilIfEmpty(a.Attachments), nilIfEmpty(b.Attachments)) && a.RequestAck == b.RequestAck
}

func nilIfEmpty(a []c.Attachment) []c.Attachment {
	if len(a) == 0 {
		return nil
	}
	return a
}

func (b *Backend) SendMessage(ctx context.Context, req c.SendMessageRequest) (c.MessageView, error) {
	return mutate(b, ctx, c.OpSendMessage, req.Mutation, req, func(p c.Principal) (c.MessageView, error) {
		if err := requireNotNode(p); err != nil {
			return c.MessageView{}, err
		}
		return b.sendLocked(p, req, "")
	})
}

func (b *Backend) sendLocked(p c.Principal, req c.SendMessageRequest, reissues string) (c.MessageView, error) {
	mode := req.Mode
	if mode == "" {
		mode = c.ModeQueue
	}
	switch mode {
	case c.ModeQueue, c.ModeSteer, c.ModeInfo:
	default:
		return c.MessageView{}, c.Errorf(c.ErrInvalid, "unknown mode %q", mode)
	}
	if strings.TrimSpace(req.Body.Text) == "" && len(req.Attachments) == 0 {
		return c.MessageView{}, c.Errorf(c.ErrInvalid, "message body is empty")
	}
	if len(req.Body.Text) > maxMessageBytes {
		return c.MessageView{}, c.Errorf(c.ErrPayloadTooLarge, "message body exceeds %d bytes", maxMessageBytes)
	}
	rcpt := req.Recipient
	switch rcpt.Kind {
	case c.AddrSession:
		if _, ok := b.sessions[rcpt.ID]; !ok {
			return c.MessageView{}, notFound(c.KindSession, rcpt.ID)
		}
	case c.AddrOwner:
		rcpt.ID = "owner"
	case c.AddrProject:
		if _, ok := b.projects[rcpt.ID]; !ok {
			return c.MessageView{}, notFound(c.KindProject, rcpt.ID)
		}
	case c.AddrChannel:
		if rcpt.ID == "" || len(rcpt.ID) > 100 {
			return c.MessageView{}, c.Errorf(c.ErrInvalid, "channel name required (<=100 chars)")
		}
	default:
		return c.MessageView{}, c.Errorf(c.ErrInvalid, "unknown recipient kind %q", rcpt.Kind)
	}
	for _, a := range req.Attachments {
		n := 0
		if a.ArtifactVersionID != "" {
			n++
			if _, ok := b.versions[a.ArtifactVersionID]; !ok {
				return c.MessageView{}, notFound(c.KindArtifactVersion, a.ArtifactVersionID)
			}
		}
		if a.DocumentRevisionID != "" {
			n++
			if _, ok := b.revisions[a.DocumentRevisionID]; !ok {
				return c.MessageView{}, notFound(c.KindDocumentRevision, a.DocumentRevisionID)
			}
		}
		if a.NoteID != "" {
			n++
			if _, ok := b.notes[a.NoteID]; !ok {
				return c.MessageView{}, notFound(c.KindNote, a.NoteID)
			}
		}
		if n != 1 {
			return c.MessageView{}, c.Errorf(c.ErrInvalid, "each attachment references exactly one published object (paths are not transferable)")
		}
	}
	var orig *c.MessageView
	if req.InReplyTo != "" {
		o, ok := b.messages[req.InReplyTo]
		if !ok {
			return c.MessageView{}, notFound(c.KindMessage, req.InReplyTo)
		}
		orig = o
	}
	projectID := cloneStrPtr(req.ProjectID)
	if projectID == nil && rcpt.Kind == c.AddrProject {
		projectID = strPtr(rcpt.ID)
	}
	if projectID != nil {
		if _, ok := b.projects[*projectID]; !ok {
			return c.MessageView{}, notFound(c.KindProject, *projectID)
		}
	}
	threadID := req.ThreadID
	if threadID == "" && orig != nil {
		threadID = orig.Envelope.ThreadID
	}
	msgID := req.MessageID
	if msgID == "" {
		msgID = c.NewID(c.PrefixMessage)
	} else if !c.ValidID(msgID, c.PrefixMessage) {
		return c.MessageView{}, c.Errorf(c.ErrInvalid, "message_id must look like msg_<id>")
	}
	now := b.now()
	env := c.MessageEnvelope{
		SchemaVersion: c.SchemaVersion, MessageID: msgID, OperationID: req.OperationID, Sender: senderOf(p),
		Recipient: rcpt, ProjectID: projectID, ThreadID: threadID, InReplyTo: req.InReplyTo, Reissues: reissues,
		Mode: mode, Body: req.Body, Attachments: req.Attachments, CreatedAt: now, RequestAck: req.RequestAck,
	}
	if existing, ok := b.messages[msgID]; ok {
		if sameContent(existing.Envelope, env) {
			return *existing, nil // storage dedup by message_id
		}
		return c.MessageView{}, c.Errorf(c.ErrConflict, "message_id %s already stored with different content", msgID).WithRef(c.KindMessage, msgID)
	}
	if req.TTLSeconds > 0 {
		env.ExpiresAt = timePtr(now.Add(time.Duration(req.TTLSeconds) * time.Second))
	}
	st := c.DeliveryStatus{Storage: c.StorageStored, StoredAt: &now, Ack: c.AckNone, UpdatedAt: now, Runtime: c.DeliveryNotApplicable}
	if rcpt.Kind == c.AddrSession && mode != c.ModeInfo {
		st.Runtime = c.DeliveryQueued
		r := b.currentRun(rcpt.ID)
		switch {
		case r != nil && mode == c.ModeSteer && !c.HasCap(r.Capabilities, c.CapInputSteer):
			st.Runtime = c.DeliveryRejected
			st.RuntimeReason = "unsupported: adapter does not advertise " + c.CapInputSteer
		case r != nil && mode == c.ModeQueue && !c.HasCap(r.Capabilities, c.CapInputQueue):
			st.Runtime = c.DeliveryRejected
			st.RuntimeReason = "unsupported: adapter does not advertise " + c.CapInputQueue
		default:
			st.RuntimeReason = b.waitReason(rcpt.ID, mode)
		}
	}
	b.msgSeq++
	mv := &c.MessageView{Envelope: env, Status: st, Cursor: b.msgSeq}
	b.messages[msgID] = mv
	sessID := ""
	if rcpt.Kind == c.AddrSession {
		sessID = rcpt.ID
	}
	b.emit(c.EvMessageStored, ref(c.KindMessage, msgID), sessID, projectID, mv)
	if orig != nil && b.isRecipient(p, orig.Envelope.Recipient) {
		orig.Status.Ack = c.AckReplied
		orig.Status.AckRef = msgID
		orig.Status.UpdatedAt = now
		b.emit(c.EvMessageAck, ref(c.KindMessage, orig.Envelope.MessageID), recipientSession(orig), orig.Envelope.ProjectID, orig)
		b.withdrawAttentionLocked("message:"+orig.Envelope.MessageID, c.AttnHandled)
	}
	if rcpt.Kind == c.AddrSession && p.Kind == c.PrincipalOwner {
		// The owner messaging a session engages with it.
		b.engageLocked(rcpt.ID, "Replied.", c.AttnMessage, c.AttnFlag, c.AttnWaiting)
		b.ownerActiveLocked(p, rcpt.ID)
	}
	if rcpt.Kind == c.AddrOwner {
		sid := ""
		title := "Message from " + principalName(p)
		if p.Kind == c.PrincipalSession {
			sid = p.ID
			if s := b.sessions[p.ID]; s != nil {
				title = "Message from " + s.Title
			}
		}
		sev := "normal"
		if req.RequestAck {
			sev = "urgent"
		}
		b.raiseAttentionLocked(c.AttnMessage, sev, "message:"+msgID, ref(c.KindMessage, msgID), sid, projectID, title, truncate(req.Body.Text, 120))
	}
	return *mv, nil
}

func recipientSession(m *c.MessageView) string {
	if m.Envelope.Recipient.Kind == c.AddrSession {
		return m.Envelope.Recipient.ID
	}
	return ""
}

// isRecipient reports whether p is the addressee of addr.
func (b *Backend) isRecipient(p c.Principal, addr c.Address) bool {
	switch addr.Kind {
	case c.AddrSession:
		return p.Kind == c.PrincipalSession && p.ID == addr.ID
	case c.AddrOwner:
		return p.Kind == c.PrincipalOwner
	}
	return false
}

// waitReason explains why a queued message is not yet delivered.
func (b *Backend) waitReason(sessionID string, mode c.MessageMode) string {
	r := b.currentRun(sessionID)
	if r == nil || r.State.Terminal() {
		return "no live run; will deliver to the session's next run"
	}
	n := b.nodes[r.NodeID]
	if n == nil || n.Connection.State != c.ConnConnected {
		return "recipient node disconnected; queued at hub"
	}
	switch r.State {
	case c.RunWaitingInput, c.RunWaitingBackground:
		return "recipient idle; delivery pending"
	case c.RunStarting:
		return "recipient run starting"
	case c.RunWaitingApproval:
		return "recipient waiting on an approval; queued"
	case c.RunUnknown:
		return "recipient state unknown; queued"
	}
	if mode == c.ModeSteer {
		return "steer pending"
	}
	return "recipient busy; waiting for idle turn"
}

func (b *Backend) GetMessage(ctx context.Context, req c.GetRequest) (c.MessageView, error) {
	return query(b, ctx, func(p c.Principal) (c.MessageView, error) {
		m, ok := b.messages[req.ID]
		if !ok {
			return c.MessageView{}, notFound(c.KindMessage, req.ID)
		}
		if p.Kind == c.PrincipalOwner && m.Envelope.Recipient.Kind == c.AddrOwner && m.Envelope.Sender.Kind == c.PrincipalSession {
			// The owner viewed a session's message to them.
			b.engageLocked(m.Envelope.Sender.ID, "Read.", c.AttnMessage, c.AttnWaiting)
		}
		return *m, nil
	})
}

func (b *Backend) matchMessages(p c.Principal, req c.ListMessagesRequest) ([]c.MessageView, error) {
	if req.Address == nil && req.ThreadID == "" {
		// Default: the caller's own inbox.
		addr := c.Address{Kind: c.AddrOwner, ID: "owner"}
		if p.Kind == c.PrincipalSession {
			addr = c.Address{Kind: c.AddrSession, ID: p.ID}
		}
		req.Address = &addr
	}
	if req.Address != nil && req.Address.Kind == c.AddrOwner {
		req.Address.ID = "owner"
	}
	box := req.Box
	if box == "" {
		box = "inbox"
	}
	if box != "inbox" && box != "outbox" && box != "all" {
		return nil, c.Errorf(c.ErrInvalid, "box must be inbox, outbox or all")
	}
	var out []c.MessageView
	for _, m := range b.messages {
		e := m.Envelope
		if req.ThreadID != "" && e.ThreadID != req.ThreadID {
			continue
		}
		if req.Address != nil {
			in := e.Recipient == *req.Address
			outb := addrOf(e.Sender) == *req.Address
			switch box {
			case "inbox":
				if !in {
					continue
				}
			case "outbox":
				if !outb {
					continue
				}
			default:
				if !in && !outb {
					continue
				}
			}
		}
		if req.ProjectID != nil && !eqStrPtr(e.ProjectID, req.ProjectID) {
			continue
		}
		if req.Unacked && m.Status.Ack != c.AckNone {
			continue
		}
		if len(req.RuntimeStates) > 0 {
			ok := false
			for _, s := range req.RuntimeStates {
				if m.Status.Runtime == s {
					ok = true
				}
			}
			if !ok {
				continue
			}
		}
		if m.Cursor <= req.AfterCursor {
			continue
		}
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Cursor < out[j].Cursor })
	return out, nil
}

func addrOf(p c.Principal) c.Address {
	switch p.Kind {
	case c.PrincipalSession:
		return c.Address{Kind: c.AddrSession, ID: p.ID}
	case c.PrincipalOwner:
		return c.Address{Kind: c.AddrOwner, ID: "owner"}
	}
	return c.Address{Kind: c.AddressKind(p.Kind), ID: p.ID}
}

func (b *Backend) ListMessages(ctx context.Context, req c.ListMessagesRequest) (c.Page[c.MessageView], error) {
	p, err := b.principal(ctx)
	if err != nil {
		return c.Page[c.MessageView]{}, err
	}
	wait := time.Duration(min(max(req.WaitSeconds, 0), 60)) * time.Second
	deadline := time.Now().Add(wait)
	for {
		b.mu.Lock()
		items, err := b.matchMessages(p, req)
		ch := b.changed
		if err == nil && (len(items) > 0 || req.Cursor != "" || wait <= 0 || !time.Now().Before(deadline)) {
			page, err := paginate(items, req.PageRequest, b.now())
			b.mu.Unlock()
			return clone(page), err
		}
		b.mu.Unlock()
		if err != nil {
			return c.Page[c.MessageView]{}, err
		}
		b.waitChange(ctx, ch, time.Until(deadline))
		if ctx.Err() != nil {
			return c.Page[c.MessageView]{}, ctx.Err()
		}
	}
}

func (b *Backend) AckMessage(ctx context.Context, req c.AckMessageRequest) (c.MessageView, error) {
	return mutate(b, ctx, c.OpAckMessage, req.Mutation, req, func(p c.Principal) (c.MessageView, error) {
		m, ok := b.messages[req.MessageID]
		if !ok {
			return c.MessageView{}, notFound(c.KindMessage, req.MessageID)
		}
		if !b.isRecipient(p, m.Envelope.Recipient) && p.Kind != c.PrincipalOwner {
			return c.MessageView{}, c.Errorf(c.ErrForbidden, "only the recipient may acknowledge a message").WithRef(c.KindMessage, req.MessageID)
		}
		if m.Status.Ack == c.AckNone {
			m.Status.Ack = c.AckAcknowledged
			m.Status.AckRef = req.OperationID
			m.Status.UpdatedAt = b.now()
			b.emit(c.EvMessageAck, ref(c.KindMessage, req.MessageID), recipientSession(m), m.Envelope.ProjectID, m)
		}
		b.withdrawAttentionLocked("message:"+req.MessageID, c.AttnHandled)
		return *m, nil
	})
}

func canAct(p c.Principal, m *c.MessageView) bool {
	return p.Kind == c.PrincipalOwner || samePrincipal(p, m.Envelope.Sender)
}

func (b *Backend) ReissueMessage(ctx context.Context, req c.ReissueMessageRequest) (c.MessageView, error) {
	return mutate(b, ctx, c.OpReissueMessage, req.Mutation, req, func(p c.Principal) (c.MessageView, error) {
		m, ok := b.messages[req.MessageID]
		if !ok {
			return c.MessageView{}, notFound(c.KindMessage, req.MessageID)
		}
		if !canAct(p, m) {
			return c.MessageView{}, c.Errorf(c.ErrForbidden, "only the sender or the owner may reissue").WithRef(c.KindMessage, req.MessageID)
		}
		switch m.Status.Runtime {
		case c.DeliveryIndeterminate:
			if !req.AcceptDuplicateRisk {
				return c.MessageView{}, c.Errorf(c.ErrConflict, "original delivery is indeterminate and may have been accepted; reissue requires accept_duplicate_risk").WithRef(c.KindMessage, req.MessageID)
			}
		case c.DeliveryRejected, c.DeliveryExpired, c.DeliveryCancelled:
		case c.DeliveryAccepted:
			return c.MessageView{}, c.Errorf(c.ErrConflict, "original was accepted by the runtime; send a new message instead of reissuing").WithRef(c.KindMessage, req.MessageID)
		default:
			return c.MessageView{}, c.Errorf(c.ErrConflict, "original is still %s; cancel it or wait for an outcome", m.Status.Runtime).WithRef(c.KindMessage, req.MessageID)
		}
		body := m.Envelope.Body
		if req.Body != nil {
			body = *req.Body
		}
		nr := c.SendMessageRequest{
			Mutation: c.Mutation{OperationID: req.OperationID, CorrelationID: req.CorrelationID}, Recipient: m.Envelope.Recipient,
			ProjectID: m.Envelope.ProjectID, ThreadID: m.Envelope.ThreadID, Mode: m.Envelope.Mode, Body: body,
			Attachments: m.Envelope.Attachments, RequestAck: m.Envelope.RequestAck,
		}
		mv, err := b.sendLocked(p, nr, m.Envelope.MessageID)
		if err != nil {
			return c.MessageView{}, err
		}
		if m.Status.Runtime == c.DeliveryIndeterminate {
			b.withdrawAttentionLocked("indeterminate:"+m.Envelope.MessageID, c.AttnHandled)
		}
		return mv, nil
	})
}

func (b *Backend) ResolveDelivery(ctx context.Context, req c.ResolveDeliveryRequest) (c.MessageView, error) {
	return mutate(b, ctx, c.OpResolveDelivery, req.Mutation, req, func(p c.Principal) (c.MessageView, error) {
		m, ok := b.messages[req.MessageID]
		if !ok {
			return c.MessageView{}, notFound(c.KindMessage, req.MessageID)
		}
		if !canAct(p, m) {
			return c.MessageView{}, c.Errorf(c.ErrForbidden, "only the sender or the owner may resolve a delivery").WithRef(c.KindMessage, req.MessageID)
		}
		if req.Resolution != c.DeliveryAccepted && req.Resolution != c.DeliveryRejected {
			return c.MessageView{}, c.Errorf(c.ErrInvalid, "resolution must be accepted or rejected")
		}
		if m.Status.Runtime != c.DeliveryIndeterminate {
			return c.MessageView{}, c.Errorf(c.ErrConflict, "delivery is %s, not indeterminate", m.Status.Runtime).WithRef(c.KindMessage, req.MessageID)
		}
		now := b.now()
		detail := "resolved by " + principalName(p)
		if req.Note != "" {
			detail += ": " + truncate(req.Note, 500)
		}
		rc := c.DeliveryReceipt{MessageID: m.Envelope.MessageID, AttemptID: lastAttempt(m), SessionID: recipientSession(m), RunID: m.Status.BoundRunID,
			State: req.Resolution, Evidence: c.EvidenceNone, Detail: detail, ObservedAt: now}
		if r := b.runs[m.Status.BoundRunID]; r != nil {
			rc.NodeID = r.NodeID
		}
		m.Status.Runtime = req.Resolution
		m.Status.RuntimeReason = detail
		m.Status.Receipts = append(m.Status.Receipts, rc)
		m.Status.UpdatedAt = now
		b.emit(c.EvMessageDelivery, ref(c.KindMessage, m.Envelope.MessageID), recipientSession(m), m.Envelope.ProjectID, m)
		b.withdrawAttentionLocked("indeterminate:"+m.Envelope.MessageID, c.AttnHandled)
		return *m, nil
	})
}

func lastAttempt(m *c.MessageView) string {
	for i := len(m.Status.Receipts) - 1; i >= 0; i-- {
		if m.Status.Receipts[i].AttemptID != "" {
			return m.Status.Receipts[i].AttemptID
		}
	}
	return ""
}

func (b *Backend) CancelMessage(ctx context.Context, req c.CancelMessageRequest) (c.MessageView, error) {
	return mutate(b, ctx, c.OpCancelMessage, req.Mutation, req, func(p c.Principal) (c.MessageView, error) {
		m, ok := b.messages[req.MessageID]
		if !ok {
			return c.MessageView{}, notFound(c.KindMessage, req.MessageID)
		}
		if !canAct(p, m) {
			return c.MessageView{}, c.Errorf(c.ErrForbidden, "only the sender or the owner may cancel").WithRef(c.KindMessage, req.MessageID)
		}
		switch m.Status.Runtime {
		case c.DeliveryQueued, c.DeliveryNodeReceived:
		case c.DeliveryCancelled:
			return *m, nil
		default:
			return c.MessageView{}, c.Errorf(c.ErrConflict, "cannot cancel: delivery is %s (an attempt may already have begun)", m.Status.Runtime).WithRef(c.KindMessage, req.MessageID)
		}
		b.setDeliveryLocked(m, c.DeliveryCancelled, "cancelled by "+principalName(p), nil)
		return *m, nil
	})
}

// setDeliveryLocked transitions runtime delivery and emits message.delivery.
func (b *Backend) setDeliveryLocked(m *c.MessageView, st c.RuntimeDeliveryState, reason string, receipt *c.DeliveryReceipt) {
	m.Status.Runtime = st
	m.Status.RuntimeReason = reason
	m.Status.UpdatedAt = b.now()
	if receipt != nil {
		m.Status.Receipts = append(m.Status.Receipts, *receipt)
	}
	b.emit(c.EvMessageDelivery, ref(c.KindMessage, m.Envelope.MessageID), recipientSession(m), m.Envelope.ProjectID, m)
}
