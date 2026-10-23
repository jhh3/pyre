package hub

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/persistence"
)

// ---- lifecycle cleanup (W-lifecycle) ----

// cleanupCmds lists the node actions queued to node for action.
func cleanupCmds(x *harness, node, action string) []contracts.NodeActionRequest {
	x.t.Helper()
	var out []contracts.NodeActionRequest
	for _, c := range x.ch.sentKinds(node, contracts.CmdAction) {
		var ar contracts.NodeActionRequest
		if json.Unmarshal(c.Payload, &ar) == nil && ar.Action == action {
			out = append(out, ar)
		}
	}
	return out
}

// observeExit reports the child's current run as exited with code (nil:
// no code recorded).
func (f *childFixture) observeExit(code *int) {
	f.x.t.Helper()
	r := *f.x.session(f.child.Session.SessionID).Run
	at := f.x.clock.Now()
	r.State, r.ExitCode, r.EndedAt = contracts.RunExited, code, &at // a node records both on exit
	f.x.report(nodeMac, contracts.ReportRun, r)
}

func TestChildArchivedAfterCleanExitAndReport(t *testing.T) {
	f := newChildFixture(t, nil)
	x, sid := f.x, f.child.Session.SessionID
	f.observe(contracts.RunRunning, "")
	zero := 0
	f.observeExit(&zero)
	// The ended report is still pending: not archived yet.
	if got, _ := x.h.ArchiveFinishedChildren(context.Background()); len(got) != 0 {
		t.Fatalf("archived before the parent was told: %v", got)
	}
	f.process()
	msgs := f.toParent()
	if len(msgs) == 0 || !strings.Contains(msgs[len(msgs)-1].Body.Text, "Pyre archives it") {
		t.Fatalf("the ended report says the child will be archived: %+v", msgs)
	}
	x.clock.Add(6 * time.Minute)
	// A planned successor run (account switch or move between turns): not
	// finished, never archived.
	run := x.session(sid).Run.RunID
	if _, err := x.db.Exec(`INSERT INTO fallback_plans(session_id,from_run_id,node_id,plan,planned_at) VALUES(?,?,?,?,?)`, sid, run, nodeMac, "{}", "2026-10-04T12:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if got, _ := x.h.ArchiveFinishedChildren(context.Background()); len(got) != 0 {
		t.Fatalf("archived during a planned move: %v", got)
	}
	if _, err := x.db.Exec(`DELETE FROM fallback_plans WHERE session_id=?`, sid); err != nil {
		t.Fatal(err)
	}
	got, err := x.h.ArchiveFinishedChildren(context.Background())
	if err != nil || len(got) != 1 || got[0] != sid {
		t.Fatalf("archived after the report: %v %v", got, err)
	}
	if !x.session(sid).Session.Archived {
		t.Fatal("child archived")
	}
	e := findAudit(t, x.audit(contracts.ListAuditRequest{}), contracts.AuditSessionArchived)
	if e.Actor.Kind != "pyre" || !strings.Contains(e.Reason, "exited cleanly") {
		t.Fatalf("audit: %+v", e)
	}
	waitFor(t, func() bool { return len(cleanupCmds(x, nodeMac, contracts.ActionSessionCleanup)) == 1 })
	var p contracts.SessionCleanupParams
	_ = json.Unmarshal(cleanupCmds(x, nodeMac, contracts.ActionSessionCleanup)[0].Params, &p)
	if len(p.SessionIDs) != 1 || p.SessionIDs[0] != sid {
		t.Fatalf("cleanup params: %+v", p)
	}
	// The parent (owner-started, top level) is never archived by this pass.
	if x.session(f.parent.Session.SessionID).Session.Archived {
		t.Fatal("parent archived")
	}
}

