// Package msgstatus derives a message's human-readable delivery status
// (contracts.DeliverySummary) from facts Pyre already records: the
// message's storage/runtime/ack state and receipts, and the recipient
// session's current run (state, reason, node connectivity). It is a pure
// presentation; it never claims delivery without acceptance evidence
// (W-stuckmail). The hub fills MessageView.Delivery with it; clients use it
// as a fallback against older hubs.
package msgstatus

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/messaging/ownermail"
)

// IdleGrace is how long a message to an idle, connected recipient is shown
// as "delivering" before a missing acceptance is reported as a stall.
const IdleGrace = 2 * time.Minute

// NeedsRecipient reports whether Derive needs the recipient session to
// describe m (a session-addressed message still waiting, or one accepted
// but not answered).
func NeedsRecipient(m c.MessageView) bool {
	e, st := m.Envelope, m.Status
	if e.Recipient.Kind != c.AddrSession || e.Mode == c.ModeInfo || st.Storage != c.StorageStored || st.Ack != c.AckNone {
		return false
	}
	switch st.Runtime {
	case c.DeliveryQueued, c.DeliveryNodeReceived, c.DeliveryAccepted:
		return true
	}
	return false
}

// Derive computes the delivery summary at now. rcpt is the recipient
// session's view for session-addressed mail (nil when unavailable).
func Derive(m c.MessageView, rcpt *c.SessionView, now time.Time) c.DeliverySummary {
	e, st := m.Envelope, m.Status
	switch st.Storage {
	case c.StorageRejected:
		return c.DeliverySummary{State: c.SummaryUndeliverable, Cause: c.CauseStorageRejected, Final: true,
			Summary: "undeliverable: the hub rejected it" + paren(st.RuntimeReason)}
	case c.StoragePendingLocal:
		return c.DeliverySummary{State: c.SummaryPending,
			Summary: "pending: not yet stored on the hub" + paren(st.RuntimeReason) + "; it is sent when the hub is reachable"}
	}
	switch st.Ack {
	case c.AckReplied:
		s := c.DeliverySummary{State: c.SummaryReplied, Summary: "replied"}
		if st.AckRef != "" {
			s.Summary += " (" + st.AckRef + ")"
		}
		return s
	case c.AckAcknowledged:
		switch st.AckRef {
		case c.AckRefUpdate:
			return c.DeliverySummary{State: c.SummaryAcknowledged, Since: st.StoredAt,
				Summary: "stored as an update for the owner: it asks nothing of him, so it is not in his inbox and nobody is notified (he sees it in your session's and project's history). For a decision use ask_owner; if he must read it, send with request_ack"}
		case c.AckRefAskClosed:
			return c.DeliverySummary{State: c.SummaryAcknowledged, Summary: "acknowledged: its ask was closed"}
		case c.AckRefEngaged:
			return c.DeliverySummary{State: c.SummaryAcknowledged, Summary: "acknowledged: the owner engaged with the session"}
		}
		return c.DeliverySummary{State: c.SummaryAcknowledged, Summary: "acknowledged by the recipient"}
	}
	stored := st.StoredAt
	switch e.Recipient.Kind {
	case c.AddrOwner:
		if ownermail.Question(e) {
			return c.DeliverySummary{State: c.SummaryStored, Since: stored,
				Summary: "in the owner's inbox (not acknowledged yet). It reads as a question: decisions go in ask_owner (a topic, 2–4 options, a default), which he answers in one tap and which merges repeats; a message stays one more thing to read"}
		}
		return c.DeliverySummary{State: c.SummaryStored, Since: stored, Summary: "in the owner's inbox (not acknowledged yet)"}
	case c.AddrChannel, c.AddrProject:
		return c.DeliverySummary{State: c.SummaryStored, Since: stored, Summary: "stored in " + string(e.Recipient.Kind) + " " + e.Recipient.ID + " (not injected into a runtime)"}
	}
	if e.Mode == c.ModeInfo {
		return c.DeliverySummary{State: c.SummaryStored, Since: stored, Summary: "stored (info: never injected into a runtime)"}
	}
	switch st.Runtime {
	case c.DeliveryAccepted:
		return accepted(m, rcpt, now)
	case c.DeliveryIndeterminate:
		run := ""
		if st.BoundRunID != "" {
			run = " by run " + st.BoundRunID
		}
		return c.DeliverySummary{State: c.SummaryIndeterminate, Summary: "indeterminate: it may have been accepted" + run +
			"; Pyre never retries it — inspect the recipient, then resolve_delivery or reissue"}
	case c.DeliveryRejected:
		return c.DeliverySummary{State: c.SummaryUndeliverable, Cause: c.CauseRuntimeRejected, Final: true,
			Summary: "undeliverable: the recipient's runtime refused it" + paren(st.RuntimeReason) + "; nothing ran, safe to reissue"}
	case c.DeliveryExpired:
		return c.DeliverySummary{State: c.SummaryUndeliverable, Cause: c.CauseExpired, Final: true,
			Summary: "undeliverable: retention ended before it could be delivered"}
	case c.DeliveryCancelled:
		return c.DeliverySummary{State: c.SummaryUndeliverable, Cause: c.CauseCancelled, Final: true,
			Summary: "undeliverable: cancelled" + paren(st.RuntimeReason)}
	case c.DeliveryNotApplicable:
		return c.DeliverySummary{State: c.SummaryStored, Since: stored, Summary: "stored (not delivered to a runtime" + colon(st.RuntimeReason) + ")"}
	case c.DeliveryAttempting:
		u := st.UpdatedAt
		return c.DeliverySummary{State: c.SummaryDelivering, Since: &u, Summary: "delivering: Pyre is submitting it to the recipient now (not yet accepted)"}
	}
	return waiting(m, rcpt, now)
}

