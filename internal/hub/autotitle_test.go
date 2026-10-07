package hub

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/jhh3/pyre/internal/autotitle"
	"github.com/jhh3/pyre/internal/contracts"
)

func titleEvent(sessionID, runID, title string) contracts.Event {
	b, _ := json.Marshal(autotitle.Suggestion{SessionID: sessionID, RunID: runID, Title: title, Source: autotitle.SourceNative})
	return contracts.Event{Kind: autotitle.EventTitleSuggested, Object: contracts.ObjectRef{Kind: contracts.KindSession, ID: sessionID}, SessionID: sessionID, Payload: b}
}

// Precedence: untitled sessions start automatic; node suggestions apply only
// while automatic and only from the run's node; an agent may rename any
// session but never over the owner's title; the owner's rename always wins
// and stops auto-naming.
func TestAutoTitlePrecedence(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	x.connect(nodeLin, 0)
	v := x.start(owner, "op_s", nodeMac)
	sid, rid := v.Session.SessionID, v.Run.RunID
	if v.Session.Title != "work" || !slices.Contains(v.Session.Tags, autotitle.TagAuto) {
		t.Fatalf("untitled session should start automatic: %q %v", v.Session.Title, v.Session.Tags)
	}
	// A titled launch (the launch flow's t) is not automatic.
	resp, err := x.h.StartSession(as(owner), contracts.StartSessionRequest{Mutation: contracts.Mutation{OperationID: "op_t"}, NodeID: nodeMac,
		Harness: contracts.HarnessClaude, Cwd: "/home/synth/work", Title: "synthetic chosen"})
	if err != nil || slices.Contains(resp.Session.Session.Tags, autotitle.TagAuto) {
		t.Fatalf("titled launch tagged auto: %v %v", err, resp.Session.Session.Tags)
	}
	// Shells keep the cwd name.
	sh, err := x.h.StartSession(as(owner), contracts.StartSessionRequest{Mutation: contracts.Mutation{OperationID: "op_sh"}, NodeID: nodeMac,
		Harness: contracts.HarnessShell, Cwd: "/home/synth/work"})
	if err != nil || slices.Contains(sh.Session.Session.Tags, autotitle.TagAuto) {
		t.Fatalf("shell tagged auto: %v %v", err, sh.Session.Session.Tags)
	}

	x.report(nodeMac, contracts.ReportEvent, titleEvent(sid, rid, "Fix synthetic webhook retries"))
	if got := x.session(sid).Session; got.Title != "Fix synthetic webhook retries" || !slices.Contains(got.Tags, autotitle.TagAuto) {
		t.Fatalf("suggestion not applied: %q %v", got.Title, got.Tags)
	}
	if n := x.countEvents(autotitle.EventTitleSuggested); n != 0 {
		t.Fatalf("raw suggestion events stored: %d", n)
	}
	// Another node cannot name this node's run.
	x.report(nodeLin, contracts.ReportEvent, titleEvent(sid, rid, "hijack"))
	if got := x.session(sid).Session.Title; got != "Fix synthetic webhook retries" {
		t.Fatalf("other node renamed: %q", got)
	}

	// An agent may rename any session (owner decision, W-childnotify), and
	// it marks the title as agent-chosen.
	agent := contracts.Principal{Kind: contracts.PrincipalSession, ID: sid}
	other := contracts.Principal{Kind: contracts.PrincipalSession, ID: resp.Session.Session.SessionID}
	peerName := "Peer picked name"
	if got, err := x.h.UpdateSession(as(other), contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: "op_r0"}, SessionID: sid, Title: &peerName}); err != nil ||
		got.Session.Title != peerName || !slices.Contains(got.Session.Tags, autotitle.TagAgent) {
		t.Fatalf("agent renaming another session: %v", err)
	}
	name := "Agent picked name"
	if _, err := x.h.UpdateSession(as(other), contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: "op_r1"}, SessionID: other.ID, Title: &name}); err != nil {
		t.Fatalf("agent renaming over the owner's launch title: %v", err)
	}
	got, err := x.h.UpdateSession(as(agent), contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: "op_r2"}, SessionID: sid, Title: &name})
	if err != nil || got.Session.Title != name || slices.Contains(got.Session.Tags, autotitle.TagAuto) || !slices.Contains(got.Session.Tags, autotitle.TagAgent) {
		t.Fatalf("agent self-rename: %v %q %v", err, got.Session.Title, got.Session.Tags)
	}
	x.report(nodeMac, contracts.ReportEvent, titleEvent(sid, rid, "Claude later title"))
	if got := x.session(sid).Session.Title; got != name {
		t.Fatalf("suggestion overrode the agent's name: %q", got)
	}

	// The owner's rename ends auto-naming; an agent may still rename on request.
	mine := "Owner's name"
	got, err = x.h.UpdateSession(as(owner), contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: "op_r3"}, SessionID: sid, Title: &mine})
	if err != nil || got.Session.Title != mine || slices.ContainsFunc(got.Session.Tags, autotitle.Reserved) {
		t.Fatalf("owner rename: %v %q %v", err, got.Session.Title, got.Session.Tags)
	}
	if _, err := x.h.UpdateSession(as(agent), contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: "op_r4"}, SessionID: sid, Title: &name}); err != nil {
		t.Fatalf("agent renaming over the owner's name: %v", err)
	}
	if _, err := x.h.UpdateSession(as(owner), contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: "op_r4b"}, SessionID: sid, Title: &mine}); err != nil {
		t.Fatal(err)
	}
	x.report(nodeMac, contracts.ReportEvent, titleEvent(sid, rid, "Claude later title"))
	if got := x.session(sid).Session.Title; got != mine {
		t.Fatalf("suggestion overrode the owner's name: %q", got)
	}
	empty := "  "
	if _, err := x.h.UpdateSession(as(owner), contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: "op_r5"}, SessionID: sid, Title: &empty}); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("empty title accepted: %v", err)
	}
}
