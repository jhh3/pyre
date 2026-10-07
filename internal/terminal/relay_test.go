package terminal_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/terminal"
)

// Relay unit tests over plain HTTP with fake tickets/backend; the real
// stack is covered by e2e_test.go.

type memTickets struct {
	mu sync.Mutex
	n  int
	m  map[string]*memTicket
}

type memTicket struct {
	ref      c.ObjectRef
	exp      time.Time
	redeemed bool
}

func (m *memTickets) IssueSessionToken(context.Context, string) (string, error) { return "", nil }
func (m *memTickets) RevokeSessionTokens(context.Context, string) error         { return nil }
func (m *memTickets) IssueTicket(_ context.Context, purpose string, ref c.ObjectRef, ttl time.Duration) (string, time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.n++
	tk := purpose + "-synthetic-ticket-" + string(rune('a'+m.n))
	m.m[tk] = &memTicket{ref: ref, exp: time.Now().Add(ttl)}
	return tk, m.m[tk].exp, nil
}
func (m *memTickets) RedeemTicket(_ context.Context, ticket, purpose string) (c.ObjectRef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.m[ticket]
	switch {
	case !ok || !strings.HasPrefix(ticket, purpose+"-"):
		return c.ObjectRef{}, c.Errorf(c.ErrUnauthorized, "invalid ticket")
	case t.redeemed:
		return c.ObjectRef{}, c.Errorf(c.ErrConflict, "ticket already redeemed")
	case time.Now().After(t.exp):
		return c.ObjectRef{}, c.Errorf(c.ErrExpired, "ticket expired")
	}
	t.redeemed = true
	return t.ref, nil
}

type nodeAuth struct{}

func (nodeAuth) Authenticate(_ context.Context, bearer string) (c.Principal, error) {
	switch bearer {
	case "synthetic-node-a":
		return c.Principal{Kind: c.PrincipalNode, ID: "node_a"}, nil
	case "synthetic-node-b":
		return c.Principal{Kind: c.PrincipalNode, ID: "node_b"}, nil
	}
	return c.Principal{}, c.Errorf(c.ErrUnauthorized, "unknown")
}

type fakeBackend struct {
	mu        sync.Mutex
	queued    []c.TerminalOpenCommand
	result    error // returned once finished
	finished  bool
	inputDeny atomic.Bool
	queuedCh  chan c.TerminalOpenCommand
}

func (b *fakeBackend) PrepareTerminal(context.Context, c.Principal, string, bool) (string, error) {
	return "node_a", nil
}
func (b *fakeBackend) QueueTerminalOpen(_ context.Context, _ c.Principal, _ string, cmd c.TerminalOpenCommand, _ time.Duration) (string, error) {
	b.mu.Lock()
	b.queued = append(b.queued, cmd)
	b.mu.Unlock()
	if b.queuedCh != nil {
		b.queuedCh <- cmd
	}
	return "cmd_synthetic", nil
}
func (b *fakeBackend) TerminalOpenResult(ctx context.Context, _ string, wait time.Duration) (bool, error) {
	b.mu.Lock()
	fin, res := b.finished, b.result
	b.mu.Unlock()
	if fin {
		return true, res
	}
	select {
	case <-ctx.Done():
	case <-time.After(wait):
	}
	return false, nil
}
func (b *fakeBackend) TerminalInputAllowed(context.Context, c.Principal, string) error {
	if b.inputDeny.Load() {
		return c.Errorf(c.ErrConflict, "leased by someone else")
	}
	return nil
}

type relayEnv struct {
	t       *testing.T
	relay   *terminal.Relay
	backend *fakeBackend
	srv     *httptest.Server
}

func newRelayEnv(t *testing.T, mod func(*terminal.RelayConfig)) *relayEnv {
	t.Helper()
	b := &fakeBackend{queuedCh: make(chan c.TerminalOpenCommand, 8)}
	cfg := terminal.RelayConfig{Tickets: &memTickets{m: map[string]*memTicket{}}, Auth: nodeAuth{}, Backend: b, Logger: quiet(), DialWait: 5 * time.Second, LeaseRecheck: time.Millisecond}
	if mod != nil {
		mod(&cfg)
	}
	r, err := terminal.NewRelay(cfg)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle(c.PathTerminal, r.ClientHandler())
	mux.Handle(c.PathNodeTerminal, r.NodeHandler())
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &relayEnv{t: t, relay: r, backend: b, srv: srv}
}

func (e *relayEnv) client(readOnly bool) *termClient {
	e.t.Helper()
	tk, _, err := e.relay.IssueTerminalTicket(context.Background(), c.Principal{Kind: c.PrincipalOwner, ID: "cli_synth"}, "run_synth", readOnly)
	if err != nil {
		e.t.Fatal(err)
	}
	ws, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(e.srv.URL, "http")+c.PathTerminal, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { ws.CloseNow() })
	tc := &termClient{t: e.t, ws: ws, closed: make(chan struct{})}
	tc.send(terminal.Frame{Type: terminal.FrameAuth, Ticket: tk, Cols: 80, Rows: 24})
	go tc.read()
	return tc
}

func (e *relayEnv) node(token, stream string) (*websocket.Conn, int) {
	e.t.Helper()
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+token)
	ws, resp, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(e.srv.URL, "http")+c.PathNodeTerminal+"?stream="+stream, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		if resp == nil {
			e.t.Fatal(err)
		}
		return nil, resp.StatusCode
	}
	e.t.Cleanup(func() { ws.CloseNow() })
	return ws, http.StatusSwitchingProtocols
}

