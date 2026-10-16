package hub

import (
	"encoding/json"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

// TestRecipientReadWithdrawsPaste: the recipient session reading a message
// through the API (get_message / inbox via messages.mark_read), acking it,
// or replying to it records acceptance (pyre.read) and withdraws the node's
// paste delivery with a read payload. The owner reading or acking it does
// not, and settled deliveries never change (W-delivery).
func TestRecipientReadWithdrawsPaste(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	v := x.start(owner, "op_1", nodeMac)
	sid := v.Session.SessionID
	self := contracts.Principal{Kind: contracts.PrincipalSession, ID: sid}
	peer := contracts.Principal{Kind: contracts.PrincipalSession, ID: "ses_synth-peer"}
	send := func(op string) string {
		m, err := x.h.SendMessage(as(owner), contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: op},
			Recipient: contracts.Address{Kind: contracts.AddrSession, ID: sid}, Body: contracts.MessageBody{Text: "synthetic " + op}})
		if err != nil {
			t.Fatal(err)
		}
		return m.Envelope.MessageID
	}
	cancels := func() []map[string]string {
		var out []map[string]string
		for _, c := range x.ch.sentKinds(nodeMac, contracts.CmdCancelDelivery) {
			var p map[string]string
			_ = json.Unmarshal(c.Payload, &p)
			out = append(out, p)
		}
		return out
	}

	read := send("op_read")
	// The owner reading it (TUI, CLI) and another session marking it are no
	// delivery to the recipient's runtime.
	if r, err := x.h.MarkMessagesRead(as(owner), contracts.MarkMessagesReadRequest{MessageIDs: []string{read}}); err != nil || len(r.Recorded) != 0 {
		t.Fatalf("owner read recorded: %+v %v", r, err)
	}
	if r, _ := x.h.MarkMessagesRead(as(peer), contracts.MarkMessagesReadRequest{MessageIDs: []string{read}}); len(r.Recorded) != 0 {
		t.Fatalf("foreign session read recorded: %+v", r)
	}
	if got := x.msg(read).Status.Runtime; got != contracts.DeliveryQueued {
		t.Fatalf("runtime after foreign reads = %s", got)
	}
	r, err := x.h.MarkMessagesRead(as(self), contracts.MarkMessagesReadRequest{MessageIDs: []string{read}, Via: "mcp get_message"})
	if err != nil || len(r.Recorded) != 1 {
		t.Fatalf("recipient read: %+v %v", r, err)
	}
	m := x.msg(read)
	last := m.Status.Receipts[len(m.Status.Receipts)-1]
	if m.Status.Runtime != contracts.DeliveryAccepted || m.Status.ConsumedAt == nil || m.Status.BoundRunID != v.Run.RunID ||
		last.Evidence != contracts.EvidencePyreRead || last.State != contracts.DeliveryAccepted {
		t.Fatalf("after read: %+v", m.Status)
	}
	cs := cancels()
	if len(cs) != 1 || cs[0]["message_id"] != read || cs[0]["read"] != string(contracts.EvidencePyreRead) || cs[0]["run_id"] != v.Run.RunID {
		t.Fatalf("cancel_delivery = %+v", cs)
	}
	// Idempotent: a second read changes nothing and sends nothing.
	if r, _ := x.h.MarkMessagesRead(as(self), contracts.MarkMessagesReadRequest{MessageIDs: []string{read}}); len(r.Recorded) != 0 || len(cancels()) != 1 {
		t.Fatalf("second read: %+v cancels=%d", r, len(cancels()))
	}

	// Ack by the recipient session also withdraws the paste.
	acked := send("op_ack")
	if _, err := x.h.AckMessage(as(self), contracts.AckMessageRequest{Mutation: contracts.Mutation{OperationID: "op_a"}, MessageID: acked}); err != nil {
		t.Fatal(err)
	}
	if got := x.msg(acked).Status.Runtime; got != contracts.DeliveryAccepted || len(cancels()) != 2 {
		t.Fatalf("ack: runtime=%s cancels=%d", got, len(cancels()))
	}
	// The owner acking does not (the runtime never saw it).
	ownerAck := send("op_oack")
	if _, err := x.h.AckMessage(as(owner), contracts.AckMessageRequest{Mutation: contracts.Mutation{OperationID: "op_oa"}, MessageID: ownerAck}); err != nil {
		t.Fatal(err)
	}
	if got := x.msg(ownerAck).Status.Runtime; got != contracts.DeliveryQueued {
		t.Fatalf("owner ack changed runtime: %s", got)
	}
	// A reply from the recipient proves it has the original.
	replied := send("op_rep")
	if _, err := x.h.SendMessage(as(self), contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: "op_r"}, InReplyTo: replied,
		Recipient: contracts.Address{Kind: contracts.AddrOwner, ID: "owner"}, Body: contracts.MessageBody{Text: "synthetic reply"}}); err != nil {
		t.Fatal(err)
	}
	if got := x.msg(replied).Status.Runtime; got != contracts.DeliveryAccepted {
		t.Fatalf("reply: runtime=%s", got)
	}
	// An attempt in flight is left to finish on its own.
	busy := send("op_busy")
	x.report(nodeMac, contracts.ReportReceipt, contracts.DeliveryReceipt{MessageID: busy, RunID: v.Run.RunID, State: contracts.DeliveryAttempting, Evidence: contracts.EvidenceNone})
	n := len(cancels())
	if r, _ := x.h.MarkMessagesRead(as(self), contracts.MarkMessagesReadRequest{MessageIDs: []string{busy}}); len(r.Recorded) != 0 || len(cancels()) != n {
		t.Fatalf("attempting message withdrawn: %+v", r)
	}
	if got := x.msg(busy).Status.Runtime; got != contracts.DeliveryAttempting {
		t.Fatalf("attempting changed: %s", got)
	}
}

