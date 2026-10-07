package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/agentguide"
	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/mock"
)

// mcpClient is a minimal in-process MCP client over pipes.
type mcpClient struct {
	t    *testing.T
	in   *io.PipeWriter
	out  *bufio.Reader
	next int
	done chan error
}

func startMCP(t *testing.T, ops *Ops) *mcpClient {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	srv := &MCPServer{Connect: func() (*Ops, error) { return ops, nil }, Tools: MCPTools(), Instructions: agentguide.Text(agentguide.Options{MCP: true})}
	cl := &mcpClient{t: t, in: inW, out: bufio.NewReader(outR), done: make(chan error, 1)}
	go func() { cl.done <- srv.Serve(context.Background(), inR, outW); outW.Close() }()
	t.Cleanup(func() { inW.Close() })
	return cl
}

func (m *mcpClient) send(v any) {
	b, _ := json.Marshal(v)
	if _, err := m.in.Write(append(b, '\n')); err != nil {
		m.t.Fatal(err)
	}
}

func (m *mcpClient) call(method string, params any) map[string]any {
	m.t.Helper()
	m.next++
	m.send(map[string]any{"jsonrpc": "2.0", "id": m.next, "method": method, "params": params})
	type res struct {
		line []byte
		err  error
	}
	ch := make(chan res, 1)
	go func() { l, err := m.out.ReadBytes('\n'); ch <- res{l, err} }()
	select {
	case r := <-ch:
		if r.err != nil {
			m.t.Fatalf("read: %v", r.err)
		}
		var resp map[string]any
		if err := json.Unmarshal(r.line, &resp); err != nil {
			m.t.Fatalf("bad response %q", r.line)
		}
		if id, _ := resp["id"].(float64); int(id) != m.next {
			m.t.Fatalf("response id %v, want %d", resp["id"], m.next)
		}
		return resp
	case <-time.After(20 * time.Second):
		m.t.Fatalf("no response to %s", method)
	}
	return nil
}

