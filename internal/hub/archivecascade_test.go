package hub

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// ---- archive cascade (iss_01M5J9271SRZQ4WDX4AKNWBWFM) ----

// connectWide connects nodeMac with room for a session tree.
func (x *harness) connectWide() {
	x.t.Helper()
	x.ch.setConnected(nodeMac, true)
	if _, err := x.h.HandleHello(nodeCtx(nodeMac), nodeMac, helloFor(nodeMac, 16)); err != nil {
		x.t.Fatal(err)
	}
}

// startUnder starts an idle session as the session parent.
func (x *harness) startUnder(parent, op, title string) string {
	x.t.Helper()
	resp, err := x.h.StartSession(as(sesPrincipal(parent)), contracts.StartSessionRequest{
		Mutation: contracts.Mutation{OperationID: op}, NodeID: nodeMac, Harness: contracts.HarnessClaude, Cwd: "/home/synth/child",
		Title: title, InitialPrompt: "run the synthetic task"})
	if err != nil {
		x.t.Fatalf("start under %s: %v", parent, err)
	}
	cmds := x.ch.sentKinds(nodeMac, contracts.CmdStartRun)
	cmd := cmds[len(cmds)-1]
	x.ack(nodeMac, cmd)
	run := *resp.Session.Run
	run.State = contracts.RunWaitingInput
	run.Adapter = "claude-synth"
	x.result(nodeMac, cmd, contracts.OutcomeSucceeded, run)
	return resp.Session.Session.SessionID
}

// observeState reports a state of the session's current run (exited with
// code 0 and an end time for RunExited).
func (x *harness) observeState(sid string, state contracts.RunState) {
	x.t.Helper()
	r := *x.session(sid).Run
	r.State = state
	if state == contracts.RunExited {
		code, at := 0, x.clock.Now()
		r.ExitCode, r.EndedAt, r.StateReason = &code, &at, "stopped by pyre (/exit)"
	}
	x.report(nodeMac, contracts.ReportRun, r)
}

func (x *harness) processArchives() {
	x.t.Helper()
	if err := x.h.ProcessArchiveWhenDone(context.Background()); err != nil {
		x.t.Fatal(err)
	}
}

func (x *harness) processReports() {
	x.t.Helper()
	x.clock.Add(10 * time.Minute)
	if _, err := x.h.ProcessChildReports(context.Background()); err != nil {
		x.t.Fatal(err)
	}
}

func cascadeIDs(cs []contracts.CascadeSession) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.SessionID)
	}
	slices.Sort(out)
	return out
}

func sortedIDs(ids ...string) []string {
	slices.Sort(ids)
	return ids
}

