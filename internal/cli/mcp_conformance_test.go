package cli

// MCP conformance: drive `pyre mcp` (in-process, over its real stdio
// handling, against the HTTP mock hub) with the official MCP Go SDK client
// at each protocol version Pyre negotiates, and validate every structured
// result against the tool's published outputSchema. The SDK is a
// test-only dependency.

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type mcpProc struct {
	session *mcp.ClientSession
	done    chan int
	stdinW  io.Closer
}

// startPyreMCP runs `pyre mcp` in-process with pipes for stdio and connects
// the SDK client with the requested protocol version ("" = SDK latest).
func startPyreMCP(t *testing.T, e env, version string) *mcpProc {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	app := &App{Stdin: inR, Stdout: outW, Stderr: io.Discard, Getenv: e.get, PollInterval: 20 * time.Millisecond}
	done := make(chan int, 1)
	go func() {
		code := app.Main(context.Background(), []string{"mcp"})
		outW.Close()
		done <- code
	}()
	client := mcp.NewClient(&mcp.Implementation{Name: "pyre-conformance", Version: "0"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cs, err := client.Connect(ctx, &mcp.IOTransport{Reader: outR, Writer: inW}, &mcp.ClientSessionOptions{ProtocolVersion: version})
	if err != nil {
		t.Fatalf("connect at %q: %v", version, err)
	}
	p := &mcpProc{session: cs, done: done, stdinW: inW}
	t.Cleanup(func() { cs.Close() })
	return p
}

var toolNameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

func TestMCPConformanceWithOfficialSDK(t *testing.T) {
	for _, tc := range []struct{ request, want string }{
		{"2025-06-18", "2025-06-18"},
		{"2025-11-25", "2025-11-25"},
		// SDK default: probes 2026-07-28 with server/discover, gets -32601,
		// and falls back to the legacy handshake. Pyre must not advertise
		// 2026-07-28 (Claude Code will not register such a channel server).
		{"", "2025-11-25"},
		{"2026-07-28", "2025-11-25"},
	} {
		t.Run("protocol="+tc.request, func(t *testing.T) {
			h := newHub(t)
			e := newEnv(t, h)
			p := startPyreMCP(t, e, tc.request)
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			ir := p.session.InitializeResult()
			if ir.ProtocolVersion != tc.want {
				t.Fatalf("negotiated %q, want %q", ir.ProtocolVersion, tc.want)
			}
			if ir.ServerInfo == nil || ir.ServerInfo.Name != "pyre" || !strings.Contains(ir.Instructions, "[pyre-msg:<id>]") {
				t.Fatalf("initialize result: %+v", ir)
			}
			if ir.Capabilities == nil || ir.Capabilities.Tools == nil {
				t.Fatal("tools capability missing")
			}

			lt, err := p.session.ListTools(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			schemas := map[string]*jsonschema.Resolved{}
			tools := map[string]*mcp.Tool{}
			for _, tl := range lt.Tools {
				tools[tl.Name] = tl
				if !toolNameRe.MatchString(tl.Name) || tl.Title == "" || tl.Description == "" {
					t.Errorf("%s: name/title/description", tl.Name)
				}
				a := tl.Annotations
				if a == nil || a.Title == "" || a.DestructiveHint == nil || a.OpenWorldHint == nil {
					t.Errorf("%s: annotations must state every hint explicitly: %+v", tl.Name, a)
					continue
				}
				// connection_invoke reaches the external service behind an
				// Outfit connection; every other tool stays inside Pyre.
				if openWorld := tl.Name == "connection_invoke"; *a.OpenWorldHint != openWorld {
					t.Errorf("%s: openWorldHint=%v, want %v", tl.Name, *a.OpenWorldHint, openWorld)
				}
				if a.ReadOnlyHint && (*a.DestructiveHint || !a.IdempotentHint) {
					t.Errorf("%s: a read-only tool is non-destructive and idempotent", tl.Name)
				}
				in, _ := tl.InputSchema.(map[string]any)
				if in["type"] != "object" {
					t.Errorf("%s: inputSchema type %v", tl.Name, in["type"])
				}
				out, _ := tl.OutputSchema.(map[string]any)
				if out["type"] != "object" {
					t.Errorf("%s: outputSchema must be an object schema", tl.Name)
					continue
				}
				schemas[tl.Name] = resolveSchema(t, tl.Name, out)
			}
			for name, want := range map[string]struct{ ro, destr bool }{
				"list_sessions": {true, false}, "stop_session": {false, true}, "reissue_message": {false, true},
				"send_message": {false, false}, "answer_approval": {false, true}, "ack_message": {false, false},
			} {
				a := tools[name].Annotations
				if a.ReadOnlyHint != want.ro || *a.DestructiveHint != want.destr {
					t.Errorf("%s: readOnly=%v destructive=%v, want %v/%v", name, a.ReadOnlyHint, *a.DestructiveHint, want.ro, want.destr)
				}
			}
			if !tools["stop_session"].Annotations.IdempotentHint || tools["send_message"].Annotations.IdempotentHint {
				t.Error("idempotency hints")
			}

			file := filepath.Join(t.TempDir(), "conformance.md")
			os.WriteFile(file, []byte("# synthetic conformance artifact\n"), 0o600)
			call := func(name string, args map[string]any) map[string]any {
				t.Helper()
				res, err := p.session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
				if err != nil {
					t.Fatalf("%s: protocol error %v", name, err)
				}
				if res.IsError {
					t.Fatalf("%s: tool error %s", name, textOf(res))
				}
				var sc any
				b, _ := json.Marshal(res.StructuredContent)
				if err := json.Unmarshal(b, &sc); err != nil || sc == nil {
					t.Fatalf("%s: no structuredContent", name)
				}
				if err := schemas[name].Validate(sc); err != nil {
					t.Fatalf("%s: structuredContent does not conform to outputSchema: %v\n%s", name, err, b)
				}
				var text any
				if err := json.Unmarshal([]byte(textOf(res)), &text); err != nil {
					t.Fatalf("%s: text content must carry the same JSON: %v", name, err)
				}
				m, _ := sc.(map[string]any)
				return m
			}

			// Every read-only tool, plus representative mutations.
			call("whoami", nil)
			call("list_nodes", map[string]any{"include_revoked": true})
			call("list_sessions", map[string]any{})
			call("get_session", map[string]any{"session": "ses_synth-offline"}) // stale run, nullable fields
			call("read_session", map[string]any{"session": "ses_synth-b", "tail": 5})
			call("list_approvals", map[string]any{"all": true})
			call("list_projects", map[string]any{"include_archived": true})
			call("list_documents", map[string]any{})
			call("get_document", map[string]any{"document": "brief", "project": "prj_synth-outfit"})
			call("list_artifacts", map[string]any{})
			call("list_comments", map[string]any{"target": "art_synth-report"})
			call("get_context", map[string]any{"project": "prj_synth-outfit"})
			call("memory_search", map[string]any{"query": "brief", "node": "synth-mac"})
			call("get_message", map[string]any{"message_id": "msg_synth-indeterminate"})
			call("inbox", map[string]any{"session": "ses_synth-b"})
			started := call("start_session", map[string]any{"node": "synth-mac", "harness": "claude", "cwd": "/home/synth/conf", "title": "conformance"})
			sid := started["session"].(map[string]any)["session"].(map[string]any)["session_id"].(string)
			sent := call("send_message", map[string]any{"to": sid, "text": "hello", "request_ack": true})
			mid := sent["envelope"].(map[string]any)["message_id"].(string)
			call("ack_message", map[string]any{"message_id": mid})
			call("comment", map[string]any{"target": "ver_synth-report-2", "text": "conformance comment", "anchor": "line:1"})
			call("create_note", map[string]any{"title": "conformance note", "body": "b", "kind": "decision"})
			call("publish", map[string]any{"path": file, "title": "conformance"})
			prj := call("create_project", map[string]any{"name": "synthetic-conformance", "repos": []string{"app=https://example.invalid/synthetic/app.git"},
				"checkouts": []string{"synth-mac:/srv/synth/conf:app:shared"}})
			call("associate_session_project", map[string]any{"session": sid, "project": prj["project_id"].(string)})
			call("associate_session_project", map[string]any{"session": sid, "none": true})
			call("archive_session", map[string]any{"session": "ses_synth-fbx"})
			call("answer_approval", map[string]any{"approval_id": "apr_synth-pending", "option": "allow"})

			// Tool errors are tool results, without structuredContent.
			res, err := p.session.CallTool(ctx, &mcp.CallToolParams{Name: "get_session", Arguments: map[string]any{"session": "ses_nope"}})
			if err != nil || !res.IsError || res.StructuredContent != nil || !strings.Contains(textOf(res), `"not_found"`) {
				t.Fatalf("tool error shape: %+v %v", res, err)
			}
			res, err = p.session.CallTool(ctx, &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{"to": "owner", "text": "x", "bogus": true}})
			if err != nil || !res.IsError || !strings.Contains(textOf(res), "invalid arguments") {
				t.Fatalf("argument validation must be a tool execution error: %+v %v", res, err)
			}
			if _, err := p.session.CallTool(ctx, &mcp.CallToolParams{Name: "no_such_tool"}); err == nil {
				t.Fatal("unknown tool must be a protocol error")
			}
			if err := p.session.Ping(ctx, nil); err != nil {
				t.Fatalf("ping: %v", err)
			}
			p.session.Close()
			select {
			case code := <-p.done:
				if code != 0 {
					t.Fatalf("pyre mcp exited %d", code)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("pyre mcp did not exit when the client closed stdin")
			}
		})
	}
}

func resolveSchema(t *testing.T, name string, m map[string]any) *jsonschema.Resolved {
	t.Helper()
	b, _ := json.Marshal(m)
	var s jsonschema.Schema
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("%s: outputSchema is not a JSON Schema: %v", name, err)
	}
	rs, err := s.Resolve(nil)
	if err != nil {
		t.Fatalf("%s: outputSchema does not resolve: %v", name, err)
	}
	return rs
}

func textOf(res *mcp.CallToolResult) string {
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			return tc.Text
		}
	}
	return ""
}

