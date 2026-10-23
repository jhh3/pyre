package hub

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/persistence"
)

// ---- a first turn the hub never saw running (launchTurnTx) ----

// childTurnSeq is the child's counted turns.
func (f *childFixture) childTurnSeq() (turn, closed int64) {
	f.x.t.Helper()
	_ = f.x.db.QueryRow(`SELECT turn_seq, closed_seq FROM child_turns WHERE session_id=?`, f.child.Session.SessionID).Scan(&turn, &closed)
	return turn, closed
}

// A prompted child whose first turn ran between two observations (live:
// a Codex child answered "ok" in 1.6 s and went starting -> waiting_input)
// still reports that turn, after childLaunchSettle; a parent that
// archives it after that turn ended is not told about it again (the
// ended report used to fall back to its own creation time and went out).
func TestChildReportFastFirstTurn(t *testing.T) {
	f := newChildFixture(t, nil) // launched with a prompt, straight to waiting_input
	sid, parent := f.child.Session.SessionID, f.parent.Session.SessionID
	if turn, closed := f.childTurnSeq(); turn != 1 || closed != 1 {
		t.Fatalf("the prompt's turn is counted and closed: turn %d closed %d", turn, closed)
	}
	f.process()
	if n := len(f.toParent()); n != 0 {
		t.Fatalf("reported before childLaunchSettle: %v", f.statuses())
	}
	f.x.clock.Add(childLaunchSettle)
	f.process()
	msgs := f.live()
	if len(msgs) != 1 {
		t.Fatalf("the fast first turn is reported once: %v", f.statuses())
	}
	if lead, _, _ := strings.Cut(msgs[0].Body.Text, "\n"); lead != "synthetic child finished: Synthetic final answer: the tests pass." {
		t.Fatalf("lead line %q", lead)
	}

	// The parent read it, then archives the child when done (it is idle,
	// so Pyre stops and archives it at once).
	f.x.clock.Add(18 * time.Second)
	if _, err := archiveWhenDone(f.x, sesPrincipal(parent), "op_synth-parent-archive", sid, true); err != nil {
		t.Fatal(err)
	}
	f.stopAndArchive()
	f.process()
	if n := len(f.live()); n != 1 {
		t.Fatalf("the ended report of a child the parent archived after its turn went out: %v", f.statuses())
	}
	if st := f.statuses(); len(st) != 2 || st[0] != "idle:sent:" || st[1] != "ended:skipped:"+reportParentArchivedDetail {
		t.Fatalf("statuses: %v", st)
	}

	// A later turn is turn 2 as usual.
	g := newChildFixture(t, nil)
	g.x.clock.Add(childLaunchSettle)
	g.process()
	g.observe(contracts.RunRunning, "working")
	g.observe(contracts.RunWaitingInput, "idle")
	g.process()
	if n := len(g.live()); n != 2 {
		t.Fatalf("second turn: %v", g.statuses())
	}
	if turn, closed := g.childTurnSeq(); turn != 2 || closed != 2 {
		t.Fatalf("turn %d closed %d", turn, closed)
	}
}

// The usual launch: idle for a moment before the initial prompt's turn
// starts. The running observation withdraws the turn counted at launch;
// the parent gets one report, when the turn really ends.
func TestChildReportIdleBeforeFirstTurn(t *testing.T) {
	f := newChildFixture(t, nil)
	f.x.clock.Add(5 * time.Second)
	f.observe(contracts.RunRunning, "working")
	if turn, closed := f.childTurnSeq(); turn != 1 || closed != 0 {
		t.Fatalf("the first turn is open again: turn %d closed %d", turn, closed)
	}
	f.x.clock.Add(childLaunchSettle)
	f.process()
	if n := len(f.toParent()); n != 0 {
		t.Fatalf("reported mid-turn: %v", f.statuses())
	}
	f.observe(contracts.RunWaitingInput, "idle")
	f.process()
	if n := len(f.live()); n != 1 {
		t.Fatalf("one report for the first turn: %v", f.statuses())
	}
	if st := f.statuses(); len(st) != 1 || st[0] != "idle:sent:" {
		t.Fatalf("statuses: %v", st)
	}

	// Ended within childLaunchSettle of a fast turn: one ended report,
	// with the last message, in place of the pending idle one.
	g := newChildFixture(t, nil)
	g.observe(contracts.RunExited, "process exited")
	g.process()
	msgs := g.live()
	if len(msgs) != 1 || !strings.Contains(msgs[0].Body.Text, "Synthetic final answer") {
		t.Fatalf("ended right after a fast turn: %v", g.statuses())
	}
	if st := g.statuses(); len(st) != 2 || st[0] != "idle:skipped:"+reportReplacedDetail {
		t.Fatalf("statuses: %v", st)
	}
}

