package hub

import (
	"encoding/json"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

// A start whose answer was lost (the client gave up, or the hub restarted
// mid-call) is retried with the same operation_id after a hub restart: the
// hub returns the session it created and starts no second one; a node
// action (the clone or worktree a fresh launch makes first) returns the same
// command, never a second clone (iss_…).
func TestOperationReplayAcrossHubRestart(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	caller := owner

	first := x.start(caller, "op-start-restart", nodeMac)
	actReq := contracts.NodeActionRequest{Mutation: contracts.Mutation{OperationID: "op-start-restart.clone.0"}, NodeID: nodeMac,
		Action: contracts.ActionRepoClone, Params: json.RawMessage(`{"remote":"https://example.invalid/synth/repo.git"}`)}
	act, err := x.h.InvokeNodeAction(as(owner), actReq)
	if err != nil {
		t.Fatalf("action: %v", err)
	}
	starts := distinctCommands(x.ch.sentKinds(nodeMac, contracts.CmdStartRun))
	actions := distinctCommands(x.ch.sentKinds(nodeMac, contracts.CmdAction))

	x.restart()
	x.connect(nodeMac, 0)

	again, err := x.h.StartSession(as(caller), contracts.StartSessionRequest{Mutation: contracts.Mutation{OperationID: "op-start-restart"}, NodeID: nodeMac,
		Harness: contracts.HarnessClaude, Cwd: "/home/synth/work"})
	if err != nil {
		t.Fatalf("replayed start: %v", err)
	}
	if again.Session.Session.SessionID != first.Session.SessionID {
		t.Fatalf("replay after restart started %s, want the original %s", again.Session.Session.SessionID, first.Session.SessionID)
	}
	act2, err := x.h.InvokeNodeAction(as(owner), actReq)
	if err != nil {
		t.Fatalf("replayed action: %v", err)
	}
	if act2.CommandID != act.CommandID {
		t.Fatalf("replayed action is command %s, want the original %s", act2.CommandID, act.CommandID)
	}
	// Pending commands may be re-sent on reconnect, but no new one is made.
	if n := distinctCommands(x.ch.sentKinds(nodeMac, contracts.CmdStartRun)); n != starts {
		t.Fatalf("%d start commands after the replay, want %d", n, starts)
	}
	if n := distinctCommands(x.ch.sentKinds(nodeMac, contracts.CmdAction)); n != actions {
		t.Fatalf("%d action commands after the replay, want %d", n, actions)
	}
	page, err := x.h.ListSessions(as(owner), contracts.ListSessionsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("%d sessions after the replay, want 1", len(page.Items))
	}
}

func distinctCommands(cmds []contracts.NodeCommand) int {
	seen := map[string]bool{}
	for _, c := range cmds {
		seen[c.CommandID] = true
	}
	return len(seen)
}
