package hub

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

func TestHelloProtocolMismatchAndIdentity(t *testing.T) {
	x := newHarness(t)
	h := helloFor(nodeMac, 4)
	h.Protocol, h.MinProtocol = 3, 2
	_, err := x.h.HandleHello(nodeCtx(nodeMac), nodeMac, h)
	wantCode(t, err, contracts.ErrProtocolMismatch)
	h = helloFor(nodeMac, 4)
	_, err = x.h.HandleHello(nodeCtx(nodeLin), nodeLin, h) // hello names another node
	wantCode(t, err, contracts.ErrForbidden)
}

func TestDuplicateAndOutOfOrderReports(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	v := x.start(owner, "op_1", nodeMac)
	run := *v.Run
	run.State = contracts.RunRunning
	ack := x.reportSeq(nodeMac, 10, contracts.ReportRun, run)
	if ack.CommittedSeq != 10 {
		t.Fatalf("ack %d", ack.CommittedSeq)
	}
	// Duplicate seq 10 with different content is ignored (dedup by origin seq).
	dup := run
	dup.State = contracts.RunExited
	x.reportSeq(nodeMac, 10, contracts.ReportRun, dup)
	if x.storedRunState(run.RunID) != contracts.RunRunning {
		t.Fatal("duplicate report applied")
	}
	// An older observation (seq 5) arriving late does not win.
	old := run
	old.State = contracts.RunWaitingInput
	x.reportSeq(nodeMac, 5, contracts.ReportRun, old)
	if x.storedRunState(run.RunID) != contracts.RunRunning {
		t.Fatal("older observation won")
	}
	newer := run
	newer.State = contracts.RunWaitingApproval
	if ack := x.reportSeq(nodeMac, 11, contracts.ReportRun, newer); ack.CommittedSeq != 11 {
		t.Fatalf("ack %d", ack.CommittedSeq)
	}
	if x.storedRunState(run.RunID) != contracts.RunWaitingApproval {
		t.Fatal("newer observation lost")
	}
	// Welcome reports the committed watermark after restart.
	x.restart()
	w := x.connect(nodeMac, 99)
	if w.CommittedReportSeq != 11 {
		t.Fatalf("welcome committed %d", w.CommittedReportSeq)
	}
}

func TestInvalidAndForeignReportsAreRejectedNotWedged(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	x.connect(nodeLin, 0)
	v := x.start(owner, "op_1", nodeMac)
	// Malformed payload: recorded, acked.
	ack, err := x.h.HandleReport(nodeCtx(nodeMac), nodeMac, contracts.NodeReport{OriginSeq: 50, Kind: contracts.ReportRun, Payload: []byte(`{"run_id":`)})
	if err != nil || ack.CommittedSeq != 50 {
		t.Fatalf("malformed: %v %+v", err, ack)
	}
	// The Linux node may not report the Mac's run.
	foreign := *v.Run
	foreign.State = contracts.RunFailed
	foreign.NodeID = ""
	x.reportSeq(nodeLin, 1, contracts.ReportRun, foreign)
	if x.storedRunState(v.Run.RunID) == contracts.RunFailed {
		t.Fatal("foreign node changed run state")
	}
	rej, _ := x.h.NodeReportRejects(context.Background(), nodeLin)
	if len(rej) != 1 || !strings.Contains(rej[0], "cannot report run") {
		t.Fatalf("rejects: %v", rej)
	}
	// Unknown kind.
	x.reportSeq(nodeMac, 51, "bogus", map[string]string{})
	rej, _ = x.h.NodeReportRejects(context.Background(), nodeMac)
	if len(rej) != 2 {
		t.Fatalf("mac rejects: %v", rej)
	}
}