// A child started without a prompt is idle at launch: no turn, no report.
func TestChildReportNoPromptNoTurn(t *testing.T) {
	f := newChildFixture(t, nil)
	x := f.x
	resp, err := x.h.StartSession(as(sesPrincipal(f.parent.Session.SessionID)), contracts.StartSessionRequest{
		Mutation: contracts.Mutation{OperationID: "op_synth-child-noprompt"}, NodeID: nodeMac, Harness: contracts.HarnessClaude, Cwd: "/home/synth/child",
		Title: "synthetic idle child"})
	if err != nil {
		t.Fatal(err)
	}
	cmds := x.ch.sentKinds(nodeMac, contracts.CmdStartRun)
	cmd := cmds[len(cmds)-1]
	x.ack(nodeMac, cmd)
	run := *resp.Session.Run
	run.State, run.StateReason = contracts.RunWaitingInput, "thread idle"
	x.result(nodeMac, cmd, contracts.OutcomeSucceeded, run)
	x.clock.Add(time.Hour)
	f.process()
	var n int
	_ = x.db.QueryRow(`SELECT COUNT(*) FROM child_reports WHERE child_session_id=?`, resp.Session.Session.SessionID).Scan(&n)
	if n != 0 {
		t.Fatalf("a child launched without a prompt got %d reports: %v", n, f.statuses())
	}
	var turn int64
	_ = x.db.QueryRow(`SELECT turn_seq FROM child_turns WHERE session_id=?`, resp.Session.Session.SessionID).Scan(&turn)
	if turn != 0 {
		t.Fatalf("turn %d", turn)
	}
}

// reportTurnEnd: an ended report knows when its turn ended (the run went
// idle), not when the run was stopped; rows from before the column use
// the turn's idle report, then their own creation time.
func TestChildReportTurnEnd(t *testing.T) {
	f := newChildFixture(t, nil)
	f.observe(contracts.RunRunning, "working")
	f.x.clock.Add(time.Minute)
	f.observe(contracts.RunWaitingInput, "idle")
	idleAt := f.x.clock.Now()
	f.x.clock.Add(time.Minute)
	f.observe(contracts.RunExited, "process exited")
	var cr childReport
	rows, err := f.x.db.Query(`SELECT `+childReportCols+` FROM child_reports WHERE kind=?`, reportEnded)
	if err != nil {
		t.Fatal(err)
	}
	got, err := scanChildReports(rows)
	if err != nil || len(got) != 1 {
		t.Fatalf("ended rows: %v %v", got, err)
	}
	cr = got[0]
	if at := f.x.h.reportTurnEnd(t.Context(), cr); !at.Equal(idleAt) {
		t.Fatalf("turn end %v, want %v (idle)", at, idleAt)
	}
	cr.TurnEnded = ""
	if at := f.x.h.reportTurnEnd(t.Context(), cr); !at.Equal(idleAt) {
		t.Fatalf("legacy row: turn end %v, want the idle report's %v", at, idleAt)
	}

	// Ended mid-turn: the turn ended with the run.
	g := newChildFixture(t, nil)
	g.observe(contracts.RunRunning, "working")
	g.x.clock.Add(time.Minute)
	g.observe(contracts.RunFailed, "crashed")
	var ended string
	_ = g.x.db.QueryRow(`SELECT COALESCE(turn_ended_at,'') FROM child_reports WHERE kind=?`, reportEnded).Scan(&ended)
	if ended == "" || !persistence.ParseTime(ended).Equal(g.x.clock.Now()) {
		t.Fatalf("turn_ended_at %q, want the end of the run %v", ended, g.x.clock.Now())
	}
}

// launchSummary makes the child's transcript empty until reply is set (a
// prompt whose turn has not run yet has no assistant message).
func (f *childFixture) launchSummary() *string {
	reply := new(string)
	f.x.h.childSummary = func(ctx context.Context, sessionID, runID string) (string, error) {
		return *reply, nil
	}
	return reply
}