func (e *relayEnv) queued() c.TerminalOpenCommand {
	e.t.Helper()
	select {
	case cmd := <-e.backend.queuedCh:
		return cmd
	case <-time.After(5 * time.Second):
		e.t.Fatal("terminal_open not queued")
	}
	return c.TerminalOpenCommand{}
}

func TestRelayNodeNeverDials(t *testing.T) {
	e := newRelayEnv(t, func(cfg *terminal.RelayConfig) { cfg.DialWait = 300 * time.Millisecond })
	tc := e.client(false)
	tc.expectError(c.ErrUnavailable)
	waitFor(t, "stream released", func() bool { return e.relay.Streams() == 0 })
	// A late dial-back finds nothing to claim.
	e.backend.mu.Lock()
	cmd := append([]c.TerminalOpenCommand{}, e.backend.queued...)
	e.backend.mu.Unlock()
	if len(cmd) != 1 {
		t.Fatalf("queued %d", len(cmd))
	}
	if _, code := e.node("synthetic-node-a", cmd[0].StreamID); code != http.StatusNotFound {
		t.Fatalf("late dial: HTTP %d", code)
	}
}

func TestRelayNodeReportsFailure(t *testing.T) {
	e := newRelayEnv(t, nil)
	e.backend.mu.Lock()
	e.backend.finished, e.backend.result = true, c.Errorf(c.ErrConflict, "run is exited (not live)")
	e.backend.mu.Unlock()
	start := time.Now()
	tc := e.client(false)
	tc.expectError(c.ErrConflict)
	if time.Since(start) > 3*time.Second {
		t.Fatal("node failure was not reported promptly")
	}
}

func TestRelayIdleTimeoutAndLimits(t *testing.T) {
	e := newRelayEnv(t, func(cfg *terminal.RelayConfig) {
		cfg.IdleTimeout = 400 * time.Millisecond
		cfg.MaxStreams = 1
	})
	tc := e.client(false)
	cmd := e.queued()
	if cmd.Cols != 80 || cmd.Rows != 24 || cmd.RunID != "run_synth" || !strings.HasPrefix(cmd.StreamID, "tstr_") || len(cmd.StreamID) < 30 {
		t.Fatalf("terminal_open = %+v", cmd)
	}
	// Only one stream at a time.
	second := e.client(false)
	second.expectError(c.ErrCapacityExhausted)
	if _, code := e.node("synthetic-node-b", cmd.StreamID); code != http.StatusForbidden {
		t.Fatalf("foreign node: HTTP %d", code)
	}
	ws, code := e.node("synthetic-node-a", cmd.StreamID)
	if code != http.StatusSwitchingProtocols {
		t.Fatalf("owning node: HTTP %d", code)
	}
	// No traffic: both sides are closed.
	tc.expectError(c.ErrExpired)
	_, _, err := ws.Read(context.Background())
	if websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Fatalf("node side after idle: %v", err)
	}
	waitFor(t, "slot released", func() bool { return e.relay.Streams() == 0 })
}

func TestRelayInputLeaseAndReadOnly(t *testing.T) {
	e := newRelayEnv(t, nil)
	for _, ro := range []bool{false, true} {
		tc := e.client(ro)
		cmd := e.queued()
		if cmd.ReadOnly != ro {
			t.Fatalf("read_only %v not carried: %+v", ro, cmd)
		}
		ws, _ := e.node("synthetic-node-a", cmd.StreamID)
		// readUntilResize collects input bytes up to the next resize frame
		// (frames are relayed in order, so the resize marks a sync point).
		readUntilResize := func(cols int) []string {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var got []string
			for {
				typ, b, err := ws.Read(ctx)
				if err != nil {
					t.Fatalf("node read: %v (got %q)", err, got)
				}
				if typ == websocket.MessageText {
					var f terminal.Frame
					json.Unmarshal(b, &f)
					if f.Type != terminal.FrameResize || f.Cols != cols || f.Rows != 43 {
						t.Fatalf("node control frame %s", b)
					}
					return got
				}
				got = append(got, string(b))
			}
		}
		// Another principal holds the input lease: input is dropped.
		e.backend.inputDeny.Store(true)
		tc.input("blocked")
		tc.send(terminal.Frame{Type: "unknown"})
		tc.send(terminal.Frame{Type: terminal.FrameResize, Cols: 100, Rows: 43})
		if got := readUntilResize(100); len(got) != 0 {
			t.Fatalf("read_only=%v: leased-away input reached the node: %q", ro, got)
		}
		e.backend.inputDeny.Store(false)
		time.Sleep(5 * time.Millisecond)
		tc.input("allowed")
		tc.send(terminal.Frame{Type: terminal.FrameResize, Cols: 132, Rows: 43})
		got := readUntilResize(132)
		want := "allowed"
		if ro {
			want = ""
		}
		if strings.Join(got, ",") != want {
			t.Fatalf("read_only=%v: node received %q, want %q", ro, got, want)
		}
		tc.ws.Close(websocket.StatusNormalClosure, "done")
		_, _, err := ws.Read(context.Background())
		if websocket.CloseStatus(err) != websocket.StatusNormalClosure {
			t.Fatalf("node side after client close: %v", err)
		}
		waitFor(t, "stream released", func() bool { return e.relay.Streams() == 0 })
	}
}