func TestHubRestartResendsQueuedCommands(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeLin, 0)
	b := x.start(owner, "op_b", nodeLin)
	startCmds := len(x.ch.sentTo(nodeLin))
	x.disconnect(nodeLin)
	// Message sent while the recipient node is disconnected.
	m, err := x.h.SendMessage(as(owner), contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: "op_m"}, Recipient: contracts.Address{Kind: contracts.AddrSession, ID: b.Session.SessionID}, Body: contracts.MessageBody{Text: "Synthetic: run the tests."}})
	if err != nil {
		t.Fatal(err)
	}
	if m.Status.Runtime != contracts.DeliveryQueued || !strings.Contains(m.Status.RuntimeReason, "disconnected") {
		t.Fatalf("queued: %+v", m.Status)
	}
	// Hub crash/restart.
	x.restart()
	if got := x.msg(m.Envelope.MessageID); got.Status.Storage != contracts.StorageStored {
		t.Fatal("message lost across restart")
	}
	pending, _ := x.h.PendingCommands(context.Background(), nodeLin)
	if len(pending) != 1 || pending[0].Kind != contracts.CmdDeliver {
		t.Fatalf("pending after restart: %+v", pending)
	}
	// Node reconnects having durably recorded only the start command.
	run := *b.Run
	x.connect(nodeLin, int64(startCmds), run)
	sent := x.ch.sentKinds(nodeLin, contracts.CmdDeliver)
	if len(sent) != 1 {
		t.Fatalf("deliver resent %d times", len(sent))
	}
	var dc contracts.DeliverCommand
	json.Unmarshal(sent[0].Payload, &dc)
	if dc.Message.MessageID != m.Envelope.MessageID || dc.SessionID != b.Session.SessionID || dc.BoundRunID != "" {
		t.Fatalf("deliver payload: %+v", dc)
	}
	x.ack(nodeLin, sent[0])
	if got := x.msg(m.Envelope.MessageID); got.Status.Runtime != contracts.DeliveryNodeReceived {
		t.Fatalf("after ack: %s", got.Status.Runtime)
	}
	// A second reconnect that covers the deliver command does not resend it.
	x.disconnect(nodeLin)
	x.connect(nodeLin, sent[0].Seq, run)
	if n := len(x.ch.sentKinds(nodeLin, contracts.CmdDeliver)); n != 1 {
		t.Fatalf("acked command resent: %d", n)
	}
	// Receipts: accepted with hook evidence.
	x.report(nodeLin, contracts.ReportReceipt, contracts.DeliveryReceipt{MessageID: m.Envelope.MessageID, AttemptID: "dlv_1", RunID: run.RunID, State: contracts.DeliveryAccepted, Evidence: contracts.EvidenceClaudePromptHook, OriginSeq: 0})
	got := x.msg(m.Envelope.MessageID)
	if got.Status.Runtime != contracts.DeliveryAccepted || got.Status.BoundRunID != run.RunID {
		t.Fatalf("accepted: %+v", got.Status)
	}
}

func TestSentButUnackedCommandResentOnceAfterReconnect(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	v := x.start(owner, "op_1", nodeMac)
	before := len(x.ch.sentTo(nodeMac))
	rcpt := contracts.Address{Kind: contracts.AddrSession, ID: v.Session.SessionID}
	if _, err := x.h.SendMessage(as(owner), contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: "op_m"}, Recipient: rcpt, Body: contracts.MessageBody{Text: "x"}}); err != nil {
		t.Fatal(err)
	}
	if len(x.ch.sentTo(nodeMac)) != before+1 {
		t.Fatal("deliver not sent while connected")
	}
	deliver := x.ch.sentKinds(nodeMac, contracts.CmdDeliver)[0]
	// The node durably recorded it but the ack was lost. LastCommandSeq is
	// not proof (commands can arrive out of order), so the hub resends it
	// once; the node dedupes by command_id and re-acknowledges.
	x.disconnect(nodeMac)
	x.connect(nodeMac, deliver.Seq, *v.Run)
	resent := x.ch.sentKinds(nodeMac, contracts.CmdDeliver)
	if len(resent) != 2 || resent[1].CommandID != deliver.CommandID {
		t.Fatalf("unacked command resent %d times", len(resent)-1)
	}
	x.ack(nodeMac, resent[1])
	var st string
	x.db.QueryRow(`SELECT state FROM commands WHERE command_id=?`, deliver.CommandID).Scan(&st)
	if st != cmdAcked {
		t.Fatalf("state %s", st)
	}
	// Once acknowledged it is never resent.
	x.disconnect(nodeMac)
	x.connect(nodeMac, deliver.Seq, *v.Run)
	if n := len(x.ch.sentKinds(nodeMac, contracts.CmdDeliver)); n != 2 {
		t.Fatalf("acked command resent: %d sends", n)
	}
}

