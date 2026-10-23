package hub

import (
	"context"
	"slices"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

// ---- archive when done (W-archive2) ----

func archiveWhenDone(x *harness, p contracts.Principal, op, sid string, archived bool) (contracts.SessionView, error) {
	return x.h.UpdateSession(as(p), contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: op}, SessionID: sid, Archived: &archived, WhenDone: true})
}

func (f *childFixture) processArchives() {
	f.x.t.Helper()
	if err := f.x.h.ProcessArchiveWhenDone(context.Background()); err != nil {
		f.x.t.Fatal(err)
	}
}

func TestSelfArchiveWhenDone(t *testing.T) {
	f := newChildFixture(t, nil)
	x, sid := f.x, f.child.Session.SessionID
	f.observe(contracts.RunRunning, "") // the child is mid-turn when it asks
	v, err := archiveWhenDone(x, sesPrincipal(sid), "op_synth-self-archive", sid, true)
	if err != nil {
		t.Fatal(err)
	}
	if v.Session.Archived || !slices.Contains(v.Session.Tags, contracts.TagArchiveWhenDone) {
		t.Fatalf("a live session is scheduled, not archived: %+v", v.Session)
	}
	findAudit(t, x.audit(contracts.ListAuditRequest{}), contracts.AuditSessionArchiveScheduled)
	if n := len(x.tokens.revoked); n != 0 {
		t.Fatalf("tokens revoked before the turn ended: %d", n)
	}
	stops := func() int { return len(x.ch.sentKinds(nodeMac, contracts.CmdStop)) }
	f.processArchives()
	if stops() != 0 {
		t.Fatal("stopped mid-turn")
	}
	f.observe(contracts.RunWaitingInput, "") // the Stop hook: the turn ended
	f.processArchives()
	if stops() != 1 {
		t.Fatalf("want one stop after the turn ended, got %d", stops())
	}
	f.processArchives()
	if stops() != 1 {
		t.Fatal("the stop is not repeated at once")
	}
	if x.session(sid).Session.Archived {
		t.Fatal("archived before the run ended")
	}
	f.observe(contracts.RunExited, "stopped by pyre (/exit)")
	f.processArchives()
	got, err := x.h.GetSession(as(owner), contracts.GetRequest{ID: sid})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Session.Archived || slices.Contains(got.Session.Tags, contracts.TagArchiveWhenDone) {
		t.Fatalf("archived once the run ended, tag cleared: %+v", got.Session)
	}
	e := findAudit(t, x.audit(contracts.ListAuditRequest{}), contracts.AuditSessionArchived)
	if e.Actor.Kind != "pyre" || e.Reason == "" {
		t.Fatalf("final archive audit: %+v", e)
	}
	findAudit(t, x.audit(contracts.ListAuditRequest{}), contracts.AuditSessionStopped)
	if !slices.Contains(x.tokens.revoked, sid) {
		t.Fatal("tokens revoked on archive")
	}
}

func TestParentArchivesIdleChildWhenDoneAndCancel(t *testing.T) {
	f := newChildFixture(t, nil)
	x, parent, sid := f.x, f.parent.Session.SessionID, f.child.Session.SessionID
	// A peer that did not start it only files it for the owner's approval.
	other := x.start(owner, "op_synth-other", nodeMac)
	if _, err := archiveWhenDone(x, sesPrincipal(other.Session.SessionID), "op_synth-other-archive", sid, true); !contracts.IsCode(err, contracts.ErrApprovalRequested) {
		t.Fatalf("a stranger cannot schedule: %v", err)
	}
	// Scheduled, then canceled before anything happens.
	if _, err := archiveWhenDone(x, sesPrincipal(parent), "op_synth-p-archive", sid, true); err != nil {
		t.Fatal(err)
	}
	if _, err := archiveWhenDone(x, owner, "op_synth-cancel", sid, false); err != nil {
		t.Fatal(err)
	}
	if v := x.session(sid); v.Session.Archived || slices.Contains(v.Session.Tags, contracts.TagArchiveWhenDone) {
		t.Fatalf("canceled: %+v", v.Session)
	}
	findAudit(t, x.audit(contracts.ListAuditRequest{}), contracts.AuditSessionArchiveCanceled)
	// Scheduled again: the child is idle, so it is stopped at once.
	if _, err := archiveWhenDone(x, sesPrincipal(parent), "op_synth-p-archive-2", sid, true); err != nil {
		t.Fatal(err)
	}
	f.processArchives()
	if n := len(x.ch.sentKinds(nodeMac, contracts.CmdStop)); n != 1 {
		t.Fatalf("an idle child is stopped at once: %d stops", n)
	}
	// An ended session asked to archive when done is archived at once.
	f.observe(contracts.RunExited, "")
	f.processArchives()
	if !x.session(sid).Session.Archived {
		t.Fatal("archived after exit")
	}
	ended := f.startChild("op_synth-child-2", nil)
	f2 := &childFixture{x: x, parent: f.parent, child: ended}
	f2.observe(contracts.RunExited, "")
	v, err := archiveWhenDone(x, sesPrincipal(parent), "op_synth-ended-archive", ended.Session.SessionID, true)
	if err != nil || !v.Session.Archived {
		t.Fatalf("ended: archived now: %v %+v", err, v.Session)
	}
}
