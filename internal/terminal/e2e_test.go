package terminal_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/hub"
	"github.com/jhh3/pyre/internal/hub/assemble"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
	"github.com/jhh3/pyre/internal/supervisor"
	"github.com/jhh3/pyre/internal/terminal"
	"github.com/jhh3/pyre/internal/transport"
)

// End-to-end web terminal tests: the real composed hub (assemble) behind
// TLS, a real node WebSocket link, the real NodeBridge, and a real isolated
// tmux server (socket under a private /tmp dir, never the default server)
// running /bin/sh. All identities and data are synthetic.

var shellCaps = []string{c.CapAttachNative, c.CapAttachReadOnly, c.CapScreen, c.CapStop}

type offsetClock struct{ off atomic.Int64 }

func (k *offsetClock) Now() time.Time          { return time.Now().UTC().Add(time.Duration(k.off.Load())) }
func (k *offsetClock) Advance(d time.Duration) { k.off.Add(int64(d)) }

func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "pyre-wt-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func waitFor(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// fakeNode is a minimal node runtime: runs are real tmux sessions; the
// terminal_open command is served by the real NodeBridge.
type fakeNode struct {
	t      *testing.T
	dir    string
	bridge *terminal.NodeBridge
	// hold, when set, records terminal_open stream IDs without dialing.
	hold atomic.Bool

	mu      sync.Mutex
	runs    map[string]c.Run
	tmux    map[string]supervisor.Tmux
	streams []string
	opens   int
}

func (n *fakeNode) HandleCommand(ctx context.Context, cmd c.NodeCommand) (c.CommandResult, error) {
	ok := func(v any) (c.CommandResult, error) {
		b, _ := json.Marshal(v)
		return c.CommandResult{Outcome: c.OutcomeSucceeded, Result: b}, nil
	}
	switch cmd.Kind {
	case c.CmdStartRun:
		var sr c.StartRunCommand
		if err := json.Unmarshal(cmd.Payload, &sr); err != nil {
			return c.CommandResult{}, err
		}
		tm := supervisor.Tmux{Socket: filepath.Join(n.dir, "tmux-"+sr.RunID[len(sr.RunID)-6:]+".sock"), OwnedRoot: n.dir}
		if err := tm.NewSession(ctx, supervisor.SessionSpec{Name: "pyre", Argv: []string{"/bin/sh"}, Env: map[string]string{"PS1": "$ ", "HOME": n.dir}, Width: 80, Height: 24}); err != nil {
			return c.CommandResult{}, err
		}
		n.t.Cleanup(func() { tm.Kill(context.Background()) })
		run := c.Run{RunID: sr.RunID, SessionID: sr.Session.SessionID, Adapter: "synthetic-shell", Harness: c.HarnessShell, State: c.RunWaitingInput,
			StartedAt: time.Now().UTC(), Capabilities: shellCaps}
		n.mu.Lock()
		n.runs[run.RunID], n.tmux[run.RunID] = run, tm
		n.mu.Unlock()
		return ok(run)
	case c.CmdAttach:
		var ac c.AttachCommand
		if err := json.Unmarshal(cmd.Payload, &ac); err != nil {
			return c.CommandResult{}, err
		}
		tm, err := n.tmuxFor(ac.RunID)
		if err != nil {
			return c.CommandResult{}, err
		}
		return ok(c.AttachDescriptor{RunID: ac.RunID, Mechanism: c.AttachLocalTmux, Command: tm.AttachArgv("pyre", ac.ReadOnly), ReadOnly: ac.ReadOnly})
	case terminal.CmdTerminalOpen:
		var tc c.TerminalOpenCommand
		if err := json.Unmarshal(cmd.Payload, &tc); err != nil {
			return c.CommandResult{}, err
		}
		n.mu.Lock()
		n.streams = append(n.streams, tc.StreamID)
		n.opens++
		n.mu.Unlock()
		if n.hold.Load() {
			return ok(map[string]string{"stream_id": tc.StreamID})
		}
		tm, err := n.tmuxFor(tc.RunID)
		if err != nil {
			return c.CommandResult{Outcome: c.OutcomeFailed, Error: c.AsError(err)}, nil
		}
		if err := n.bridge.Open(ctx, tc, tm.AttachArgv("pyre", tc.ReadOnly)); err != nil {
			return c.CommandResult{Outcome: c.OutcomeFailed, Error: c.AsError(err)}, nil
		}
		return ok(map[string]string{"stream_id": tc.StreamID})
	}
	return c.CommandResult{Outcome: c.OutcomeSucceeded}, nil
}