// A run that "exited" without a recorded exit code (reboot, OOM kill, pyred
// restart) or with a non-zero one is not a clean exit: the child is kept
// for an hour (childEndedArchiveGrace), then archived like any finished
// child (Oct 2026: children that exited 143 stayed in the sidebar).
func TestChildUncleanExitArchivedAfterGrace(t *testing.T) {
	f := newChildFixture(t, nil)
	x := f.x
	f.observe(contracts.RunRunning, "")
	f.observeExit(nil)
	f.process()
	if msgs := f.toParent(); len(msgs) > 0 && strings.Contains(msgs[len(msgs)-1].Body.Text, "Pyre archives it") {
		t.Fatal("the report promises an archive for an unknown exit")
	}
	one := 143
	f2 := &childFixture{x: x, parent: f.parent, child: f.startChild("op_synth-child-exit143", nil)}
	f2.observe(contracts.RunRunning, "")
	f2.observeExit(&one)
	f2.process()
	reports := len(f.allToParent())
	x.clock.Add(childEndedArchiveGrace - time.Minute)
	if got, _ := x.h.ArchiveFinishedChildren(context.Background()); len(got) != 0 {
		t.Fatalf("archived before the grace: %v", got)
	}
	x.clock.Add(2 * time.Minute)
	got, err := x.h.ArchiveFinishedChildren(context.Background())
	if err != nil || len(got) != 2 {
		t.Fatalf("archived after the grace: %v %v", got, err)
	}
	for _, sid := range []string{f.child.Session.SessionID, f2.child.Session.SessionID} {
		if !x.session(sid).Session.Archived {
			t.Fatalf("%s not archived", sid)
		}
	}
	var reasons []string
	for _, e := range x.audit(contracts.ListAuditRequest{}) {
		if e.Action == contracts.AuditSessionArchived && e.SessionID == f2.child.Session.SessionID {
			if e.Actor.Kind != "pyre" || !e.Automatic {
				t.Fatalf("audit actor: %+v", e)
			}
			reasons = append(reasons, e.Reason)
		}
	}
	if len(reasons) != 1 || !strings.Contains(reasons[0], "exited child (exit 143), report delivered") {
		t.Fatalf("audit reasons: %v", reasons)
	}
	// The archive tells the parent nothing new.
	f.process()
	if n := len(f.allToParent()); n != reports {
		t.Fatalf("archive sent the parent %d more reports", n-reports)
	}
	// The parent (owner-started) is never archived, however long it idles.
	x.clock.Add(48 * time.Hour)
	_, _ = x.h.ArchiveFinishedChildren(context.Background())
	if x.session(f.parent.Session.SessionID).Session.Archived {
		t.Fatal("owner-started session archived")
	}
}

func TestChildArchivedAfterFailureAndGrace(t *testing.T) {
	f := newChildFixture(t, nil)
	x := f.x
	f.observe(contracts.RunRunning, "")
	setRun(x, f.child, contracts.RunFailed, "synthetic crash")
	f.process()
	x.clock.Add(childEndedArchiveGrace - time.Minute)
	if got, _ := x.h.ArchiveFinishedChildren(context.Background()); len(got) != 0 {
		t.Fatalf("a failed child is kept for the parent to inspect during the grace: %v", got)
	}
	x.clock.Add(2 * time.Minute)
	if got, _ := x.h.ArchiveFinishedChildren(context.Background()); len(got) != 1 {
		t.Fatalf("failed child not archived after the grace: %v", got)
	}
}

// An owner-started session whose run ended is never auto-archived here.
func TestOwnerSessionNotAutoArchived(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	v := x.start(owner, "op_synth-owner-ended", nodeMac)
	zero := 0
	r := *x.session(v.Session.SessionID).Run
	at := x.clock.Now()
	r.State, r.ExitCode, r.EndedAt = contracts.RunExited, &zero, &at
	x.report(nodeMac, contracts.ReportRun, r)
	x.clock.Add(3 * time.Hour)
	if got, _ := x.h.ArchiveFinishedChildren(context.Background()); len(got) != 0 {
		t.Fatalf("owner session archived: %v", got)
	}
}

