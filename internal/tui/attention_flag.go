package tui

import (
	"context"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/contracts"
)

// Session attention flags (W-attn). There is no Inbox view: each session
// carries its own flag (SessionView.Flag, computed by the hub) and the
// Sessions list, Grid, switcher and strip read it through sessionAttention.
// Blocking reasons (approval, question, asked, failed, reauth, blocked)
// read "! needs you"; a session with only a message for you reads
// "✉ message". The message itself shows in the session's own view.

// sessionAttention is the session's attention flag as (level, reason):
// level is contracts.FlagNeedsYou (blocking), contracts.FlagMessage
// (informational) or "" (nothing). A hub without flags (Flag nil) but with
// open items reads as needing you, as before. This is the accessor list
// grouping/sorting and every "needs you" signal use.
func sessionAttention(s contracts.SessionView) (level, reason string) {
	if f := s.Flag; f != nil && f.Level != "" {
		return f.Level, f.Reason
	}
	if s.Attention > 0 {
		return contracts.FlagNeedsYou, "attention"
	}
	return "", ""
}

// sessionNeedsYou reports a blocking flag (messages do not count).
func sessionNeedsYou(s contracts.SessionView) bool {
	level, _ := sessionAttention(s)
	return level == contracts.FlagNeedsYou
}

// sessionMessages is how many informational items (messages for you) the
// session has.
func sessionMessages(s contracts.SessionView) int {
	if s.Flag == nil {
		return 0
	}
	return s.Flag.Messages
}

// flagStatus is the list status for a flag level.
func flagStatus(level string) (status, bool) {
	switch level {
	case contracts.FlagNeedsYou:
		return status{"!", "needs you", toneWarn}, true
	case contracts.FlagMessage:
		return status{"✉", "message", toneAccent}, true
	}
	return status{}, false
}

// reasonStatus is the glyph and tone for a flag reason (strip segments).
func reasonStatus(reason string) status {
	switch reason {
	case "question":
		return status{"?", "question", toneWarn}
	case "failed":
		return status{"✗", "failed", toneBad}
	case "reauth":
		return status{"⊘", "re-auth", toneBad}
	case "delivery":
		return status{"⁇", "uncertain", toneWarn}
	case "conflict":
		return status{"≠", "conflict", toneWarn}
	case "message", "comment", "review":
		return status{"✉", reason, toneAccent}
	}
	return status{"!", reason, toneWarn}
}

// flagCounts counts sessions that need you and messages for you among ss.
func flagCounts(ss []contracts.SessionView) (needs, messages int) {
	for _, s := range ss {
		if s.Session.Archived {
			continue
		}
		if sessionNeedsYou(s) {
			needs++
		}
		messages += sessionMessages(s)
	}
	return needs, messages
}

// headerNeedsYou is the Sessions tab's count: sessions that need you.
func (m *Model) headerNeedsYou() int {
	n, _ := flagCounts(m.sessions())
	return n
}

// needsYouSessions are the visible sessions with a blocking flag, most
// important first (approval/question before the rest), then list order.
func (m *Model) needsYouSessions() []contracts.SessionView {
	var out []contracts.SessionView
	for _, s := range m.visibleSessions() {
		if sessionNeedsYou(s) {
			out = append(out, s)
		}
	}
	rank := func(s contracts.SessionView) int {
		_, r := sessionAttention(s)
		switch r {
		case "approval", "question", "blocked":
			return 0
		case "asked":
			return 1
		}
		return 2
	}
	sort.SliceStable(out, func(i, j int) bool { return rank(out[i]) < rank(out[j]) })
	return out
}

// ---- the session's message for you ----

// ownerInboxKey caches the owner's unacknowledged messages. It is listed
// (ListMessages), never read one by one: GetMessage by the owner counts as
// viewing the message at the hub and would clear the flag while browsing.
const ownerInboxKey = "messages:owner"