func (n *fakeNode) tmuxFor(runID string) (supervisor.Tmux, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	tm, ok := n.tmux[runID]
	if !ok {
		return tm, c.Errorf(c.ErrNotFound, "run not on this node")
	}
	return tm, nil
}

func (n *fakeNode) liveRuns() []c.Run {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []c.Run
	for _, r := range n.runs {
		out = append(out, r)
	}
	return out
}

type env struct {
	t     *testing.T
	dir   string
	clock *offsetClock
	st    *assemble.Stack
	srv   *http.Server
	addr  string
	owner context.Context
	cred  c.NodeCredential
	node  *fakeNode
	stop  func()
}

func newEnv(t *testing.T) *env {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	e := &env{t: t, dir: shortDir(t), clock: &offsetClock{}}
	st, err := assemble.Open(context.Background(), assemble.Config{DataDir: filepath.Join(e.dir, "hub"), Clock: e.clock, Logger: quiet(),
		Hub: hub.Options{PollInterval: 20 * time.Millisecond, MaintenanceInterval: 200 * time.Millisecond, CommandWait: 5 * time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	st.Start(context.Background())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e.st, e.addr = st, ln.Addr().String()
	e.srv = &http.Server{Handler: st.Handler, TLSConfig: st.Cert.ServerTLSConfig()}
	go e.srv.ServeTLS(ln, "", "")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		st.NodeServer.Close()
		e.srv.Shutdown(ctx)
		st.Close()
	})
	e.owner = c.WithPrincipal(context.Background(), c.Principal{Kind: c.PrincipalOwner, ID: "cli_synth-laptop", Name: "owner (synthetic)"})
	e.cred = e.enroll("synth-node-a")
	tlsCfg, err := transport.PinnedTLSConfig(st.Cert.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	bridge, err := terminal.NewNodeBridge(terminal.BridgeConfig{HubURL: "https://" + e.addr, Token: e.cred.Token, TLS: tlsCfg, Logger: quiet(), KillGrace: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bridge.Close() })
	e.node = &fakeNode{t: t, dir: e.dir, bridge: bridge, runs: map[string]c.Run{}, tmux: map[string]supervisor.Tmux{}}
	e.startLink()
	return e
}

func (e *env) enroll(name string) c.NodeCredential {
	e.t.Helper()
	tk, err := e.st.Fleet.CreateEnrollment(e.owner, c.EnrollmentRequest{Mutation: c.Mutation{OperationID: c.NewID(c.PrefixOperation)}, Name: name})
	if err != nil {
		e.t.Fatal(err)
	}
	cred, err := e.st.Enrollment.RedeemNode(context.Background(), c.NodeEnrollExchange{NodeID: tk.NodeID, Secret: tk.Secret, Hostname: name, Platform: "darwin", Arch: "arm64"})
	if err != nil {
		e.t.Fatal(err)
	}
	return cred
}

func (e *env) startLink() {
	e.t.Helper()
	nodeDB, err := sqlitex.Open(filepath.Join(e.dir, "node-"+c.NewID("db")+".db"))
	if err != nil {
		e.t.Fatal(err)
	}
	tlsCfg, _ := transport.PinnedTLSConfig(e.st.Cert.Fingerprint)
	link, err := transport.NewNodeLink(transport.NodeLinkConfig{HubURL: "https://" + e.addr, NodeID: e.cred.NodeID, Token: e.cred.Token, TLS: tlsCfg, DB: nodeDB, Logger: quiet(),
		Hello: func(ctx context.Context) (c.NodeHello, error) {
			return c.NodeHello{Hostname: "synth-node-a", Platform: "darwin", Arch: "arm64", PyredVersion: "0.1.0-synth",
				Harnesses: []c.HarnessInfo{{Harness: c.HarnessShell, Version: "tmux", Available: true, Capabilities: shellCaps}},
				Resources: c.NodeResources{CPUCount: 4, MaxRuns: 8}, LiveRuns: e.node.liveRuns()}, nil
		},
		Handler: e.node, Backoff: transport.BackoffConfig{Initial: 20 * time.Millisecond, Max: 200 * time.Millisecond}})
	if err != nil {
		e.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { link.Run(ctx); close(done) }()
	var once sync.Once
	e.stop = func() { once.Do(func() { cancel(); <-done; nodeDB.Close() }) }
	e.t.Cleanup(e.stop)
	waitFor(e.t, "node connected", func() bool { return e.st.NodeServer.Connected(e.cred.NodeID) })
}

// startShell starts a synthetic shell session and returns (session, run).
func (e *env) startShell() (string, string) {
	e.t.Helper()
	resp, err := e.st.Hub.StartSession(e.owner, c.StartSessionRequest{Mutation: c.Mutation{OperationID: c.NewID(c.PrefixOperation)}, NodeID: e.cred.NodeID, Harness: c.HarnessShell, Cwd: e.dir})
	if err != nil {
		e.t.Fatal(err)
	}
	sid := resp.Session.Session.SessionID
	waitFor(e.t, "run waiting_input", func() bool {
		v, err := e.st.Hub.GetSession(e.owner, c.GetRequest{ID: sid})
		return err == nil && v.Run != nil && v.Run.State == c.RunWaitingInput
	})
	return sid, resp.Session.Run.RunID
}

func (e *env) attach(ctx context.Context, sid string, readOnly bool) (c.AttachDescriptor, error) {
	return e.st.Hub.AttachSession(ctx, c.AttachRequest{Mutation: c.Mutation{OperationID: c.NewID(c.PrefixOperation)}, SessionID: sid,
		Preferred: []c.AttachMechanism{c.AttachWebTerminal, c.AttachSSHTmux, c.AttachReadOnly}, ReadOnly: readOnly})
}

func (e *env) httpClient() *http.Client {
	tlsCfg, _ := transport.PinnedTLSConfig(e.st.Cert.Fingerprint)
	return &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}}
}

// termClient is a test web-terminal client.
type termClient struct {
	t      *testing.T
	ws     *websocket.Conn
	mu     sync.Mutex
	out    bytes.Buffer
	frames []terminal.Frame
	closed chan struct{}
	err    error
}

func (e *env) dialTerminal(url string) *termClient {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "wss://"+e.addr+url, &websocket.DialOptions{HTTPClient: e.httpClient()})
	if err != nil {
		e.t.Fatal(err)
	}
	tc := &termClient{t: e.t, ws: ws, closed: make(chan struct{})}
	e.t.Cleanup(func() { ws.CloseNow() })
	return tc
}

