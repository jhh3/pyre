package hub

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

func TestHelloProtocolAndWhoAmI(t *testing.T) {
	x := newHarness(t)
	resp, err := x.h.Hello(as(owner), contracts.HelloRequest{Protocol: 1, Client: "pyre-cli/0.1.0"})
	if err != nil || resp.Protocol != 1 || resp.HubID == "" || resp.Principal != owner || resp.Mock {
		t.Fatalf("hello: %v %+v", err, resp)
	}
	_, err = x.h.Hello(context.Background(), contracts.HelloRequest{Protocol: 99, Client: "pyre-cli/9.0.0"})
	wantCode(t, err, contracts.ErrProtocolMismatch)
	if e := contracts.AsError(err); e.Message != "client protocol 99 not supported; hub supports 1..1" || e.Details["min_protocol"] != 1 {
		t.Fatalf("mismatch detail: %+v", e)
	}
	_, err = x.h.WhoAmI(context.Background())
	wantCode(t, err, contracts.ErrUnauthorized)
	// Hub identity survives restart.
	id := x.h.HubID()
	x.restart()
	if x.h.HubID() != id {
		t.Fatal("hub id changed across restart")
	}
}

func TestUnconfiguredDomainsUnsupported(t *testing.T) {
	x := newHarness(t)
	_, err := x.h.ListNodes(as(owner), contracts.ListNodesRequest{})
	wantCode(t, err, contracts.ErrUnsupported)
	_, err = x.h.CreateProject(as(owner), contracts.CreateProjectRequest{Name: "p"})
	wantCode(t, err, contracts.ErrUnsupported)
	_, _, err = x.h.OpenContent(as(owner), "ver_x")
	wantCode(t, err, contracts.ErrUnsupported)
	_, err = x.h.ListAttention(as(owner), contracts.ListAttentionRequest{})
	wantCode(t, err, contracts.ErrUnsupported)
}

func TestStartSessionValidation(t *testing.T) {
	x := newHarness(t)
	req := func(op, node string, hn contracts.Harness) contracts.StartSessionRequest {
		return contracts.StartSessionRequest{Mutation: contracts.Mutation{OperationID: op}, NodeID: node, Harness: hn}
	}
	_, err := x.h.StartSession(as(owner), req("op_1", nodeMac, contracts.HarnessClaude))
	wantCode(t, err, contracts.ErrNotFound) // never said hello
	x.connect(nodeMac, 0)
	x.disconnect(nodeMac)
	_, err = x.h.StartSession(as(owner), req("op_2", nodeMac, contracts.HarnessClaude))
	wantCode(t, err, contracts.ErrUnavailable)
	x.connect(nodeMac, 0)
	_, err = x.h.StartSession(as(owner), req("op_3", nodeMac, contracts.HarnessCodex))
	wantCode(t, err, contracts.ErrUnsupported)
	if !strings.Contains(err.Error(), "codex executable not found") {
		t.Fatalf("reason missing: %v", err)
	}
	_, err = x.h.StartSession(as(owner), req("op_4", nodeMac, ""))
	wantCode(t, err, contracts.ErrInvalid)
	_, err = x.h.StartSession(context.Background(), req("op_5", nodeMac, contracts.HarnessClaude))
	wantCode(t, err, contracts.ErrUnauthorized)
	r := req("op_6", nodeMac, contracts.HarnessClaude)
	r.AccountPolicy = contracts.AccountPolicy{Mode: contracts.AccountExplicit}
	_, err = x.h.StartSession(as(owner), r)
	wantCode(t, err, contracts.ErrInvalid)
	// Capacity: max_runs 4 in helloFor.
	for i := 0; i < 4; i++ {
		if _, err := x.h.StartSession(as(owner), req("op_cap"+string(rune('a'+i)), nodeMac, contracts.HarnessShell)); err != nil {
			t.Fatal(err)
		}
	}
	_, err = x.h.StartSession(as(owner), req("op_cap_full", nodeMac, contracts.HarnessShell))
	wantCode(t, err, contracts.ErrCapacityExhausted)
}