func TestLiveRunReconciliation(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	a := x.start(owner, "op_a", nodeMac)
	b := x.start(owner, "op_b", nodeMac)
	// A launch queued but not yet delivered when the node drops.
	x.disconnect(nodeMac)
	c, err := x.h.StartSession(as(owner), contracts.StartSessionRequest{Mutation: contracts.Mutation{OperationID: "op_c"}, NodeID: nodeMac, Harness: contracts.HarnessClaude})
	if err == nil {
		t.Fatalf("launch on disconnected node accepted: %+v", c)
	}
	x.connect(nodeMac, 99)
	cResp, err := x.h.StartSession(as(owner), contracts.StartSessionRequest{Mutation: contracts.Mutation{OperationID: "op_c2"}, NodeID: nodeMac, Harness: contracts.HarnessClaude})
	if err != nil {
		t.Fatal(err)
	}
	x.disconnect(nodeMac)
	// Node restarts and reports only run A as live. Its LastCommandSeq does
	// not cover C's launch.
	live := *a.Run
	live.State = contracts.RunRunning
	startSeq := x.ch.sentKinds(nodeMac, contracts.CmdStartRun)
	x.connect(nodeMac, startSeq[1].Seq, live)
	if s := x.storedRunState(a.Run.RunID); s != contracts.RunRunning {
		t.Fatalf("A: %s", s)
	}
	if s := x.storedRunState(b.Run.RunID); s != contracts.RunUnknown {
		t.Fatalf("B should be unknown (not invented exited/failed), got %s", s)
	}
	if s := x.storedRunState(cResp.Session.Run.RunID); s != contracts.RunStarting {
		t.Fatalf("C launch untouched, got %s", s)
	}
	// Later evidence (exit report) resolves B.
	ex := *b.Run
	ex.State = contracts.RunExited
	code := 0
	ex.ExitCode = &code
	x.report(nodeMac, contracts.ReportRun, ex)
	if s := x.storedRunState(b.Run.RunID); s != contracts.RunExited {
		t.Fatalf("B after evidence: %s", s)
	}
}

func TestHostReturnsAfterReplacementRun(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	x.connect(nodeLin, 0)
	v := x.start(owner, "op_1", nodeMac)
	oldRun := *v.Run
	x.disconnect(nodeMac)
	// Owner explicitly resumes elsewhere while the Mac is unreachable.
	resp, err := x.h.ResumeSession(as(owner), contracts.ResumeRequest{Mutation: contracts.Mutation{OperationID: "op_r"}, SessionID: v.Session.SessionID, NodeID: nodeLin, AllowHandoff: true})
	if err != nil {
		t.Fatal(err)
	}
	newRun := *resp.Session.Run
	cmd := x.ch.sentKinds(nodeLin, contracts.CmdResume)[0]
	newRun.State = contracts.RunRunning
	x.result(nodeLin, cmd, contracts.OutcomeSucceeded, newRun)
	// The Mac returns still running the old run.
	oldRun.State = contracts.RunRunning
	x.connect(nodeMac, 99, oldRun)
	s := x.session(v.Session.SessionID)
	if s.Session.CurrentRunID != newRun.RunID {
		t.Fatalf("current run switched back to %s", s.Session.CurrentRunID)
	}
	or, _ := x.h.getRun(context.Background(), x.db, oldRun.RunID)
	if or.State != contracts.RunRunning || or.AdapterData[ConflictKey] == "" {
		t.Fatalf("old run conflict not visible: %+v", or)
	}
	var n int
	x.db.QueryRow(`SELECT COUNT(*) FROM events WHERE kind='run.observed' AND payload LIKE '%concurrent_run%'`).Scan(&n)
	if n == 0 {
		t.Fatal("no conflict event")
	}
}

func TestNodeCommandBackpressure(t *testing.T) {
	x := newHarness(t)
	x.opts.MaxPendingCommandsPerNode = 3
	x.restart()
	x.connect(nodeMac, 0)
	v := x.start(owner, "op_1", nodeMac) // 1 command (acked+done)
	x.disconnect(nodeMac)
	to := contracts.Address{Kind: contracts.AddrSession, ID: v.Session.SessionID}
	for i := 0; i < 3; i++ {
		if _, err := x.h.SendMessage(as(owner), contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: "op_m" + string(rune('a'+i))}, Recipient: to, Body: contracts.MessageBody{Text: "x"}}); err != nil {
			t.Fatal(err)
		}
	}
	_, err := x.h.SendMessage(as(owner), contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: "op_md"}, Recipient: to, Body: contracts.MessageBody{Text: "x"}})
	wantCode(t, err, contracts.ErrCapacityExhausted)
	var n int
	x.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&n)
	if n != 3 {
		t.Fatalf("stored %d (rejected message must not be stored)", n)
	}
}

