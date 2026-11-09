package hub

import (
	"slices"
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

// ---- pending archives (iss_…) ----

func selfArchive(x *harness, sid, op string, includeChildren bool) (contracts.SessionView, error) {
	yes := true
	return x.h.UpdateSession(as(sesPrincipal(sid)), contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: op},
		SessionID: sid, Archived: &yes, WhenDone: true, IncludeChildren: includeChildren})
}

func cancelArchive(x *harness, p contracts.Principal, sid, op string) (contracts.SessionView, error) {
	return x.h.UpdateSession(as(p), contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: op}, SessionID: sid, CancelArchive: true})
}

// A lead with a working child: a plain self-archive is refused and changes
// nothing; with only idle and ended children it goes through and they
// cascade; include_children archives working ones too.
func TestSelfArchiveRefusedWithWorkingChildren(t *testing.T) {
	x := newHarness(t)
	x.connectWide()
	coord := x.start(owner, "op_synth-coord", nodeMac).Session.SessionID
	lead := x.startUnder(coord, "op_synth-lead", "synthetic lead")
	busy := x.startUnder(lead, "op_synth-busy", "synthetic busy worker")
	idle := x.startUnder(lead, "op_synth-idle", "synthetic idle worker")
	done := x.startUnder(lead, "op_synth-done", "synthetic done worker")
	x.observeState(done, contracts.RunExited)
	x.observeState(busy, contracts.RunRunning)
	x.observeState(lead, contracts.RunRunning)

	_, err := selfArchive(x, lead, "op_synth-lead-archive-1", false)
	wantCode(t, err, contracts.ErrConflict)
	if !strings.Contains(err.Error(), busy) || strings.Contains(err.Error(), idle) || !strings.Contains(err.Error(), "include_children") {
		t.Fatalf("refusal names the working child and the way out: %v", err)
	}
	for _, sid := range []string{lead, busy, idle} {
		if s := x.session(sid); hasArchiveWhenDone(s.Session) || s.Session.Archived || s.ArchivePending {
			t.Fatalf("%s: a refused archive changed nothing: %+v", sid, s.Session)
		}
	}
	// A working grandchild under an idle child counts too.
	grand := x.startUnder(idle, "op_synth-grand", "synthetic grandchild")
	x.observeState(busy, contracts.RunWaitingInput)
	x.observeState(grand, contracts.RunWaitingApproval)
	_, err = selfArchive(x, lead, "op_synth-lead-archive-2", false)
	wantCode(t, err, contracts.ErrConflict)
	if !strings.Contains(err.Error(), grand) {
		t.Fatalf("refusal names the working grandchild: %v", err)
	}
	// Everything idle or ended: allowed, and the cascade is announced.
	x.observeState(grand, contracts.RunWaitingInput)
	v, err := selfArchive(x, lead, "op_synth-lead-archive-3", false)
	if err != nil {
		t.Fatal(err)
	}
	if !hasArchiveWhenDone(v.Session) || v.ArchiveCascade == nil || len(v.ArchiveCascade.Archived) != 4 {
		t.Fatalf("scheduled with the idle tree: %+v %+v", v.Session.Tags, v.ArchiveCascade)
	}

	// Another lead with a working child archives with include_children.
	lead2 := x.startUnder(coord, "op_synth-lead2", "synthetic lead 2")
	busy2 := x.startUnder(lead2, "op_synth-busy2", "synthetic busy worker 2")
	x.observeState(busy2, contracts.RunRunning)
	x.observeState(lead2, contracts.RunRunning)
	if _, err := selfArchive(x, lead2, "op_synth-lead2-archive", true); err != nil {
		t.Fatal(err)
	}
	if !hasArchiveWhenDone(x.session(lead2).Session) {
		t.Fatal("include_children schedules the archive")
	}
	// The session that started a child may still archive that child.
	lead3 := x.startUnder(coord, "op_synth-lead3", "synthetic lead 3")
	busy3 := x.startUnder(lead3, "op_synth-busy3", "synthetic busy worker 3")
	x.observeState(busy3, contracts.RunRunning)
	if _, err := archiveWhenDone(x, sesPrincipal(lead3), "op_synth-lead3-archive-child", busy3, true); err != nil {
		t.Fatalf("archiving a child is not a self-archive: %v", err)
	}
}