func TestStartSessionLaunchFlowAndIdempotency(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	resp, err := x.h.StartSession(as(owner), contracts.StartSessionRequest{Mutation: contracts.Mutation{OperationID: "op_synth-start-1"}, NodeID: nodeMac,
		Harness: contracts.HarnessClaude, Cwd: "/Users/synth/scratch", AccountPolicy: contracts.AccountPolicy{Mode: contracts.AccountEligible}})
	if err != nil {
		t.Fatal(err)
	}
	s := resp.Session
	if s.Session.ProjectID != nil || s.Run == nil || s.Run.State != contracts.RunStarting || s.Session.CurrentRunID != s.Run.RunID || resp.CommandID == "" {
		t.Fatalf("start response: %+v", resp)
	}
	if s.Session.Title != "scratch" || s.Session.CreatedBy != owner {
		t.Fatalf("session: %+v", s.Session)
	}
	sent := x.ch.sentKinds(nodeMac, contracts.CmdStartRun)
	if len(sent) != 1 || sent[0].CommandID != resp.CommandID || sent[0].Seq != 1 {
		t.Fatalf("start command: %+v", sent)
	}
	var sr contracts.StartRunCommand
	json.Unmarshal(sent[0].Payload, &sr)
	if sr.SessionToken == "" || sr.RunID != s.Run.RunID || sr.Cwd != "/Users/synth/scratch" {
		t.Fatalf("payload: %+v", sr)
	}
	// The session token is injected at send time and never persisted.
	var stored string
	x.db.QueryRow(`SELECT payload FROM commands WHERE command_id=?`, resp.CommandID).Scan(&stored)
	if strings.Contains(stored, "synthetic-session-token") {
		t.Fatal("session token persisted")
	}
	var leaked int
	x.db.QueryRow(`SELECT COUNT(*) FROM events WHERE payload LIKE '%synthetic-session-token%'`).Scan(&leaked)
	if leaked != 0 {
		t.Fatal("token in events")
	}
	// Replay returns the same session; no second command.
	again, err := x.h.StartSession(as(owner), contracts.StartSessionRequest{Mutation: contracts.Mutation{OperationID: "op_synth-start-1"}, NodeID: nodeMac,
		Harness: contracts.HarnessClaude, Cwd: "/Users/synth/scratch", AccountPolicy: contracts.AccountPolicy{Mode: contracts.AccountEligible}})
	if err != nil || again.Session.Session.SessionID != s.Session.SessionID || len(x.ch.sentKinds(nodeMac, contracts.CmdStartRun)) != 1 {
		t.Fatalf("replay: %v", err)
	}
	// A replay still succeeds after the node disconnects.
	x.disconnect(nodeMac)
	if _, err := x.h.StartSession(as(owner), contracts.StartSessionRequest{Mutation: contracts.Mutation{OperationID: "op_synth-start-1"}, NodeID: nodeMac,
		Harness: contracts.HarnessClaude, Cwd: "/Users/synth/scratch", AccountPolicy: contracts.AccountPolicy{Mode: contracts.AccountEligible}}); err != nil {
		t.Fatalf("replay after disconnect: %v", err)
	}
	x.connect(nodeMac, 1)
	// Ack + launch result -> run observed.
	x.ack(nodeMac, sent[0])
	run := *s.Run
	run.State = contracts.RunWaitingInput
	run.NativeRef = &contracts.NativeRef{Kind: "claude_session", ID: "00000000-0000-4000-8000-000000000001"}
	x.result(nodeMac, sent[0], contracts.OutcomeSucceeded, run)
	v := x.session(s.Session.SessionID)
	if v.Run.State != contracts.RunWaitingInput || v.Run.Freshness.Stale || v.Run.NativeRef == nil || v.NodeName == "" {
		t.Fatalf("after launch: %+v", v.Run)
	}
	if x.countEvents(contracts.EvSessionCreated) != 1 || x.countEvents(contracts.EvRunStarted) != 1 || x.countEvents(contracts.EvCommandResult) != 1 {
		t.Fatal("unexpected event counts")
	}
	// Pinned payload shapes.
	var payload string
	x.db.QueryRow(`SELECT payload FROM events WHERE kind='run.started'`).Scan(&payload)
	var pr contracts.Run
	if err := json.Unmarshal([]byte(payload), &pr); err != nil || pr.RunID != s.Run.RunID {
		t.Fatalf("run.started payload: %v %s", err, payload)
	}
	x.db.QueryRow(`SELECT payload FROM events WHERE kind='session.created'`).Scan(&payload)
	var ps contracts.Session
	if err := json.Unmarshal([]byte(payload), &ps); err != nil || ps.SessionID != s.Session.SessionID {
		t.Fatalf("session.created payload: %v", err)
	}
	x.db.QueryRow(`SELECT payload FROM events WHERE kind='command.result'`).Scan(&payload)
	var pc contracts.CommandResult
	if err := json.Unmarshal([]byte(payload), &pc); err != nil || pc.CommandID != resp.CommandID || pc.Outcome != contracts.OutcomeSucceeded {
		t.Fatalf("command.result payload: %v %s", err, payload)
	}
}

