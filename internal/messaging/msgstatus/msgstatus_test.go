package msgstatus

import (
	"strings"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

var t0 = time.Date(2026, 10, 8, 19, 0, 0, 0, time.UTC)

func queuedMsg(storedAgo time.Duration, reason string) c.MessageView {
	st := t0.Add(-storedAgo)
	return c.MessageView{
		Envelope: c.MessageEnvelope{MessageID: "msg_synth-1", Recipient: c.Address{Kind: c.AddrSession, ID: "ses_synth-lane"}, Mode: c.ModeQueue},
		Status:   c.DeliveryStatus{Storage: c.StorageStored, StoredAt: &st, Runtime: c.DeliveryQueued, RuntimeReason: reason, Ack: c.AckNone, UpdatedAt: st},
	}
}

func lane(state c.RunState, reason string) *c.SessionView {
	return &c.SessionView{Session: c.Session{SessionID: "ses_synth-lane", Title: "synthetic lane", CurrentRunID: "run_synth-1"},
		NodeName: "synth-server", Run: &c.Run{RunID: "run_synth-1", NodeID: "node_synth-server", State: state, StateReason: reason, Freshness: c.Freshness{ObservedAt: t0}}}
}

func TestDeriveRecipientStates(t *testing.T) {
	disc := lane(c.RunUnknown, "node disconnected; last observed waiting_input")
	disc.Run.Freshness = c.Freshness{ObservedAt: t0.Add(-5 * time.Minute), Stale: true, Reason: "node disconnected"}
	ended := lane(c.RunExited, "")
	end := t0.Add(-time.Hour)
	ended.Run.EndedAt = &end
	archived := lane(c.RunExited, "")
	archived.Session.Archived = true
	working := lane(c.RunRunning, "turn active")
	working.Run.Activity = &c.RunActivity{Text: "running tests", Since: t0}

	cases := []struct {
		name   string
		m      c.MessageView
		rcpt   *c.SessionView
		state  c.DeliverySummaryState
		cause  string
		final  bool
		substr []string
	}{
		{"idle connected, fresh", queuedMsg(3*time.Second, ""), lane(c.RunWaitingInput, "thread idle"), c.SummaryDelivering, "", false,
			[]string{"delivering: recipient is idle", "not yet accepted"}},
		{"idle, input held", queuedMsg(30*time.Second, "a terminal client is attached; raw delivery blocked until detach or lease"), lane(c.RunWaitingInput, "idle"),
			c.SummaryQueued, c.CauseInputBlocked, false, []string{"input is held: a terminal client is attached"}},
		{"idle, stale busy reason", queuedMsg(10*time.Second, "run run_synth-1 is running"), lane(c.RunWaitingInput, "idle"), c.SummaryDelivering, "", false, nil},
		{"idle but stalled", queuedMsg(5*time.Minute, ""), lane(c.RunWaitingInput, "idle"), c.SummaryQueued, c.CauseInputBlocked, false,
			[]string{"has not accepted it yet", "(queued 5m)"}},
		{"working", queuedMsg(20*time.Second, ""), working, c.SummaryQueued, c.CauseRecipientWorking, false,
			[]string{"queued: recipient is working (running tests) — will be delivered at its next idle"}},
		{"failed turn at capacity", queuedMsg(38*time.Minute, ""),
			lane(c.RunWaitingInput, "last turn failed: Selected model is at capacity. Please try a different model. · retry 2/4 at 19:52"),
			c.SummaryQueued, c.CauseRecipientFailed, false,
			[]string{"queued: recipient's last turn failed (model at capacity); Pyre retries at 19:52 (automatic retry 2 of 4)", "(queued 38m)"}},
		{"failed turn other error", queuedMsg(time.Second, ""), lane(c.RunWaitingInput, "last turn failed: stream disconnected"),
			c.SummaryQueued, c.CauseRecipientFailed, false, []string{"last turn failed (stream disconnected)", "delivery should follow"}},
		{"disconnected", queuedMsg(12*time.Minute, "run run_synth-1 is unknown"), disc, c.SummaryQueued, c.CauseNodeDisconnected, false,
			[]string{"queued: recipient's machine synth-server is disconnected, last seen 5m ago"}},
		{"unknown", queuedMsg(time.Minute, ""), lane(c.RunUnknown, "thread status not yet known"), c.SummaryQueued, c.CauseRecipientUnknown, false,
			[]string{"state is unknown (thread status not yet known)"}},
		{"approval", queuedMsg(time.Second, ""), lane(c.RunWaitingApproval, "approval pending"), c.SummaryQueued, c.CauseWaitingApproval, false,
			[]string{"queued: recipient is waiting on an approval"}},
		{"ended", queuedMsg(time.Minute, ""), ended, c.SummaryHeld, c.CauseRecipientEnded, true,
			[]string{"held: recipient session has ended — resume it or message someone else"}},
		{"archived", queuedMsg(time.Minute, ""), archived, c.SummaryUndeliverable, c.CauseRecipientArchived, true, []string{"undeliverable: recipient archived"}},
		{"no recipient view", queuedMsg(time.Second, ""), nil, c.SummaryQueued, c.CauseRecipientUnknown, false, []string{"unavailable"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := Derive(tc.m, tc.rcpt, t0)
			if d.State != tc.state || d.Cause != tc.cause || d.Final != tc.final {
				t.Fatalf("got %+v", d)
			}
			for _, s := range tc.substr {
				if !strings.Contains(d.Summary, s) {
					t.Fatalf("summary %q lacks %q", d.Summary, s)
				}
			}
			if strings.Contains(d.Summary, "delivered (") {
				t.Fatalf("claims delivery without acceptance: %q", d.Summary)
			}
		})
	}
}

func TestDeriveMessageFacts(t *testing.T) {
	acc := queuedMsg(time.Minute, "")
	acc.Status.Runtime = c.DeliveryAccepted
	acc.Status.Receipts = []c.DeliveryReceipt{{State: c.DeliveryAccepted, Evidence: c.EvidenceCodexUserMessage, ObservedAt: t0.Add(-12 * time.Second)}}
	if d := Derive(acc, lane(c.RunRunning, ""), t0); d.State != c.SummaryDelivered || d.Summary != "delivered (accepted 12s ago)" || d.Final {
		t.Fatalf("accepted: %+v", d)
	}
	if d := Derive(acc, lane(c.RunExited, ""), t0); !d.Final || !strings.Contains(d.Summary, "since ended without replying") {
		t.Fatalf("accepted then ended: %+v", d)
	}
	read := acc
	read.Status.Receipts = []c.DeliveryReceipt{{State: c.DeliveryAccepted, Evidence: c.EvidencePyreRead, ObservedAt: t0.Add(-time.Minute)}}
	if d := Derive(read, nil, t0); d.State != c.SummaryRead || !strings.HasPrefix(d.Summary, "read") {
		t.Fatalf("read: %+v", d)
	}
	// A terminal write alone is never acceptance: the state stays whatever
	// the hub recorded (queued here), never "delivered".
	weak := queuedMsg(time.Second, "")
	weak.Status.Receipts = []c.DeliveryReceipt{{State: c.DeliveryQueued, Evidence: c.EvidenceTerminalWrite, ObservedAt: t0}}
	if d := Derive(weak, lane(c.RunRunning, ""), t0); d.State == c.SummaryDelivered {
		t.Fatalf("terminal write is not delivery: %+v", d)
	}
	rep := acc
	rep.Status.Ack, rep.Status.AckRef = c.AckReplied, "msg_synth-reply"
	if d := Derive(rep, nil, t0); d.State != c.SummaryReplied || d.Summary != "replied (msg_synth-reply)" {
		t.Fatalf("replied: %+v", d)
	}
	rej := queuedMsg(time.Second, "unsupported: no steering")
	rej.Status.Runtime = c.DeliveryRejected
	if d := Derive(rej, nil, t0); d.State != c.SummaryUndeliverable || !d.Final || !strings.Contains(d.Summary, "safe to reissue") {
		t.Fatalf("rejected: %+v", d)
	}
	ind := queuedMsg(time.Second, "")
	ind.Status.Runtime, ind.Status.BoundRunID = c.DeliveryIndeterminate, "run_synth-1"
	if d := Derive(ind, nil, t0); d.State != c.SummaryIndeterminate || d.Final {
		t.Fatalf("indeterminate: %+v", d)
	}
	own := queuedMsg(time.Second, "")
	own.Envelope.Recipient = c.Address{Kind: c.AddrOwner, ID: "owner"}
	own.Status.Runtime = c.DeliveryNotApplicable
	if d := Derive(own, nil, t0); d.State != c.SummaryStored || NeedsRecipient(own) {
		t.Fatalf("owner: %+v", d)
	}
	pend := queuedMsg(time.Second, "hub unreachable; held in node outbox node_synth")
	pend.Status.Storage = c.StoragePendingLocal
	if d := Derive(pend, nil, t0); d.State != c.SummaryPending {
		t.Fatalf("pending: %+v", d)
	}
}
