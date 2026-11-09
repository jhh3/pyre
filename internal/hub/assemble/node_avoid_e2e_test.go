package assemble

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/cli"
	c "github.com/jhh3/pyre/internal/contracts"
)

// mcpToolCall serves one MCP tools/call through the real MCP server and
// returns the result text and whether it is a tool error.
func mcpToolCall(t *testing.T, ctx context.Context, ops *cli.Ops, name string, args map[string]any) (string, bool) {
	t.Helper()
	var in bytes.Buffer
	enc := json.NewEncoder(&in)
	_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "synth", "version": "0"}}})
	_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	var out bytes.Buffer
	srv := &cli.MCPServer{Connect: func() (*cli.Ops, error) { return ops, nil }, Tools: cli.MCPTools()}
	if err := srv.Serve(ctx, &in, &out); err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(&out)
	sc.Buffer(make([]byte, 1<<20), 1<<22)
	for sc.Scan() {
		var resp struct {
			ID     int `json:"id"`
			Result struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
				IsError bool `json:"isError"`
			} `json:"result"`
			Error any `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &resp) != nil || resp.ID != 2 {
			continue
		}
		if resp.Error != nil || len(resp.Result.Content) == 0 {
			t.Fatalf("%s: protocol error %v", name, resp.Error)
		}
		return resp.Result.Content[0].Text, resp.Result.IsError
	}
	t.Fatalf("%s: no response in %s", name, out.String())
	return "", false
}

// Node avoid flags end to end through the assembled hub (real fleet,
// notes and storage; simulated node): the owner's CLI sets a hard flag,
// `pyre nodes` shows it, a named start is refused until --ignore-avoid,
// and a session sees it through MCP get_context and list_nodes but cannot
// clear or override it; a session's own flag is attributed to it.
func TestNodeAvoidEndToEnd(t *testing.T) {
	st, n := newStack(t)
	bg := context.Background()
	owner := c.WithPrincipal(bg, ownerP)
	resp, err := st.Hub.StartSession(owner, c.StartSessionRequest{Mutation: c.Mutation{OperationID: c.NewID(c.PrefixOperation)}, NodeID: n.nodeID, Harness: c.HarnessMock, Cwd: "/tmp/synth", Title: "synthetic avoid peer"})
	if err != nil {
		t.Fatal(err)
	}
	n.idle()
	sid := resp.Session.Session.SessionID
	sess := c.WithPrincipal(bg, c.Principal{Kind: c.PrincipalSession, ID: sid})

	pyre := func(args ...string) (int, string) {
		var out, errb bytes.Buffer
		app := &cli.App{API: st.Hub, Stdout: &out, Stderr: &errb, Stdin: strings.NewReader(""), Getenv: func(string) string { return "" }}
		code := app.Main(owner, args)
		n.idle()
		return code, out.String() + errb.String()
	}
	if code, out := pyre("node", "avoid", "synth-mac", "--hard", "--for", "1h", "--reason", "synthetic: presenting"); code != 0 || !strings.Contains(out, "avoid synth-mac: hard until") || !strings.Contains(out, "set by the owner — synthetic: presenting") {
		t.Fatalf("set: exit %d\n%s", code, out)
	}
	code, out := pyre("--json", "nodes")
	if code != 0 {
		t.Fatalf("nodes: %d\n%s", code, out)
	}
	var page c.Page[c.Node]
	if err := json.Unmarshal([]byte(out), &page); err != nil || len(page.Items) != 1 || len(page.Items[0].Avoid) != 1 || !page.Items[0].Avoid[0].OwnerSet || !page.Items[0].Avoid[0].Hard {
		t.Fatalf("nodes --json: %v\n%s", err, out)
	}
	if code, out = pyre("nodes"); code != 0 || !strings.Contains(out, "avoid synth-mac: hard until") {
		t.Fatalf("nodes text: %d\n%s", code, out)
	}
	start := []string{"start", "--node", "synth-mac", "--harness", "mock", "--cwd", "/tmp/synth", "--wait", "0"}
	if code, out = pyre(start...); code == 0 || !strings.Contains(out, "avoided (hard)") || !strings.Contains(out, "--ignore-avoid") {
		t.Fatalf("hard flag did not refuse: %d\n%s", code, out)
	}
	if code, out = pyre(append(start, "--ignore-avoid")...); code != 0 || !strings.Contains(out, "is avoided (hard until") {
		t.Fatalf("owner override: %d\n%s", code, out)
	}

	// The session: sees the flag, cannot clear or override the owner's.
	ops := &cli.Ops{API: st.Hub, Self: cli.Identity{SessionID: sid, NodeID: n.nodeID}}
	if text, isErr := mcpToolCall(t, sess, ops, "get_context", map[string]any{}); isErr || !strings.Contains(text, "synth-mac: hard, until") || !strings.Contains(text, "synthetic: presenting") {
		t.Fatalf("get_context: %v\n%s", isErr, text)
	}
	if text, isErr := mcpToolCall(t, sess, ops, "list_nodes", map[string]any{}); isErr || !strings.Contains(text, "avoid synth-mac: hard until") {
		t.Fatalf("list_nodes: %v\n%s", isErr, text)
	}
	if text, isErr := mcpToolCall(t, sess, ops, "avoid_node", map[string]any{"node": "synth-mac", "clear": true}); !isErr || !strings.Contains(text, "only the owner clears it") {
		t.Fatalf("session cleared the owner's flag: %v\n%s", isErr, text)
	}
	if text, isErr := mcpToolCall(t, sess, ops, "start_session", map[string]any{"node": "synth-mac", "harness": "mock", "cwd": "/tmp/synth", "ignore_avoid": true}); !isErr || !strings.Contains(text, "only the owner's credentials override it") {
		t.Fatalf("session overrode the owner's flag: %v\n%s", isErr, text)
	}
	n.idle()

	// The owner clears it; the session sets its own, attributed to it.
	if code, out = pyre("node", "avoid", "--clear", "synth-mac"); code != 0 || !strings.Contains(out, "cleared 1 avoid flag(s) on synth-mac") {
		t.Fatalf("owner clear: %d\n%s", code, out)
	}
	text, isErr := mcpToolCall(t, sess, ops, "avoid_node", map[string]any{"node": "synth-mac", "for": "30m", "reason": "synthetic: timed benchmark"})
	if isErr {
		t.Fatalf("session set: %s", text)
	}
	var res c.AvoidNodeResponse
	if err := json.Unmarshal([]byte(text), &res); err != nil || len(res.Avoid) != 1 || res.Avoid[0].OwnerSet || res.Avoid[0].SetBy.ID != sid || res.Avoid[0].Hard {
		t.Fatalf("session flag: %v %s", err, text)
	}
	if code, out = pyre("nodes"); code != 0 || !strings.Contains(out, "soft until") || !strings.Contains(out, "set by session "+sid) {
		t.Fatalf("nodes after the session's flag: %d\n%s", code, out)
	}
	// Soft: a named start goes ahead with a warning.
	if code, out = pyre(start...); code != 0 || !strings.Contains(out, "warning: synth-mac is avoided (soft until") {
		t.Fatalf("soft named start: %d\n%s", code, out)
	}
}