// accepted describes runtime-accepted mail (evidence recorded).
func accepted(m c.MessageView, rcpt *c.SessionView, now time.Time) c.DeliverySummary {
	st := m.Status
	at := st.UpdatedAt
	pulled := false
	for i := len(st.Receipts) - 1; i >= 0; i-- {
		r := st.Receipts[i]
		if r.State == c.DeliveryAccepted && c.AcceptanceEvidence(r.Evidence) {
			at, pulled = r.ObservedAt, c.PullEvidence(r.Evidence)
			break
		}
	}
	s := c.DeliverySummary{State: c.SummaryDelivered, Since: &at}
	switch {
	case pulled:
		s.State, s.Summary = c.SummaryRead, "read by the recipient ("+ago(now, at)+")"
	case st.ConsumedAt != nil:
		s.Summary = "delivered (taken into a turn " + ago(now, *st.ConsumedAt) + ")"
	default:
		s.Summary = "delivered (accepted " + ago(now, at) + ")"
	}
	if ended(rcpt) {
		s.Summary += "; the recipient session has since ended without replying"
		s.Final = true
	} else if rcpt != nil && rcpt.Session.Archived {
		s.Summary += "; the recipient has since been archived"
		s.Final = true
	}
	return s
}

func ended(rcpt *c.SessionView) bool {
	return rcpt != nil && (rcpt.Run == nil || rcpt.Run.State.Terminal())
}

// waiting describes queued (or node-received) mail from the recipient's
// current state.
func waiting(m c.MessageView, rcpt *c.SessionView, now time.Time) c.DeliverySummary {
	st := m.Status
	since := st.StoredAt
	age := time.Duration(0)
	if since != nil {
		age = now.Sub(*since)
	}
	q := func(cause, text string) c.DeliverySummary {
		if age >= time.Minute {
			text += " (queued " + dur(age) + ")"
		}
		return c.DeliverySummary{State: c.SummaryQueued, Cause: cause, Summary: "queued: " + text, Since: since}
	}
	if rcpt == nil {
		return q(c.CauseRecipientUnknown, "the recipient's state is unavailable"+colonReason(st.RuntimeReason))
	}
	if rcpt.Session.Archived {
		return c.DeliverySummary{State: c.SummaryUndeliverable, Cause: c.CauseRecipientArchived, Final: true, Since: since,
			Summary: "undeliverable: recipient archived — message someone else (or unarchive and resume it)"}
	}
	r := rcpt.Run
	if r == nil || r.State.Terminal() {
		s := c.DeliverySummary{State: c.SummaryHeld, Cause: c.CauseRecipientEnded, Final: true, Since: since}
		why := ""
		if r != nil {
			s.Since = r.EndedAt
			if r.State == c.RunFailed {
				why = " (its run failed" + colon(r.StateReason) + ")"
			}
		}
		s.Summary = "held: recipient session has ended" + why + " — resume it or message someone else"
		return s
	}
	machine := "its machine"
	if rcpt.NodeName != "" {
		machine = "machine " + rcpt.NodeName
	} else if r.NodeID != "" {
		machine = "machine " + r.NodeID
	}
	if disconnected(r) {
		last := ""
		if !r.Freshness.ObservedAt.IsZero() {
			last = ", last seen " + ago(now, r.Freshness.ObservedAt)
		}
		return q(c.CauseNodeDisconnected, "recipient's "+machine+" is disconnected"+last+" — delivered when it reconnects")
	}
	switch r.State {
	case c.RunStarting:
		return q(c.CauseRecipientStarting, "recipient is starting — delivered once it is ready for input")
	case c.RunExiting:
		return q(c.CauseRecipientExiting, "recipient is exiting — resume it or message someone else")
	case c.RunUnknown:
		return q(c.CauseRecipientUnknown, "recipient's state is unknown"+paren(r.StateReason)+" — delivered once Pyre sees it idle")
	case c.RunWaitingApproval:
		return q(c.CauseWaitingApproval, "recipient is waiting on an approval"+paren(r.StateReason)+" — delivered once it is answered")
	case c.RunRunning:
		doing := r.StateReason
		if r.Activity != nil && r.Activity.Text != "" {
			doing = r.Activity.Text
		}
		if doing == "" {
			doing = "turn active"
		}
		return q(c.CauseRecipientWorking, "recipient is working ("+clip(doing, 60)+") — will be delivered at its next idle")
	}
	// Idle (waiting_input / waiting_on_background): it can take input.
	if failure, retry, ok := lastTurnFailed(r.StateReason); ok {
		text := "recipient's last turn failed (" + failure + ")"
		if retry != "" {
			text += "; Pyre retries at " + retry
		}
		if age < IdleGrace && st.RuntimeReason == "" {
			text += "; it takes input, so delivery should follow"
		} else if st.RuntimeReason != "" {
			text += "; last delivery note: " + clip(st.RuntimeReason, 120)
		}
		return q(c.CauseRecipientFailed, text)
	}
	if reason := st.RuntimeReason; reason != "" && !staleBusyReason(reason) {
		return q(c.CauseInputBlocked, "recipient is idle but its input is held: "+clip(reason, 160))
	}
	if age < IdleGrace {
		return c.DeliverySummary{State: c.SummaryDelivering, Since: since, Summary: "delivering: recipient is idle and connected; Pyre is delivering it now (not yet accepted)"}
	}
	return q(c.CauseInputBlocked, "recipient is idle but has not accepted it yet; Pyre keeps trying")
}