func TestAttachReadAndActions(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	v := x.start(owner, "op_1", nodeMac)
	var attachClient string
	x.ch.onSend = func(node string, cmd contracts.NodeCommand) {
		switch cmd.Kind {
		case contracts.CmdAttach:
			var ac contracts.AttachCommand
			_ = json.Unmarshal(cmd.Payload, &ac)
			attachClient = ac.ClientNodeID
			go x.result(node, cmd, contracts.OutcomeSucceeded, contracts.AttachDescriptor{Mechanism: contracts.AttachWebTerminal, Ticket: "node-made-secret", ExpiresAt: time.Now().Add(time.Minute)})
		case contracts.CmdReadTranscript:
			go x.result(node, cmd, contracts.OutcomeSucceeded, contracts.ReadSessionResponse{Entries: []contracts.TranscriptEntry{{Seq: 1, Kind: "assistant", Text: "synthetic"}}})
		case contracts.CmdAction:
			go x.result(node, cmd, contracts.OutcomeSucceeded, contracts.DirListResult{Path: "/home/synth"})
		}
	}
	d, err := x.h.AttachSession(as(owner), contracts.AttachRequest{Mutation: contracts.Mutation{OperationID: "op_att"}, SessionID: v.Session.SessionID})
	if err != nil || d.Ticket != "synthetic-ticket" || d.URL != contracts.PathTerminal || d.RunID != v.Run.RunID {
		t.Fatalf("attach: %v %+v", err, d)
	}
	// A client that does not say which node it is on is treated as remote,
	// never as local to the run's node.
	if attachClient != RemoteClient {
		t.Fatalf("unknown client sent to node as %q, want %q", attachClient, RemoteClient)
	}
	var leaked int
	x.db.QueryRow(`SELECT COUNT(*) FROM commands WHERE result LIKE '%node-made-secret%'`).Scan(&leaked)
	if leaked != 0 {
		t.Fatal("attach ticket persisted")
	}
	r, err := x.h.ReadSession(as(owner), contracts.ReadSessionRequest{SessionID: v.Session.SessionID, Tail: true})
	if err != nil || len(r.Entries) != 1 {
		t.Fatalf("read: %v %+v", err, r)
	}
	res, err := x.h.InvokeNodeAction(as(owner), contracts.NodeActionRequest{Mutation: contracts.Mutation{OperationID: "op_act"}, NodeID: nodeMac, Action: contracts.ActionDirList})
	if err != nil || res.Outcome != contracts.OutcomeSucceeded || !strings.Contains(string(res.Result), "/home/synth") {
		t.Fatalf("action: %v %+v", err, res)
	}
	// Session principals are limited.
	_, err = x.h.InvokeNodeAction(as(sesPrincipal(v.Session.SessionID)), contracts.NodeActionRequest{Mutation: contracts.Mutation{OperationID: "op_act2"}, NodeID: nodeMac, Action: contracts.ActionEnvApply})
	wantCode(t, err, contracts.ErrForbidden)
	// Unresponsive node: timeout yields the command id.
	x.ch.onSend = nil
	x.opts.CommandWait = 200 * time.Millisecond
	x.h.opt.CommandWait = 200 * time.Millisecond
	_, err = x.h.ReadSession(as(owner), contracts.ReadSessionRequest{SessionID: v.Session.SessionID})
	wantCode(t, err, contracts.ErrUnavailable)
	if contracts.AsError(err).Details["command_id"] == nil {
		t.Fatal("timeout lacks command_id")
	}
	// Disconnected node: action stays queued, pending with command id.
	x.disconnect(nodeMac)
	res, err = x.h.InvokeNodeAction(as(owner), contracts.NodeActionRequest{Mutation: contracts.Mutation{OperationID: "op_act3"}, NodeID: nodeMac, Action: contracts.ActionInventory, TimeoutSeconds: 1})
	if err != nil || res.Outcome != contracts.OutcomePending || res.CommandID == "" {
		t.Fatalf("pending action: %v %+v", err, res)
	}
	_, err = x.h.ReadSession(as(owner), contracts.ReadSessionRequest{SessionID: v.Session.SessionID})
	wantCode(t, err, contracts.ErrUnavailable)
}

