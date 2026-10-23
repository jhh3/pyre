package hub

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// office-box1, Oct 2026: pyred restarted while a child's start_run executed;
// the node finished it indeterminate and the run sat "unknown" with no
// process. Once the node is back and its inventory leaves the run out, the
// hub fails it after the grace and the parent gets its report.
func TestUnknownRunMissingFromInventoryFails(t *testing.T) {
	f := newChildFixture(t, nil)
	x := f.x
	parentRun := *x.session(f.parent.Session.SessionID).Run
	resp, err := x.h.StartSession(as(sesPrincipal(f.parent.Session.SessionID)), contracts.StartSessionRequest{
		Mutation: contracts.Mutation{OperationID: "op_synth-lost-child"}, NodeID: nodeMac, Harness: contracts.HarnessClaude, Cwd: "/home/synth/child",
		Title: "synthetic lost child", InitialPrompt: "run the synthetic tests"})
	if err != nil {
		t.Fatal(err)
	}
	sid, runID := resp.Session.Session.SessionID, resp.Session.Run.RunID
	cmds := x.ch.sentKinds(nodeMac, contracts.CmdStartRun)
	cmd := cmds[len(cmds)-1]
	x.ack(nodeMac, cmd)
	// pyred restarts mid-launch: it reconnects listing only the parent's run,
	// then reports the launch indeterminate.
	x.clock.Add(time.Minute)
	x.disconnect(nodeMac)
	x.connect(nodeMac, cmd.Seq, parentRun, *x.session(f.child.Session.SessionID).Run)
	x.result(nodeMac, cmd, contracts.OutcomeIndeterminate, nil)
	if st := x.storedRunState(runID); st != contracts.RunUnknown {
		t.Fatalf("after indeterminate: %s", st)
	}
	x.clock.Add(unknownRunGrace - time.Second)
	if got, _ := x.h.EndMissingUnknownRuns(context.Background()); len(got) != 0 {
		t.Fatalf("ended before the grace: %v", got)
	}
	x.clock.Add(2 * time.Second)
	got, err := x.h.EndMissingUnknownRuns(context.Background())
	if err != nil || len(got) != 1 || got[0] != runID {
		t.Fatalf("ended = %v %v", got, err)
	}
	r := x.session(sid).Run
	if r.State != contracts.RunFailed || r.StateReason != unknownLaunchGoneReason || r.EndedAt == nil {
		t.Fatalf("run = %s %q", r.State, r.StateReason)
	}
	f.process()
	var told bool
	for _, m := range f.allToParent() {
		if strings.Contains(m.Envelope.Body.Text, "never started") {
			told = true
		}
	}
	if !told {
		t.Fatalf("parent not told: %+v", f.allToParent())
	}
	// The parent's run, listed in the inventory, is untouched; a second pass
	// does nothing.
	if st := x.storedRunState(parentRun.RunID); st != contracts.RunWaitingInput {
		t.Fatalf("parent run = %s", st)
	}
	if got, _ := x.h.EndMissingUnknownRuns(context.Background()); len(got) != 0 {
		t.Fatalf("second pass ended %v", got)
	}
}

// An unknown run the node still lists (orphaned: outcome unknown on the
// node too) or whose node is disconnected is left alone.
func TestUnknownRunListedOrOfflineKept(t *testing.T) {
	f := newChildFixture(t, nil)
	x := f.x
	parentRun := *x.session(f.parent.Session.SessionID).Run
	child := *x.session(f.child.Session.SessionID).Run
	child.State, child.StateReason = contracts.RunUnknown, "synthetic orphan"
	x.report(nodeMac, contracts.ReportRun, child)
	x.connect(nodeMac, 0, parentRun, child)
	x.clock.Add(time.Hour)
	if got, _ := x.h.EndMissingUnknownRuns(context.Background()); len(got) != 0 {
		t.Fatalf("ended a listed run: %v", got)
	}
	x.connect(nodeMac, 100, parentRun) // no longer listed ...
	x.clock.Add(time.Hour)
	x.disconnect(nodeMac) // ... but the node is offline
	if got, _ := x.h.EndMissingUnknownRuns(context.Background()); len(got) != 0 {
		t.Fatalf("ended a run of an offline node: %v", got)
	}
	// Back online, still not listed: the run (which did start) is gone.
	x.connect(nodeMac, 100, parentRun)
	x.clock.Add(unknownRunGrace)
	if got, _ := x.h.EndMissingUnknownRuns(context.Background()); len(got) != 1 || got[0] != child.RunID {
		t.Fatalf("ended = %v", got)
	}
	if r := x.session(f.child.Session.SessionID).Run; r.State != contracts.RunExited || r.StateReason != unknownRunGoneReason {
		t.Fatalf("run = %s %q", r.State, r.StateReason)
	}
}