// open dials the descriptor URL, authenticates, and starts reading.
func (e *env) open(d c.AttachDescriptor, ticket string) *termClient {
	e.t.Helper()
	tc := e.dialTerminal(d.URL)
	tc.send(terminal.Frame{Type: terminal.FrameAuth, Ticket: ticket, Cols: 100, Rows: 30})
	go tc.read()
	return tc
}

func (tc *termClient) send(f terminal.Frame) {
	tc.t.Helper()
	b, _ := json.Marshal(f)
	if err := tc.ws.Write(context.Background(), websocket.MessageText, b); err != nil {
		tc.t.Fatal(err)
	}
}

func (tc *termClient) input(s string) {
	tc.t.Helper()
	if err := tc.ws.Write(context.Background(), websocket.MessageBinary, []byte(s)); err != nil {
		tc.t.Fatal(err)
	}
}

func (tc *termClient) read() {
	defer close(tc.closed)
	for {
		typ, b, err := tc.ws.Read(context.Background())
		if err != nil {
			tc.mu.Lock()
			tc.err = err
			tc.mu.Unlock()
			return
		}
		tc.mu.Lock()
		if typ == websocket.MessageBinary {
			tc.out.Write(b)
		} else {
			var f terminal.Frame
			_ = json.Unmarshal(b, &f)
			tc.frames = append(tc.frames, f)
		}
		tc.mu.Unlock()
	}
}

func (tc *termClient) output() string {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	return tc.out.String()
}

// expectError waits for an error frame and the matching close code.
func (tc *termClient) expectError(code c.ErrorCode) {
	tc.t.Helper()
	select {
	case <-tc.closed:
	case <-time.After(20 * time.Second):
		tc.t.Fatal("terminal did not close")
	}
	tc.mu.Lock()
	defer tc.mu.Unlock()
	if len(tc.frames) == 0 || tc.frames[0].Type != terminal.FrameError || tc.frames[0].Error == nil || tc.frames[0].Error.Code != code {
		tc.t.Fatalf("frames = %+v, want error %s", tc.frames, code)
	}
	if got, want := websocket.CloseStatus(tc.err), terminal.CloseCode(c.Errorf(code, "")); got != want {
		tc.t.Fatalf("close status %d, want %d (%v)", got, want, tc.err)
	}
}

