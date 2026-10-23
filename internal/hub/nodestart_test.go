package hub

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

// A node credential (readable by every process of the user pyred runs as)
// starts only a headless pyre run session on its own node: claude, the exec
// claim argument and nothing else, no parent, prompt or report, tags
// forced, and no session token minted for it. Before this check a node
// credential could start any harness with any prompt on any node.
func TestNodeStartOnlyHeadlessOnItself(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	x.connect(nodeLin, 0)
	hash := "--pyre-exec=" + strings.Repeat("ab", 32)
	no, yes := false, true
	ok := func(op string) contracts.StartSessionRequest {
		return contracts.StartSessionRequest{Mutation: contracts.Mutation{OperationID: op}, NodeID: nodeLin, Harness: contracts.HarnessClaude,
			Title: "sentry triage", Cwd: "/home/synth/triage", Args: []string{hash}, ReportToParent: &no,
			Tags: []string{contracts.TagPyreRun, contracts.TagReportNone, contracts.TagArchiveWhenDone}}
	}
	parent := x.start(owner, "op_parent", nodeLin)
	cases := map[string]func(r *contracts.StartSessionRequest){
		"another node": func(r *contracts.StartSessionRequest) { r.NodeID = nodeMac },
		"node selector": func(r *contracts.StartSessionRequest) {
			r.NodeID, r.NodeSelector = "", &contracts.NodeSelector{LeastLoaded: true}
		},
		"shell":            func(r *contracts.StartSessionRequest) { r.Harness = contracts.HarnessShell },
		"agy":              func(r *contracts.StartSessionRequest) { r.Harness = contracts.HarnessAgy },
		"no exec arg":      func(r *contracts.StartSessionRequest) { r.Args = nil },
		"bad hash":         func(r *contracts.StartSessionRequest) { r.Args = []string{"--pyre-exec=abc"} },
		"split exec arg":   func(r *contracts.StartSessionRequest) { r.Args = []string{"--pyre-exec", strings.Repeat("ab", 32)} },
		"two hashes":       func(r *contracts.StartSessionRequest) { r.Args = []string{hash, hash} },
		"extra arg":        func(r *contracts.StartSessionRequest) { r.Args = []string{hash, "--dangerously-skip-permissions"} },
		"prompt":           func(r *contracts.StartSessionRequest) { r.InitialPrompt = "rm -rf ~" },
		"parent":           func(r *contracts.StartSessionRequest) { r.ParentSessionID = parent.Session.SessionID },
		"report to parent": func(r *contracts.StartSessionRequest) { r.ReportToParent = &yes },
		"checkout":         func(r *contracts.StartSessionRequest) { r.Checkout = &contracts.CheckoutRequest{} },
		"env profile":      func(r *contracts.StartSessionRequest) { r.EnvProfile = "prod" },
		"launch context":   func(r *contracts.StartSessionRequest) { r.IncludeContext = true },
	}
	for name, mod := range cases {
		r := ok("op_bad_" + strings.ReplaceAll(name, " ", "_"))
		mod(&r)
		_, err := x.h.StartSession(nodeCtx(nodeLin), r)
		if !contracts.IsCode(err, contracts.ErrForbidden) {
			t.Errorf("%s: want forbidden, got %v", name, err)
		}
	}
	if n := len(x.ch.sentKinds(nodeMac, contracts.CmdStartRun)); n != 0 {
		t.Fatalf("%d start commands reached the other node", n)
	}
	before := x.tokens.issued

	// The allowed shape, with the caller's tags replaced by the forced ones.
	r := ok("op_node_ok")
	r.Tags = []string{"report:parent", "owner-looking"}
	r.ReportToParent = nil
	r.Args = []string{hash, "--pyre-exec-resume=0b6a6c1e-0000-4000-8000-000000000001"}
	resp, err := x.h.StartSession(nodeCtx(nodeLin), r)
	if err != nil {
		t.Fatal(err)
	}
	s := resp.Session.Session
	want := []string{contracts.TagPyreRun, contracts.TagReportNone, contracts.TagArchiveWhenDone}
	if !slices.Equal(s.Tags, want) || s.Parent() != "" || s.CreatedBy.Kind != contracts.PrincipalNode || s.CreatedBy.ID != nodeLin || s.Title != "sentry triage" {
		t.Fatalf("session = %+v", s)
	}
	sent := x.ch.sentKinds(nodeLin, contracts.CmdStartRun)
	var sr contracts.StartRunCommand
	_ = json.Unmarshal(sent[len(sent)-1].Payload, &sr)
	if sr.Session.SessionID != s.SessionID || sr.SessionToken != "" || sr.LaunchContext != nil || !slices.Equal(sr.Args, r.Args) {
		t.Fatalf("start command = %+v", sr)
	}
	if x.tokens.issued != before {
		t.Fatal("a session token was minted for a node-started run")
	}
	// A node cannot resume sessions (its own headless one, or the owner's:
	// a resume is a start of any harness, with a fresh session token), nor
	// retag the session it started; the owner can.
	for _, id := range []string{s.SessionID, parent.Session.SessionID} {
		_, err := x.h.ResumeSession(nodeCtx(nodeLin), contracts.ResumeRequest{Mutation: contracts.Mutation{OperationID: "op_resume_" + id}, SessionID: id})
		wantCode(t, err, contracts.ErrForbidden)
	}
	retag := []string{"owner-looking"}
	_, err = x.h.UpdateSession(nodeCtx(nodeLin), contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: "op_retag_node"}, SessionID: s.SessionID, Tags: &retag})
	wantCode(t, err, contracts.ErrForbidden)
	if _, err := x.h.UpdateSession(as(owner), contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: "op_retag_owner"}, SessionID: s.SessionID, Tags: &retag}); err != nil {
		t.Fatalf("owner retag: %v", err)
	}
	if x.tokens.issued != before {
		t.Fatal("a session token was minted for a node request")
	}

	// The owner's starts are unchanged: any harness, a session token.
	x.start(owner, "op_owner_shell", nodeMac)
	if x.tokens.issued == before {
		t.Fatal("owner start minted no session token")
	}
}
