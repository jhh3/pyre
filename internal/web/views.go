package web

import (
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/messaging/msgstatus"
)

// ---- JSON views (docs/web.md lists them; the web UI reads exactly these) ----

type ProjectRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type SessionRef struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	State string `json:"state"`
	Node  string `json:"node"`
}

type Thread struct {
	ThreadID     string        `json:"thread_id"`
	Kind         string        `json:"kind"` // "ask" | "message"
	Title        string        `json:"title"`
	Preview      string        `json:"preview"`
	Project      *ProjectRef   `json:"project"`
	Session      *SessionRef   `json:"session"`
	Ask          *c.Ask        `json:"ask"`
	Held         *c.HeldAnswer `json:"held"`
	LastAt       time.Time     `json:"last_at"`
	Unread       bool          `json:"unread"`
	MessageCount int           `json:"message_count"`
	NeedsReply   bool          `json:"needs_reply"`

	open bool // belongs in the open box
}

type MsgFrom struct {
	Kind  string `json:"kind"` // "owner" | "session"
	ID    string `json:"id"`
	Label string `json:"label"`
}

type Delivery struct {
	// State is stored | queued | delivered | read | replied | failed.
	State   string `json:"state"`
	Summary string `json:"summary"`
}

type Msg struct {
	MessageID string    `json:"message_id"`
	From      MsgFrom   `json:"from"`
	Text      string    `json:"text"`
	At        time.Time `json:"at"`
	ReplyTo   string    `json:"reply_to"`
	Delivery  Delivery  `json:"delivery"`
	AskID     string    `json:"ask_id,omitempty"`
}

type Activity struct {
	Line string    `json:"line"`
	At   time.Time `json:"at"`
}

type SessionCard struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	State        string    `json:"state"`
	Node         string    `json:"node"`
	IsLead       bool      `json:"is_lead"`
	LastActivity *Activity `json:"last_activity"`
}

type ProjectCard struct {
	ProjectID      string        `json:"project_id"`
	Name           string        `json:"name"`
	Description    string        `json:"description"`
	OpenAsks       int           `json:"open_asks"`
	Waiting        bool          `json:"waiting"`
	Leads          []SessionCard `json:"leads"`
	PageUpdatedAt  *time.Time    `json:"page_updated_at"`
	PageSummary    string        `json:"page_summary"`
	LastActivityAt time.Time     `json:"last_activity_at"`
	// LiveSessions counts sessions that are starting, working, idle or
	// need you (not ended, failed or archived). Active is LiveSessions > 0
	// or an open ask: the app hides inactive projects by default.
	LiveSessions int  `json:"live_sessions"`
	Active       bool `json:"active"`
}

