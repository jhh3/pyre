package hub

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// ---- reports of archived children (a bounded child archives itself) ----

// finishTurn ends the child's turn and runs the archive-when-done worker
// through stop, exit and archive, as on a live hub: the turn ends, Pyre
// stops the run, the node reports the exit, Pyre archives the session.
// processBetween runs the report worker right after the turn ends too.
func (f *childFixture) finishTurn(processBetween bool) {
	f.x.t.Helper()
	f.observe(contracts.RunWaitingInput, "idle at prompt")
	if processBetween {
		f.process()
	}
	f.x.clock.Add(time.Second)
	f.stopAndArchive()
}

// stopAndArchive runs the archive-when-done worker on an idle child: Pyre
// stops the run (no exit code, as Codex reports it), then archives it.
func (f *childFixture) stopAndArchive() {
	f.x.t.Helper()
	f.processArchives()
	if n := len(f.x.ch.sentKinds(nodeMac, contracts.CmdStop)); n == 0 {
		f.x.t.Fatal("the archive worker stops the run once its turn ends")
	}
	f.observe(contracts.RunExited, "stopped by pyre")
	f.processArchives()
	if !f.x.session(f.child.Session.SessionID).Session.Archived {
		f.x.t.Fatal("archived once its run ended")
	}
}

// live lists the reports the parent got from the child that were not
// cancelled (an undelivered idle report replaced by the ended one is).
func (f *childFixture) live() []contracts.MessageEnvelope {
	f.x.t.Helper()
	var out []contracts.MessageEnvelope
	for _, m := range f.toParent() {
		if f.x.msg(m.MessageID).Status.Runtime != contracts.DeliveryCancelled {
			out = append(out, m)
		}
	}
	return out
}

// The normal finish of a bounded child: archive_session on itself at the
// end of its last turn. Its parent gets exactly one report, with the last
// message (owner-reported bug: the ended report was skipped as "child
// session is archived" after it had replaced the idle one).
func TestChildReportSelfArchiveAtTurnEnd(t *testing.T) {
	for _, between := range []bool{false, true} {
		f := newChildFixture(t, nil)
		sid := f.child.Session.SessionID
		f.observe(contracts.RunRunning, "working")
		if _, err := archiveWhenDone(f.x, sesPrincipal(sid), "op_synth-self-archive", sid, true); err != nil {
			t.Fatal(err)
		}
		f.finishTurn(between)
		if !f.x.session(sid).Session.Archived {
			t.Fatal("the child is archived once its run ended")
		}
		f.process()
		f.process()
		msgs := f.live()
		if len(msgs) != 1 {
			t.Fatalf("between=%v: want exactly one report, got %d: %v", between, len(msgs), f.statuses())
		}
		text := msgs[0].Body.Text
		t.Logf("between=%v report:\n%s", between, text)
		if lead, _, _ := strings.Cut(text, "\n"); lead != "synthetic child finished and archived itself: Synthetic final answer: the tests pass." {
			t.Fatalf("between=%v: lead line %q\n%s", between, lead, text)
		}
		for _, bad := range []string{"waiting for input", "Pyre archives it in about five minutes", "stopped by pyre"} {
			if strings.Contains(text, bad) {
				t.Fatalf("between=%v: report says %q:\n%s", between, bad, text)
			}
		}
		if !strings.Contains(text, "It is archived") {
			t.Fatalf("between=%v: report does not say it is archived:\n%s", between, text)
		}
		if !between {
			if st := f.statuses(); len(st) != 2 || st[0] != "idle:skipped:replaced by the ended report" || st[1] != "ended:sent:" {
				t.Fatalf("statuses: %v", st)
			}
		}
	}
}

// A parent that schedules the archive of a child mid-turn still gets that
// turn's result: it asked before it knew the outcome.
func TestChildReportParentArchivesMidTurn(t *testing.T) {
	for _, between := range []bool{false, true} {
		f := newChildFixture(t, nil)
		sid, parent := f.child.Session.SessionID, f.parent.Session.SessionID
		f.observe(contracts.RunRunning, "working")
		if _, err := archiveWhenDone(f.x, sesPrincipal(parent), "op_synth-parent-archive", sid, true); err != nil {
			t.Fatal(err)
		}
		f.x.clock.Add(time.Second)
		f.finishTurn(between)
		f.process()
		msgs := f.live()
		if len(msgs) != 1 || !strings.Contains(msgs[0].Body.Text, "Synthetic final answer") {
			t.Fatalf("between=%v: the mid-turn archive lost the turn's result: %d %v", between, len(msgs), f.statuses())
		}
	}
}