func TestParentSessionSetFromPrincipal(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	parent := x.start(owner, "op_1", nodeMac)
	child, err := x.h.StartSession(as(sesPrincipal(parent.Session.SessionID)), contracts.StartSessionRequest{
		Mutation: contracts.Mutation{OperationID: "op_2"}, NodeID: nodeMac, Harness: contracts.HarnessClaude, ParentSessionID: "ses_spoofed"})
	if err != nil {
		t.Fatal(err)
	}
	if child.Session.Session.Lineage.ParentSessionID != parent.Session.SessionID {
		t.Fatalf("parent: %+v", child.Session.Session.Lineage)
	}
	_, err = x.h.StartSession(as(owner), contracts.StartSessionRequest{Mutation: contracts.Mutation{OperationID: "op_3"}, NodeID: nodeMac, Harness: contracts.HarnessClaude, ParentSessionID: "ses_missing"})
	wantCode(t, err, contracts.ErrInvalid)
}

func TestLaunchFailureAndExpiry(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	resp, err := x.h.StartSession(as(owner), contracts.StartSessionRequest{Mutation: contracts.Mutation{OperationID: "op_1"}, NodeID: nodeMac, Harness: contracts.HarnessClaude})
	if err != nil {
		t.Fatal(err)
	}
	cmd := x.ch.sentKinds(nodeMac, contracts.CmdStartRun)[0]
	b, _ := json.Marshal(contracts.CommandResult{CommandID: cmd.CommandID, Kind: cmd.Kind, Outcome: contracts.OutcomeFailed, Error: contracts.Errorf(contracts.ErrInvalid, "cwd does not exist")})
	x.h.HandleReport(nodeCtx(nodeMac), nodeMac, contracts.NodeReport{OriginSeq: 1, Kind: contracts.ReportCommandResult, Payload: b})
	v := x.session(resp.Session.Session.SessionID)
	if v.Run.State != contracts.RunFailed || !strings.Contains(v.Run.StateReason, "cwd does not exist") {
		t.Fatalf("failed launch: %+v", v.Run)
	}
	// A launch sent but never acknowledged is resent after reconnect even
	// past its expiry: the node may hold it (lost ack), and it refuses to
	// start an expired command it does not hold.
	if _, err := x.h.StartSession(as(owner), contracts.StartSessionRequest{Mutation: contracts.Mutation{OperationID: "op_2"}, NodeID: nodeMac, Harness: contracts.HarnessClaude}); err != nil {
		t.Fatal(err)
	}
	x.disconnect(nodeMac)
	// A command queued while the node is offline expires undelivered.
	act, err := x.h.InvokeNodeAction(as(owner), contracts.NodeActionRequest{Mutation: contracts.Mutation{OperationID: "op_3"}, NodeID: nodeMac, Action: contracts.ActionInventory, TimeoutSeconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	x.clock.Add(time.Hour)
	x.connect(nodeMac, 1)
	starts := x.ch.sentKinds(nodeMac, contracts.CmdStartRun)
	if len(starts) != 3 || starts[2].CommandID != starts[1].CommandID {
		t.Fatalf("start sends: %d (want the unacked launch resent once)", len(starts))
	}
	if n := len(x.ch.sentKinds(nodeMac, contracts.CmdAction)); n != 0 {
		t.Fatalf("never-sent expired command was sent (%d)", n)
	}
	if r, err := x.h.GetCommandResult(as(owner), contracts.GetRequest{ID: act.CommandID}); err != nil || r.Error == nil || r.Error.Code != contracts.ErrExpired {
		t.Fatalf("expired action: %+v %v", r, err)
	}
}

func TestDisconnectedNodeShowsStaleUnknownWithoutMutation(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeLin, 0)
	v := x.start(owner, "op_1", nodeLin)
	runID := v.Run.RunID
	x.clock.Add(time.Minute)
	x.disconnect(nodeLin)
	x.clock.Add(7 * time.Minute)
	got := x.session(v.Session.SessionID)
	if got.Run.State != contracts.RunUnknown || !got.Run.Freshness.Stale || got.Run.Freshness.Reason != "node disconnected" ||
		!got.Freshness.Stale || got.Freshness.Source != "hub-cache" {
		t.Fatalf("stale view: %+v / %+v", got.Run, got.Freshness)
	}
	if !strings.Contains(got.Run.StateReason, "waiting_input") {
		t.Fatalf("last observed state missing: %q", got.Run.StateReason)
	}
	if x.storedRunState(runID) != contracts.RunWaitingInput {
		t.Fatalf("stored state mutated to %s", x.storedRunState(runID))
	}
	// State filters apply to the presented state.
	page, _ := x.h.ListSessions(as(owner), contracts.ListSessionsRequest{States: []contracts.RunState{contracts.RunUnknown}})
	if len(page.Items) != 1 {
		t.Fatalf("filter unknown: %d", len(page.Items))
	}
	// Reconnect with the run live: fresh again.
	live := *got.Run
	live.State = contracts.RunRunning
	x.connect(nodeLin, 1, live)
	got = x.session(v.Session.SessionID)
	if got.Run.State != contracts.RunRunning || got.Run.Freshness.Stale {
		t.Fatalf("after reconnect: %+v", got.Run)
	}
}

func TestListSessionsFiltersAndPaging(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	x.connect(nodeLin, 0)
	for i := 0; i < 3; i++ {
		x.start(owner, "op_m"+string(rune('a'+i)), nodeMac)
		x.clock.Add(time.Second)
	}
	l := x.start(owner, "op_l", nodeLin)
	pid := "prj_synth-1"
	if _, err := x.h.AssociateSession(as(owner), contracts.AssociateRequest{Mutation: contracts.Mutation{OperationID: "op_assoc"}, SessionID: l.Session.SessionID, ProjectID: &pid}); err != nil {
		t.Fatal(err)
	}
	all, _ := x.h.ListSessions(as(owner), contracts.ListSessionsRequest{})
	if len(all.Items) != 4 || all.Items[0].Session.SessionID != l.Session.SessionID {
		t.Fatalf("all: %d", len(all.Items))
	}
	byNode, _ := x.h.ListSessions(as(owner), contracts.ListSessionsRequest{NodeID: nodeMac})
	empty := ""
	standalone, _ := x.h.ListSessions(as(owner), contracts.ListSessionsRequest{ProjectID: &empty})
	inProject, _ := x.h.ListSessions(as(owner), contracts.ListSessionsRequest{ProjectID: &pid})
	if len(byNode.Items) != 3 || len(standalone.Items) != 3 || len(inProject.Items) != 1 {
		t.Fatalf("filters: %d %d %d", len(byNode.Items), len(standalone.Items), len(inProject.Items))
	}
	var ids []string
	cur := ""
	for {
		p, err := x.h.ListSessions(as(owner), contracts.ListSessionsRequest{PageRequest: contracts.PageRequest{Limit: 3, Cursor: cur}})
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range p.Items {
			ids = append(ids, it.Session.SessionID)
		}
		if p.NextCursor == "" {
			break
		}
		cur = p.NextCursor
	}
	if len(ids) != 4 {
		t.Fatalf("paging: %v", ids)
	}
	// Archive hides and revokes tokens.
	arch := true
	if _, err := x.h.UpdateSession(as(owner), contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: "op_arch"}, SessionID: l.Session.SessionID, Archived: &arch}); err != nil {
		t.Fatal(err)
	}
	if len(x.tokens.revoked) != 1 {
		t.Fatal("tokens not revoked on archive")
	}
	all, _ = x.h.ListSessions(as(owner), contracts.ListSessionsRequest{})
	if len(all.Items) != 3 {
		t.Fatalf("archived listed: %d", len(all.Items))
	}
	runs, err := x.h.ListRuns(as(owner), contracts.ListRunsRequest{SessionID: l.Session.SessionID})
	if err != nil || len(runs.Items) != 1 {
		t.Fatalf("runs: %v", err)
	}
}