func tmuxClients(t *testing.T, tm supervisor.Tmux) []string {
	t.Helper()
	out, err := exec.Command("tmux", "-S", tm.Socket, "list-clients", "-F", "#{client_width}x#{client_height} ro=#{client_readonly}").Output()
	if err != nil {
		return nil
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

func TestWebTerminalEndToEnd(t *testing.T) {
	e := newEnv(t)
	sid, runID := e.startShell()
	d, err := e.attach(e.owner, sid, false)
	if err != nil {
		t.Fatal(err)
	}
	if d.Mechanism != c.AttachWebTerminal || d.URL != c.PathTerminal || d.Ticket == "" || len(d.Command) != 0 || d.ReadOnly || d.RunID != runID || d.NodeID != e.cred.NodeID {
		t.Fatalf("descriptor = %+v", d)
	}
	if time.Until(d.ExpiresAt) > 90*time.Second || time.Until(d.ExpiresAt) < 30*time.Second {
		t.Fatalf("ticket expiry %s", d.ExpiresAt)
	}
	tm, _ := e.node.tmuxFor(runID)
	serverPID, err := tm.ServerPID(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	tc := e.open(d, d.Ticket)
	waitFor(t, "attach client on the run's tmux", func() bool { return len(tmuxClients(t, tm)) == 1 })
	if cl := tmuxClients(t, tm); cl[0] != "100x30 ro=0" {
		t.Fatalf("client = %q, want initial size 100x30 interactive", cl)
	}
	tc.input("echo hi-$((40+2))\r")
	waitFor(t, "command output relayed", func() bool { return strings.Contains(tc.output(), "hi-42") })

	tc.send(terminal.Frame{Type: terminal.FrameResize, Cols: 120, Rows: 40})
	waitFor(t, "resize applied", func() bool {
		cl := tmuxClients(t, tm)
		return len(cl) == 1 && cl[0] == "120x40 ro=0"
	})

	// The same ticket cannot open a second stream.
	again := e.open(d, d.Ticket)
	again.expectError(c.ErrConflict)

	// Detach: only the attach client goes away; the run keeps going.
	if err := tc.ws.Close(websocket.StatusNormalClosure, "detach"); err != nil && !strings.Contains(err.Error(), "close") {
		t.Fatal(err)
	}
	waitFor(t, "attach client gone", func() bool {
		return len(tmuxClients(t, tm)) == 0 && e.node.bridge.Sessions() == 0 && e.st.Terminal.Streams() == 0
	})
	if ok, err := tm.HasSession(context.Background(), "pyre"); err != nil || !ok {
		t.Fatalf("tmux session gone after detach: %v", err)
	}
	if pid, err := tm.ServerPID(context.Background()); err != nil || pid != serverPID {
		t.Fatalf("tmux server changed: %d -> %d (%v)", serverPID, pid, err)
	}
	if p, err := tm.Pane(context.Background(), "pyre"); err != nil || p.Dead {
		t.Fatalf("shell pane dead after detach: %+v %v", p, err)
	}
	v, _ := e.st.Hub.GetSession(e.owner, c.GetRequest{ID: sid})
	if v.Run == nil || v.Run.State.Terminal() {
		t.Fatalf("run ended after detach: %+v", v.Run)
	}

	// A fresh attach works again and sees the earlier output.
	d2, err := e.attach(e.owner, sid, false)
	if err != nil {
		t.Fatal(err)
	}
	tc2 := e.open(d2, d2.Ticket)
	waitFor(t, "screen redrawn on reattach", func() bool { return strings.Contains(tc2.output(), "hi-42") })
	tc2.ws.Close(websocket.StatusNormalClosure, "detach")
	waitFor(t, "second stream closed", func() bool { return e.node.bridge.Sessions() == 0 && e.st.Terminal.Streams() == 0 })
}

func TestWebTerminalReadOnlyIgnoresInput(t *testing.T) {
	e := newEnv(t)
	sid, runID := e.startShell()
	d, err := e.attach(e.owner, sid, true)
	if err != nil {
		t.Fatal(err)
	}
	if d.Mechanism != c.AttachWebTerminal || !d.ReadOnly {
		t.Fatalf("descriptor = %+v", d)
	}
	tm, _ := e.node.tmuxFor(runID)
	tc := e.open(d, d.Ticket)
	waitFor(t, "read-only attach client", func() bool {
		cl := tmuxClients(t, tm)
		return len(cl) == 1 && strings.HasSuffix(cl[0], "ro=1")
	})
	// Output still flows to the viewer.
	tm.SendKeys(context.Background(), "pyre", true, "echo seen-$((6*7))")
	tm.SendKeys(context.Background(), "pyre", false, "Enter")
	waitFor(t, "viewer sees output", func() bool { return strings.Contains(tc.output(), "seen-42") })
	// Viewer input is dropped.
	tc.input("echo typed-$((50+5))\r")
	time.Sleep(500 * time.Millisecond)
	if scr, _ := tm.Capture(context.Background(), "pyre", 0); strings.Contains(scr, "typed") {
		t.Fatalf("read-only input reached the run:\n%s", scr)
	}
	tc.ws.Close(websocket.StatusNormalClosure, "done")
	waitFor(t, "stream closed", func() bool { return e.node.bridge.Sessions() == 0 && len(tmuxClients(t, tm)) == 0 })
}

func TestWebTerminalTicketRejections(t *testing.T) {
	e := newEnv(t)
	sid, _ := e.startShell()

	// Garbage and missing auth.
	bad := e.open(c.AttachDescriptor{URL: c.PathTerminal}, "pyt_synthetic-not-a-ticket")
	bad.expectError(c.ErrUnauthorized)
	noAuth := e.dialTerminal(c.PathTerminal)
	noAuth.input("not json")
	go noAuth.read()
	noAuth.expectError(c.ErrInvalid)

	// Expired ticket.
	d, err := e.attach(e.owner, sid, false)
	if err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(2 * time.Minute)
	exp := e.open(d, d.Ticket)
	exp.expectError(c.ErrExpired)
	e.node.mu.Lock()
	opens := e.node.opens
	e.node.mu.Unlock()
	if opens != 0 {
		t.Fatalf("node received %d terminal_open commands for rejected tickets", opens)
	}
}

func TestWebTerminalNodeOffline(t *testing.T) {
	e := newEnv(t)
	sid, _ := e.startShell()
	d, err := e.attach(e.owner, sid, false)
	if err != nil {
		t.Fatal(err)
	}
	e.stop()
	waitFor(t, "node disconnected", func() bool { return !e.st.NodeServer.Connected(e.cred.NodeID) })
	if _, err := e.attach(e.owner, sid, false); !c.IsCode(err, c.ErrUnavailable) {
		t.Fatalf("attach while offline: %v", err)
	}
	tc := e.open(d, d.Ticket)
	tc.expectError(c.ErrUnavailable)
}

func TestWebTerminalStreamClaimedOnlyByOwningNode(t *testing.T) {
	e := newEnv(t)
	sid, _ := e.startShell()
	other := e.enroll("synth-node-b")
	ownerTok, err := e.st.Auth.Issue(context.Background(), c.PrincipalOwner, "cli_synth-other", "synthetic owner")
	if err != nil {
		t.Fatal(err)
	}
	e.node.hold.Store(true) // the node accepts terminal_open but does not dial
	d, err := e.attach(e.owner, sid, false)
	if err != nil {
		t.Fatal(err)
	}
	tc := e.open(d, d.Ticket)
	var stream string
	waitFor(t, "terminal_open delivered", func() bool {
		e.node.mu.Lock()
		defer e.node.mu.Unlock()
		if len(e.node.streams) > 0 {
			stream = e.node.streams[0]
		}
		return stream != ""
	})
	dialNode := func(token string) (int, *websocket.Conn) {
		hdr := http.Header{}
		hdr.Set("Authorization", "Bearer "+token)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ws, resp, err := websocket.Dial(ctx, "wss://"+e.addr+c.PathNodeTerminal+"?stream="+stream, &websocket.DialOptions{HTTPClient: e.httpClient(), HTTPHeader: hdr})
		if err != nil {
			if resp == nil {
				t.Fatal(err)
			}
			return resp.StatusCode, nil
		}
		return http.StatusSwitchingProtocols, ws
	}
	if code, _ := dialNode(other.Token); code != http.StatusForbidden {
		t.Fatalf("other node claim: HTTP %d, want 403", code)
	}
	if code, _ := dialNode(ownerTok); code != http.StatusForbidden {
		t.Fatalf("owner token claim: HTTP %d, want 403", code)
	}
	if code, _ := dialNode("pyn_synthetic-revoked-or-unknown"); code != http.StatusUnauthorized {
		t.Fatalf("unknown token claim: HTTP %d, want 401", code)
	}
	// The owning node can still claim the stream; bytes flow both ways.
	code, ws := dialNode(e.cred.Token)
	if code != http.StatusSwitchingProtocols {
		t.Fatalf("owning node claim: HTTP %d", code)
	}
	defer ws.CloseNow()
	if code, _ := dialNode(e.cred.Token); code != http.StatusConflict {
		t.Fatalf("second claim: HTTP %d, want 409", code)
	}
	if err := ws.Write(context.Background(), websocket.MessageBinary, []byte("synthetic-output")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "node bytes relayed", func() bool { return strings.Contains(tc.output(), "synthetic-output") })
	tc.input("synthetic-input")
	rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	typ, b, err := ws.Read(rctx)
	if err != nil || typ != websocket.MessageBinary || string(b) != "synthetic-input" {
		t.Fatalf("client input at node: %v %v %q", err, typ, b)
	}
	tc.send(terminal.Frame{Type: terminal.FrameResize, Cols: 5000, Rows: 0})
	typ, b, err = ws.Read(rctx)
	var f terminal.Frame
	if err != nil || typ != websocket.MessageText || json.Unmarshal(b, &f) != nil || f.Type != terminal.FrameResize || f.Cols != terminal.MaxDim || f.Rows != terminal.DefaultRows {
		t.Fatalf("resize at node: %v %s", err, b)
	}
	// Node closes: the client sees an orderly close.
	ws.Close(websocket.StatusNormalClosure, "terminal exited")
	select {
	case <-tc.closed:
	case <-time.After(10 * time.Second):
		t.Fatal("client not closed after node close")
	}
	if websocket.CloseStatus(tc.err) != websocket.StatusNormalClosure {
		t.Fatalf("client close: %v", tc.err)
	}
}

func TestWebTerminalRespectsInputLease(t *testing.T) {
	e := newEnv(t)
	sid, runID := e.startShell()
	phone := c.WithPrincipal(context.Background(), c.Principal{Kind: c.PrincipalOwner, ID: "cli_synth-phone", Name: "phone (synthetic)"})
	if _, err := e.st.Hub.AcquireLease(phone, c.LeaseRequest{Mutation: c.Mutation{OperationID: c.NewID(c.PrefixOperation)}, RunID: runID, TTLSeconds: 600}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.attach(e.owner, sid, false); !c.IsCode(err, c.ErrConflict) {
		t.Fatalf("interactive attach while another client holds the lease: %v", err)
	}
	if d, err := e.attach(e.owner, sid, true); err != nil || !d.ReadOnly || d.Lease == nil {
		t.Fatalf("read-only attach: %v %+v", err, d)
	}
	// The lease holder may attach interactively.
	d, err := e.attach(phone, sid, false)
	if err != nil {
		t.Fatal(err)
	}
	tm, _ := e.node.tmuxFor(runID)
	tc := e.open(d, d.Ticket)
	tc.input("echo lease-$((1+1))\r")
	waitFor(t, "lease holder input", func() bool { return strings.Contains(tc.output(), "lease-2") })
	tc.ws.Close(websocket.StatusNormalClosure, "done")
	waitFor(t, "stream closed", func() bool { return len(tmuxClients(t, tm)) == 0 })
}

func TestFramesHelpers(t *testing.T) {
	if cols, rows := terminal.ClampSize(0, -1); cols != 80 || rows != 24 {
		t.Fatalf("defaults %dx%d", cols, rows)
	}
	if cols, rows := terminal.ClampSize(99999, 50); cols != terminal.MaxDim || rows != 50 {
		t.Fatalf("clamp %dx%d", cols, rows)
	}
	if terminal.CloseCode(c.Errorf(c.ErrExpired, "x")) != 4410 || terminal.CloseCode(c.Errorf(c.ErrUnavailable, "x")) != 4503 || terminal.CloseCode(nil) != websocket.StatusNormalClosure {
		t.Fatal("close codes")
	}
}