// A child the owner messaged or attached to after it ended is his to
// archive.
func TestChildOwnerTouchedNotAutoArchived(t *testing.T) {
	f := newChildFixture(t, nil)
	x := f.x
	f.observe(contracts.RunRunning, "")
	code := 1
	f.observeExit(&code)
	f.process()
	f2 := &childFixture{x: x, parent: f.parent, child: f.startChild("op_synth-child-attached", nil)}
	f2.observe(contracts.RunRunning, "")
	f2.observeExit(&code)
	f2.process()
	f3 := &childFixture{x: x, parent: f.parent, child: f.startChild("op_synth-child-untouched", nil)}
	f3.observe(contracts.RunRunning, "")
	f3.observeExit(&code)
	f3.process()
	x.clock.Add(time.Minute)
	// The owner messages the first child ...
	at := x.clock.Now()
	if _, err := x.h.markOwnerActive(context.Background(), f.child.Session.SessionID, at); err != nil {
		t.Fatal(err)
	}
	// ... and attaches to the second.
	run := x.session(f2.child.Session.SessionID).Run
	if _, err := x.db.Exec(`INSERT INTO commands(command_id,node_id,seq,kind,actor,issued_at,payload,state,ref_kind,ref_id) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		"cmd_synth-attach", nodeMac, 9999, string(contracts.CmdAttach), `{"kind":"owner","id":"cli_synth"}`, persistence.Time(at), "{}", "done",
		string(contracts.KindRun), run.RunID); err != nil {
		t.Fatal(err)
	}
	x.clock.Add(2 * childEndedArchiveGrace)
	if got, _ := x.h.ArchiveFinishedChildren(context.Background()); len(got) != 1 || got[0] != f3.child.Session.SessionID {
		t.Fatalf("archived %v; want only the untouched child", got)
	}
}

// A folded ended report (a clean exit whose result the parent already has;
// it waits up to childReportHold to ride along) does not block the archive
// (Oct 2026: a child its parent stopped stayed in the sidebar for days).
func TestChildArchivedWithFoldedReport(t *testing.T) {
	f := newChildFixture(t, nil)
	x, sid := f.x, f.child.Session.SessionID
	f.observe(contracts.RunRunning, "")
	zero := 0
	f.observeExit(&zero)
	if _, err := x.db.Exec(`UPDATE child_reports SET hold_state=?, detail=? WHERE child_session_id=? AND status=?`, holdFolded, reportFoldDetail, sid, reportQueued); err != nil {
		t.Fatal(err)
	}
	x.clock.Add(childArchiveGrace + time.Minute)
	if got, _ := x.h.ArchiveFinishedChildren(context.Background()); len(got) != 1 || got[0] != sid {
		t.Fatalf("archived = %v", got)
	}
}

func setRun(x *harness, v contracts.SessionView, state contracts.RunState, reason string) {
	x.t.Helper()
	r := *x.session(v.Session.SessionID).Run
	r.State, r.StateReason = state, reason
	if state.Terminal() {
		at := x.clock.Now()
		r.EndedAt = &at
	}
	x.report(nodeMac, contracts.ReportRun, r)
}

func TestNightlySweep(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	old := x.start(owner, "op_synth-old", nodeMac)
	setRun(x, old, contracts.RunExited, "")
	x.clock.Add(23 * time.Hour)
	failed := x.start(owner, "op_synth-failed", nodeMac)
	setRun(x, failed, contracts.RunFailed, "launch failed: synthetic missing folder")
	x.clock.Add(2 * time.Hour)
	recent := x.start(owner, "op_synth-recent", nodeMac)
	setRun(x, recent, contracts.RunExited, "")
	live := x.start(owner, "op_synth-live", nodeMac)

	// Answer the node sweep like a node would.
	go func() {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			for _, c := range x.ch.sentKinds(nodeMac, contracts.CmdAction) {
				var ar contracts.NodeActionRequest
				if json.Unmarshal(c.Payload, &ar) != nil || ar.Action != contracts.ActionNodeSweep {
					continue
				}
				var p contracts.NodeSweepParams
				_ = json.Unmarshal(ar.Params, &p)
				res := contracts.NodeSweepResult{
					Worktrees: []contracts.WorktreeRemoval{
						{Path: "/home/synth/wt/done", Branch: "pyre/done", Outcome: "removed", BranchOutcome: "deleted"},
						{Path: "/home/synth/wt/wip", Branch: "pyre/wip", Outcome: "kept_dirty"},
					},
					TmpDirsRemoved: 2, TmpBytesFreed: 3 << 30,
				}
				for _, id := range p.SessionIDs {
					res.Sessions = append(res.Sessions, contracts.SessionCleanup{SessionID: id, ProcessesStopped: 1})
				}
				keepSet := map[string]bool{}
				for _, id := range p.KeepSessions {
					keepSet[id] = true
				}
				if !keepSet[recent.Session.SessionID] || !keepSet[live.Session.SessionID] || keepSet[old.Session.SessionID] {
					res.Errors = append(res.Errors, "keep list wrong")
				}
				x.ack(nodeMac, c)
				x.result(nodeMac, c, contracts.OutcomeSucceeded, res)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	rep, err := x.h.RunNightlySweep(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	archived := map[string]bool{}
	for _, a := range rep.Archived {
		archived[a.SessionID] = true
	}
	if !archived[old.Session.SessionID] || !archived[failed.Session.SessionID] || archived[recent.Session.SessionID] || archived[live.Session.SessionID] || len(archived) != 2 {
		t.Fatalf("archived: %+v", rep.Archived)
	}
	if !x.session(old.Session.SessionID).Session.Archived || x.session(live.Session.SessionID).Session.Archived {
		t.Fatal("archive state")
	}
	if len(rep.Nodes) != 1 || rep.Nodes[0].Result == nil || len(rep.Nodes[0].Result.Errors) != 0 {
		t.Fatalf("unarchived sessions are sent as keep: %+v", rep.Nodes)
	}
	for _, want := range []string{"Nightly cleanup", "archived 2 sessions (1 ended over a day ago, 1 failed to launch)", "stopped 2 leftover processes",
		"removed 1 worktree", "3.0 GB", "kept 1 worktree: /home/synth/wt/wip (uncommitted changes)"} {
		if !strings.Contains(rep.Text, want) {
			t.Fatalf("notice lacks %q:\n%s", want, rep.Text)
		}
	}
	var n int
	x.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE recipient_kind='owner' AND sender_id='cleanup'`).Scan(&n)
	if n != 1 {
		t.Fatalf("one owner notice per sweep, got %d", n)
	}
	// Nothing failed: the notice is an update, not inbox mail.
	var mode, ack string
	x.db.QueryRow(`SELECT mode, ack FROM messages WHERE recipient_kind='owner' AND sender_id='cleanup'`).Scan(&mode, &ack)
	if mode != string(contracts.ModeInfo) || ack != string(contracts.AckAcknowledged) {
		t.Fatalf("cleanup notice: mode %s ack %s", mode, ack)
	}
	findAudit(t, x.audit(contracts.ListAuditRequest{}), contracts.AuditSessionCleanedUp)

	// A second sweep with nothing to do sends nothing.
	go func() {
		for i := 0; i < 500; i++ {
			for _, c := range x.ch.sentKinds(nodeMac, contracts.CmdAction) {
				var ar contracts.NodeActionRequest
				if json.Unmarshal(c.Payload, &ar) == nil && ar.Action == contracts.ActionNodeSweep && x.storedCmdState(c.CommandID) != cmdDone {
					x.ack(nodeMac, c)
					x.result(nodeMac, c, contracts.OutcomeSucceeded, contracts.NodeSweepResult{})
					return
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	rep, err = x.h.RunNightlySweep(context.Background(), false)
	if err != nil || rep.Text != "" || len(rep.Archived) != 0 {
		t.Fatalf("quiet second sweep: %v %q %+v", err, rep.Text, rep.Archived)
	}
}

func TestSweepDueOnlyInWindowFirstIsDry(t *testing.T) {
	x := newHarness(t)
	ctx := context.Background()
	due := func() (bool, bool) { return x.h.sweepDue(ctx) }
	// 2026-10-04 12:00 UTC is 05:00 Pacific: past the window (a hub that
	// starts during the day does not sweep).
	if d, _ := due(); d {
		t.Fatal("not outside 03:30-05:00")
	}
	x.clock.Add(22 * time.Hour) // 03:00 Pacific next day: before the window
	if d, _ := due(); d {
		t.Fatal("not before 03:30")
	}
	x.clock.Add(time.Hour) // 04:00 Pacific
	if d, first := due(); !d || !first {
		t.Fatalf("due, and the first sweep is a dry run: %v %v", d, first)
	}
	if d, _ := due(); d {
		t.Fatal("once a night")
	}
	x.clock.Add(24 * time.Hour)
	if d, dry := due(); !d || !dry {
		t.Fatalf("no dry-run report reached the owner yet: still dry: %v %v", d, dry)
	}
	if _, err := x.db.Exec(`INSERT INTO hub_meta(key,value) VALUES(?,?)`, dryRunMetaKey, "2026-10-05T11:00:00Z"); err != nil {
		t.Fatal(err)
	}
	x.clock.Add(24 * time.Hour)
	if d, dry := due(); !d || dry {
		t.Fatalf("after the dry-run report: real: %v %v", d, dry)
	}
}

func TestNightlySweepDryRun(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	old := x.start(owner, "op_synth-old", nodeMac)
	setRun(x, old, contracts.RunExited, "")
	x.clock.Add(25 * time.Hour)
	go func() {
		for i := 0; i < 500; i++ {
			for _, c := range x.ch.sentKinds(nodeMac, contracts.CmdAction) {
				var ar contracts.NodeActionRequest
				if json.Unmarshal(c.Payload, &ar) != nil || ar.Action != contracts.ActionNodeSweep {
					continue
				}
				var p contracts.NodeSweepParams
				_ = json.Unmarshal(ar.Params, &p)
				res := contracts.NodeSweepResult{Worktrees: []contracts.WorktreeRemoval{{Path: "/home/synth/wt/done", Outcome: "would_remove"}}}
				if !p.DryRun || len(p.SessionIDs) != 0 {
					res.Errors = []string{"not a dry run"}
				}
				x.ack(nodeMac, c)
				x.result(nodeMac, c, contracts.OutcomeSucceeded, res)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	rep, err := x.h.RunNightlySweep(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if x.session(old.Session.SessionID).Session.Archived {
		t.Fatal("a dry run archives nothing")
	}
	var reported string
	x.db.QueryRow(`SELECT value FROM hub_meta WHERE key=?`, dryRunMetaKey).Scan(&reported)
	if reported == "" {
		t.Fatal("the dry-run report is recorded once sent")
	}
	if len(rep.Nodes) != 1 || rep.Nodes[0].Result == nil || len(rep.Nodes[0].Result.Errors) != 0 {
		t.Fatalf("node asked for a dry run: %+v", rep.Nodes)
	}
	for _, want := range []string{"dry run", "would archive 1 session", "would remove 1 worktree"} {
		if !strings.Contains(rep.Text, want) {
			t.Fatalf("dry-run notice lacks %q:\n%s", want, rep.Text)
		}
	}
}

func TestSweepNeedsOwner(t *testing.T) {
	ok := contracts.NodeSweepResult{Worktrees: []contracts.WorktreeRemoval{{Path: "/home/synth/wt/a", Outcome: "kept_dirty"}}}
	cases := []struct {
		ns   NodeSweep
		want bool
	}{
		{NodeSweep{Result: &ok}, false},
		{NodeSweep{Error: "offline"}, false},
		{NodeSweep{Error: "action timed out"}, true},
		{NodeSweep{Result: &contracts.NodeSweepResult{Errors: []string{"tmp: permission denied"}}}, true},
		{NodeSweep{Result: &contracts.NodeSweepResult{Worktrees: []contracts.WorktreeRemoval{{Outcome: "failed"}}}}, true},
		{NodeSweep{Result: &contracts.NodeSweepResult{Sessions: []contracts.SessionCleanup{{SessionID: "ses_synth", Error: "kill failed"}}}}, true},
	}
	for i, c := range cases {
		if got := sweepNeedsOwner(SweepReport{Nodes: []NodeSweep{c.ns}}); got != c.want {
			t.Errorf("case %d: %v, want %v", i, got, c.want)
		}
	}
}

func (x *harness) storedCmdState(id string) string {
	var st string
	x.db.QueryRow(`SELECT state FROM commands WHERE command_id=?`, id).Scan(&st)
	return st
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 250; i++ {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}
