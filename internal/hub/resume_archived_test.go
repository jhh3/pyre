package hub

import (
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

// Resuming an archived session unarchives it (it is live again), under the
// unarchive rule for agents: only the session itself or one it sits under.
// sessions.get reports archived and archived_at throughout.
func TestResumeUnarchives(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	parent := x.start(owner, "op_synth-ra-parent", nodeMac)
	other := x.start(owner, "op_synth-ra-other", nodeMac)
	child := x.start(sesPrincipal(parent.Session.SessionID), "op_synth-ra-child", nodeMac)
	cid := child.Session.SessionID

	run := *child.Run
	run.State = contracts.RunExited
	x.report(nodeMac, contracts.ReportRun, run)
	yes := true
	if _, err := x.h.UpdateSession(as(owner), contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: "op_synth-ra-arch"}, SessionID: cid, Archived: &yes}); err != nil {
		t.Fatal(err)
	}
	s := x.session(cid).Session
	if !s.Archived || s.ArchivedAt == nil || !s.ArchivedAt.Equal(x.clock.Now()) {
		t.Fatalf("archived session: archived=%v archived_at=%v", s.Archived, s.ArchivedAt)
	}

	resume := func(p contracts.Principal, op string) error {
		_, err := x.h.ResumeSession(as(p), contracts.ResumeRequest{Mutation: contracts.Mutation{OperationID: op}, SessionID: cid})
		return err
	}
	// An unrelated agent may not resume (and so unarchive) it.
	wantCode(t, resume(sesPrincipal(other.Session.SessionID), "op_synth-ra-r1"), contracts.ErrForbidden)
	if !x.session(cid).Session.Archived || len(x.ch.sentKinds(nodeMac, contracts.CmdResume)) != 0 {
		t.Fatal("forbidden resume took effect")
	}
	// The session that started it may.
	if err := resume(sesPrincipal(parent.Session.SessionID), "op_synth-ra-r2"); err != nil {
		t.Fatal(err)
	}
	s = x.session(cid).Session
	if s.Archived || s.ArchivedAt != nil {
		t.Fatalf("resumed session still archived: archived=%v archived_at=%v", s.Archived, s.ArchivedAt)
	}
	var n int
	if err := x.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE session_id=? AND action=? AND reason='resumed'`, cid, contracts.AuditSessionUnarchived).Scan(&n); err != nil || n != 1 {
		t.Fatalf("unarchive audit rows: %d %v", n, err)
	}
	page, err := x.h.ListSessions(as(owner), contracts.ListSessionsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	listed := false
	for _, sv := range page.Items {
		listed = listed || sv.Session.SessionID == cid
	}
	if !listed {
		t.Fatal("resumed session missing from the default listing")
	}
}

// The owner may resume any archived session.
func TestOwnerResumeUnarchives(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	v := x.start(owner, "op_synth-ro", nodeMac)
	run := *v.Run
	run.State = contracts.RunExited
	x.report(nodeMac, contracts.ReportRun, run)
	yes := true
	if _, err := x.h.UpdateSession(as(owner), contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: "op_synth-ro-arch"}, SessionID: v.Session.SessionID, Archived: &yes}); err != nil {
		t.Fatal(err)
	}
	if _, err := x.h.ResumeSession(as(owner), contracts.ResumeRequest{Mutation: contracts.Mutation{OperationID: "op_synth-ro-r"}, SessionID: v.Session.SessionID}); err != nil {
		t.Fatal(err)
	}
	if s := x.session(v.Session.SessionID).Session; s.Archived || s.ArchivedAt != nil {
		t.Fatalf("owner resume left it archived: %+v", s)
	}
}