func TestNegotiateMCPVersion(t *testing.T) {
	for in, want := range map[string]string{
		"2025-11-25": "2025-11-25", "2025-06-18": "2025-06-18", "2025-03-26": "2025-03-26", "2024-11-05": "2024-11-05",
		"2026-07-28": "2025-11-25", "": "2025-11-25", "1999-01-01": "2025-11-25",
	} {
		if got := NegotiateMCPVersion(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
	for _, v := range mcpVersions {
		if v >= "2026-07-28" {
			t.Fatalf("must not advertise %s", v)
		}
	}
}

// The derived schemas must actually constrain results (not be vacuous).
func TestOutputSchemaRejectsWrongShapes(t *testing.T) {
	var sv *MCPTool
	ts := MCPTools()
	for i := range ts {
		if ts[i].Name == "get_session" {
			sv = &ts[i]
		}
	}
	rs := resolveSchema(t, "get_session", sv.OutputSchema)
	good := map[string]any{
		"session": map[string]any{"session_id": "ses_x", "harness": "claude", "title": "t", "project_id": nil,
			"created_by": map[string]any{"kind": "owner", "id": "cli_x"}, "created_at": "2026-10-04T12:00:00Z",
			"lineage": map[string]any{}, "archived": false},
		"attention": 0, "freshness": map[string]any{"observed_at": "2026-10-04T12:00:00Z", "source": "x", "stale": false},
	}
	if err := rs.Validate(good); err != nil {
		t.Fatalf("valid value rejected: %v", err)
	}
	bad := map[string]any{"session": map[string]any{"session_id": 5}, "attention": "x"}
	if err := rs.Validate(bad); err == nil {
		t.Fatal("schema accepted a wrong-typed value")
	}
	missing := map[string]any{"attention": 0}
	if err := rs.Validate(missing); err == nil {
		t.Fatal("schema accepted a value missing required fields")
	}
}