func TestControlCapabilitiesAndReceipts(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	v := x.start(owner, "op_1", nodeMac)
	// Auto-answer stop commands.
	x.ch.onSend = func(node string, cmd contracts.NodeCommand) {
		if cmd.Kind == contracts.CmdStop {
			go x.result(node, cmd, contracts.OutcomeSucceeded, contracts.CommandReceipt{CommandID: cmd.CommandID, Accepted: true, Outcome: contracts.OutcomeSucceeded, Detail: "SessionEnd observed"})
		}
	}
	rc, err := x.h.StopSession(as(owner), contracts.SessionCommandRequest{Mutation: contracts.Mutation{OperationID: "op_stop"}, SessionID: v.Session.SessionID})
	if err != nil || rc.Outcome != contracts.OutcomeSucceeded || rc.Detail != "SessionEnd observed" || !rc.Accepted {
		t.Fatalf("stop: %v %+v", err, rc)
	}
	// Run id must match the current run.
	_, err = x.h.InterruptSession(as(owner), contracts.SessionCommandRequest{Mutation: contracts.Mutation{OperationID: "op_int"}, SessionID: v.Session.SessionID, RunID: "run_other"})
	wantCode(t, err, contracts.ErrConflict)
	// A shell run has no interrupt capability: unsupported, never degraded.
	sh, err := x.h.StartSession(as(owner), contracts.StartSessionRequest{Mutation: contracts.Mutation{OperationID: "op_sh"}, NodeID: nodeMac, Harness: contracts.HarnessShell})
	if err != nil {
		t.Fatal(err)
	}
	_, err = x.h.InterruptSession(as(owner), contracts.SessionCommandRequest{Mutation: contracts.Mutation{OperationID: "op_int2"}, SessionID: sh.Session.Session.SessionID})
	wantCode(t, err, contracts.ErrUnsupported)
	// Interrupt to a disconnected node: accepted and pending, no wait.
	x.disconnect(nodeMac)
	start := time.Now()
	rc, err = x.h.InterruptSession(as(owner), contracts.SessionCommandRequest{Mutation: contracts.Mutation{OperationID: "op_int3"}, SessionID: v.Session.SessionID})
	if err != nil || rc.Outcome != contracts.OutcomePending || time.Since(start) > time.Second {
		t.Fatalf("pending interrupt: %v %+v", err, rc)
	}
}