// tool calls a tool and returns (structured result JSON, isError).
func (m *mcpClient) tool(name string, args any) (json.RawMessage, bool) {
	m.t.Helper()
	resp := m.call("tools/call", map[string]any{"name": name, "arguments": args})
	if resp["error"] != nil {
		m.t.Fatalf("protocol error calling %s: %v", name, resp["error"])
	}
	res := resp["result"].(map[string]any)
	content := res["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	isErr, _ := res["isError"].(bool)
	return json.RawMessage(text), isErr
}

func TestMCPServerToolsAndCalls(t *testing.T) {
	b := mock.New(mock.Options{Clock: mock.NewOffsetClock(mock.FixtureEpoch), DefaultPrincipal: &testOwner})
	cl := startMCP(t, &Ops{API: b, PollInterval: 10 * time.Millisecond})

	init := cl.call("initialize", map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "0"}})
	ir := init["result"].(map[string]any)
	if ir["protocolVersion"] != "2025-03-26" || !strings.Contains(ir["instructions"].(string), "[pyre-msg:<id>]") {
		t.Fatalf("initialize: %v", ir)
	}
	cl.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})

	lr := cl.call("tools/list", map[string]any{})["result"].(map[string]any)
	names := map[string]bool{}
	for _, x := range lr["tools"].([]any) {
		tm := x.(map[string]any)
		names[tm["name"].(string)] = true
		if tm["inputSchema"].(map[string]any)["type"] != "object" || len(tm["description"].(string)) > 240 {
			t.Errorf("tool %v: schema/description", tm["name"])
		}
	}
	for _, want := range []string{"list_nodes", "list_sessions", "start_session", "read_session", "send_message", "inbox", "wait_reply", "ack_message", "publish", "comment", "create_note", "get_context", "memory_search", "list_approvals", "answer_approval"} {
		if !names[want] {
			t.Errorf("missing tool %s", want)
		}
	}

	out, isErr := cl.tool("list_nodes", map[string]any{})
	var nodes c.Page[c.Node]
	if isErr || json.Unmarshal(out, &nodes) != nil || len(nodes.Items) == 0 {
		t.Fatalf("list_nodes: %s", out)
	}

	// Same semantics as the CLI: start, then message the peer and read the facts.
	out, isErr = cl.tool("start_session", map[string]any{"node": "synth-mac", "harness": "claude", "cwd": "/home/synth/mcp", "title": "mcp peer"})
	var started c.StartSessionResponse
	if isErr || json.Unmarshal(out, &started) != nil || started.Session.Session.SessionID == "" {
		t.Fatalf("start_session: %s", out)
	}
	out, isErr = cl.tool("send_message", map[string]any{"to": started.Session.Session.SessionID, "text": "hello peer", "request_ack": true})
	var sent c.MessageView
	if isErr || json.Unmarshal(out, &sent) != nil || sent.Status.Storage != c.StorageStored {
		t.Fatalf("send_message: %s", out)
	}
	out, isErr = cl.tool("get_message", map[string]any{"message_id": sent.Envelope.MessageID})
	var got c.MessageView
	if isErr || json.Unmarshal(out, &got) != nil || got.Envelope.MessageID != sent.Envelope.MessageID {
		t.Fatalf("get_message: %s", out)
	}

	// Errors are tool results with the contract error and the CLI exit code.
	out, isErr = cl.tool("get_session", map[string]any{"session": "ses_nope"})
	var te ToolError
	if !isErr || json.Unmarshal(out, &te) != nil || te.Error.Code != c.ErrNotFound || te.ExitCode != ExitNotFound || te.Result != nil {
		t.Fatalf("not found: %s", out)
	}
	out, isErr = cl.tool("reissue_message", map[string]any{"message_id": "msg_synth-indeterminate", "accept_duplicate_risk": false})
	if !isErr || !strings.Contains(string(out), "accept-duplicate-risk") {
		t.Fatalf("reissue without risk acceptance must fail: %s", out)
	}
	out, isErr = cl.tool("send_message", map[string]any{"to": "owner", "text": "x", "bogus": 1})
	if !isErr || !strings.Contains(string(out), "invalid arguments") {
		t.Fatalf("unknown argument must be rejected: %s", out)
	}
	out, isErr = cl.tool("wait_reply", map[string]any{"message_id": "msg_synth-indeterminate", "timeout_seconds": 1})
	if !isErr || json.Unmarshal(out, &te) != nil || te.ExitCode != ExitUnavailable || te.Result == nil {
		t.Fatalf("wait_reply timeout should carry the latest state: %s", out)
	}

	// Protocol-level errors.
	if e := cl.call("tools/call", map[string]any{"name": "no_such_tool", "arguments": map[string]any{}})["error"]; e == nil {
		t.Fatal("unknown tool must be a protocol error")
	}
	if e := cl.call("no/such/method", map[string]any{})["error"]; e == nil {
		t.Fatal("unknown method must be a protocol error")
	}
	if r := cl.call("ping", nil)["result"]; r == nil {
		t.Fatal("ping")
	}
	cl.in.Close()
	if err := <-cl.done; err != nil {
		t.Fatalf("serve: %v", err)
	}
}

// TestMCPCommandOverHTTP runs `pyre mcp` itself against the HTTP mock hub.
func TestMCPCommandOverHTTP(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	in := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}
{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"whoami","arguments":{}}}
`
	r := run(t, e, &App{Stdin: strings.NewReader(in)}, "mcp")
	expectCode(t, r, 0)
	lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
	if len(lines) != 2 || !strings.Contains(lines[1], testOwner.ID) {
		t.Fatalf("mcp over http: %s", r)
	}
}

// The committed manifest in agents/tools must match the server's tool list.
// Regenerate with: PYRE_UPDATE_GOLDEN=1 go test ./internal/cli -run TestMCPToolManifest
func TestMCPToolManifest(t *testing.T) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(MCPTools()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("..", "..", "agents", "tools", "pyre-mcp-tools.json")
	if os.Getenv("PYRE_UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (regenerate with PYRE_UPDATE_GOLDEN=1)", err)
	}
	if !bytes.Equal(want, buf.Bytes()) {
		t.Fatal("agents/tools/pyre-mcp-tools.json is stale; regenerate with PYRE_UPDATE_GOLDEN=1 go test ./internal/cli -run TestMCPToolManifest")
	}
	r := run(t, newEnv(t, nil), nil, "mcp", "--list-tools")
	expectCode(t, r, 0)
	if r.stdout != buf.String() {
		t.Fatal("`pyre mcp --list-tools` must print the manifest")
	}
}
