package hub

import (
	"context"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

// namedFleet is a fleet registry whose node names differ from the hosts'
// names, like two Pyre nodes on one machine.
type namedFleet struct {
	contracts.FleetAPI
	names map[string]string
}

func (f namedFleet) GetNode(ctx context.Context, req contracts.GetRequest) (contracts.Node, error) {
	n, ok := f.names[req.ID]
	if !ok {
		return contracts.Node{}, contracts.Errorf(contracts.ErrNotFound, "no node %s", req.ID)
	}
	return contracts.Node{NodeID: req.ID, Name: n, Platform: "linux"}, nil
}

// The start (and resume) response names the node by its registry name, not
// the host name from its hello: two nodes on one host must not look alike.
func TestStartResponseShowsNodeNameNotHostname(t *testing.T) {
	x := newHarness(t)
	x.fleet = namedFleet{names: map[string]string{nodeMac: "wp-synth-b"}}
	x.restart()
	x.connect(nodeMac, 0)
	resp, err := x.h.StartSession(as(owner), contracts.StartSessionRequest{Mutation: contracts.Mutation{OperationID: "op_synth-name-1"}, NodeID: nodeMac,
		Harness: contracts.HarnessClaude, Cwd: "/home/synth/work"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Session.NodeName != "wp-synth-b" {
		t.Fatalf("start response node name %q, want the registry name wp-synth-b (hello hostname is %q)", resp.Session.NodeName, nodeMac[5:])
	}
	cmd := x.ch.sentKinds(nodeMac, contracts.CmdStartRun)[0]
	x.ack(nodeMac, cmd)
	run := *resp.Session.Run
	run.State = contracts.RunExited
	x.result(nodeMac, cmd, contracts.OutcomeSucceeded, run)
	rv, err := x.h.ResumeSession(as(owner), contracts.ResumeRequest{Mutation: contracts.Mutation{OperationID: "op_synth-name-2"}, SessionID: resp.Session.Session.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if rv.Session.NodeName != "wp-synth-b" {
		t.Fatalf("resume response node name %q, want wp-synth-b", rv.Session.NodeName)
	}
}

// An approval whose remote answer window expired while the harness's own
// prompt is still on screen is not the end of the story: when the node
// later reports how the prompt was resolved (answered or interrupted in
// the terminal), the hub records it and emits approval.resolved so every
// surface can drop its "waiting in the terminal" item. A run that ends
// withdraws it.
func TestExpiredApprovalSettlesFromNativeOutcome(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	v := x.start(owner, "op_1", nodeMac)
	ap := approvalFor(*v.Run, "apr_synth-native", "perm-synth-1", contracts.ApprovalPermission, x.clock.Now())
	x.report(nodeMac, contracts.ReportApproval, ap)
	exp := ap
	exp.State = contracts.ApprovalExpired
	x.report(nodeMac, contracts.ReportApproval, exp)
	if g, _ := x.h.GetApproval(as(owner), contracts.GetRequest{ID: ap.ApprovalID}); g.State != contracts.ApprovalExpired {
		t.Fatalf("after expiry report: %s", g.State)
	}
	resolvedBefore := x.countEvents(contracts.EvApprovalResolved)
	done := ap
	done.State = contracts.ApprovalResolved
	done.Resolution = &contracts.ApprovalResolution{Source: "native", ResolvedAt: x.clock.Now()}
	x.report(nodeMac, contracts.ReportApproval, done)
	g, _ := x.h.GetApproval(as(owner), contracts.GetRequest{ID: ap.ApprovalID})
	if g.State != contracts.ApprovalResolved || g.Resolution == nil || g.Resolution.Source != "native" {
		t.Fatalf("native outcome after expiry not recorded: %+v", g)
	}
	if n := x.countEvents(contracts.EvApprovalResolved); n != resolvedBefore+1 {
		t.Fatalf("approval.resolved events %d -> %d", resolvedBefore, n)
	}
	// A Pyre answer is still refused (it expired; the terminal answered).
	_, err := x.h.AnswerApproval(as(owner), contracts.AnswerApprovalRequest{Mutation: contracts.Mutation{OperationID: "op_late"}, ApprovalID: ap.ApprovalID, RunID: v.Run.RunID, OptionID: "allow"})
	if err == nil {
		t.Fatal("answered a settled approval")
	}

	// Second approval: expired, then the run ends -> withdrawn.
	ap2 := approvalFor(*v.Run, "apr_synth-native2", "perm-synth-2", contracts.ApprovalPermission, x.clock.Now())
	x.report(nodeMac, contracts.ReportApproval, ap2)
	exp2 := ap2
	exp2.State = contracts.ApprovalExpired
	x.report(nodeMac, contracts.ReportApproval, exp2)
	ended := *v.Run
	ended.State = contracts.RunExited
	x.report(nodeMac, contracts.ReportRun, ended)
	if g, _ := x.h.GetApproval(as(owner), contracts.GetRequest{ID: ap2.ApprovalID}); g.State != contracts.ApprovalWithdrawn {
		t.Fatalf("expired approval of an ended run: %s", g.State)
	}
}

// Live smoke: `pyre sessions rename --json` named the node by its hostname
// ("alice-server") while lists said "server". Every SessionView path uses the
// registry name.
func TestRenameAndAssociateShowNodeName(t *testing.T) {
	x := newHarness(t)
	x.fleet = namedFleet{names: map[string]string{nodeMac: "wp-synth-b"}}
	x.restart()
	x.connect(nodeMac, 0)
	v := x.start(owner, "op_synth-rn-1", nodeMac)
	title := "synthetic renamed"
	rv, err := x.h.UpdateSession(as(owner), contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: "op_synth-rn-2"}, SessionID: v.Session.SessionID, Title: &title})
	if err != nil {
		t.Fatal(err)
	}
	if rv.NodeName != "wp-synth-b" {
		t.Fatalf("rename response node name %q, want wp-synth-b", rv.NodeName)
	}
	// A retried rename (stored result) says the same.
	rv, err = x.h.UpdateSession(as(owner), contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: "op_synth-rn-2"}, SessionID: v.Session.SessionID, Title: &title})
	if err != nil || rv.NodeName != "wp-synth-b" {
		t.Fatalf("retried rename node name %q (%v)", rv.NodeName, err)
	}
	gv, err := x.h.GetSession(as(owner), contracts.GetRequest{ID: v.Session.SessionID})
	if err != nil || gv.NodeName != rv.NodeName {
		t.Fatalf("get %q vs rename %q (%v)", gv.NodeName, rv.NodeName, err)
	}
}

// A run that never started is in the audit log with its reason, both when
// the start command itself fails and when the node reports the run failed
// later ("launch failed: <reason>"); once per run.
func TestAuditLaunchFailed(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	resp, err := x.h.StartSession(as(owner), contracts.StartSessionRequest{Mutation: contracts.Mutation{OperationID: "op_synth-lf-1"}, NodeID: nodeMac,
		Harness: contracts.HarnessClaude, Cwd: "/home/synth/work"})
	if err != nil {
		t.Fatal(err)
	}
	cmd := x.ch.sentKinds(nodeMac, contracts.CmdStartRun)[0]
	x.ack(nodeMac, cmd)
	x.report(nodeMac, contracts.ReportCommandResult, contracts.CommandResult{CommandID: cmd.CommandID, Kind: cmd.Kind, Outcome: contracts.OutcomeFailed,
		Error: contracts.Errorf(contracts.ErrNotFound, "account not found"), At: x.clock.Now()})
	es := x.audit(contracts.ListAuditRequest{SessionID: resp.Session.Session.SessionID})
	e := findAudit(t, es, "session.launch_failed")
	if e.Reason != "launch failed: account not found" || e.RunID != resp.Session.Run.RunID || e.NodeID != nodeMac || !e.Automatic {
		t.Fatalf("entry %+v", e)
	}
	wantSummary(t, e, "could not start", "launch failed: account not found")

	v := x.start(owner, "op_synth-lf-2", nodeMac)
	run := *v.Run
	run.State, run.StateReason = contracts.RunFailed, "launch failed: no eligible openai subscription on this node: acct_synth-1: cooldown"
	x.report(nodeMac, contracts.ReportRun, run)
	x.report(nodeMac, contracts.ReportRun, run)
	es = x.audit(contracts.ListAuditRequest{SessionID: v.Session.SessionID})
	if n := countAudit(es, "session.launch_failed"); n != 1 {
		t.Fatalf("launch_failed entries %d", n)
	}
	if e := findAudit(t, es, "session.launch_failed"); e.Reason != run.StateReason {
		t.Fatalf("reason %q", e.Reason)
	}
}