// The issue's tree: a coordinator's lead archives itself mid-turn while its
// own child is mid-turn and two grandchildren (one idle, one exited) sit
// under a reviewer that already archived itself. Once the lead's archive
// completes, all three follow (the idle one at once, the busy one when its
// turn ends, the exited one now), each audited with the lead; nothing is
// reported to the archived lead or reviewer; the lead's final report to the
// coordinator says what went with it.
func TestArchiveCascadeSelfArchivedLead(t *testing.T) {
	x := newHarness(t)
	x.connectWide()
	x.h.childSummary = func(ctx context.Context, sessionID, runID string) (string, error) {
		return "Synthetic final answer: the tests pass.", nil
	}
	coord := x.start(owner, "op_synth-coord", nodeMac).Session.SessionID
	lead := x.startUnder(coord, "op_synth-lead", "synthetic lead")
	child := x.startUnder(lead, "op_synth-lead-child", "synthetic worker")
	reviewer := x.startUnder(lead, "op_synth-reviewer", "synthetic reviewer")
	grandIdle := x.startUnder(reviewer, "op_synth-grand-idle", "synthetic fixer")
	grandDone := x.startUnder(reviewer, "op_synth-grand-done", "synthetic checker")
	x.observeState(grandDone, contracts.RunExited)
	// The reviewer was archived before this fix: its children stayed live.
	x.observeState(reviewer, contracts.RunExited)
	if ok, err := x.h.archiveAsPyre(context.Background(), x.session(reviewer).Session, "synthetic earlier archive", nil); err != nil || !ok {
		t.Fatalf("archive reviewer: %v %v", ok, err)
	}
	x.processReports() // settle the reports made so far
	x.observeState(child, contracts.RunRunning)
	x.observeState(lead, contracts.RunRunning)

	v, err := archiveWhenDone(x, sesPrincipal(lead), "op_synth-lead-self-archive", lead, true)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cascadeIDs(v.ArchiveCascade.Archived), sortedIDs(child, grandIdle, grandDone); !slices.Equal(got, want) {
		t.Fatalf("answer lists %v, want %v", got, want)
	}
	if line := v.ArchiveCascade.Line(); line != "also archived 3 sessions it started" {
		t.Fatalf("line %q", line)
	}
	// Nothing happens under it until the lead itself is archived.
	x.processArchives()
	if hasArchiveWhenDone(x.session(child).Session) || x.session(grandDone).Session.Archived {
		t.Fatal("cascaded before the lead was archived")
	}
	x.observeState(lead, contracts.RunWaitingInput)
	x.processArchives() // stops the lead
	x.observeState(lead, contracts.RunExited)
	x.processArchives() // archives the lead, cascades
	if !x.session(lead).Session.Archived {
		t.Fatal("lead archived")
	}
	if !x.session(grandDone).Session.Archived {
		t.Fatal("an exited grandchild is archived at once")
	}
	for _, sid := range []string{child, grandIdle} {
		s := x.session(sid).Session
		if s.Archived || !hasArchiveWhenDone(s) {
			t.Fatalf("%s: a live descendant is archived when done: %+v", sid, s)
		}
	}
	for _, sid := range []string{child, grandIdle, grandDone} {
		var found bool
		for _, e := range x.audit(contracts.ListAuditRequest{SessionID: sid}) {
			if (e.Action == contracts.AuditSessionArchiveScheduled || e.Action == contracts.AuditSessionArchived) &&
				strings.Contains(e.Reason, "archived with ancestor "+lead) && e.Actor.Kind == "pyre" {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s: no cascade audit naming the lead", sid)
		}
	}
	stopsFor := func(sid string) int {
		n := 0
		for _, c := range x.ch.sentKinds(nodeMac, contracts.CmdStop) {
			if strings.Contains(string(c.Payload), x.session(sid).Run.RunID) {
				n++
			}
		}
		return n
	}
	x.processArchives()
	if stopsFor(grandIdle) != 1 || stopsFor(child) != 0 {
		t.Fatalf("idle grandchild stopped at once (%d), busy child not (%d)", stopsFor(grandIdle), stopsFor(child))
	}
	x.observeState(child, contracts.RunWaitingInput) // its turn ends
	x.processArchives()
	if stopsFor(child) != 1 {
		t.Fatal("the busy child is stopped when its turn ends")
	}
	x.observeState(child, contracts.RunExited)
	x.observeState(grandIdle, contracts.RunExited)
	x.processArchives()
	for _, sid := range []string{child, grandIdle} {
		if !x.session(sid).Session.Archived {
			t.Fatalf("%s archived once its run ended", sid)
		}
	}
	x.processReports()
	x.processReports()
	if got := x.msgsBetween(child, lead); len(got) != 0 {
		t.Fatalf("the archived lead got reports: %q", got)
	}
	for _, sid := range []string{grandIdle, grandDone} {
		if got := x.msgsBetween(sid, reviewer); len(got) != 0 {
			t.Fatalf("the archived reviewer got reports: %q", got)
		}
	}
	reports := x.msgsBetween(lead, coord)
	if countWith(reports, "Pyre also archived 3 sessions it started") != 1 {
		t.Fatalf("the lead's final report names the cascade: %q", reports)
	}
}

// A descendant the owner talked to is left alone, with what it started,
// and listed; the others go with the archived session.
func TestArchiveCascadeLeavesOwnerTouched(t *testing.T) {
	x := newHarness(t)
	x.connectWide()
	coord := x.start(owner, "op_synth-coord", nodeMac).Session.SessionID
	lead := x.startUnder(coord, "op_synth-lead", "synthetic lead")
	touched := x.startUnder(lead, "op_synth-touched", "synthetic chat")
	under := x.startUnder(touched, "op_synth-under-touched", "synthetic helper")
	plain := x.startUnder(lead, "op_synth-plain", "synthetic worker")
	x.clock.Add(time.Minute)
	if _, err := x.h.markOwnerActive(context.Background(), touched, x.clock.Now()); err != nil {
		t.Fatal(err)
	}
	x.observeState(lead, contracts.RunExited)
	yes := true
	v, err := x.h.UpdateSession(as(sesPrincipal(coord)), contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: "op_synth-coord-archive"}, SessionID: lead, Archived: &yes})
	if err != nil {
		t.Fatal(err)
	}
	if !v.Session.Archived || v.ArchiveCascade == nil {
		t.Fatalf("archived with a cascade: %+v", v)
	}
	if got := cascadeIDs(v.ArchiveCascade.Archived); !slices.Equal(got, []string{plain}) {
		t.Fatalf("archived %v, want only %s", got, plain)
	}
	if got := cascadeIDs(v.ArchiveCascade.Left); !slices.Equal(got, []string{touched}) {
		t.Fatalf("left %v, want %s", got, touched)
	}
	if line := v.ArchiveCascade.Line(); !strings.Contains(line, `left 1 the owner is using: "synthetic chat" (`+touched+`)`) {
		t.Fatalf("line %q", line)
	}
	if !hasArchiveWhenDone(x.session(plain).Session) {
		t.Fatal("the untouched child is archived when done")
	}
	for _, sid := range []string{touched, under} {
		if s := x.session(sid).Session; s.Archived || hasArchiveWhenDone(s) {
			t.Fatalf("%s is the owner's: left alone", sid)
		}
	}
}

// A session may archive any session under it without the owner; a session
// outside its tree (a sibling, a stranger) still needs his approval.
func TestArchiveDescendantPermission(t *testing.T) {
	x := newHarness(t)
	x.connectWide()
	coord := x.start(owner, "op_synth-coord", nodeMac).Session.SessionID
	lead := x.startUnder(coord, "op_synth-lead", "synthetic lead")
	sibling := x.startUnder(coord, "op_synth-sibling", "synthetic sibling")
	grand := x.startUnder(lead, "op_synth-grand", "synthetic grandchild")
	stranger := x.start(owner, "op_synth-stranger", nodeMac).Session.SessionID
	for _, p := range []string{sibling, stranger} {
		if _, err := archiveWhenDone(x, sesPrincipal(p), "op_synth-no-"+p, grand, true); !contracts.IsCode(err, contracts.ErrApprovalRequested) {
			t.Fatalf("%s is not an ancestor: want an approval, got %v", p, err)
		}
	}
	if _, err := archiveWhenDone(x, sesPrincipal(coord), "op_synth-coord-grand", grand, true); err != nil {
		t.Fatalf("an ancestor archives a grandchild: %v", err)
	}
	if !hasArchiveWhenDone(x.session(grand).Session) {
		t.Fatal("the idle grandchild is scheduled")
	}
	if !x.h.isAncestor(context.Background(), coord, x.session(grand).Session) || x.h.isAncestor(context.Background(), grand, x.session(coord).Session) {
		t.Fatal("isAncestor")
	}
}