// Views mark a pending archive: the session with the tag, and the
// descendants its archive will cascade to (naming it); once the cascade
// runs, they carry their own.
func TestArchivePendingVisible(t *testing.T) {
	x := newHarness(t)
	x.connectWide()
	coord := x.start(owner, "op_synth-coord", nodeMac).Session.SessionID
	lead := x.startUnder(coord, "op_synth-lead", "synthetic lead")
	child := x.startUnder(lead, "op_synth-child", "synthetic worker")
	other := x.startUnder(coord, "op_synth-other", "synthetic bystander")
	x.observeState(child, contracts.RunRunning)
	x.observeState(lead, contracts.RunRunning)
	if s := x.session(lead); s.ArchivePending {
		t.Fatal("nothing pending yet")
	}
	if _, err := selfArchive(x, lead, "op_synth-lead-archive", true); err != nil {
		t.Fatal(err)
	}
	if s := x.session(lead); !s.ArchivePending || s.ArchivePendingWith != "" {
		t.Fatalf("lead: pending, its own: %+v", s)
	}
	if s := x.session(child); !s.ArchivePending || s.ArchivePendingWith != lead {
		t.Fatalf("child: pending with the lead: %v %q", s.ArchivePending, s.ArchivePendingWith)
	}
	page, err := x.h.ListSessions(as(owner), contracts.ListSessionsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	marked := map[string]string{}
	for _, v := range page.Items {
		if v.ArchivePending {
			marked[v.Session.SessionID] = v.ArchivePendingWith
		}
	}
	if len(marked) != 2 || marked[child] != lead || marked[lead] != "" {
		t.Fatalf("list marks lead and child only: %v", marked)
	}
	many, err := x.h.GetManySessions(as(owner), contracts.GetManySessionsRequest{IDs: []string{child, other}})
	if err != nil {
		t.Fatal(err)
	}
	if !many.Items[0].ArchivePending || many.Items[1].ArchivePending {
		t.Fatalf("get_many marks the child only: %+v", many.Items)
	}
	// The lead's archive completes: the child is scheduled, its own.
	x.observeState(lead, contracts.RunWaitingInput)
	x.processArchives()
	x.observeState(lead, contracts.RunExited)
	x.processArchives()
	if s := x.session(child); !s.ArchivePending || s.ArchivePendingWith != "" || !hasArchiveWhenDone(s.Session) {
		t.Fatalf("child: pending, its own tag: %v %q", s.ArchivePending, s.ArchivePendingWith)
	}
	if s := x.session(lead); s.ArchivePending || !s.Session.Archived {
		t.Fatalf("an archived lead is no longer pending: %+v", s)
	}
}

// cancel_archive removes the pending tag from the session (not archived),
// and, on an archived lead, from the children its archive cascaded to,
// leaving a child that scheduled its own archive alone. Unarchive cancels
// the cascaded ones too.
func TestCancelArchive(t *testing.T) {
	x := newHarness(t)
	x.connectWide()
	coord := x.start(owner, "op_synth-coord", nodeMac).Session.SessionID
	lead := x.startUnder(coord, "op_synth-lead", "synthetic lead")
	child := x.startUnder(lead, "op_synth-child", "synthetic worker")
	x.observeState(child, contracts.RunRunning)
	x.observeState(lead, contracts.RunRunning)

	// 1. Pending on the lead itself: canceled; the child is no longer
	// going with it.
	if _, err := selfArchive(x, lead, "op_synth-lead-archive-1", true); err != nil {
		t.Fatal(err)
	}
	v, err := cancelArchive(x, sesPrincipal(lead), lead, "op_synth-lead-cancel-1")
	if err != nil {
		t.Fatal(err)
	}
	if hasArchiveWhenDone(v.Session) || v.Session.Archived || len(v.ArchiveCanceled) != 1 || v.ArchiveCanceled[0].SessionID != lead {
		t.Fatalf("canceled on the lead: %+v %+v", v.Session.Tags, v.ArchiveCanceled)
	}
	if s := x.session(child); s.ArchivePending {
		t.Fatal("the child is no longer pending")
	}
	var canceledAudit bool
	for _, e := range x.audit(contracts.ListAuditRequest{SessionID: lead}) {
		canceledAudit = canceledAudit || e.Action == contracts.AuditSessionArchiveCanceled
	}
	if !canceledAudit {
		t.Fatal("the cancel is audited")
	}
	x.observeState(lead, contracts.RunWaitingInput)
	x.processArchives()
	if n := len(x.ch.sentKinds(nodeMac, contracts.CmdStop)); n != 0 {
		t.Fatalf("a canceled archive stops nothing (%d stops)", n)
	}
	// Nothing pending: a cancel says so and changes nothing.
	if v, err := cancelArchive(x, sesPrincipal(lead), lead, "op_synth-lead-cancel-noop"); err != nil || len(v.ArchiveCanceled) != 0 {
		t.Fatalf("no-op cancel: %v %+v", err, v.ArchiveCanceled)
	}

	// 2. The lead archives for real; the cascade schedules both children.
	// One of them had scheduled its own archive first: it keeps it.
	self := x.startUnder(lead, "op_synth-self", "synthetic self-archiver")
	x.observeState(self, contracts.RunRunning)
	if _, err := selfArchive(x, self, "op_synth-self-archive", false); err != nil {
		t.Fatal(err)
	}
	x.observeState(lead, contracts.RunRunning)
	if _, err := selfArchive(x, lead, "op_synth-lead-archive-2", true); err != nil {
		t.Fatal(err)
	}
	x.observeState(lead, contracts.RunWaitingInput)
	x.processArchives()
	x.observeState(lead, contracts.RunExited)
	x.processArchives()
	if !x.session(lead).Session.Archived || !hasArchiveWhenDone(x.session(child).Session) {
		t.Fatal("lead archived, child scheduled by the cascade")
	}
	v, err = cancelArchive(x, sesPrincipal(coord), lead, "op_synth-lead-cancel-2")
	if err != nil {
		t.Fatal(err)
	}
	if got := cascadeIDs(v.ArchiveCanceled); !slices.Equal(got, []string{child}) || !v.Session.Archived {
		t.Fatalf("canceled the cascaded child only, the lead stays archived: %v %v", got, v.Session.Archived)
	}
	if s := x.session(child).Session; hasArchiveWhenDone(s) {
		t.Fatal("child's cascaded archive canceled")
	}
	if s := x.session(self).Session; !hasArchiveWhenDone(s) {
		t.Fatal("a child that scheduled its own archive keeps it")
	}
	var cascadeCancel bool
	for _, e := range x.audit(contracts.ListAuditRequest{SessionID: child}) {
		cascadeCancel = cascadeCancel || e.Action == contracts.AuditSessionArchiveCanceled && strings.Contains(e.Reason, "canceled with ancestor "+lead)
	}
	if !cascadeCancel {
		t.Fatal("the cascaded cancel is audited naming the lead")
	}

	// 3. Unarchiving an archived lead cancels its cascade too.
	lead2 := x.startUnder(coord, "op_synth-lead2", "synthetic lead 2")
	child2 := x.startUnder(lead2, "op_synth-child2", "synthetic worker 2")
	x.observeState(child2, contracts.RunRunning)
	x.observeState(lead2, contracts.RunRunning)
	if _, err := selfArchive(x, lead2, "op_synth-lead2-archive", true); err != nil {
		t.Fatal(err)
	}
	x.observeState(lead2, contracts.RunWaitingInput)
	x.processArchives()
	x.observeState(lead2, contracts.RunExited)
	x.processArchives()
	no := false
	v, err = x.h.UpdateSession(as(sesPrincipal(coord)), contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: "op_synth-lead2-unarchive"}, SessionID: lead2, Archived: &no})
	if err != nil {
		t.Fatal(err)
	}
	if v.Session.Archived || hasArchiveWhenDone(x.session(child2).Session) || len(v.ArchiveCanceled) != 1 {
		t.Fatalf("unarchive restores the lead and cancels the child's: %+v", v.ArchiveCanceled)
	}

	// 4. A session may not cancel another tree's archive; cancel with
	// archived is invalid.
	stranger := x.start(owner, "op_synth-stranger", nodeMac).Session.SessionID
	_, err = cancelArchive(x, sesPrincipal(stranger), self, "op_synth-stranger-cancel")
	wantCode(t, err, contracts.ErrForbidden)
	yes := true
	_, err = x.h.UpdateSession(as(owner), contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: "op_synth-both"}, SessionID: self, Archived: &yes, CancelArchive: true})
	wantCode(t, err, contracts.ErrInvalid)
}
