package hub

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/messaging/msgstatus"
	"github.com/jhh3/pyre/internal/persistence"
)

// W-stuckmail: senders learn when their message is not getting through.
// MessageView.Delivery is filled on messages.send / messages.get, and a
// session whose message has waited StuckNoticeAfter because the recipient
// ended, was archived, failed its last turn, or sits on a disconnected
// machine gets one short notice per message (batched per sender, at most one
// notice per sender per StuckNoticeAfter).

const (
	// StuckNoticeAfter is how long mail must wait before its sender hears.
	StuckNoticeAfter = 10 * time.Minute
	// stuckLookback bounds the sweep (older mail is never announced).
	stuckLookback = 24 * time.Hour
	// stuckSweepEvery throttles the sweep inside the maintenance pass.
	stuckSweepEvery = time.Minute
	stuckBatch      = 200
	stuckMaxListed  = 5
)

// stuckSender is the system principal stuck-mail notices are sent as; its
// messages to a session are the durable record of what was announced.
var stuckSender = contracts.Principal{Kind: contracts.PrincipalSystem, ID: "stuckmail"}

type stuckMailState struct {
	mu        sync.Mutex
	lastSweep time.Time
	announced map[string]time.Time // message ID -> announced (or found announced)
	senders   map[string]time.Time // sender session -> last notice
}

// withDelivery fills mv.Delivery from the message's facts and, for
// session-addressed mail that still waits, the recipient's current run.
func (h *Hub) withDelivery(ctx context.Context, mv *contracts.MessageView) {
	d := msgstatus.Derive(*mv, h.recipientView(ctx, *mv), h.now())
	mv.Delivery = &d
}

func (h *Hub) recipientView(ctx context.Context, mv contracts.MessageView) *contracts.SessionView {
	if !msgstatus.NeedsRecipient(mv) {
		return nil
	}
	s, err := h.getSession(ctx, h.db, mv.Envelope.Recipient.ID)
	if err != nil {
		return nil
	}
	v, err := h.sessionView(ctx, h.db, s, false)
	if err != nil {
		return nil
	}
	h.withNodeName(ctx, &v)
	return &v
}

// stuckWorthNotice: waiting for a reason that will not clear by itself
// soon (busy, approval and blocked-input waits are not announced).
func stuckWorthNotice(d contracts.DeliverySummary) bool {
	switch d.State {
	case contracts.SummaryHeld:
		return true
	case contracts.SummaryUndeliverable:
		return d.Cause == contracts.CauseRecipientArchived
	case contracts.SummaryQueued:
		switch d.Cause {
		case contracts.CauseNodeDisconnected, contracts.CauseRecipientFailed, contracts.CauseRecipientUnknown, contracts.CauseRecipientExiting:
			return true
		}
	}
	return false
}

type stuckItem struct {
	mv  contracts.MessageView
	d   contracts.DeliverySummary
	rcp *contracts.SessionView
}

// notifyStuckMail runs at most once per stuckSweepEvery.
func (h *Hub) notifyStuckMail(ctx context.Context) error {
	now := h.now()
	st := &h.stuck
	st.mu.Lock()
	if !st.lastSweep.IsZero() && now.Sub(st.lastSweep) < stuckSweepEvery {
		st.mu.Unlock()
		return nil
	}
	st.lastSweep = now
	if st.announced == nil {
		st.announced, st.senders = map[string]time.Time{}, map[string]time.Time{}
	}
	for id, at := range st.announced {
		if now.Sub(at) > stuckLookback+time.Hour {
			delete(st.announced, id)
		}
	}
	st.mu.Unlock()

	rows, err := h.db.QueryContext(ctx, `SELECT message_id FROM messages WHERE runtime IN (?,?) AND storage=? AND recipient_kind=? AND sender_kind=?
		AND mode<>? AND stored_at<=? AND stored_at>=? ORDER BY seq LIMIT ?`,
		string(contracts.DeliveryQueued), string(contracts.DeliveryNodeReceived), string(contracts.StorageStored),
		string(contracts.AddrSession), string(contracts.AddrSession), string(contracts.ModeInfo),
		persistence.Time(now.Add(-StuckNoticeAfter)), persistence.Time(now.Add(-stuckLookback)), stuckBatch)
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
	sys := h.systemCtx(ctx)
	bySender := map[string][]stuckItem{}
	for _, id := range ids {
		st.mu.Lock()
		_, done := st.announced[id]
		st.mu.Unlock()
		if done {
			continue
		}
		mv, err := h.msgs.GetMessage(sys, contracts.GetRequest{ID: id})
		if err != nil || mv.Envelope.Sender.Kind != contracts.PrincipalSession {
			continue
		}
		rcp := h.recipientView(ctx, mv)
		d := msgstatus.Derive(mv, rcp, now)
		if !stuckWorthNotice(d) {
			continue
		}
		sender := mv.Envelope.Sender.ID
		if h.stuckAnnounced(ctx, sender, id) {
			st.mu.Lock()
			st.announced[id] = now
			st.mu.Unlock()
			continue
		}
		bySender[sender] = append(bySender[sender], stuckItem{mv: mv, d: d, rcp: rcp})
	}
	senders := make([]string, 0, len(bySender))
	for s := range bySender {
		senders = append(senders, s)
	}
	sort.Strings(senders)
	for _, sender := range senders {
		st.mu.Lock()
		last, ok := st.senders[sender]
		st.mu.Unlock()
		if ok && now.Sub(last) < StuckNoticeAfter {
			continue // rate limit; announced on a later sweep
		}
		h.sendStuckNotice(ctx, sender, bySender[sender], now)
	}
	return nil
}

