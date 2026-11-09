package cli

import (
	"context"
	"slices"
	"strings"

	c "github.com/jhh3/pyre/internal/contracts"
)

// steerAsQueue decides, before the envelope exists, whether a steer to a
// session goes out as an ordinary queued message instead. The hub rejects a
// steer to a live run that does not advertise input.steer (Claude, agy);
// such a run takes queued mail (a Claude session sees it mid-turn through
// its doorbell), so the sender gets that instead of a final rejection. It
// returns the note for the sender, or "" to send the steer unchanged.
//
// A steer to a session with no live run (its run ended, or it archived
// itself when done) goes as queued mail too: the hub holds it and hands it
// to the session's next run, so a resume delivers it. Rejecting it lost the
// instruction once the recipient was resumed (iss_…:
// two steers to a Codex reviewer arrived 2 s after it archived itself and
// its run ended; after unarchive and resume they stayed rejected).
//
// The downgrade happens here, not in hub routing, because the mode is part
// of the message's content digest (message_id dedupe) and of the
// operation's request hash: the stored envelope must say what was
// delivered. A steer to a disconnected node, or when the recipient cannot
// be read, is sent unchanged and the hub rejects it as before (steering is
// immediate-only).
func (o *Ops) steerAsQueue(ctx context.Context, sessionID string) string {
	sv, err := o.API.GetSession(ctx, c.GetRequest{ID: sessionID})
	if err != nil {
		return ""
	}
	r := sv.Run
	if r == nil || r.State.Terminal() {
		if sv.Session.Archived || slices.Contains(sv.Session.Tags, c.TagArchiveWhenDone) {
			return "sent as a queued message, not a steer: the recipient has no live run and is archived; it gets this only if it is unarchived and resumed"
		}
		return "sent as a queued message, not a steer: the recipient has no live run; it gets this when its session is resumed"
	}
	if runDisconnected(r) ||
		c.HasCap(r.Capabilities, c.CapInputSteer) || !c.HasCap(r.Capabilities, c.CapInputQueue) {
		return ""
	}
	who := "this " + harnessName(r) + " session does not take steer"
	if c.HasCap(r.Capabilities, c.CapInputMonitor) {
		return "sent as a queued message, not a steer: " + who + "; it sees queued mail mid-turn"
	}
	return "sent as a queued message, not a steer: " + who + "; it reads queued mail when it can take input"
}

// withSteerNote puts steerAsQueue's note in front of the delivery summary.
func withSteerNote(mv *c.MessageView, note string) {
	if note == "" || mv.Delivery == nil {
		return
	}
	d := *mv.Delivery // a copy: the API may share the pointer
	d.Summary = note + ". " + d.Summary
	mv.Delivery = &d
}

func harnessName(r *c.Run) string {
	switch r.Harness {
	case c.HarnessClaude:
		return "Claude"
	case "":
	default:
		return string(r.Harness)
	}
	if a, _, _ := strings.Cut(r.Adapter, "-"); a != "" {
		return a
	}
	return "recipient"
}

// runDisconnected: the hub reports a non-terminal run on a disconnected node
// with this freshness reason (hub decorateRun).
func runDisconnected(r *c.Run) bool {
	return r.Freshness.Reason == "node disconnected" || strings.HasPrefix(r.StateReason, "node disconnected")
}
