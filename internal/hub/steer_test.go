package hub

import (
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

// Steering is immediate-only: a steer to a run with input.steer is routed
// to its node; one with no live run or to a disconnected node is rejected,
// whatever the adapter advertises. A Claude-like run (input.queue, no
// input.steer) takes the queued message clients send in place of a steer
// (internal/cli steerAsQueue); an older client's steer is still rejected.
func TestSteerRouting(t *testing.T) {
	claudeCaps := []string{contracts.CapInputQueue, contracts.CapInputReconcile, contracts.CapInputPaste, contracts.CapInputMonitor}
	codexCaps := []string{contracts.CapInputQueue, contracts.CapInputSteer, contracts.CapInputReconcile, contracts.CapInputAppServer}
	for _, tc := range []struct {
		name         string
		caps         []string
		disconnected bool
		exited       bool
		mode         contracts.MessageMode
		want         contracts.RuntimeDeliveryState
		reason       string
		deliver      bool
	}{
		{"codex steer", codexCaps, false, false, contracts.ModeSteer, contracts.DeliveryQueued, "queued for delivery to run", true},
		{"claude steer from an older client", claudeCaps, false, false, contracts.ModeSteer, contracts.DeliveryRejected, "unsupported: adapter does not advertise input.steer", false},
		{"claude queued in place of steer", claudeCaps, false, false, contracts.ModeQueue, contracts.DeliveryQueued, "queued for delivery to run", true},
		{"codex steer, node disconnected", codexCaps, true, false, contracts.ModeSteer, contracts.DeliveryRejected, "rejected: recipient node is disconnected; steering is immediate-only", false},
		{"claude steer, node disconnected", claudeCaps, true, false, contracts.ModeSteer, contracts.DeliveryRejected, "rejected: recipient node is disconnected; steering is immediate-only", false},
		{"codex steer, no live run", codexCaps, false, true, contracts.ModeSteer, contracts.DeliveryRejected, "rejected: recipient has no live run to steer", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newHarness(t)
			x.connect(nodeMac, 0)
			v := x.start(owner, "op_start", nodeMac)
			run := *v.Run
			run.Capabilities = tc.caps
			run.State = contracts.RunRunning
			if tc.exited {
				run.State = contracts.RunExited
			}
			x.report(nodeMac, contracts.ReportRun, run)
			if tc.disconnected {
				x.disconnect(nodeMac)
			}
			before := len(x.ch.sentKinds(nodeMac, contracts.CmdDeliver))
			m, err := x.h.SendMessage(as(owner), contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: "op_steer"},
				Recipient: contracts.Address{Kind: contracts.AddrSession, ID: v.Session.SessionID}, Mode: tc.mode, Body: contracts.MessageBody{Text: "synthetic steer"}})
			if err != nil {
				t.Fatal(err)
			}
			if m.Status.Storage != contracts.StorageStored || m.Status.Runtime != tc.want || !strings.HasPrefix(m.Status.RuntimeReason, tc.reason) {
				t.Fatalf("status %+v; want %s %q", m.Status, tc.want, tc.reason)
			}
			if m.Envelope.Mode != tc.mode {
				t.Fatalf("stored mode %s, sent %s", m.Envelope.Mode, tc.mode)
			}
			delivered := len(x.ch.sentKinds(nodeMac, contracts.CmdDeliver)) > before
			if delivered != tc.deliver {
				t.Fatalf("deliver command sent %v, want %v", delivered, tc.deliver)
			}
		})
	}
}

// Mail held for a session whose run ended (what clients now send in place
// of a steer with no live run) reaches the session's next run after an
// unarchive and a native resume (iss_…: a Codex
// reviewer archived itself, the parent's two steers were rejected 2 s
// later, and the resumed run never saw them).
func TestQueuedMailToArchivedSessionDeliveredOnResume(t *testing.T) {
	codexCaps := []string{contracts.CapInputQueue, contracts.CapInputSteer, contracts.CapInputReconcile, contracts.CapInputAppServer, contracts.CapResumeNative}
	x := newHarness(t)
	x.connect(nodeMac, 0)
	v := x.start(owner, "op_start", nodeMac)
	sid := v.Session.SessionID
	run := *v.Run
	run.Capabilities, run.State, run.StateReason = codexCaps, contracts.RunExited, "stopped by pyre"
	x.report(nodeMac, contracts.ReportRun, run)
	yes, no := true, false
	if _, err := x.h.UpdateSession(as(owner), contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: "op_synth-archive"}, SessionID: sid, Archived: &yes}); err != nil {
		t.Fatal(err)
	}
	before := len(x.ch.sentKinds(nodeMac, contracts.CmdDeliver))
	m, err := x.h.SendMessage(as(owner), contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: "op_synth-held"},
		Recipient: contracts.Address{Kind: contracts.AddrSession, ID: sid}, Mode: contracts.ModeQueue, Body: contracts.MessageBody{Text: "synthetic: final SHA pushed, recheck"}})
	if err != nil {
		t.Fatal(err)
	}
	if m.Status.Runtime != contracts.DeliveryQueued || !strings.Contains(m.Status.RuntimeReason, "no live run") {
		t.Fatalf("held status %+v", m.Status)
	}
	if len(x.ch.sentKinds(nodeMac, contracts.CmdDeliver)) != before {
		t.Fatal("delivered to an ended run")
	}
	if _, err := x.h.UpdateSession(as(owner), contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: "op_synth-unarchive"}, SessionID: sid, Archived: &no}); err != nil {
		t.Fatal(err)
	}
	resp, err := x.h.ResumeSession(as(owner), contracts.ResumeRequest{Mutation: contracts.Mutation{OperationID: "op_synth-resume"}, SessionID: sid})
	if err != nil {
		t.Fatal(err)
	}
	cmds := x.ch.sentKinds(nodeMac, contracts.CmdResume)
	x.ack(nodeMac, cmds[len(cmds)-1])
	nr := *resp.Session.Run
	nr.State, nr.Adapter, nr.Capabilities = contracts.RunWaitingInput, "codex-appserver", codexCaps
	x.result(nodeMac, cmds[len(cmds)-1], contracts.OutcomeSucceeded, nr)
	var got bool
	for _, c := range x.ch.sentKinds(nodeMac, contracts.CmdDeliver)[before:] {
		if strings.Contains(string(c.Payload), m.Envelope.MessageID) {
			got = true
		}
	}
	if !got {
		mv, _ := x.h.GetMessage(as(owner), contracts.GetRequest{ID: m.Envelope.MessageID})
		t.Fatalf("held mail not routed to the resumed run %s: %+v", nr.RunID, mv.Status)
	}
}
