package hub

import (
	"context"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// ---- owner mail leaves the inbox when it is done (owner report 2026-10-19) ----

// ackOf is the stored ack state and ref of a message.
func (x *harness) ackOf(id string) (contracts.AckState, string) {
	x.t.Helper()
	var ack string
	var ref *string
	if err := x.db.QueryRow(`SELECT ack, ack_ref FROM messages WHERE message_id=?`, id).Scan(&ack, &ref); err != nil {
		x.t.Fatal(err)
	}
	if ref == nil {
		return contracts.AckState(ack), ""
	}
	return contracts.AckState(ack), *ref
}

// waitAck waits for a background acknowledgement (owner engagement).
func (x *harness) waitAck(id string, want contracts.AckState) string {
	x.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, ref := x.ackOf(id)
		if got == want {
			return ref
		}
		if time.Now().After(deadline) {
			x.t.Fatalf("%s: ack %s (%s), want %s", id, got, ref, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// newAsk is a synthetic ask filed as a new decision (no similarity check).
func newAsk(op, topic, title string) contracts.CreateAskRequest {
	r := synthAsk(op, topic, title, "b")
	r.ConfirmNew = true
	return r
}

func (x *harness) toOwner(p contracts.Principal, op, thread, text string) contracts.MessageView {
	x.t.Helper()
	v, err := x.h.SendMessage(as(p), contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: op},
		Recipient: contracts.Address{Kind: contracts.AddrOwner, ID: "owner"}, ThreadID: thread, Body: contracts.MessageBody{Text: text}})
	if err != nil {
		x.t.Fatal(err)
	}
	return v
}

// Every way an ask closes acknowledges its carrier and the follow-ups in
// its thread; other owner mail is left alone.
func TestAskCloseAcknowledgesItsMail(t *testing.T) {
	f := newChildFixture(t, nil)
	x := f.x
	lead := sesPrincipal(f.parent.Session.SessionID)

	answered := f.ask(lead, newAsk("op_synth-a1", "synthetic-answered", "Ship the synthetic build?")).Ask
	follow := x.toOwner(lead, "op_synth-f1", answered.ThreadID, "Also: the synthetic soak finished.")
	withdrawn := f.ask(lead, newAsk("op_synth-a2", "synthetic-withdrawn", "Rotate the synthetic key?")).Ask
	dismissed := f.ask(lead, newAsk("op_synth-a3", "synthetic-dismissed", "Rename the synthetic bucket?")).Ask
	defaulted := f.ask(lead, newAsk("op_synth-a4", "synthetic-defaulted", "Resize the synthetic pool?")).Ask
	other := x.toOwner(lead, "op_synth-q", "", "Separately: should I also bump the synthetic quota?")
	for _, id := range []string{answered.MessageID, follow.Envelope.MessageID, withdrawn.MessageID, dismissed.MessageID, defaulted.MessageID, other.Envelope.MessageID} {
		if ack, _ := x.ackOf(id); ack != contracts.AckNone {
			t.Fatalf("%s acknowledged before its ask closed: %s", id, ack)
		}
	}

	if _, err := x.h.AnswerAsks(as(owner), contracts.AnswerAsksRequest{Mutation: contracts.Mutation{OperationID: "op_synth-ans"},
		Answers: []contracts.AnswerAskItem{{AskID: answered.AskID, OptionID: "a"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := x.h.WithdrawAsk(as(lead), contracts.WithdrawAskRequest{Mutation: contracts.Mutation{OperationID: "op_synth-wd"}, AskID: withdrawn.AskID, Reason: "no longer needed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := x.h.WithdrawAsk(as(owner), contracts.WithdrawAskRequest{Mutation: contracts.Mutation{OperationID: "op_synth-dm"}, AskID: dismissed.AskID, Reason: contracts.AskDismissReason}); err != nil {
		t.Fatal(err)
	}
	x.clock.Add(4 * time.Hour) // past the default deadline (moved to the end of quiet hours, 8 AM PT)
	if err := x.h.resolveAsks(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{answered.MessageID, follow.Envelope.MessageID, withdrawn.MessageID, dismissed.MessageID, defaulted.MessageID} {
		if ack, ref := x.ackOf(id); ack == contracts.AckNone || (ack == contracts.AckAcknowledged && ref != contracts.AckRefAskClosed) {
			t.Fatalf("%s after its ask closed: %s (%s) %+v", id, ack, ref, x.envelopes(`message_id=?`, id))
		}
	}
	if ack, _ := x.ackOf(other.Envelope.MessageID); ack != contracts.AckNone {
		t.Fatalf("unrelated question acknowledged: %s", ack)
	}
	// The owner's unacked mail is just the open question now.
	page, err := x.h.ListMessages(as(owner), contracts.ListMessagesRequest{Unacked: true})
	if err != nil || len(page.Items) != 1 || page.Items[0].Envelope.MessageID != other.Envelope.MessageID {
		t.Fatalf("unacked owner mail: %v %+v", err, page.Items)
	}
}

// Mail of asks closed before this change (or stored late) is acknowledged
// once by maintenance.
func TestClosedAskMailBackfill(t *testing.T) {
	f := newChildFixture(t, nil)
	x := f.x
	lead := sesPrincipal(f.parent.Session.SessionID)
	a := f.ask(lead, newAsk("op_synth-b1", "synthetic-old", "Ship the synthetic build?")).Ask
	if _, err := x.h.WithdrawAsk(as(lead), contracts.WithdrawAskRequest{Mutation: contracts.Mutation{OperationID: "op_synth-bw"}, AskID: a.AskID}); err != nil {
		t.Fatal(err)
	}
	// As an older hub left it.
	if _, err := x.db.Exec(`UPDATE messages SET ack='none', ack_ref=NULL WHERE message_id=?`, a.MessageID); err != nil {
		t.Fatal(err)
	}
	open := f.ask(lead, newAsk("op_synth-b2", "synthetic-open", "Rotate the synthetic key?")).Ask
	if err := x.h.RunMaintenance(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ack, ref := x.ackOf(a.MessageID); ack != contracts.AckAcknowledged || ref != contracts.AckRefAskClosed {
		t.Fatalf("closed ask's carrier: %s %s", ack, ref)
	}
	if ack, _ := x.ackOf(open.MessageID); ack != contracts.AckNone {
		t.Fatalf("open ask's carrier: %s", ack)
	}
	if !x.h.askMailAcked.Load() {
		t.Fatal("backfill not marked done")
	}
}

// The owner viewing a session's message, typing into it or messaging it
// acknowledges its mail to him so far, except ask threads.
func TestOwnerEngagementAcknowledgesSessionMail(t *testing.T) {
	f := newChildFixture(t, nil)
	x := f.x
	sid := f.parent.Session.SessionID
	lead := sesPrincipal(sid)
	carrier := f.ask(lead, newAsk("op_synth-e0", "synthetic-engaged", "Ship the synthetic build?")).Ask

	// Viewing one message (pyre msg show).
	q1 := x.toOwner(lead, "op_synth-e1", "", "Should I merge the synthetic PR?")
	q2 := x.toOwner(lead, "op_synth-e2", "", "Also: which synthetic region should I use?")
	if _, err := x.h.GetMessage(as(owner), contracts.GetRequest{ID: q2.Envelope.MessageID}); err != nil {
		t.Fatal(err)
	}
	if ref := x.waitAck(q2.Envelope.MessageID, contracts.AckAcknowledged); ref != contracts.AckRefEngaged {
		t.Fatalf("ref %q", ref)
	}
	x.h.wg.Wait()
	if ack, _ := x.ackOf(q1.Envelope.MessageID); ack != contracts.AckNone {
		t.Fatalf("viewing one message acknowledged another: %s", ack)
	}
	// Mail in a thread, or asking to be acknowledged, waits to be opened.
	threaded := x.toOwner(lead, "op_synth-e2t", "thr_synth-other", "In the other thread: should I keep the synthetic flag?")
	asked, err := x.h.SendMessage(as(lead), contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: "op_synth-e2a"},
		Recipient: contracts.Address{Kind: contracts.AddrOwner, ID: "owner"}, RequestAck: true, Body: contracts.MessageBody{Text: "Prod keys rotated."}})
	if err != nil {
		t.Fatal(err)
	}
	// A session reading it is not the owner viewing it.
	x.clock.Add(time.Second)
	q3 := x.toOwner(lead, "op_synth-e3", "", "Should I rotate the synthetic key?")
	if _, err := x.h.GetMessage(as(lead), contracts.GetRequest{ID: q3.Envelope.MessageID}); err != nil {
		t.Fatal(err)
	}
	// Messaging the session.
	x.clock.Add(time.Second)
	if _, err := x.h.SendMessage(as(owner), contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: "op_synth-e4"},
		Recipient: contracts.Address{Kind: contracts.AddrSession, ID: sid}, Body: contracts.MessageBody{Text: "yes, rotate it"}}); err != nil {
		t.Fatal(err)
	}
	x.waitAck(q3.Envelope.MessageID, contracts.AckAcknowledged)
	x.waitAck(q1.Envelope.MessageID, contracts.AckAcknowledged)
	x.h.wg.Wait()
	for _, m := range []contracts.MessageView{threaded, asked} {
		if ack, _ := x.ackOf(m.Envelope.MessageID); ack != contracts.AckNone {
			t.Fatalf("%q acknowledged by engagement: %s", m.Envelope.Body.Text, ack)
		}
	}
	// Typing into it (taking its input lease).
	x.clock.Add(time.Second)
	q4 := x.toOwner(lead, "op_synth-e5", "", "Should I delete the synthetic cache?")
	if _, err := x.h.AcquireLease(as(owner), contracts.LeaseRequest{Mutation: contracts.Mutation{OperationID: "op_synth-e6"}, RunID: f.parent.Run.RunID}); err != nil {
		t.Fatal(err)
	}
	x.waitAck(q4.Envelope.MessageID, contracts.AckAcknowledged)
	x.h.wg.Wait()
	// (The owner's message answered the ask in chat; its carrier closes
	// with the ask, never by engagement.)
	if _, ref := x.ackOf(carrier.MessageID); ref == contracts.AckRefEngaged {
		t.Fatal("an ask's carrier was acknowledged by engagement")
	}
}

// A carrier stored after its ask closed is acknowledged with the thread's
// earlier mail only; a follow-up sent after the decision stays.
func TestLateCarrierLeavesLaterFollowUps(t *testing.T) {
	f := newChildFixture(t, nil)
	x := f.x
	lead := sesPrincipal(f.parent.Session.SessionID)
	a := f.ask(lead, newAsk("op_synth-l1", "synthetic-late", "Ship the synthetic build?")).Ask
	if _, err := x.h.WithdrawAsk(as(lead), contracts.WithdrawAskRequest{Mutation: contracts.Mutation{OperationID: "op_synth-lw"}, AskID: a.AskID}); err != nil {
		t.Fatal(err)
	}
	x.clock.Add(time.Minute)
	later := x.toOwner(lead, "op_synth-l2", a.ThreadID, "New since the decision: should I also tag the synthetic release?")
	if _, err := x.db.Exec(`UPDATE messages SET ack='none', ack_ref=NULL WHERE message_id=?`, a.MessageID); err != nil {
		t.Fatal(err)
	}
	x.h.sendAskMessage(context.Background(), *a)
	if ack, _ := x.ackOf(a.MessageID); ack != contracts.AckAcknowledged {
		t.Fatalf("late carrier: %s", ack)
	}
	if ack, _ := x.ackOf(later.Envelope.MessageID); ack != contracts.AckNone {
		t.Fatalf("follow-up after the decision acknowledged: %s", ack)
	}
}
