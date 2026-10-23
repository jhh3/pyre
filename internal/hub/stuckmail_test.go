package hub

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// W-stuckmail: the hub derives delivery_status on send/get and tells the
// sender once when its mail is stuck behind a failed or unreachable lane.

func (x *harness) stuckNotices(sessionID string) []string {
	x.t.Helper()
	rows, err := x.db.Query(`SELECT envelope FROM messages WHERE sender_kind='system' AND sender_id=? AND recipient_id=?`, stuckSender.ID, sessionID)
	if err != nil {
		x.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

func TestDeliveryStatusAndStuckNotice(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	x.connect(nodeLin, 0)
	lead := x.start(owner, "op_synth-lead", nodeMac)
	lane := x.start(owner, "op_synth-lane", nodeLin)
	leadCtx := as(sesPrincipal(lead.Session.SessionID))

	mv, err := x.h.SendMessage(leadCtx, contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: "op_synth-send"},
		Recipient: contracts.Address{Kind: contracts.AddrSession, ID: lane.Session.SessionID}, Mode: contracts.ModeQueue, Body: contracts.MessageBody{Text: "synthetic task"}})
	if err != nil {
		t.Fatal(err)
	}
	if mv.Delivery == nil || mv.Delivery.State != contracts.SummaryDelivering {
		t.Fatalf("send to an idle, connected lane: %+v", mv.Delivery)
	}
	id := mv.Envelope.MessageID

	// The lane's last turn failed at model capacity.
	r := *x.session(lane.Session.SessionID).Run
	r.State, r.StateReason = contracts.RunWaitingInput, "last turn failed: Selected model is at capacity. · retry 1/4 at 12:14"
	x.report(nodeLin, contracts.ReportRun, r)
	x.clock.Add(11 * time.Minute)
	got := x.msg(id)
	if got.Delivery == nil || got.Delivery.Cause != contracts.CauseRecipientFailed ||
		!strings.Contains(got.Delivery.Summary, "last turn failed (model at capacity); Pyre retries at 12:14") {
		t.Fatalf("failed lane: %+v", got.Delivery)
	}

	// One notice to the sender, never repeated (not even after a restart).
	ctx := context.Background()
	if err := x.h.notifyStuckMail(ctx); err != nil {
		t.Fatal(err)
	}
	n := x.stuckNotices(lead.Session.SessionID)
	if len(n) != 1 || !strings.Contains(n[0], id) || !strings.Contains(n[0], "model at capacity") {
		t.Fatalf("notices: %v", n)
	}
	x.clock.Add(15 * time.Minute)
	_ = x.h.notifyStuckMail(ctx)
	x.restart()
	x.connect(nodeMac, 0)
	_ = x.h.notifyStuckMail(ctx)
	if n := x.stuckNotices(lead.Session.SessionID); len(n) != 1 {
		t.Fatalf("notice repeated: %d", len(n))
	}

	// The lane's machine is disconnected: queued, saying so.
	got = x.msg(id)
	if got.Delivery == nil || got.Delivery.Cause != contracts.CauseNodeDisconnected || !strings.Contains(got.Delivery.Summary, "is disconnected") {
		t.Fatalf("disconnected lane: %+v", got.Delivery)
	}

	// Owner mail is stored, never "delivered".
	om, err := x.h.SendMessage(leadCtx, contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: "op_synth-owner"},
		Recipient: contracts.Address{Kind: contracts.AddrOwner, ID: "owner"}, Mode: contracts.ModeQueue, Body: contracts.MessageBody{Text: "fyi"}, RequestAck: true})
	if err != nil || om.Delivery == nil || om.Delivery.State != contracts.SummaryStored {
		t.Fatalf("owner mail: %v %+v", err, om.Delivery)
	}
	// A report that asks nothing is an update: the sender is told it is
	// not in his inbox.
	up, err := x.h.SendMessage(leadCtx, contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: "op_synth-owner-update"},
		Recipient: contracts.Address{Kind: contracts.AddrOwner, ID: "owner"}, Mode: contracts.ModeQueue, Body: contracts.MessageBody{Text: "fyi"}})
	if err != nil || up.Status.AckRef != contracts.AckRefUpdate || up.Delivery == nil || !strings.Contains(up.Delivery.Summary, "not in his inbox") {
		t.Fatalf("owner update: %v %+v", err, up.Delivery)
	}
}