// A parent that archived its child after the turn ended already knows: no
// report beyond what had gone out.
func TestChildReportParentArchivedChild(t *testing.T) {
	for _, between := range []bool{false, true} {
		f := newChildFixture(t, nil)
		sid, parent := f.child.Session.SessionID, f.parent.Session.SessionID
		f.observe(contracts.RunRunning, "working")
		f.observe(contracts.RunWaitingInput, "idle")
		if between {
			f.process() // the idle report goes out before the parent acts
		}
		f.x.clock.Add(time.Second)
		if _, err := archiveWhenDone(f.x, sesPrincipal(parent), "op_synth-parent-archive", sid, true); err != nil {
			t.Fatal(err)
		}
		f.stopAndArchive()
		f.process()
		want := 0
		if between {
			want = 1
		}
		if n := len(f.live()); n != want {
			t.Fatalf("between=%v: reports of a child the parent archived: %d %v", between, n, f.statuses())
		}
		if st := f.statuses(); !strings.HasSuffix(st[len(st)-1], reportParentArchivedDetail) {
			t.Fatalf("between=%v: statuses: %v", between, st)
		}
	}

	f := newChildFixture(t, nil)
	sid, parent := f.child.Session.SessionID, f.parent.Session.SessionID
	f.observe(contracts.RunRunning, "working")
	f.observe(contracts.RunWaitingInput, "idle")
	f.observe(contracts.RunExited, "process exited")
	f.x.clock.Add(time.Second)
	yes := true
	if _, err := f.x.h.UpdateSession(as(sesPrincipal(parent)), contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: "op_synth-parent-archive-ended"},
		SessionID: sid, Archived: &yes}); err != nil {
		t.Fatal(err)
	}
	f.process()
	if n := len(f.toParent()); n != 0 {
		t.Fatalf("report of an ended child the parent archived: %v", f.statuses())
	}
	// An archive the parent later undid and the owner redid is the owner's.
	no := false
	for i, v := range []*bool{&no, &yes} {
		if _, err := f.x.h.UpdateSession(as(owner), contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: "op_synth-owner-toggle-" + string(rune('a'+i))},
			SessionID: sid, Archived: v}); err != nil {
			t.Fatal(err)
		}
	}
	if by, _ := f.x.h.archivedBy(t.Context(), f.x.session(sid).Session, parent); by != archiverOwner {
		t.Fatalf("archivedBy after the owner re-archived: %q", by)
	}
}

// The owner (or Pyre) archiving a child tells the parent nothing: the
// report goes out and says so.
func TestChildReportOwnerArchivedChild(t *testing.T) {
	// Ended, then archived by the owner before the worker ran.
	f := newChildFixture(t, nil)
	sid := f.child.Session.SessionID
	f.observe(contracts.RunRunning, "working")
	f.observe(contracts.RunWaitingInput, "idle")
	f.observe(contracts.RunExited, "process exited")
	yes := true
	if _, err := f.x.h.UpdateSession(as(owner), contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: "op_synth-owner-archive"},
		SessionID: sid, Archived: &yes}); err != nil {
		t.Fatal(err)
	}
	f.process()
	msgs := f.live()
	if len(msgs) != 1 {
		t.Fatalf("owner-archived child: %d %v", len(msgs), f.statuses())
	}
	if lead, _, _ := strings.Cut(msgs[0].Body.Text, "\n"); lead != "synthetic child exited and was archived by the owner: Synthetic final answer: the tests pass." {
		t.Fatalf("lead line %q", lead)
	}

	// Live, scheduled by the owner when done.
	g := newChildFixture(t, nil)
	gid := g.child.Session.SessionID
	g.observe(contracts.RunRunning, "working")
	if _, err := archiveWhenDone(g.x, owner, "op_synth-owner-when-done", gid, true); err != nil {
		t.Fatal(err)
	}
	g.finishTurn(false)
	g.process()
	if msgs := g.live(); len(msgs) != 1 || !strings.Contains(msgs[0].Body.Text, "Synthetic final answer") {
		t.Fatalf("owner when-done archive: %d %v", len(msgs), g.statuses())
	}
}