type ArtifactRef struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	URL       string    `json:"url"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ---- sessions ----

// Session states the app shows (SessionRef.State, SessionCard.State).
const (
	StateWorking  = "working"
	StateNeedsYou = "needs_you"
	StateIdle     = "idle"
	StateStarting = "starting"
	StateEnded    = "ended"
	StateFailed   = "failed"
	StateUnknown  = "unknown"
	StateArchived = "archived"
)

func sessionState(v c.SessionView) string {
	if v.Session.Archived {
		return StateArchived
	}
	if v.Flag != nil && v.Flag.Level == c.FlagNeedsYou {
		return StateNeedsYou
	}
	r := v.Run
	if r == nil {
		return StateEnded
	}
	switch r.State {
	case c.RunRunning, c.RunWaitingBackground:
		return StateWorking
	case c.RunWaitingApproval:
		return StateNeedsYou
	case c.RunWaitingInput:
		return StateIdle
	case c.RunStarting:
		return StateStarting
	case c.RunExiting, c.RunExited:
		return StateEnded
	case c.RunFailed:
		return StateFailed
	}
	return StateUnknown
}

func nodeName(v c.SessionView) string {
	if v.NodeName != "" {
		return v.NodeName
	}
	if v.Run != nil {
		return v.Run.NodeID
	}
	return ""
}

func sessionTitle(v c.SessionView, id string) string {
	if t := strings.TrimSpace(v.Session.Title); t != "" {
		if v.Session.Icon != "" {
			return v.Session.Icon + " " + t
		}
		return t
	}
	if id != "" {
		return "Session " + id
	}
	return "Session"
}

func (sn *snap) sessionRef(id string) *SessionRef {
	if id == "" {
		return nil
	}
	v := sn.sessions[id]
	return &SessionRef{ID: id, Title: sessionTitle(v, id), State: sessionState(v), Node: nodeName(v)}
}

func (sn *snap) title(id string) string { return sessionTitle(sn.sessions[id], id) }

func (sn *snap) projectRef(p *string) *ProjectRef {
	if p == nil || *p == "" {
		return nil
	}
	name := *p
	if pr, ok := sn.projects[*p]; ok && pr.Name != "" {
		name = pr.Name
	}
	return &ProjectRef{ID: *p, Name: name}
}

// isLead: a top-level session (it reports to nobody) in a project.
func isLead(v c.SessionView) bool {
	return v.Session.ProjectID != nil && *v.Session.ProjectID != "" && v.Session.Parent() == "" && !v.Session.Archived
}

func lastActivity(v c.SessionView) *Activity {
	if r := v.Run; r != nil {
		if r.Activity != nil && r.Activity.Text != "" {
			return &Activity{Line: r.Activity.Text, At: r.Activity.Since}
		}
		if v.Flag != nil && v.Flag.Text != "" {
			return &Activity{Line: v.Flag.Text, At: v.Flag.Since}
		}
		if r.EndedAt != nil {
			line := "Ended"
			if r.State == c.RunFailed {
				line = "Failed"
				if r.StateReason != "" {
					line += ": " + r.StateReason
				}
			}
			return &Activity{Line: line, At: *r.EndedAt}
		}
		if r.State == c.RunWaitingInput {
			return &Activity{Line: "Waiting for input", At: r.Freshness.ObservedAt}
		}
	}
	if v.Flag != nil && v.Flag.Text != "" {
		return &Activity{Line: v.Flag.Text, At: v.Flag.Since}
	}
	return nil
}

// activeAt is the latest time something happened in a session.
func activeAt(v c.SessionView) time.Time {
	t := v.Session.CreatedAt
	later := func(u time.Time) {
		if u.After(t) {
			t = u
		}
	}
	if v.Session.OwnerActiveAt != nil {
		later(*v.Session.OwnerActiveAt)
	}
	if r := v.Run; r != nil {
		later(r.StartedAt)
		if r.EndedAt != nil {
			later(*r.EndedAt)
		}
		if r.Activity != nil {
			later(r.Activity.Since)
		}
		if r.State == c.RunRunning {
			later(r.Freshness.ObservedAt)
		}
	}
	if v.Flag != nil {
		later(v.Flag.Since)
	}
	return t
}

func sessionCard(v c.SessionView) SessionCard {
	id := v.Session.SessionID
	return SessionCard{ID: id, Title: sessionTitle(v, id), State: sessionState(v), Node: nodeName(v), IsLead: isLead(v), LastActivity: lastActivity(v)}
}

// sortCards puts leads first, then the most recently active.
func sortCards(cards []SessionCard, at map[string]time.Time) {
	sort.SliceStable(cards, func(i, j int) bool {
		if cards[i].IsLead != cards[j].IsLead {
			return cards[i].IsLead
		}
		return at[cards[i].ID].After(at[cards[j].ID])
	})
}

// ---- messages ----

// msgView renders one message for the thread view.
func (sn *snap) msgView(m c.MessageView, askID string) Msg {
	e := m.Envelope
	out := Msg{MessageID: e.MessageID, Text: e.Body.Text, At: msgTime(m), ReplyTo: e.InReplyTo, AskID: askID}
	switch {
	case e.Sender.Kind == c.PrincipalOwner:
		out.From = MsgFrom{Kind: "owner", ID: "owner", Label: "You"}
	case e.Sender.Kind == c.PrincipalSession:
		out.From = MsgFrom{Kind: "session", ID: e.Sender.ID, Label: sn.title(e.Sender.ID)}
	default:
		label := e.Sender.Name
		if label == "" {
			label = "Pyre"
		}
		out.From = MsgFrom{Kind: "session", ID: e.Sender.ID, Label: label}
	}
	out.Delivery = sn.delivery(m)
	return out
}

// delivery says, in plain English, how far a message got.
func (sn *snap) delivery(m c.MessageView) Delivery {
	e, st := m.Envelope, m.Status
	now := sn.now
	at := func(t time.Time) string { return clock(t, now) }
	if e.Recipient.Kind == c.AddrOwner {
		// Mail to the owner: the owner is reading it.
		switch st.Ack {
		case c.AckReplied:
			return Delivery{State: "replied", Summary: "You replied"}
		case c.AckAcknowledged:
			return Delivery{State: "read", Summary: "You marked it handled"}
		}
		return Delivery{State: "stored", Summary: "Received " + at(msgTime(m))}
	}
	rcptID := e.Recipient.ID
	name := sn.title(rcptID)
	var rcpt *c.SessionView
	if v, ok := sn.sessions[rcptID]; ok {
		rcpt = &v
	}
	d := msgstatus.Derive(m, rcpt, now)
	if m.Delivery != nil && m.Delivery.State != "" {
		d = *m.Delivery // the hub's own derivation (send responses carry it)
	}
	sent := "Sent " + at(msgTime(m))
	switch d.State {
	case c.SummaryPending:
		return Delivery{State: "stored", Summary: "Not on the hub yet; Pyre sends it when the hub is reachable"}
	case c.SummaryStored:
		return Delivery{State: "stored", Summary: sent}
	case c.SummaryDelivering:
		return Delivery{State: "queued", Summary: sent + " · delivering to " + name + " now"}
	case c.SummaryQueued:
		return Delivery{State: "queued", Summary: sent + " · waiting: " + waitWhy(d.Summary, name)}
	case c.SummaryHeld:
		return Delivery{State: "failed", Summary: "Not delivered: " + name + " has ended. Resume it to deliver this, or write to another session."}
	case c.SummaryDelivered:
		return Delivery{State: "delivered", Summary: "Delivered to " + name + " at " + at(acceptedAt(m, d))}
	case c.SummaryRead:
		return Delivery{State: "read", Summary: "Read by the agent at " + at(acceptedAt(m, d))}
	case c.SummaryAcknowledged:
		return Delivery{State: "read", Summary: "Read by the agent (acknowledged)"}
	case c.SummaryReplied:
		s := name + " replied"
		if r, ok := sn.mail.msgs[st.AckRef]; ok {
			s += " at " + at(msgTime(r))
		}
		return Delivery{State: "replied", Summary: s}
	case c.SummaryIndeterminate:
		return Delivery{State: "queued", Summary: sent + " · delivery uncertain: it may have reached " + name + ". Check the session before sending it again."}
	case c.SummaryUndeliverable:
		return Delivery{State: "failed", Summary: "Not delivered: " + undeliverableWhy(d, name)}
	}
	return Delivery{State: "stored", Summary: sent}
}

func acceptedAt(m c.MessageView, d c.DeliverySummary) time.Time {
	for i := len(m.Status.Receipts) - 1; i >= 0; i-- {
		r := m.Status.Receipts[i]
		if r.State == c.DeliveryAccepted && c.AcceptanceEvidence(r.Evidence) {
			return r.ObservedAt
		}
	}
	if d.Since != nil {
		return *d.Since
	}
	return m.Status.UpdatedAt
}

// waitWhy names the recipient in msgstatus's reason: "queued: recipient
// is working (x) — will be delivered at its next idle" becomes "<name> is
// working (x); will be delivered at its next idle".
func waitWhy(s, name string) string {
	s = strings.TrimPrefix(s, "queued: ")
	for _, r := range []struct{ from, to string }{
		{"the recipient's", name + "'s"}, {"recipient's", name + "'s"}, {"recipient is", name + " is"},
	} {
		if strings.HasPrefix(s, r.from) {
			s = r.to + strings.TrimPrefix(s, r.from)
			break
		}
	}
	return strings.ReplaceAll(s, " — ", "; ")
}

func undeliverableWhy(d c.DeliverySummary, name string) string {
	switch d.Cause {
	case c.CauseRecipientArchived:
		return name + " is archived."
	case c.CauseRuntimeRejected:
		return name + " refused it; nothing ran, so it is safe to send again."
	case c.CauseExpired:
		return "it waited too long and expired."
	case c.CauseCancelled:
		return "it was cancelled."
	case c.CauseStorageRejected:
		return "the hub rejected it."
	}
	return strings.TrimPrefix(d.Summary, "undeliverable: ")
}

// ---- text ----

// firstLine is the first non-empty line of s, at most n runes.
func firstLine(s string, n int) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return clip(l, n)
		}
	}
	return ""
}

// excerpt is s with whitespace collapsed, at most n runes.
func excerpt(s string, n int) string {
	return clip(strings.Join(strings.Fields(s), " "), n)
}

func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n-1])) + "…"
}

// stripAskTag drops the "[pyre ask] " prefix of the message carrying an
// ask (the thread shows the ask itself).
func stripAskTag(s string) string { return strings.TrimPrefix(s, "[pyre ask] ") }