func TestConnectionsMerge(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	x.connect(nodeLin, 0)
	now := x.clock.Now()
	x.report(nodeMac, contracts.ReportConnections, []contracts.Connection{{ConnectionID: "conn_gh", Name: "github-personal", Service: "github", Modes: []string{"local_credential"},
		Availability: []contracts.ConnectionAvailability{{NodeID: nodeMac, State: "verified", VerifiedAt: now}, {NodeID: nodeLin, State: "verified"}}}})
	x.report(nodeLin, contracts.ReportConnections, []contracts.Connection{{ConnectionID: "conn_gh", Name: "github-personal", Service: "github",
		Availability: []contracts.ConnectionAvailability{{State: "missing"}}}, {ConnectionID: "conn_lin", Name: "linear", Scope: contracts.ConnectionScope{Projects: []string{"prj_a"}}}})
	page, err := x.h.ListConnections(as(owner), contracts.ListConnectionsRequest{})
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("list: %v %+v", err, page)
	}
	gh := page.Items[0]
	if gh.ConnectionID != "conn_gh" || len(gh.Availability) != 2 {
		t.Fatalf("merge: %+v", gh)
	}
	for _, a := range gh.Availability {
		if a.NodeID == nodeLin && a.State != "missing" {
			t.Fatal("a node's claim about another node's availability was trusted")
		}
	}
	page, _ = x.h.ListConnections(as(owner), contracts.ListConnectionsRequest{ProjectID: "prj_b"})
	if len(page.Items) != 1 {
		t.Fatalf("project filter: %d", len(page.Items))
	}
}

// A node that reconnects with a fresh hello is launchable with the
// harnesses that hello reports, even when an older periodic status (from
// before the restart) said otherwise.
func TestHelloHarnessesSupersedeOlderStatus(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	// Before the restart, a status report said shell was missing.
	x.report(nodeMac, contracts.ReportStatus, contracts.NodeStatusReport{Harnesses: []contracts.HarnessInfo{
		{Harness: contracts.HarnessShell, Available: false, Reason: "not installed"},
	}})
	_, err := x.h.StartSession(as(owner), contracts.StartSessionRequest{Mutation: contracts.Mutation{OperationID: "op_before"}, NodeID: nodeMac, Harness: contracts.HarnessShell, Cwd: "/tmp/synth"})
	wantCode(t, err, contracts.ErrUnsupported)
	// pyred restarts (wider PATH) and says hello with shell available.
	x.disconnect(nodeMac)
	x.connect(nodeMac, 0)
	if _, err := x.h.StartSession(as(owner), contracts.StartSessionRequest{Mutation: contracts.Mutation{OperationID: "op_after"}, NodeID: nodeMac, Harness: contracts.HarnessShell, Cwd: "/tmp/synth"}); err != nil {
		t.Fatalf("launch right after reconnect: %v", err)
	}
	// A later status report still updates the view.
	x.report(nodeMac, contracts.ReportStatus, contracts.NodeStatusReport{Harnesses: []contracts.HarnessInfo{
		{Harness: contracts.HarnessShell, Available: false, Reason: "tmux removed"},
	}})
	_, err = x.h.StartSession(as(owner), contracts.StartSessionRequest{Mutation: contracts.Mutation{OperationID: "op_later"}, NodeID: nodeMac, Harness: contracts.HarnessShell, Cwd: "/tmp/synth"})
	if err == nil {
		t.Fatal("a newer status saying shell is unavailable was ignored")
	}
}

