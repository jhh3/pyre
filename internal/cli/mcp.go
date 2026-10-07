package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/buildinfo"
	"github.com/jhh3/pyre/internal/client"
	c "github.com/jhh3/pyre/internal/contracts"
)

// NegotiateMCPVersion echoes a supported requested version, otherwise
// offers the newest supported one (never 2026-07-28; see mcpVersions).
func NegotiateMCPVersion(requested string) string {
	for _, v := range mcpVersions {
		if v == requested {
			return v
		}
	}
	return mcpVersions[0]
}

// ---- JSON-RPC over stdio ----

type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcErr         `json:"error,omitempty"`
}

type mcpContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type mcpCallResult struct {
	Content           []mcpContent `json:"content"`
	StructuredContent any          `json:"structuredContent,omitempty"`
	IsError           bool         `json:"isError,omitempty"`
}

// MCPServer serves Pyre tools over newline-delimited JSON-RPC (MCP stdio).
type MCPServer struct {
	Connect      func() (*Ops, error) // lazy, so tools/list works without a hub
	Instructions string
	Tools        []MCPTool
	Log          io.Writer

	wmu     sync.Mutex
	out     io.Writer
	cmu     sync.Mutex
	cancels map[string]context.CancelFunc
}

// Serve reads requests from in until EOF or ctx ends.
func (s *MCPServer) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	s.out = out
	s.cancels = map[string]context.CancelFunc{}
	if s.Log == nil {
		s.Log = io.Discard
	}
	byName := map[string]MCPTool{}
	for _, t := range s.Tools {
		byName[t.Name] = t
	}
	ctx, cancelAll := context.WithCancel(ctx)
	defer cancelAll()
	var wg sync.WaitGroup
	defer wg.Wait()
	r := bufio.NewReaderSize(in, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			s.handle(ctx, &wg, byName, line)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
	}
}

func (s *MCPServer) write(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		fmt.Fprintf(s.Log, "pyre mcp: encode: %v\n", err)
		return
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	s.out.Write(append(b, '\n'))
}

func (s *MCPServer) reply(id json.RawMessage, result any) {
	s.write(rpcResp{JSONRPC: "2.0", ID: id, Result: result})
}

func (s *MCPServer) fail(id json.RawMessage, code int, msg string) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	s.write(rpcResp{JSONRPC: "2.0", ID: id, Error: &rpcErr{Code: code, Message: msg}})
}

func (s *MCPServer) handle(ctx context.Context, wg *sync.WaitGroup, byName map[string]MCPTool, line []byte) {
	var m rpcMsg
	if err := json.Unmarshal(line, &m); err != nil {
		s.fail(nil, -32700, "parse error")
		return
	}
	isNotification := len(m.ID) == 0 || string(m.ID) == "null"
	switch m.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(m.Params, &p)
		s.reply(m.ID, map[string]any{
			"protocolVersion": NegotiateMCPVersion(p.ProtocolVersion),
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "pyre", "title": "Pyre", "version": buildinfo.Version},
			"instructions":    s.Instructions,
		})
	case "ping":
		s.reply(m.ID, map[string]any{})
	case "tools/list":
		s.reply(m.ID, map[string]any{"tools": s.Tools})
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(m.Params, &p); err != nil {
			s.fail(m.ID, -32602, "invalid params")
			return
		}
		t, ok := byName[p.Name]
		if !ok {
			s.fail(m.ID, -32602, "unknown tool "+p.Name)
			return
		}
		cctx, cancel := context.WithCancel(ctx)
		key := string(m.ID)
		s.cmu.Lock()
		s.cancels[key] = cancel
		s.cmu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				s.cmu.Lock()
				delete(s.cancels, key)
				s.cmu.Unlock()
				cancel()
			}()
			res := s.call(cctx, t, p.Arguments)
			if cctx.Err() != nil && ctx.Err() == nil {
				return // cancelled by the client: no response
			}
			s.reply(m.ID, res)
		}()
	case "notifications/cancelled":
		var p struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		_ = json.Unmarshal(m.Params, &p)
		s.cmu.Lock()
		if cancel := s.cancels[string(p.RequestID)]; cancel != nil {
			cancel()
		}
		s.cmu.Unlock()
	default:
		if !isNotification {
			s.fail(m.ID, -32601, "method not found: "+m.Method)
		}
	}
}

// ToolError is the JSON text of a failed tool call.
type ToolError struct {
	Error    *c.Error `json:"error"`
	ExitCode int      `json:"exit_code"`
	Result   any      `json:"result,omitempty"`
}

func (s *MCPServer) call(ctx context.Context, t MCPTool, args json.RawMessage) mcpCallResult {
	ops, err := s.Connect()
	var out any
	var cached *c.Freshness
	if err == nil {
		out, err = t.run(client.WithFreshnessSink(ctx, func(_ string, f c.Freshness) { cached = &f }), ops, args)
	}
	if err != nil {
		code := ExitCodeFor(err)
		var ce *c.Error
		if !errors.As(err, &ce) {
			ce = &c.Error{Code: codeForExit(code), Message: err.Error()}
		} else if ce.Error() != err.Error() {
			// keep context added by wrapping (e.g. "your draft is kept at ...")
			ce = &c.Error{Code: ce.Code, Message: err.Error(), Retryable: ce.Retryable, Ref: ce.Ref, Details: ce.Details}
		}
		te := ToolError{Error: ce, ExitCode: code}
		if out != nil && !reflect.ValueOf(out).IsZero() {
			te.Result = out // e.g. a receipt with an indeterminate outcome, or wait_reply's latest state
		}
		// No structuredContent on errors: it would not conform to the
		// tool's outputSchema (which describes successful results).
		b, _ := json.MarshalIndent(te, "", "  ")
		return mcpCallResult{Content: []mcpContent{{Type: "text", Text: string(b)}}, IsError: true}
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return mcpCallResult{Content: []mcpContent{{Type: "text", Text: "encode result: " + err.Error()}}, IsError: true}
	}
	// The text block carries the same JSON for clients that ignore
	// structuredContent (spec: SHOULD also return serialized JSON as text).
	res := mcpCallResult{Content: []mcpContent{{Type: "text", Text: string(b)}}}
	if cached != nil {
		// The result came from a node's read cache during a hub outage:
		// say so next to it (structuredContent stays the plain result).
		res.Content = append(res.Content, mcpContent{Type: "text", Text: "note: the hub is unavailable; this is the copy cached on node " +
			strings.TrimPrefix(cached.Source, "node-cache:") + ", last confirmed with the hub at " + cached.ObservedAt.UTC().Format(time.RFC3339)})
	}
	if t.OutputSchema != nil && bytes.HasPrefix(bytes.TrimSpace(b), []byte("{")) {
		res.StructuredContent = json.RawMessage(b)
	}
	return res
}
