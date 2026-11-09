package hub

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// ---- processes an archived session left running (rc122) ----

// answerCleanup answers the node's next session.cleanup for the child as a
// node whose scan found a detached job of the child still running.
func (f *childFixture) answerCleanup(left ...contracts.LeftProcess) {
	f.x.t.Helper()
	sid := f.child.Session.SessionID
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, c := range f.x.ch.sentKinds(nodeMac, contracts.CmdAction) {
			var ar contracts.NodeActionRequest
			if json.Unmarshal(c.Payload, &ar) != nil || ar.Action != contracts.ActionSessionCleanup {
				continue
			}
			res := contracts.SessionCleanupResult{Sessions: []contracts.SessionCleanup{{SessionID: sid, LeftRunning: left, LeftRunningCount: len(left)}}}
			f.x.ack(nodeMac, c)
			f.x.result(nodeMac, c, contracts.OutcomeSucceeded, res)
			waitFor(f.x.t, func() bool {
				var n int
				_ = f.x.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE session_id=? AND action=?`, sid, contracts.AuditSessionCleanedUp).Scan(&n)
				return n > 0
			})
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.x.t.Fatal("no session.cleanup was queued")
}

var scorerJob = []contracts.LeftProcess{{PID: 4242, Command: "python3 score.py --node office-box2"}, {PID: 4243, Command: "sleep 600"}}

const scorerLine = "2 processes it started are still running (pid 4242, cmd python3 score.py --node office-box2; pid 4243, cmd sleep 600); they lose this session's Pyre access now that it is archived."

// A child that archived itself after its final report went out, leaving a
// detached job running: the parent gets one short report saying so.
func TestLeftRunningAfterFinalReport(t *testing.T) {
	f := newChildFixture(t, nil)
	sid := f.child.Session.SessionID
	f.observe(contracts.RunRunning, "working")
	if _, err := archiveWhenDone(f.x, sesPrincipal(sid), "op_synth-self-archive", sid, true); err != nil {
		t.Fatal(err)
	}
	f.finishTurn(true) // the final report goes out before the stop
	f.process()
	if n := len(f.live()); n != 1 {
		t.Fatalf("final report: %d %v", n, f.statuses())
	}
	f.answerCleanup(scorerJob...)
	f.process()
	msgs := f.live()
	if len(msgs) != 2 {
		t.Fatalf("want the final report and the left-running note, got %d: %v", len(msgs), f.statuses())
	}
	text := msgs[1].Body.Text
	t.Logf("note:\n%s", text)
	if lead, _, _ := strings.Cut(text, "\n"); lead != "synthetic child was archived with processes still running" {
		t.Fatalf("lead line %q", lead)
	}
	if !strings.Contains(text, scorerLine) || !strings.Contains(text, sid) {
		t.Fatalf("note lacks the line:\n%s", text)
	}
	if strings.Contains(msgs[0].Body.Text, "still running") {
		t.Fatalf("the sent final report was changed:\n%s", msgs[0].Body.Text)
	}
	e := findAudit(t, f.x.audit(contracts.ListAuditRequest{}), contracts.AuditSessionCleanedUp)
	if !strings.Contains(e.Reason, "2 processes it started are still running (pid 4242, 4243; not stopped)") {
		t.Fatalf("audit: %+v", e)
	}
	// A repeated cleanup of the same run (the nightly sweep) adds nothing.
	f.x.h.noteLeftRunning(t.Context(), nodeMac, contracts.SessionCleanup{SessionID: sid, LeftRunning: scorerJob, LeftRunningCount: 2})
	f.process()
	if n := len(f.live()); n != 2 {
		t.Fatalf("repeated cleanup: %d reports", n)
	}
}

// A final report still held for a busy parent carries the line itself:
// one message.
func TestLeftRunningJoinsPendingReport(t *testing.T) {
	f := newChildFixture(t, nil)
	sid, parent := f.child.Session.SessionID, f.parent.Session.SessionID
	f.observeSession(parent, contracts.RunRunning) // the parent is mid-turn
	f.observe(contracts.RunRunning, "working")
	if _, err := archiveWhenDone(f.x, sesPrincipal(sid), "op_synth-self-archive", sid, true); err != nil {
		t.Fatal(err)
	}
	f.finishTurn(true)
	f.process()
	if n := len(f.live()); n != 0 {
		t.Fatalf("a busy parent got %d reports", n)
	}
	f.answerCleanup(contracts.LeftProcess{PID: 4242, Command: "python3 score.py"})
	f.process()
	f.x.clock.Add(30 * time.Second)
	f.observeSession(parent, contracts.RunWaitingInput) // its turn ends
	f.process()
	msgs := f.live()
	if len(msgs) != 1 {
		t.Fatalf("want one report, got %d: %v", len(msgs), f.statuses())
	}
	text := msgs[0].Body.Text
	t.Logf("report:\n%s", text)
	line := "1 process it started is still running (pid 4242, cmd python3 score.py); it loses this session's Pyre access now that it is archived."
	if !strings.Contains(text, "finished and archived itself") || !strings.Contains(text, line+"\n"+strings.TrimSuffix(childArchivedHint, "\n")) {
		t.Fatalf("report lacks the line before the archived hint:\n%s", text)
	}
}

// Nothing is said when the cleanup found nothing running, or for a
// session that does not report to a parent.
func TestLeftRunningQuiet(t *testing.T) {
	f := newChildFixture(t, nil)
	sid := f.child.Session.SessionID
	f.observe(contracts.RunRunning, "working")
	if _, err := archiveWhenDone(f.x, sesPrincipal(sid), "op_synth-self-archive", sid, true); err != nil {
		t.Fatal(err)
	}
	f.finishTurn(true)
	f.process()
	f.x.h.noteLeftRunning(t.Context(), nodeMac, contracts.SessionCleanup{SessionID: sid})
	f.process()
	if n := len(f.live()); n != 1 {
		t.Fatalf("nothing left running: %d reports %v", n, f.statuses())
	}

	g := newChildFixture(t, boolp(false))
	gid := g.child.Session.SessionID
	if _, err := archiveWhenDone(g.x, sesPrincipal(gid), "op_synth-self-archive-quiet", gid, true); err != nil {
		t.Fatal(err)
	}
	g.finishTurn(true)
	g.x.h.noteLeftRunning(t.Context(), nodeMac, contracts.SessionCleanup{SessionID: gid, LeftRunning: scorerJob, LeftRunningCount: 2})
	g.process()
	if n := len(g.toParent()); n != 0 {
		t.Fatalf("reports off: %d messages", n)
	}
}

func TestLeftRunningLine(t *testing.T) {
	many := make([]contracts.LeftProcess, 0, 5)
	for i := range 5 {
		many = append(many, contracts.LeftProcess{PID: 100 + i, Command: "worker"})
	}
	for _, tc := range []struct {
		c    contracts.SessionCleanup
		want string
	}{
		{contracts.SessionCleanup{}, ""},
		{contracts.SessionCleanup{LeftRunning: many, LeftRunningCount: 12},
			"12 processes it started are still running (pid 100, cmd worker; pid 101, cmd worker; pid 102, cmd worker; 9 more); they lose this session's Pyre access now that it is archived."},
		{contracts.SessionCleanup{LeftRunning: []contracts.LeftProcess{{PID: 7}}, LeftRunningCount: 1},
			"1 process it started is still running (pid 7); it loses this session's Pyre access now that it is archived."},
	} {
		if got := leftRunningLine(tc.c); got != tc.want {
			t.Errorf("line = %q\nwant %q", got, tc.want)
		}
	}
}