func TestResumeRules(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	v := x.start(owner, "op_1", nodeMac)
	_, err := x.h.ResumeSession(as(owner), contracts.ResumeRequest{Mutation: contracts.Mutation{OperationID: "op_r1"}, SessionID: v.Session.SessionID})
	wantCode(t, err, contracts.ErrConflict) // current run is live
	// Run exits.
	run := *v.Run
	run.State = contracts.RunExited
	x.report(nodeMac, contracts.ReportRun, run)
	resp, err := x.h.ResumeSession(as(owner), contracts.ResumeRequest{Mutation: contracts.Mutation{OperationID: "op_r2"}, SessionID: v.Session.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	nr := resp.Session.Run
	if nr.PreviousRunID != run.RunID || nr.ContinuationKind != contracts.ContinuationNativeResume || resp.Session.Session.CurrentRunID != nr.RunID {
		t.Fatalf("resume: %+v", nr)
	}
	cmds := x.ch.sentKinds(nodeMac, contracts.CmdResume)
	if len(cmds) != 1 {
		t.Fatalf("resume command: %d", len(cmds))
	}
	var rcmd contracts.ResumeCommand
	json.Unmarshal(cmds[0].Payload, &rcmd)
	if rcmd.PreviousRun == nil || rcmd.PreviousRun.RunID != run.RunID || rcmd.SessionToken == "" {
		t.Fatalf("resume payload: %+v", rcmd)
	}
	// Native resume on another node requires allow_handoff.
	x.connect(nodeLin, 0)
	run2 := *nr
	run2.State = contracts.RunExited
	x.report(nodeMac, contracts.ReportRun, run2)
	_, err = x.h.ResumeSession(as(owner), contracts.ResumeRequest{Mutation: contracts.Mutation{OperationID: "op_r3"}, SessionID: v.Session.SessionID, NodeID: nodeLin})
	wantCode(t, err, contracts.ErrUnsupported)
	resp, err = x.h.ResumeSession(as(owner), contracts.ResumeRequest{Mutation: contracts.Mutation{OperationID: "op_r4"}, SessionID: v.Session.SessionID, NodeID: nodeLin, AllowHandoff: true})
	if err != nil || resp.Session.Run.ContinuationKind != contracts.ContinuationHandoff || resp.Session.Run.NodeID != nodeLin {
		t.Fatalf("handoff: %v", err)
	}
}