// stuckAnnounced reports whether an earlier notice (possibly before a hub
// restart) already named this message.
func (h *Hub) stuckAnnounced(ctx context.Context, sender, messageID string) bool {
	var n int
	_ = h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE sender_kind=? AND sender_id=? AND recipient_kind=? AND recipient_id=? AND envelope LIKE ?`,
		string(stuckSender.Kind), stuckSender.ID, string(contracts.AddrSession), sender, "%"+messageID+"%").Scan(&n)
	return n > 0
}

func (h *Hub) sendStuckNotice(ctx context.Context, sender string, items []stuckItem, now time.Time) {
	s, err := h.getSession(ctx, h.db, sender)
	mark := func() {
		h.stuck.mu.Lock()
		for _, it := range items {
			h.stuck.announced[it.mv.Envelope.MessageID] = now
		}
		h.stuck.mu.Unlock()
	}
	if err != nil || s.Archived || s.CurrentRunID == "" {
		mark() // nobody to tell
		return
	}
	if r, err := h.getRun(ctx, h.db, s.CurrentRunID); err != nil || r.State.Terminal() {
		mark()
		return
	}
	text := renderStuckNotice(items, now)
	_, err = h.msgs.SendMessage(contracts.WithPrincipal(ctx, stuckSender), contracts.SendMessageRequest{
		Mutation:  contracts.Mutation{OperationID: fmt.Sprintf("op_stuckmail-%s-%d", items[0].mv.Envelope.MessageID, len(items))},
		Recipient: contracts.Address{Kind: contracts.AddrSession, ID: sender},
		ProjectID: s.ProjectID,
		Mode:      contracts.ModeQueue,
		Body:      contracts.MessageBody{Text: text},
	})
	if err != nil {
		return // retried on a later sweep
	}
	mark()
	h.stuck.mu.Lock()
	h.stuck.senders[sender] = now
	h.stuck.mu.Unlock()
}

func renderStuckNotice(items []stuckItem, now time.Time) string {
	var b strings.Builder
	who := func(it stuckItem) string {
		if it.rcp != nil && it.rcp.Session.Title != "" {
			return fmt.Sprintf("%q (%s)", it.rcp.Session.Title, it.mv.Envelope.Recipient.ID)
		}
		return it.mv.Envelope.Recipient.ID
	}
	age := func(it stuckItem) string {
		if it.mv.Status.StoredAt == nil {
			return ""
		}
		return " after " + now.Sub(*it.mv.Status.StoredAt).Round(time.Minute).String()
	}
	if len(items) == 1 {
		it := items[0]
		fmt.Fprintf(&b, "[pyre] Your message %s to %s is not delivered%s: %s.", it.mv.Envelope.MessageID, who(it), strings.TrimSuffix(age(it), "0s"), it.d.Summary)
	} else {
		fmt.Fprintf(&b, "[pyre] %d of your messages are not delivered:", len(items))
		for i, it := range items {
			if i == stuckMaxListed {
				rest := make([]string, 0, len(items)-i)
				for _, r := range items[i:] {
					rest = append(rest, r.mv.Envelope.MessageID)
				}
				fmt.Fprintf(&b, "\n- and %d more (pyre msg show <id> for each): %s", len(rest), strings.Join(rest, ", "))
				break
			}
			fmt.Fprintf(&b, "\n- %s to %s%s: %s", it.mv.Envelope.MessageID, who(it), strings.TrimSuffix(age(it), "0s"), it.d.Summary)
		}
	}
	b.WriteString("\nPyre keeps queued mail and delivers it if the recipient recovers; message someone else if it is urgent. This is a one-time notice; no reply needed.")
	return b.String()
}
