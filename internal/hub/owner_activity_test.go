package hub

import (
	"context"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// ownerActiveAt polls the session until its owner activity is want (the
// write runs off the request path).
func (x *harness) ownerActiveAt(sid string, want time.Time) {
	x.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := x.session(sid).Session.OwnerActiveAt
		if got != nil && got.Equal(want) {
			return
		}
		if time.Now().After(deadline) {
			x.t.Fatalf("owner_active_at = %v, want %v", got, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// sessionUpdates counts session.updated events for one session.
func (x *harness) sessionUpdates(sid string) int {
	var n int
	x.db.QueryRow(`SELECT COUNT(*) FROM events WHERE kind=? AND session_id=?`, string(contracts.EvSessionUpdated), sid).Scan(&n)
	return n
}

func TestOwnerActiveAtMessages(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	sid := x.start(owner, "op_1", nodeMac).Session.SessionID
	send := func(p contracts.Principal, op string) {
		t.Helper()
		if _, err := x.h.SendMessage(as(p), contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: op},
			Recipient: contracts.Address{Kind: contracts.AddrSession, ID: sid}, Body: contracts.MessageBody{Text: "synthetic " + op}}); err != nil {
			t.Fatal(err)
		}
	}

	// A session messaging it (even on the owner's behalf) is not the owner.
	send(sesPrincipal("ses_synth-peer"), "op_peer")
	if got := x.session(sid).Session.OwnerActiveAt; got != nil {
		t.Fatalf("session message set owner_active_at = %v", got)
	}
	if x.h.ownerActive(sesPrincipal("ses_synth-peer"), sid) {
		t.Fatal("a session principal scheduled an owner-activity write")
	}

	// The owner's message marks it and patches the row via session.updated.
	before := x.sessionUpdates(sid)
	t1 := x.clock.Now()
	send(owner, "op_owner1")
	x.ownerActiveAt(sid, t1)
	if n := x.sessionUpdates(sid); n != before+1 {
		t.Fatalf("session.updated events = %d, want %d", n, before+1)
	}

	// Throttle: a second message within the minute writes nothing.
	x.clock.Add(30 * time.Second)
	if x.h.ownerActive(owner, sid) {
		t.Fatal("second mark within a minute was scheduled")
	}
	send(owner, "op_owner2")
	// The DB guard holds even when the in-memory throttle is empty (restart).
	if changed, err := x.h.markOwnerActive(context.Background(), sid, x.clock.Now()); changed || err != nil {
		t.Fatalf("guarded write = %v %v", changed, err)
	}
	if n := x.sessionUpdates(sid); n != before+1 {
		t.Fatalf("throttled sends wrote: session.updated events = %d, want %d", n, before+1)
	}
	x.ownerActiveAt(sid, t1)

	// A reply to the session's message counts too, once the minute passed.
	x.clock.Add(31 * time.Second)
	m, err := x.h.SendMessage(as(sesPrincipal(sid)), contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: "op_q"},
		Recipient: contracts.Address{Kind: contracts.AddrOwner, ID: "owner"}, Body: contracts.MessageBody{Text: "synthetic question"}})
	if err != nil {
		t.Fatal(err)
	}
	t2 := x.clock.Now()
	if _, err := x.h.SendMessage(as(owner), contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: "op_reply"}, InReplyTo: m.Envelope.MessageID,
		Recipient: contracts.Address{Kind: contracts.AddrSession, ID: sid}, Body: contracts.MessageBody{Text: "synthetic answer"}}); err != nil {
		t.Fatal(err)
	}
	x.ownerActiveAt(sid, t2)

	// sessions.list and sessions.get_many carry it; it survives a restart.
	page, err := x.h.ListSessions(as(owner), contracts.ListSessionsRequest{})
	if err != nil || len(page.Items) != 1 || page.Items[0].Session.OwnerActiveAt == nil || !page.Items[0].Session.OwnerActiveAt.Equal(t2) {
		t.Fatalf("list = %+v %v", page.Items, err)
	}
	many, err := x.h.GetManySessions(as(owner), contracts.GetManySessionsRequest{IDs: []string{sid}})
	if err != nil || len(many.Items) != 1 || many.Items[0].Session.OwnerActiveAt == nil || !many.Items[0].Session.OwnerActiveAt.Equal(t2) {
		t.Fatalf("get_many = %+v %v", many.Items, err)
	}
	x.restart()
	x.ownerActiveAt(sid, t2)
}

func TestOwnerActiveAtTyping(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	typed := x.start(owner, "op_1", nodeMac)
	leased := x.start(owner, "op_2", nodeMac)
	untouched := x.start(owner, "op_3", nodeMac)

	// Raw terminal input from the owner; a session principal's does not count.
	if err := x.h.TerminalInputAllowed(context.Background(), sesPrincipal("ses_synth-peer"), typed.Run.RunID); err != nil {
		t.Fatal(err)
	}
	if got := x.session(typed.Session.SessionID).Session.OwnerActiveAt; got != nil {
		t.Fatalf("session input set owner_active_at = %v", got)
	}
	t1 := x.clock.Now()
	if err := x.h.TerminalInputAllowed(context.Background(), owner, typed.Run.RunID); err != nil {
		t.Fatal(err)
	}
	x.ownerActiveAt(typed.Session.SessionID, t1)

	// Taking the input lease.
	x.clock.Add(time.Second)
	t2 := x.clock.Now()
	if _, err := x.h.AcquireLease(as(owner), contracts.LeaseRequest{Mutation: contracts.Mutation{OperationID: "op_lease"}, RunID: leased.Run.RunID}); err != nil {
		t.Fatal(err)
	}
	x.ownerActiveAt(leased.Session.SessionID, t2)

	if got := x.session(untouched.Session.SessionID).Session.OwnerActiveAt; got != nil {
		t.Fatalf("untouched session owner_active_at = %v", got)
	}
}
