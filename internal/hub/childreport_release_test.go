package hub

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// iss_…: a parent mid-turn that reads its inbox
// gets the reports held for it ("batched: the parent is mid-turn") at
// once, as at turn end (one digest for several); one that does not look
// keeps the batching. Only the parent session itself releases them.
func TestChildReportsReleasedWhenParentReadsInbox(t *testing.T) {
	f := newChildFixture(t, nil)
	second := f.startChild("op_synth-child-2", nil)
	parent := f.parent.Session.SessionID
	f.observeSession(parent, contracts.RunRunning) // the parent is mid-turn
	for _, id := range []string{f.child.Session.SessionID, second.Session.SessionID} {
		f.observeSession(id, contracts.RunRunning)
		f.observeSession(id, contracts.RunWaitingInput)
	}
	f.process()
	if n := len(f.allToParent()); n != 0 {
		t.Fatalf("a busy parent got %d messages: %v", n, f.statuses())
	}
	for _, st := range f.statuses() {
		if !strings.Contains(st, reportBatchedDetail) {
			t.Fatalf("not batched: %v", f.statuses())
		}
	}
	ctx := context.Background()
	as := func(p contracts.Principal) context.Context { return contracts.WithPrincipal(ctx, p) }

	// Neither the owner nor another session releases them.
	for _, p := range []contracts.Principal{owner, {Kind: contracts.PrincipalSession, ID: f.child.Session.SessionID},
		{Kind: contracts.PrincipalSession, ID: parent, OnBehalfOf: &owner}} {
		if r, err := f.x.h.ReleaseChildReports(as(p), contracts.ReleaseChildReportsRequest{}); err != nil || r.Released != 0 {
			t.Fatalf("%+v released %+v %v", p, r, err)
		}
	}
	if n := len(f.allToParent()); n != 0 {
		t.Fatalf("released by someone else: %v", f.statuses())
	}

	// The parent reads its inbox, still mid-turn: one digest now.
	self := as(contracts.Principal{Kind: contracts.PrincipalSession, ID: parent})
	r, err := f.x.h.ReleaseChildReports(self, contracts.ReleaseChildReportsRequest{})
	if err != nil || r.Released != 2 {
		t.Fatalf("release: %+v %v (%v)", r, err, f.statuses())
	}
	msgs := f.allToParent()
	if len(msgs) != 1 || msgs[0].Envelope.Sender.Kind != contracts.PrincipalSystem ||
		!strings.HasPrefix(msgs[0].Envelope.Body.Text, "2 reports from sessions you started") {
		t.Fatalf("want one digest: %+v", msgs)
	}
	page, err := f.x.h.ListMessages(self, contracts.ListMessagesRequest{Box: "inbox", Unacked: true,
		Address: &contracts.Address{Kind: contracts.AddrSession, ID: parent}})
	if err != nil || len(page.Items) != 1 || page.Items[0].Envelope.MessageID != msgs[0].Envelope.MessageID {
		t.Fatalf("inbox after release: %+v %v", page.Items, err)
	}
	for _, st := range f.statuses() {
		if !strings.HasPrefix(st, "idle:sent:in "+msgs[0].Envelope.MessageID) {
			t.Fatalf("statuses: %v", f.statuses())
		}
	}
	// Nothing left: a second read releases nothing, the turn end sends nothing.
	if r, err := f.x.h.ReleaseChildReports(self, contracts.ReleaseChildReportsRequest{}); err != nil || r.Released != 0 {
		t.Fatalf("second release: %+v %v", r, err)
	}
	f.x.clock.Add(30 * time.Second)
	f.observeSession(parent, contracts.RunWaitingInput)
	f.process()
	if n := len(f.allToParent()); n != 1 {
		t.Fatalf("reports sent twice: %d", n)
	}

	// The batching itself is unchanged: a later report for the busy parent
	// waits again until it looks or its turn ends.
	f.observeSession(parent, contracts.RunRunning)
	f.observeSession(f.child.Session.SessionID, contracts.RunRunning)
	f.x.clock.Add(time.Minute)
	f.observeSession(f.child.Session.SessionID, contracts.RunWaitingInput)
	f.process()
	if n := len(f.allToParent()); n != 1 {
		t.Fatalf("a busy parent got a report without looking: %v", f.statuses())
	}
	if r, err := f.x.h.ReleaseChildReports(self, contracts.ReleaseChildReportsRequest{}); err != nil || r.Released != 1 {
		t.Fatalf("lone release: %+v %v (%v)", r, err, f.statuses())
	}
	msgs = f.allToParent()
	if len(msgs) != 2 || msgs[1].Envelope.Sender.ID != f.child.Session.SessionID {
		t.Fatalf("a lone report comes from its child: %+v", msgs[len(msgs)-1].Envelope.Sender)
	}
}
