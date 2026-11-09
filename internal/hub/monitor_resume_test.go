package hub

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

// pyre resume <monitor> (iss_…): a monitor whose
// run recorded its command relaunches fresh on its own node; one without
// (started before the record existed) is refused with what to do instead.
func TestResumeRelaunchesMonitor(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	resp, err := x.h.StartSession(as(owner), contracts.StartSessionRequest{Mutation: contracts.Mutation{OperationID: "op_m"}, NodeID: nodeMac,
		Harness: contracts.HarnessShell, Cwd: "/home/synth", Title: contracts.MonitorTitlePrefix + "date",
		Tags: []string{contracts.TagMonitor, contracts.TagMonitorToPrefix + "ses_synth-target", contracts.TagReportNone}})
	if err != nil {
		t.Fatal(err)
	}
	sid := resp.Session.Session.SessionID
	cmds := x.ch.sentKinds(nodeMac, contracts.CmdStartRun)
	x.ack(nodeMac, cmds[len(cmds)-1])
	run := *resp.Session.Run
	run.State, run.StateReason = contracts.RunExited, "run process not found after pyred restart (exit not observed): the machine restarted"
	x.result(nodeMac, cmds[len(cmds)-1], contracts.OutcomeSucceeded, run)

	_, err = x.h.ResumeSession(as(owner), contracts.ResumeRequest{Mutation: contracts.Mutation{OperationID: "op_r1"}, SessionID: sid})
	wantCode(t, err, contracts.ErrUnsupported)
	if !strings.Contains(err.Error(), "pyre monitor add") {
		t.Fatalf("legacy monitor refusal: %v", err)
	}

	run.AdapterData = map[string]string{contracts.AdapterDataMonitorArgs: `["-lc","exec pyre monitor run --to ses_synth-target --every 15m0s --cmd-b64 ZGF0ZQ"]`}
	x.report(nodeMac, contracts.ReportRun, run)
	out, err := x.h.ResumeSession(as(owner), contracts.ResumeRequest{Mutation: contracts.Mutation{OperationID: "op_r2"}, SessionID: sid})
	if err != nil {
		t.Fatal(err)
	}
	if nr := out.Session.Run; nr.ContinuationKind != contracts.ContinuationFresh || nr.PreviousRunID != run.RunID || nr.NodeID != nodeMac {
		t.Fatalf("relaunch run: %+v", nr)
	}
	rc := x.ch.sentKinds(nodeMac, contracts.CmdResume)
	if len(rc) != 1 {
		t.Fatalf("resume commands: %d", len(rc))
	}
	var cmd contracts.ResumeCommand
	_ = json.Unmarshal(rc[0].Payload, &cmd)
	if cmd.PreviousRun == nil || len(contracts.MonitorRelaunchArgs(*cmd.PreviousRun)) != 2 {
		t.Fatalf("resume payload lacks the monitor's args: %+v", cmd.PreviousRun)
	}
}
