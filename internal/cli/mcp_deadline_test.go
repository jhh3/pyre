package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/mock"
)

// stallingStart stores the first start, then never answers it (a hub or
// node that went quiet mid-call); later calls pass straight through.
type stallingStart struct {
	c.API
	stalled atomic.Bool
}

func (s *stallingStart) StartSession(ctx context.Context, req c.StartSessionRequest) (c.StartSessionResponse, error) {
	res, err := s.API.StartSession(ctx, req)
	if err != nil || !s.stalled.CompareAndSwap(false, true) {
		return res, err
	}
	<-ctx.Done()
	return c.StartSessionResponse{}, ctx.Err()
}

// A start_session that gets no answer ends at its deadline with an
// indeterminate error naming the operation_id (generated when the caller
// gave none), and the retry with that ID returns the same session, not a
// second one (iss_…).
func TestMCPStartSessionDeadline(t *testing.T) {
	b := mock.New(mock.Options{Clock: mock.NewOffsetClock(mock.FixtureEpoch), DefaultPrincipal: &testOwner})
	api := &stallingStart{API: b}
	tools := MCPTools()
	for i := range tools {
		if tools[i].Name == "start_session" {
			if tools[i].deadline != StartSessionDeadline {
				t.Fatalf("start_session deadline %s, want %s", tools[i].deadline, StartSessionDeadline)
			}
			tools[i].deadline = 300 * time.Millisecond
		}
	}
	ops := &Ops{API: api, PollInterval: 10 * time.Millisecond}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	srv := &MCPServer{Connect: func() (*Ops, error) { return ops, nil }, Tools: tools}
	cl := &mcpClient{t: t, in: inW, out: bufio.NewReader(outR), done: make(chan error, 1)}
	go func() { cl.done <- srv.Serve(context.Background(), inR, outW); outW.Close() }()
	t.Cleanup(func() { inW.Close() })

	args := map[string]any{"node": "synth-mac", "harness": "claude", "cwd": "/home/synth/deadline", "title": "deadline"}
	start := time.Now()
	out, isErr := cl.tool("start_session", args)
	if !isErr {
		t.Fatalf("a stalled start returned success: %s", out)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("the deadline did not end the call (took %s)", time.Since(start))
	}
	var te ToolError
	if err := json.Unmarshal(out, &te); err != nil || te.Error == nil {
		t.Fatalf("tool error: %s", out)
	}
	if te.Error.Code != c.ErrOutcomeIndeterminate || te.Error.Ref == nil || te.Error.Ref.Kind != c.KindOperation || te.Error.Ref.ID == "" {
		t.Fatalf("want an indeterminate error naming the operation: %s", out)
	}
	opID := te.Error.Ref.ID
	if !strings.Contains(te.Error.Message, opID) || !strings.Contains(te.Error.Message, "safe") {
		t.Fatalf("the error should say retrying with %s is safe: %s", opID, te.Error.Message)
	}

	args["operation_id"] = opID
	out, isErr = cl.tool("start_session", args)
	if isErr {
		t.Fatalf("retry with %s: %s", opID, out)
	}
	var again StartResult
	if err := json.Unmarshal(out, &again); err != nil {
		t.Fatal(err)
	}
	page, err := b.ListSessions(context.Background(), c.ListSessionsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, v := range page.Items {
		if v.Session.Title == "deadline" {
			n++
			if v.Session.SessionID != again.Session.Session.SessionID {
				t.Fatalf("retry returned %s, but the first call made %s", again.Session.Session.SessionID, v.Session.SessionID)
			}
		}
	}
	if n != 1 {
		t.Fatalf("%d sessions titled deadline, want exactly 1", n)
	}
}

// Every tool that takes operation_id accepts the one the server generates
// for a call that gave none.
func TestMCPGeneratedOperationIDAccepted(t *testing.T) {
	for _, tl := range MCPTools() {
		args, id := withOperationID(tl, json.RawMessage(`{}`))
		if !hasProp(tl.InputSchema, "operation_id") {
			if id != "" {
				t.Errorf("%s: generated an operation_id it does not take", tl.Name)
			}
			continue
		}
		if id == "" || !strings.Contains(string(args), id) {
			t.Fatalf("%s: no generated operation_id in %s", tl.Name, args)
		}
		b := mock.New(mock.Options{Clock: mock.NewOffsetClock(mock.FixtureEpoch), DefaultPrincipal: &testOwner})
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, err := tl.run(ctx, &Ops{API: b, PollInterval: 10 * time.Millisecond}, args)
		cancel()
		if err != nil && strings.Contains(err.Error(), "unknown field") {
			t.Errorf("%s rejects operation_id: %v", tl.Name, err)
		}
	}
}