func (m *Model) loadOwnerInbox() tea.Cmd {
	api := m.api
	return m.ensure(ownerInboxKey, 0, func(ctx context.Context) (any, error) {
		p, err := api.ListMessages(ctx, contracts.ListMessagesRequest{Box: "inbox", Unacked: true,
			Address:     &contracts.Address{Kind: contracts.AddrOwner, ID: "owner"},
			PageRequest: contracts.PageRequest{Limit: 100}})
		if err != nil {
			return []contracts.MessageView(nil), err
		}
		return p.Items, nil
	})
}

// loadSessionMessage loads the owner inbox when s has a message for you.
func (m *Model) loadSessionMessage(s contracts.SessionView) tea.Cmd {
	if sessionMessages(s) == 0 {
		return nil
	}
	return m.loadOwnerInbox()
}

// messageForYou is the newest unacknowledged message s sent the owner,
// while its flag still says so (engaging with the session clears the flag
// at the hub, and the line goes with it).
func (m *Model) messageForYou(s contracts.SessionView) (contracts.MessageView, bool) {
	if sessionMessages(s) == 0 {
		return contracts.MessageView{}, false
	}
	ms, _ := get[[]contracts.MessageView](m, ownerInboxKey)
	var best contracts.MessageView
	found := false
	for _, mv := range ms {
		env := mv.Envelope
		if env.Sender.Kind != contracts.PrincipalSession || env.Sender.ID != s.Session.SessionID {
			continue
		}
		if mv.Status.Ack == contracts.AckAcknowledged || mv.Status.Ack == contracts.AckReplied {
			continue
		}
		if !found || env.CreatedAt.After(best.Envelope.CreatedAt) {
			best, found = mv, true
		}
	}
	return best, found
}

// messageLine is "✉ message for you: <first line>" plus its keys, or ""
// when s has no message for you loaded.
func (m *Model) messageLine(s contracts.SessionView, w int) string {
	mv, ok := m.messageForYou(s)
	if !ok {
		return ""
	}
	keys := m.th.dim.Render(" — m reply · d dismiss")
	body := strings.TrimSpace(firstLine(mv.Envelope.Body.Text))
	if body == "" {
		body = "(no text)"
	}
	head := m.th.tone(toneAccent, "✉") + " " + m.th.bold.Render("message for you: ")
	room := max(8, w-width(head)-width(keys))
	return trunc(head+trunc(body, room)+keys, w)
}

// replyToSession opens the composer to s, threaded as a reply to its
// message for you when there is one.
func (m *Model) replyToSession(s contracts.SessionView) tea.Cmd {
	cmd := m.openCompose(s)
	if mv, ok := m.messageForYou(s); ok {
		if c, ok := m.overlay.(*composeOverlay); ok {
			c.replyTo = mv.Envelope.MessageID
			c.replyText = firstLine(mv.Envelope.Body.Text)
		}
	}
	return cmd
}

// dismissMessage acknowledges the session's message for you without a
// reply, which clears its "✉ message" flag.
func (m *Model) dismissMessage(s contracts.SessionView) tea.Cmd {
	mv, ok := m.messageForYou(s)
	if !ok {
		if sessionMessages(s) > 0 {
			return tea.Batch(m.loadOwnerInbox(), m.setToast("message loading — press d again", toneMuted))
		}
		return m.setToast(trunc(s.Session.Title, 40)+" has no message for you", toneMuted)
	}
	id, api := mv.Envelope.MessageID, m.api
	req := contracts.AckMessageRequest{Mutation: contracts.Mutation{OperationID: m.opID("ack:" + id)}, MessageID: id}
	return m.do(func(ctx context.Context) (any, error) { return api.AckMessage(ctx, req) },
		func(m *Model, v any, err error) tea.Cmd {
			if err != nil {
				return m.errToast("dismiss failed", err)
			}
			return tea.Batch(m.setToast("message dismissed", toneMuted), m.reload(ownerInboxKey, nil),
				m.reload("sessions", nil), m.reload("attention", nil))
		})
}