// Time alone does not prove the launch turn ran: a run idle past
// childLaunchSettle with no assistant message waits, and the running
// observation that comes late (45 s) still withdraws it. One report, with
// the turn's answer.
func TestChildReportLateFirstTurn(t *testing.T) {
	f := newChildFixture(t, nil)
	reply := f.launchSummary()
	f.x.clock.Add(childLaunchSettle + time.Second)
	f.process()
	if n := len(f.toParent()); n != 0 {
		t.Fatalf("a launch report without an answer went out: %v", f.statuses())
	}
	if st := f.statuses(); len(st) != 1 || !strings.HasPrefix(st[0], "idle:pending:deferred: no assistant message") {
		t.Fatalf("statuses: %v", st)
	}
	f.x.clock.Add(14 * time.Second) // running at 45 s
	f.observe(contracts.RunRunning, "working")
	*reply = "Synthetic late answer: done."
	f.x.clock.Add(10 * time.Second)
	f.observe(contracts.RunWaitingInput, "idle")
	f.process()
	f.x.clock.Add(childLaunchWait)
	f.process()
	msgs := f.live()
	if len(msgs) != 1 || !strings.Contains(msgs[0].Body.Text, "Synthetic late answer") {
		t.Fatalf("want one report with the answer: %v", f.statuses())
	}
	if st := f.statuses(); len(st) != 1 || st[0] != "idle:sent:" {
		t.Fatalf("statuses: %v", st)
	}

	// Never answered: dropped after childLaunchWait, nothing sent.
	g := newChildFixture(t, nil)
	g.launchSummary()
	for i := 0; i < 70; i++ {
		g.x.clock.Add(childLaunchSettle)
		g.process()
	}
	if n := len(g.toParent()); n != 0 {
		t.Fatalf("an unanswered launch was reported: %v", g.statuses())
	}
	if st := g.statuses(); len(st) != 1 || !strings.HasPrefix(st[0], "idle:skipped:no assistant message") {
		t.Fatalf("statuses: %v", st)
	}
}

// An approval before the launch turn was seen running: the turn is in
// progress, so the launch report is withdrawn (it said "finished" while the
// child waited on the approval) and the turn reports when it ends.
func TestChildReportApprovalInLaunchTurn(t *testing.T) {
	f := newChildFixture(t, nil)
	reply := f.launchSummary()
	f.x.clock.Add(2 * time.Second)
	f.observe(contracts.RunWaitingApproval, "waiting on approval")
	if turn, closed := f.childTurnSeq(); turn != 1 || closed != 0 {
		t.Fatalf("the approval reopens the first turn: turn %d closed %d", turn, closed)
	}
	f.x.clock.Add(time.Minute)
	f.process()
	*reply = "Synthetic approved answer."
	f.observe(contracts.RunWaitingInput, "idle")
	f.process()
	var idle []string
	for _, st := range f.statuses() {
		if strings.HasPrefix(st, "idle:") {
			idle = append(idle, st)
		}
	}
	if len(idle) != 1 || idle[0] != "idle:sent:" {
		t.Fatalf("one idle report, when the turn ended: %v", f.statuses())
	}
	var key string
	_ = f.x.db.QueryRow(`SELECT dedupe_key FROM child_reports WHERE kind=?`, reportIdle).Scan(&key)
	if key != "idle:1" {
		t.Fatalf("key %q", key)
	}
	last := f.live()
	if !strings.Contains(last[len(last)-1].Body.Text, "Synthetic approved answer") {
		t.Fatalf("report:\n%s", last[len(last)-1].Body.Text)
	}
}

// A resumed run (PreviousRun set, no prompt of its own) idle at launch has
// no launch turn.
func TestChildReportResumeNoLaunchTurn(t *testing.T) {
	f := newChildFixture(t, nil)
	x := f.x
	resp, err := x.h.StartSession(as(sesPrincipal(f.parent.Session.SessionID)), contracts.StartSessionRequest{
		Mutation: contracts.Mutation{OperationID: "op_synth-child-exits"}, NodeID: nodeMac, Harness: contracts.HarnessClaude, Cwd: "/home/synth/child",
		Title: "synthetic exiting child", InitialPrompt: "run the synthetic tests"})
	if err != nil {
		t.Fatal(err)
	}
	sid := resp.Session.Session.SessionID
	cmds := x.ch.sentKinds(nodeMac, contracts.CmdStartRun)
	x.ack(nodeMac, cmds[len(cmds)-1])
	run := *resp.Session.Run
	run.State, run.StateReason, run.Adapter = contracts.RunExited, "process exited", "claude-synth"
	x.result(nodeMac, cmds[len(cmds)-1], contracts.OutcomeSucceeded, run)
	rv, err := x.h.ResumeSession(as(owner), contracts.ResumeRequest{Mutation: contracts.Mutation{OperationID: "op_synth-resume-child"}, SessionID: sid})
	if err != nil {
		t.Fatal(err)
	}
	rc := x.ch.sentKinds(nodeMac, contracts.CmdResume)
	x.ack(nodeMac, rc[len(rc)-1])
	nr := *rv.Session.Run
	nr.State, nr.Adapter = contracts.RunWaitingInput, "claude-synth"
	x.result(nodeMac, rc[len(rc)-1], contracts.OutcomeSucceeded, nr)
	var turn int64
	_ = x.db.QueryRow(`SELECT turn_seq FROM child_turns WHERE session_id=?`, sid).Scan(&turn)
	var idle int
	_ = x.db.QueryRow(`SELECT COUNT(*) FROM child_reports WHERE child_session_id=? AND kind=?`, sid, reportIdle).Scan(&idle)
	if turn != 0 || idle != 0 {
		t.Fatalf("resumed run counted a launch turn: turn %d, %d idle reports", turn, idle)
	}
	x.clock.Add(time.Hour)
	f.process()
}