// The owner bulk-archives idle children whose turns already reached the
// parent: Pyre stops each, and nothing more is sent (the idle reports
// stand, not cancelled and resent as "exited and was archived").
func TestChildReportOwnerBulkArchiveOfReportedChildren(t *testing.T) {
	f := newChildFixture(t, nil)
	kids := []*childFixture{f}
	for i := range 2 {
		c := f.startChild(fmt.Sprintf("op_synth-child-%d", i+2), nil)
		kids = append(kids, &childFixture{x: f.x, parent: f.parent, child: c})
	}
	for _, k := range kids {
		k.observe(contracts.RunRunning, "working")
		k.observe(contracts.RunWaitingInput, "idle")
	}
	f.process()
	f.x.clock.Add(time.Minute)
	for i, k := range kids {
		if _, err := archiveWhenDone(f.x, owner, fmt.Sprintf("op_synth-bulk-%d", i), k.child.Session.SessionID, true); err != nil {
			t.Fatal(err)
		}
	}
	f.processArchives()
	for _, k := range kids {
		k.observe(contracts.RunExited, "stopped by pyre")
	}
	f.processArchives()
	f.process()
	for i, k := range kids {
		if !f.x.session(k.child.Session.SessionID).Session.Archived {
			t.Fatalf("child %d not archived", i)
		}
	}
	// The three idle reports went out as one digest; that stays the only
	// message, still deliverable.
	var ids []string
	rows, err := f.x.db.Query(`SELECT message_id FROM messages WHERE recipient_kind='session' AND recipient_id=?`, f.parent.Session.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) != 1 || f.x.msg(ids[0]).Status.Runtime == contracts.DeliveryCancelled {
		t.Fatalf("want only the idle digest, live: %v %v", ids, f.statuses())
	}
	ended := 0
	for _, st := range f.statuses() {
		if strings.HasPrefix(st, "ended:") {
			ended++
			if st != "ended:skipped:archived by the owner after this turn was reported" {
				t.Fatalf("statuses: %v", f.statuses())
			}
		}
	}
	if ended != len(kids) {
		t.Fatalf("ended reports: %v", f.statuses())
	}
}

// A child that told its parent something mid-turn and then crashed, or
// ended without finishing the turn, still gets its ended report: only a
// clean end of a turn whose idle report it replaced is folded.
func TestChildReportEndedAfterProgressNoteIsSent(t *testing.T) {
	for _, tc := range []struct {
		name  string
		idle  bool
		state contracts.RunState
	}{{"crash mid-turn", false, contracts.RunFailed}, {"exit without code mid-turn", false, contracts.RunExited},
		{"crash after the turn", true, contracts.RunFailed}, {"exit without code after the turn", true, contracts.RunExited}} {
		f := newChildFixture(t, nil)
		sid, parent := f.child.Session.SessionID, f.parent.Session.SessionID
		f.observe(contracts.RunRunning, "working")
		if _, err := f.x.h.SendMessage(as(sesPrincipal(sid)), contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: "op_synth-progress"},
			Recipient: contracts.Address{Kind: contracts.AddrSession, ID: parent}, Body: contracts.MessageBody{Text: "synthetic progress: halfway"}}); err != nil {
			t.Fatal(err)
		}
		if tc.idle {
			f.observe(contracts.RunWaitingInput, "idle")
		}
		f.observe(tc.state, "synthetic crash")
		f.process()
		if n := len(f.live()); n != 2 {
			t.Fatalf("%s: want the progress note and the ended report: %d %v", tc.name, n, f.statuses())
		}
	}
}

// A child that sent its result to the parent itself and then archived
// itself gets no automatic report on top (the ended report that replaced
// the idle one follows the idle rule).
func TestChildReportSelfArchiveAfterMessagingParent(t *testing.T) {
	for _, between := range []bool{false, true} {
		f := newChildFixture(t, nil)
		sid, parent := f.child.Session.SessionID, f.parent.Session.SessionID
		f.observe(contracts.RunRunning, "working")
		if _, err := f.x.h.SendMessage(as(sesPrincipal(sid)), contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: "op_synth-result"},
			Recipient: contracts.Address{Kind: contracts.AddrSession, ID: parent}, Body: contracts.MessageBody{Text: "synthetic result: all green"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := archiveWhenDone(f.x, sesPrincipal(sid), "op_synth-self-archive", sid, true); err != nil {
			t.Fatal(err)
		}
		f.finishTurn(between)
		f.process()
		msgs := f.toParent()
		if len(msgs) != 1 || msgs[0].Body.Text != "synthetic result: all green" {
			t.Fatalf("between=%v: duplicate report after the child's own message: %d %v", between, len(msgs), f.statuses())
		}
	}
}