// An interim acknowledgement never finishes a command, and one that
// arrives after the final result (outbox order) does not regress it.
func TestInterimCommandResult(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	res, err := x.h.InvokeNodeAction(as(owner), contracts.NodeActionRequest{Mutation: contracts.Mutation{OperationID: "op_interim"}, NodeID: nodeMac, Action: "synth.async", TimeoutSeconds: 1})
	if err != nil || res.CommandID == "" {
		t.Fatalf("invoke: %+v %v", res, err)
	}
	x.report(nodeMac, contracts.ReportCommandResult, contracts.CommandResult{CommandID: res.CommandID, Kind: contracts.CmdAction, Outcome: contracts.OutcomeDelivered,
		Result: contracts.RawJSON(`{"async":true}`), Interim: true, At: x.clock.Now()})
	got, err := x.h.GetCommandResult(as(owner), contracts.GetRequest{ID: res.CommandID})
	if err != nil || got.Outcome != contracts.OutcomeDelivered || len(got.Result) != 0 {
		t.Fatalf("after interim: %+v %v", got, err)
	}
	x.report(nodeMac, contracts.ReportCommandResult, contracts.CommandResult{CommandID: res.CommandID, Kind: contracts.CmdAction, Outcome: contracts.OutcomeSucceeded,
		Result: contracts.RawJSON(`{"ok":1}`), At: x.clock.Now()})
	x.report(nodeMac, contracts.ReportCommandResult, contracts.CommandResult{CommandID: res.CommandID, Kind: contracts.CmdAction, Outcome: contracts.OutcomeDelivered, Interim: true, At: x.clock.Now()})
	got, err = x.h.GetCommandResult(as(owner), contracts.GetRequest{ID: res.CommandID})
	if err != nil || got.Outcome != contracts.OutcomeSucceeded || string(got.Result) != `{"ok":1}` {
		t.Fatalf("final: %+v %v", got, err)
	}
	if _, err := x.h.GetCommandResult(as(contracts.Principal{Kind: contracts.PrincipalNode, ID: "node_synth-other"}), contracts.GetRequest{ID: res.CommandID}); !contracts.IsCode(err, contracts.ErrForbidden) {
		t.Fatalf("other node: %v", err)
	}
	if _, err := x.h.GetCommandResult(nodeCtx(nodeMac), contracts.GetRequest{ID: res.CommandID}); err != nil {
		t.Fatalf("own node: %v", err)
	}
}