// A message the node already stored (deliver command completed) and is
// holding, e.g. behind the owner's input lease, still gets the read
// withdrawal (W-delivery2, incident 2026-10-07: the completed command was
// skipped, no cancel_delivery went out, and the node pasted it 8 h later).
func TestRecipientReadWithdrawsNodeHeldMessage(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	v := x.start(owner, "op_1", nodeMac)
	sid := v.Session.SessionID
	self := contracts.Principal{Kind: contracts.PrincipalSession, ID: sid}
	m, err := x.h.SendMessage(as(owner), contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: "op_held"},
		Recipient: contracts.Address{Kind: contracts.AddrSession, ID: sid}, Body: contracts.MessageBody{Text: "synthetic held"}})
	if err != nil {
		t.Fatal(err)
	}
	id := m.Envelope.MessageID
	dl := x.ch.sentKinds(nodeMac, contracts.CmdDeliver)
	if len(dl) == 0 {
		t.Fatal("no deliver command")
	}
	x.result(nodeMac, dl[len(dl)-1], contracts.OutcomeSucceeded, contracts.DeliveryReceipt{MessageID: id, SessionID: sid, NodeID: nodeMac, State: contracts.DeliveryNodeReceived})
	if got := x.msg(id).Status.Runtime; got != contracts.DeliveryNodeReceived {
		t.Fatalf("runtime after node stored it = %s", got)
	}
	if r, err := x.h.MarkMessagesRead(as(self), contracts.MarkMessagesReadRequest{MessageIDs: []string{id}, Via: "get_message"}); err != nil || len(r.Recorded) != 1 {
		t.Fatalf("read: %+v %v", r, err)
	}
	var got []map[string]string
	for _, c := range x.ch.sentKinds(nodeMac, contracts.CmdCancelDelivery) {
		var p map[string]string
		_ = json.Unmarshal(c.Payload, &p)
		got = append(got, p)
	}
	if len(got) != 1 || got[0]["message_id"] != id || got[0]["read"] != string(contracts.EvidencePyreRead) {
		t.Fatalf("cancel_delivery to the holding node = %+v", got)
	}
}
