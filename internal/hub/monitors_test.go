package hub

import (
	"context"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// startMonitor starts a shell session as parent like `pyre monitor add`
// (tags nil: an untagged legacy monitor) and completes its launch in state.
func (x *harness) startMonitor(parent, op, title string, tags []string, state contracts.RunState) contracts.SessionView {
	x.t.Helper()
	resp, err := x.h.StartSession(as(sesPrincipal(parent)), contracts.StartSessionRequest{Mutation: contracts.Mutation{OperationID: op}, NodeID: nodeMac,
		Harness: contracts.HarnessShell, Title: title, Cwd: "/home/synth/work", Tags: tags, ParentSessionID: parent})
	if err != nil {
		x.t.Fatalf("start monitor: %v", err)
	}
	cmds := x.ch.sentKinds(nodeMac, contracts.CmdStartRun)
	cmd := cmds[len(cmds)-1]
	x.ack(nodeMac, cmd)
	run := *resp.Session.Run
	run.State = state
	run.Adapter = "shell-synth"
	x.result(nodeMac, cmd, contracts.OutcomeSucceeded, run)
	return x.session(resp.Session.Session.SessionID)
}

// A live monitor does not keep its long-ended parent from the nightly
// sweep (archiving the parent ends the monitor); a live ordinary child does.
func TestSweepIgnoresLiveMonitorChildren(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	watched := x.start(owner, "op_mon_sw_a", nodeMac)
	x.startMonitor(watched.Session.SessionID, "op_mon_sw_m", "monitor: gh pr checks 7",
		[]string{contracts.TagMonitor, contracts.TagMonitorToPrefix + watched.Session.SessionID}, contracts.RunRunning)
	busy := x.start(owner, "op_mon_sw_b", nodeMac)
	x.startMonitor(busy.Session.SessionID, "op_mon_sw_c", "worker shell", nil, contracts.RunRunning) // not a monitor
	setRun(x, watched, contracts.RunExited, "")
	setRun(x, busy, contracts.RunExited, "")
	x.clock.Add(25 * time.Hour)
	cands, err := x.h.staleSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, c := range cands {
		got[c.SessionID] = true
	}
	if !got[watched.Session.SessionID] || got[busy.Session.SessionID] {
		t.Fatalf("sweep candidates: %+v", cands)
	}
}

func TestListSessionsHidesMonitorsAndBadgesTheirTarget(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	parent := x.start(owner, "op_mon_parent", nodeMac).Session.SessionID
	monTags := func() []string {
		return []string{contracts.TagMonitor, contracts.TagMonitorToPrefix + parent, contracts.TagReportNone}
	}
	x.clock.Add(time.Second)
	tagged := x.startMonitor(parent, "op_mon_a", "monitor: gh pr checks 7", monTags(), contracts.RunWaitingInput)
	x.clock.Add(time.Second)
	ended := x.startMonitor(parent, "op_mon_c", "monitor: done", monTags(), contracts.RunExited)
	x.clock.Add(time.Second)
	lost := x.startMonitor(parent, "op_mon_u", "monitor: lost node", monTags(), contracts.RunUnknown)
	x.clock.Add(time.Second)
	// A shell a session titled "monitor: ..." without the tag is a real
	// session: listed, never a badge.
	shell := x.startMonitor(parent, "op_mon_b", "monitor: tail api logs", nil, contracts.RunRunning)

	if !tagged.Monitor || !ended.Monitor || !lost.Monitor || shell.Monitor {
		t.Fatalf("sessions.get monitor marks: %v %v %v %v", tagged.Monitor, ended.Monitor, lost.Monitor, shell.Monitor)
	}
	page, err := x.h.ListSessions(as(owner), contracts.ListSessionsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]contracts.SessionView{}
	for _, v := range page.Items {
		ids[v.Session.SessionID] = v
	}
	for _, m := range []string{tagged.Session.SessionID, ended.Session.SessionID, lost.Session.SessionID} {
		if _, ok := ids[m]; ok {
			t.Errorf("default list includes monitor %s", m)
		}
	}
	if _, ok := ids[shell.Session.SessionID]; !ok {
		t.Error("an untagged shell titled monitor: is not a monitor and stays listed")
	}
	p := ids[parent]
	if len(p.Monitors) != 1 || p.Monitors[0].SessionID != tagged.Session.SessionID ||
		p.Monitors[0].Title != "monitor: gh pr checks 7" || p.Monitors[0].State != contracts.RunWaitingInput {
		t.Fatalf("parent badge: the waiting monitor only; ended, unknown and the plain shell left out: %+v", p.Monitors)
	}

	all, err := x.h.ListSessions(as(owner), contracts.ListSessionsRequest{IncludeMonitors: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Items) != len(page.Items)+3 {
		t.Fatalf("include_monitors lists %d, want %d", len(all.Items), len(page.Items)+3)
	}
	// Older CLIs list monitors as harness=shell without include_monitors.
	shells, err := x.h.ListSessions(as(owner), contracts.ListSessionsRequest{Harness: contracts.HarnessShell})
	if err != nil {
		t.Fatal(err)
	}
	if len(shells.Items) != 4 {
		t.Fatalf("harness=shell keeps monitors for older CLIs: %d", len(shells.Items))
	}
	if one := x.session(parent); len(one.Monitors) != 1 {
		t.Fatalf("sessions.get badge: %+v", one.Monitors)
	}
	if one := x.session(tagged.Session.SessionID); len(one.Monitors) != 0 {
		t.Fatalf("a monitor has no badge: %+v", one.Monitors)
	}
	many, err := x.h.GetManySessions(as(owner), contracts.GetManySessionsRequest{IDs: []string{parent, tagged.Session.SessionID}})
	if err != nil {
		t.Fatal(err)
	}
	if len(many.Items) != 2 || len(many.Items[0].Monitors) != 1 || !many.Items[1].Monitor {
		t.Fatalf("sessions.get_many: %+v", many.Items)
	}

	// An archived monitor no longer counts.
	yes := true
	if _, err := x.h.UpdateSession(as(owner), contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: "op_mon_arch"}, SessionID: tagged.Session.SessionID, Archived: &yes}); err != nil {
		t.Fatal(err)
	}
	if one := x.session(parent); len(one.Monitors) != 0 {
		t.Fatalf("after archiving it: %+v", one.Monitors)
	}
}