// A pyred without CommandResult.interim acknowledges async actions with
// delivered + {"async": true}; its later final result still finishes the
// command.
func TestLegacyAsyncAckThenFinal(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	res, err := x.h.InvokeNodeAction(as(owner), contracts.NodeActionRequest{Mutation: contracts.Mutation{OperationID: "op_legacy"}, NodeID: nodeMac, Action: contracts.ActionMemoryStatus, TimeoutSeconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	x.report(nodeMac, contracts.ReportCommandResult, contracts.CommandResult{CommandID: res.CommandID, Kind: contracts.CmdAction, Outcome: contracts.OutcomeDelivered,
		Result: contracts.RawJSON(`{"action":"memory.status","async":true}`), At: x.clock.Now()})
	x.report(nodeMac, contracts.ReportCommandResult, contracts.CommandResult{CommandID: res.CommandID, Kind: contracts.CmdAction, Outcome: contracts.OutcomeSucceeded,
		Result: contracts.RawJSON(`{"backend":"synthetic"}`), At: x.clock.Now()})
	got, err := x.h.GetCommandResult(as(owner), contracts.GetRequest{ID: res.CommandID})
	if err != nil || got.Outcome != contracts.OutcomeSucceeded || string(got.Result) != `{"backend":"synthetic"}` {
		t.Fatalf("legacy async: %+v %v", got, err)
	}
}

// Out-of-order delivery then a cut: the node recorded seq 2 but never got
// seq 1, so its hello reports LastCommandSeq=2. The hub must still resend
// seq 1 (it was never acknowledged); only a recorded CommandAck counts.
func TestUnackedLowerCommandResentAfterOutOfOrderCut(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	var ids []string
	for i := 0; i < 2; i++ {
		res, err := x.h.InvokeNodeAction(as(owner), contracts.NodeActionRequest{Mutation: contracts.Mutation{OperationID: contracts.NewID(contracts.PrefixOperation)},
			NodeID: nodeMac, Action: contracts.ActionInventory, TimeoutSeconds: 1})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, res.CommandID)
	}
	sent := x.ch.sentKinds(nodeMac, contracts.CmdAction)
	if len(sent) != 2 || sent[0].CommandID != ids[0] || sent[1].Seq <= sent[0].Seq {
		t.Fatalf("sent %+v", sent)
	}
	// Only the higher command reached the node and was acknowledged.
	x.ack(nodeMac, sent[1])
	x.disconnect(nodeMac)
	before := len(x.ch.sentTo(nodeMac))
	x.connect(nodeMac, sent[1].Seq)
	resent := x.ch.sentTo(nodeMac)[before:]
	found := false
	for _, c := range resent {
		if c.CommandID == ids[0] {
			found = true
		}
		if c.CommandID == ids[1] {
			t.Fatalf("acknowledged command %s resent", c.CommandID)
		}
	}
	if !found {
		t.Fatalf("unacknowledged lower command %s was not resent after reconnect (resent %+v)", ids[0], resent)
	}
	if rec, _ := x.h.GetCommandResult(as(owner), contracts.GetRequest{ID: ids[0]}); rec.Outcome != contracts.OutcomePending {
		t.Fatalf("lost command shown as %s", rec.Outcome)
	}
}

// After the hub lost reports (restore from an older backup), a node's
// run_snapshot reconciles runs that ended meanwhile (no longer unknown) and
// sessions the hub never saw or only knows as untitled placeholders.
func TestRunSnapshotReconcilesAfterReportGap(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	v := x.start(owner, "op_snap", nodeMac)
	// The run exits while the hub's record of that is later lost: the node
	// reconnects without it in LiveRuns, so the hub marks it unknown.
	x.disconnect(nodeMac)
	newRun := contracts.Run{RunID: "run_synth-new", SessionID: "ses_synth-new", NodeID: nodeMac, Harness: contracts.HarnessShell, State: contracts.RunWaitingInput, StartedAt: x.clock.Now()}
	x.connect(nodeMac, 0, newRun) // a post-snapshot session appears only as a live run
	if got := x.session(v.Session.SessionID); got.Run.State != contracts.RunUnknown {
		t.Fatalf("before snapshot: %s", got.Run.State)
	}
	if got := x.session("ses_synth-new"); got.Session.Title != "" {
		t.Fatalf("placeholder already titled: %q", got.Session.Title)
	}
	exit := 0
	ended := *v.Run
	ended.State, ended.ExitCode, ended.StateReason = contracts.RunExited, &exit, "exited while the hub was behind"
	snap := contracts.RunSnapshot{Reason: "hub committed 0 < acked 9", Records: []contracts.SessionRun{
		{Session: v.Session, Run: ended},
		{Session: contracts.Session{SessionID: "ses_synth-new", Harness: contracts.HarnessShell, Title: "synthetic shell", Tags: []string{"synth"},
			CreatedBy: owner, CreatedAt: x.clock.Now()}, Run: newRun},
		{Session: contracts.Session{SessionID: "ses_synth-gone", Harness: contracts.HarnessShell, Title: "ended unseen", CreatedBy: owner, CreatedAt: x.clock.Now()},
			Run: contracts.Run{RunID: "run_synth-gone", SessionID: "ses_synth-gone", NodeID: nodeMac, Harness: contracts.HarnessShell, State: contracts.RunExited, ExitCode: &exit, StartedAt: x.clock.Now()}},
	}}
	x.report(nodeMac, contracts.ReportRunSnapshot, snap)
	if got := x.session(v.Session.SessionID); got.Run.State != contracts.RunExited || got.Run.ExitCode == nil {
		t.Fatalf("ended run after snapshot: %+v", got.Run)
	}
	if got := x.session("ses_synth-new"); got.Session.Title != "synthetic shell" || len(got.Session.Tags) != 1 || got.Run.State != contracts.RunWaitingInput {
		t.Fatalf("placeholder session after snapshot: %+v / %+v", got.Session, got.Run)
	}
	if got := x.session("ses_synth-gone"); got.Session.Title != "ended unseen" || got.Run.State != contracts.RunExited {
		t.Fatalf("unseen session after snapshot: %+v / %+v", got.Session, got.Run)
	}
	// A hub-side title is never overwritten by a snapshot.
	snap.Records = []contracts.SessionRun{{Session: contracts.Session{SessionID: v.Session.SessionID, Harness: v.Session.Harness, Title: "node title"}, Run: ended}}
	x.report(nodeMac, contracts.ReportRunSnapshot, snap)
	if got := x.session(v.Session.SessionID); got.Session.Title != v.Session.Title {
		t.Fatalf("hub title overwritten: %q", got.Session.Title)
	}
}