// disconnected: the hub presents a run on a disconnected node as unknown
// with a stale freshness (hub decorateRun).
func disconnected(r *c.Run) bool {
	return r.Freshness.Reason == "node disconnected" || strings.HasPrefix(r.StateReason, "node disconnected")
}

// staleBusyReason: a queued reason that does not explain a wait once the
// recipient is idle: routing notes (hub routing, node receipt) or one
// recorded while the recipient was busy.
func staleBusyReason(reason string) bool {
	l := strings.ToLower(reason)
	for _, s := range []string{"queued for delivery to run", "committed the message to its inbox", "queued until a run starts",
		"is running", "turn active", "not ready for input: running", "is starting", "is unknown", "no live run", "has no runtime adapter"} {
		if strings.Contains(l, s) {
			return true
		}
	}
	return false
}

var retryRE = regexp.MustCompile(`retry (\d+)/(\d+) at (\d{1,2}:\d{2})`)

// lastTurnFailed parses the Codex adapter's reason for an idle thread whose
// last turn failed ("last turn failed: <error> · retry 1/4 at 19:52").
func lastTurnFailed(reason string) (failure, retry string, ok bool) {
	if !strings.HasPrefix(reason, "last turn failed") {
		return "", "", false
	}
	rest := strings.TrimPrefix(reason, "last turn failed")
	if i := strings.Index(rest, " · "); i >= 0 {
		if mm := retryRE.FindStringSubmatch(rest[i:]); mm != nil {
			retry = mm[3] + " (automatic retry " + mm[1] + " of " + mm[2] + ")"
		}
		rest = rest[:i]
	}
	msg := strings.TrimSpace(strings.TrimPrefix(rest, ":"))
	l := strings.ToLower(msg)
	switch {
	case msg == "":
		failure = "error not reported"
	case strings.Contains(l, "at capacity") || strings.Contains(l, "overloaded"):
		failure = "model at capacity"
	default:
		failure = clip(msg, 80)
	}
	return failure, retry, true
}

func paren(s string) string {
	if s == "" {
		return ""
	}
	return " (" + clip(s, 160) + ")"
}

func colon(s string) string {
	if s == "" {
		return ""
	}
	return ": " + clip(s, 160)
}

func colonReason(s string) string {
	if s == "" {
		return ""
	}
	return "; last delivery note: " + clip(s, 160)
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func ago(now, t time.Time) string {
	d := now.Sub(t)
	if d < 0 {
		d = 0
	}
	return dur(d) + " ago"
}

func dur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		h := int(d.Hours())
		if m := int(d.Minutes()) % 60; m != 0 && h < 10 {
			return fmt.Sprintf("%dh%dm", h, m)
		}
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
