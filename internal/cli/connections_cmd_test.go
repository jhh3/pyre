package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/mock"
)

func TestConnectionsGrantInvokeRevokeVerify(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	r := run(t, e, nil, "--json", "connections", "grant", "github-synth", "--session", "ses_synth-b", "--node", "synth-mac")
	expectCode(t, r, 0)
	g := mustJSON[ConnectionGrantResult](t, r)
	if g.Grant.ConnectionID != "conn_synth-github" || g.Grant.SessionID != "ses_synth-b" || g.Grant.NodeID != "node_synth-mac" || strings.Join(g.PushedTo, ",") != "node_synth-mac" {
		t.Fatalf("grant: %+v", g)
	}
	r = run(t, e, nil, "--json", "connections", "invoke", "github-synth", "list_issues", "--session", "ses_synth-b", "--node", "synth-mac", "--arg", "repo=synthetic/widgets")
	expectCode(t, r, 0)
	if res := mustJSON[c.ConnectionInvokeResult](t, r); res.ConnectionID != "conn_synth-github" || !strings.Contains(string(res.Output), "list_issues") {
		t.Fatalf("invoke: %+v", res)
	}
	// An agent session invokes as itself (no --session needed).
	agent := e.with(EnvToken, sessBToken, EnvSessionID, "ses_synth-b")
	expectCode(t, run(t, agent, nil, "connections", "invoke", "github-synth", "list_issues", "--node", "synth-mac"), 0)
	expectCode(t, run(t, e, nil, "connections", "invoke", "github-synth", "list_issues", "--node", "synth-mac"), ExitUsage)
	expectCode(t, run(t, e, nil, "connections", "grant", "github-synth"), ExitUsage)

	r = run(t, e, nil, "connections", "revoke", "github-synth", "--session", "ses_synth-b", "--node", "synth-mac")
	expectCode(t, r, 0)
	if !strings.Contains(r.stdout, "revoked conn_synth-github") {
		t.Fatalf("revoke text: %s", r)
	}
	r = run(t, e, nil, "--json", "connections", "verify", "github-synth")
	expectCode(t, r, 0)
	if avs := mustJSON[[]c.ConnectionAvailability](t, r); len(avs) != 1 || avs[0].NodeID != "node_synth-mac" || avs[0].State != "verified" {
		t.Fatalf("verify: %+v", avs)
	}
}

func TestNotePromote(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	r := run(t, e, nil, "--json", "note", "promote", "note_synth-handoff-1", "--title", "Synthetic promoted handoff")
	expectCode(t, r, 0)
	res := mustJSON[c.DocumentWithRevision](t, r)
	if res.Document.DocumentID == "" || res.Revision.RevisionID == "" {
		t.Fatalf("promote: %+v", res)
	}
	expectCode(t, run(t, e, nil, "note", "promote", "note_synth-handoff-1"), ExitUsage)
	expectCode(t, run(t, e, nil, "note", "promote", "note_synth-handoff-1", "--doc", res.Document.DocumentID), ExitUsage)
}

func TestConnectionMCPTools(t *testing.T) {
	b := mock.New(mock.Options{Clock: mock.NewOffsetClock(mock.FixtureEpoch), DefaultPrincipal: &testOwner})
	ops := &Ops{API: b, PollInterval: 10 * time.Millisecond, Self: Identity{SessionID: "ses_synth-b", NodeID: "node_synth-mac"}}
	tools := map[string]MCPTool{}
	for _, p := range mcpToolProviders {
		for _, tl := range p() {
			tools[tl.Name] = tl
		}
	}
	chk, inv := tools["connection_check"], tools["connection_invoke"]
	if chk.run == nil || inv.run == nil || !chk.Annotations.ReadOnlyHint || !inv.Annotations.OpenWorldHint {
		t.Fatalf("connection tools not registered: %v", tools)
	}
	ctx := context.Background()
	v, err := chk.run(ctx, ops, json.RawMessage(`{"connection":"github-synth"}`))
	if err != nil {
		t.Fatal(err)
	}
	acc := v.(ConnectionAccess)
	if acc.ConnectionID != "conn_synth-github" || acc.Session != "ses_synth-b" || len(acc.Availability) != 2 {
		t.Fatalf("connection_check: %+v", acc)
	}
	if _, err := inv.run(ctx, ops, json.RawMessage(`{"connection":"github-synth","operation":"list","node":"synth-mac","session":"ses_other"}`)); err == nil || !strings.Contains(err.Error(), `unknown field "session"`) {
		t.Fatalf("connection_invoke must not accept a session argument: %v", err)
	}
	if _, err := b.InvokeNodeAction(ctx, c.NodeActionRequest{Mutation: c.Mutation{OperationID: "op_synth-grant-b"}, NodeID: "node_synth-mac", Action: c.ActionConnGrant,
		Params: c.RawJSON(`{"connection_id":"conn_synth-github","session_id":"ses_synth-b"}`)}); err != nil {
		t.Fatal(err)
	}
	v, err = inv.run(ctx, ops, json.RawMessage(`{"connection":"github-synth","operation":"list","node":"synth-mac","args":{"repo":"synthetic/x"}}`))
	if res, ok := v.(c.ConnectionInvokeResult); err != nil || !ok || !strings.Contains(string(res.Output), `"operation":"list"`) {
		t.Fatalf("connection_invoke: %+v %v", v, err)
	}
}
