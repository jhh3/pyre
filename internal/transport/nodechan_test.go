package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

func TestReportOutboxResumesAcrossHubRestart(t *testing.T) {
	e := newEnv(t)
	e.start()
	db := openDB(t, "node.db")
	link := e.newLink(db, &countingHandler{}, nil)
	stop := runLink(t, link)
	ctx := context.Background()

	for i := 1; i <= 3; i++ {
		if _, err := link.Enqueue(ctx, contracts.ReportEvent, map[string]int{"n": i}); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "reports 1..3 committed and pruned", func() bool {
		n, _ := link.OutboxLen(ctx)
		var c int
		e.hub.snapshot(func(h *fakeHub) { c = len(h.committed) })
		return c == 3 && n == 0
	})

	// Hub goes down. Reports produced meanwhile stay in the durable outbox.
	e.stop()
	waitFor(t, "link notices hub loss", func() bool { return !link.Connected() })
	for i := 4; i <= 6; i++ {
		if _, err := link.Enqueue(ctx, contracts.ReportEvent, map[string]int{"n": i}); err != nil {
			t.Fatalf("enqueue while hub down: %v", err)
		}
	}
	// Restart the node process too: a new NodeLink over the same database.
	stop()
	if n, _ := link.OutboxLen(ctx); n != 3 {
		t.Fatalf("outbox while hub down = %d, want 3", n)
	}
	// Simulate the hub having committed report 4 just before it crashed
	// (commit happened, ack never reached the node).
	e.hub.snapshot(func(h *fakeHub) {
		h.committed[4] = contracts.NodeReport{OriginSeq: 4, Kind: contracts.ReportEvent}
		h.received = nil
	})

	e.start() // same address, same persisted hub state
	link2 := e.newLink(db, &countingHandler{}, nil)
	runLink(t, link2)
	waitFor(t, "reports 5..6 redelivered and outbox drained", func() bool {
		n, _ := link2.OutboxLen(ctx)
		var c int
		e.hub.snapshot(func(h *fakeHub) { c = len(h.committed) })
		return c == 6 && n == 0
	})
	e.hub.snapshot(func(h *fakeHub) {
		for _, s := range h.received {
			if s <= 4 {
				t.Errorf("report %d redelivered after it was committed/acked", s)
			}
		}
		if len(h.hellos) < 2 {
			t.Fatalf("hellos = %d", len(h.hellos))
		}
		for _, hl := range h.hellos {
			if hl.NodeID != e.nodeID {
				t.Errorf("hello node id %q changed (want stable %q)", hl.NodeID, e.nodeID)
			}
		}
	})
	// New reports after reconnect continue the sequence.
	seq, err := link2.Enqueue(ctx, contracts.ReportEvent, map[string]int{"n": 7})
	if err != nil || seq != 7 {
		t.Fatalf("next seq = %d, %v; want 7", seq, err)
	}
}

func TestCommandDedupAcrossReconnect(t *testing.T) {
	e := newEnv(t)
	c1 := contracts.NodeCommand{CommandID: contracts.NewID(contracts.PrefixCommand), Seq: 1, Kind: contracts.CmdInterrupt,
		IssuedAt: time.Now().UTC(), Payload: json.RawMessage(`{}`)}
	e.hub.commands = []contracts.NodeCommand{c1}
	e.hub.resendAll = true // the hub "lost" acks and resends everything
	e.start()
	h := &countingHandler{}
	db := openDB(t, "node.db")
	link := e.newLink(db, h, nil)
	runLink(t, link)
	ctx := context.Background()

	waitFor(t, "command executed and result committed", func() bool {
		var n int
		e.hub.snapshot(func(h *fakeHub) { n = len(h.results) })
		return n == 1
	})
	waitFor(t, "result acked", func() bool { n, _ := link.OutboxLen(ctx); return n == 0 })

	// Force a reconnect; the hub resends c1.
	e.ns.Disconnect(e.nodeID, nil)
	waitFor(t, "second ack after reconnect", func() bool {
		var n int
		e.hub.snapshot(func(h *fakeHub) { n = len(h.acks) })
		return n >= 2
	})
	waitFor(t, "stored result returned for duplicate", func() bool {
		var n int
		e.hub.snapshot(func(h *fakeHub) { n = len(h.results) })
		return n >= 2
	})
	if got := h.count(c1.CommandID); got != 1 {
		t.Fatalf("handler ran %d times for one command, want 1", got)
	}
	e.hub.snapshot(func(hb *fakeHub) {
		last := hb.hellos[len(hb.hellos)-1]
		if last.LastCommandSeq != 1 {
			t.Errorf("hello LastCommandSeq = %d, want 1", last.LastCommandSeq)
		}
		for _, r := range hb.results {
			if r.CommandID != c1.CommandID || r.Outcome != contracts.OutcomeSucceeded {
				t.Errorf("unexpected result %+v", r)
			}
		}
	})

	// An expired command is recorded as failed/expired and never executed.
	past := time.Now().Add(-time.Minute).UTC()
	c2 := contracts.NodeCommand{CommandID: contracts.NewID(contracts.PrefixCommand), Seq: 2, Kind: contracts.CmdStop,
		IssuedAt: past.Add(-time.Minute), ExpiresAt: &past, Payload: json.RawMessage(`{}`)}
	waitFor(t, "reconnected", link.Connected)
	if err := e.ns.Send(ctx, e.nodeID, c2); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "expired result", func() bool {
		var found bool
		e.hub.snapshot(func(hb *fakeHub) {
			for _, r := range hb.results {
				if r.CommandID == c2.CommandID {
					found = r.Outcome == contracts.OutcomeFailed && r.Error != nil && r.Error.Code == contracts.ErrExpired
				}
			}
		})
		return found
	})
	if h.count(c2.CommandID) != 0 {
		t.Fatal("expired command was executed")
	}
}

func TestExecutingCommandIsIndeterminateAfterRestart(t *testing.T) {
	db := openDB(t, "node.db")
	if err := sqlitex.Migrate(context.Background(), db, NodeLinkMigrations...); err != nil {
		t.Fatal(err)
	}
	cmd := contracts.NodeCommand{CommandID: "cmd_stuck", Seq: 9, Kind: contracts.CmdStartRun, Payload: json.RawMessage(`{}`)}
	raw, _ := json.Marshal(cmd)
	if _, err := db.Exec(`INSERT INTO nodelink_commands(command_id, seq, kind, command, state, received_at) VALUES(?,?,?,?,?,?)`,
		cmd.CommandID, cmd.Seq, string(cmd.Kind), raw, cmdExecuting, sqlitex.FormatTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	h := &countingHandler{}
	l, err := NewNodeLink(NodeLinkConfig{HubURL: "https://127.0.0.1:1", NodeID: "node_x", Token: "pyn_synthetic-token-for-test", DB: db, Handler: h,
		Backoff: BackoffConfig{Initial: time.Hour}})
	if err != nil {
		t.Fatal(err)
	}
	stop := runLink(t, l)
	waitFor(t, "indeterminate result queued", func() bool { n, _ := l.OutboxLen(context.Background()); return n == 1 })
	stop()
	var payload []byte
	if err := db.QueryRow(`SELECT payload FROM nodelink_outbox`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var res contracts.CommandResult
	_ = json.Unmarshal(payload, &res)
	if res.Outcome != contracts.OutcomeIndeterminate || res.CommandID != "cmd_stuck" {
		t.Fatalf("result = %+v", res)
	}
	if h.count("cmd_stuck") != 0 {
		t.Fatal("stuck command re-executed")
	}
}

func TestRevokedNodeRejectedAndDisconnected(t *testing.T) {
	e := newEnv(t)
	e.start()
	e.auth.OnRevoke(func(p contracts.Principal) {
		if p.Kind == contracts.PrincipalNode {
			e.ns.Disconnect(p.ID, contracts.Errorf(contracts.ErrUnauthorized, "node credential revoked"))
		}
	})
	link := e.newLink(openDB(t, "node.db"), &countingHandler{}, nil)
	runLink(t, link)
	waitFor(t, "connected", func() bool { return link.Connected() && e.ns.Connected(e.nodeID) })

	if _, err := e.auth.RevokePrincipal(context.Background(), contracts.PrincipalNode, e.nodeID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "link rejected", func() bool {
		st := link.Status()
		return st.State == LinkRejected && st.LastError != nil && st.LastError.Code == contracts.ErrUnauthorized
	})
	if e.ns.Connected(e.nodeID) {
		t.Fatal("revoked node still connected")
	}
	// Reconnect attempts are refused at the HTTP layer and stay rejected.
	time.Sleep(300 * time.Millisecond)
	if st := link.Status(); st.State != LinkRejected && st.State != LinkConnecting {
		t.Fatalf("state after retry = %s", st.State)
	}
	if e.ns.Connected(e.nodeID) {
		t.Fatal("revoked node reconnected")
	}
	if strings.Contains(e.logs.String(), e.token) {
		t.Fatal("token leaked into logs")
	}
}

func TestRevocationBackstopViaHeartbeat(t *testing.T) {
	e := newEnv(t)
	e.hbInt = 50 * time.Millisecond
	e.start() // no OnRevoke wiring: rely on per-heartbeat re-authentication
	link := e.newLink(openDB(t, "node.db"), &countingHandler{}, nil)
	runLink(t, link)
	waitFor(t, "connected", link.Connected)
	if _, err := e.auth.RevokePrincipal(context.Background(), contracts.PrincipalNode, e.nodeID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "disconnected by heartbeat re-auth", func() bool {
		return !e.ns.Connected(e.nodeID) && link.Status().State == LinkRejected
	})
}

func TestProtocolMismatchIsActionable(t *testing.T) {
	e := newEnv(t)
	e.start()
	link := e.newLink(openDB(t, "node.db"), &countingHandler{}, nil)
	link.protoMax, link.protoMin = 99, 99
	runLink(t, link)
	waitFor(t, "protocol mismatch", func() bool {
		st := link.Status()
		return st.State == LinkRejected && st.LastError != nil && st.LastError.Code == contracts.ErrProtocolMismatch
	})
	msg := link.Status().LastError.Message
	if !strings.Contains(msg, "protocol 99") || !strings.Contains(msg, "upgrade the hub") {
		t.Fatalf("mismatch message not actionable: %q", msg)
	}
	if e.ns.Connected(e.nodeID) {
		t.Fatal("mismatched node registered")
	}
	var hellos int
	e.hub.snapshot(func(h *fakeHub) { hellos = len(h.hellos) })
	if hellos != 0 {
		t.Fatal("OnHello invoked for mismatched protocol")
	}
}

func TestNegotiateProtocol(t *testing.T) {
	if p, err := NegotiateProtocol(contracts.ProtocolVersion, contracts.MinProtocolVersion); err != nil || p != contracts.ProtocolVersion {
		t.Fatalf("same version: %d %v", p, err)
	}
	if p, err := NegotiateProtocol(contracts.ProtocolVersion+3, contracts.MinProtocolVersion); err != nil || p != contracts.ProtocolVersion {
		t.Fatalf("newer node with overlap: %d %v", p, err)
	}
	_, err := NegotiateProtocol(0, 0)
	if err == nil || err.Code != contracts.ErrProtocolMismatch || !strings.Contains(err.Message, "upgrade pyred") {
		t.Fatalf("old node: %v", err)
	}
}

func TestReconnectBackoffIsBounded(t *testing.T) {
	var attempts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		writeHTTPError(w, contracts.Errorf(contracts.ErrUnavailable, "down"))
	}))
	defer srv.Close()
	l, err := NewNodeLink(NodeLinkConfig{HubURL: srv.URL, NodeID: "node_x", Token: "pyn_synthetic-token-for-test",
		DB: openDB(t, "node.db"), Handler: &countingHandler{},
		Backoff: BackoffConfig{Initial: 10 * time.Millisecond, Max: 100 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	stop := runLink(t, l)
	time.Sleep(time.Second)
	stop()
	// Unbounded retry would make thousands of attempts; the ramp 10,20,40,80
	// then ~[50,100]ms caps it near 15.
	if n := attempts.Load(); n < 4 || n > 25 {
		t.Fatalf("attempts in 1s = %d, want bounded in [4,25]", n)
	}
}

func TestSecondConnectionReplacesFirst(t *testing.T) {
	e := newEnv(t)
	e.start()
	a := e.newLink(openDB(t, "a.db"), &countingHandler{}, nil)
	runLink(t, a)
	waitFor(t, "a connected", a.Connected)
	b := e.newLink(openDB(t, "b.db"), &countingHandler{}, func(c *NodeLinkConfig) {
		c.Backoff = BackoffConfig{Initial: time.Hour}
	})
	runLink(t, b)
	waitFor(t, "replacement observed", func() bool {
		var replaced bool
		e.hub.snapshot(func(h *fakeHub) {
			for _, d := range h.disconnects {
				replaced = replaced || d.Replaced
			}
		})
		return replaced
	})
	if n := len(e.ns.ConnectedNodes()); n != 1 {
		t.Fatalf("connected entries = %d, want 1 (no duplicate node)", n)
	}
}

func TestStatusReportsAreNeverQueued(t *testing.T) {
	e := newEnv(t)
	e.start()
	link := e.newLink(openDB(t, "node.db"), &countingHandler{}, nil)
	ctx := context.Background()
	if err := link.SendStatus(ctx, contracts.NodeStatusReport{}); !contracts.IsCode(err, contracts.ErrUnavailable) {
		t.Fatalf("status while disconnected: %v", err)
	}
	if _, err := link.Enqueue(ctx, contracts.ReportStatus, contracts.NodeStatusReport{}); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("enqueue status: %v", err)
	}
	runLink(t, link)
	waitFor(t, "connected", link.Connected)
	if err := link.SendStatus(ctx, contracts.NodeStatusReport{Resources: contracts.NodeResources{CPUCount: 4}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "status delivered", func() bool {
		var n int
		e.hub.snapshot(func(h *fakeHub) { n = h.statuses })
		return n == 1
	})
	if n, _ := link.OutboxLen(ctx); n != 0 {
		t.Fatalf("status was queued: outbox=%d", n)
	}
}

func TestServerRejectsWrongPrincipalAndNodeMismatch(t *testing.T) {
	e := newEnv(t)
	e.start()
	ctx := context.Background()
	tlsCfg, _ := PinnedTLSConfig(e.cert.Fingerprint)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}}
	wsURL := "wss://" + e.addr + contracts.PathNodeConnect

	owner, _ := e.auth.Issue(ctx, contracts.PrincipalOwner, "cli_x", "laptop")
	_, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPClient: client, HTTPHeader: http.Header{"Authorization": {"Bearer " + owner}}})
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("owner token on node channel: resp=%v err=%v", resp, err)
	}
	_, resp, err = websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPClient: client})
	if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: resp=%v err=%v", resp, err)
	}

	ws, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPClient: client, HTTPHeader: http.Header{"Authorization": {"Bearer " + e.token}}})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	if err := writeFrame(ctx, ws, time.Second, contracts.NodeFrame{Type: contracts.FrameHello, Hello: &contracts.NodeHello{
		NodeID: "node_someoneelse", Protocol: contracts.ProtocolVersion, MinProtocol: contracts.MinProtocolVersion}}); err != nil {
		t.Fatal(err)
	}
	f, err := readFrame(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if f.Type != contracts.FrameError || f.Error.Code != contracts.ErrForbidden {
		t.Fatalf("node id mismatch: %+v", f)
	}
}

func TestTokensAbsentFromLogsAndErrors(t *testing.T) {
	e := newEnv(t)
	e.start()
	link := e.newLink(openDB(t, "node.db"), &countingHandler{}, nil)
	runLink(t, link)
	waitFor(t, "connected", link.Connected)
	// Exercise failure paths that log.
	bad := e.newLink(openDB(t, "bad.db"), &countingHandler{}, func(c *NodeLinkConfig) { c.Token = e.token + "x" })
	runLink(t, bad)
	waitFor(t, "bad token rejected", func() bool { return bad.Status().State == LinkRejected })
	if _, err := e.auth.RevokePrincipal(context.Background(), contracts.PrincipalNode, e.nodeID); err != nil {
		t.Fatal(err)
	}
	e.ns.Disconnect(e.nodeID, contracts.Errorf(contracts.ErrUnauthorized, "revoked"))
	waitFor(t, "rejected", func() bool { return link.Status().State == LinkRejected })
	// A careless log line containing a token is still redacted.
	link.log.Error("oops " + e.token)

	logs := e.logs.String()
	if logs == "" {
		t.Fatal("expected some logs")
	}
	secret := strings.TrimPrefix(e.token, PrefixNodeToken)
	if strings.Contains(logs, secret) {
		t.Fatalf("token found in logs:\n%s", logs)
	}
	if st := link.Status(); st.LastError != nil && strings.Contains(st.LastError.Error(), secret) {
		t.Fatal("token in status error")
	}
	if _, err := e.auth.Authenticate(context.Background(), e.token); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("authenticate error leaks or succeeds: %v", err)
	}
}

// A Send racing a new connection must not put a command frame ahead of
// Welcome or ahead of the welcome's pending commands.
func TestSendHeldUntilWelcomeWritten(t *testing.T) {
	e := newEnv(t)
	e.hub.commands = []contracts.NodeCommand{{CommandID: "cmd_synth-pending", Seq: 1, Kind: "synth.noop"}}
	e.start()
	sendErr := make(chan error, 1)
	e.ns.beforeWelcome = func(nodeID string) {
		go func() {
			sendErr <- e.ns.Send(context.Background(), nodeID, contracts.NodeCommand{CommandID: "cmd_synth-live", Seq: 2, Kind: "synth.noop"})
		}()
		// Give an unsynchronised Send ample time to win the race.
		time.Sleep(100 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tlsCfg, _ := PinnedTLSConfig(e.cert.Fingerprint)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}}
	ws, _, err := websocket.Dial(ctx, "wss://"+e.addr+contracts.PathNodeConnect, &websocket.DialOptions{HTTPClient: client, HTTPHeader: http.Header{"Authorization": {"Bearer " + e.token}}})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	if err := writeFrame(ctx, ws, time.Second, contracts.NodeFrame{Type: contracts.FrameHello, Hello: &contracts.NodeHello{
		NodeID: e.nodeID, Protocol: contracts.ProtocolVersion, MinProtocol: contracts.MinProtocolVersion}}); err != nil {
		t.Fatal(err)
	}
	var got []string
	for len(got) < 3 {
		f, err := readFrame(ctx, ws)
		if err != nil {
			t.Fatalf("after %v: %v", got, err)
		}
		switch f.Type {
		case contracts.FrameWelcome:
			if f.Welcome.MinProtocol != contracts.MinProtocolVersion {
				t.Fatalf("welcome min_protocol = %d", f.Welcome.MinProtocol)
			}
			got = append(got, "welcome")
		case contracts.FrameCommand:
			got = append(got, f.Command.CommandID)
		}
	}
	if want := []string{"welcome", "cmd_synth-pending", "cmd_synth-live"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("frame order %v, want %v", got, want)
	}
	if err := <-sendErr; err != nil {
		t.Fatalf("send: %v", err)
	}
}

// A silent node (connection open, nothing received) is dropped close to
// HeartbeatTimeout, not up to a whole heartbeat interval later.
func TestSilentNodeDetectedNearTimeout(t *testing.T) {
	e := newEnv(t)
	e.hbInt = time.Second
	e.start()
	e.ns.cfg.HeartbeatTimeout = 1200 * time.Millisecond
	ctx := context.Background()
	tlsCfg, _ := PinnedTLSConfig(e.cert.Fingerprint)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}}
	ws, _, err := websocket.Dial(ctx, "wss://"+e.addr+contracts.PathNodeConnect, &websocket.DialOptions{HTTPClient: client, HTTPHeader: http.Header{"Authorization": {"Bearer " + e.token}}})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	if err := writeFrame(ctx, ws, time.Second, contracts.NodeFrame{Type: contracts.FrameHello, Hello: &contracts.NodeHello{
		NodeID: e.nodeID, Protocol: contracts.ProtocolVersion, MinProtocol: contracts.MinProtocolVersion}}); err != nil {
		t.Fatal(err)
	}
	if _, err := readFrame(ctx, ws); err != nil { // welcome
		t.Fatal(err)
	}
	start := time.Now()
	waitFor(t, "registered", func() bool { return e.ns.Connected(e.nodeID) })
	// Silent from now on: never read or write.
	waitFor(t, "silent node dropped", func() bool { return !e.ns.Connected(e.nodeID) })
	if took := time.Since(start); took > 1700*time.Millisecond {
		t.Fatalf("silent node dropped after %v; timeout is 1.2s", took)
	}
}

// A hub restored from an older backup welcomes the node with a committed
// watermark below what it acknowledged before. The link reports that via
// OnHubBehind (once per such welcome) and keeps delivering new reports.
func TestHubBehindAfterRestoreTriggersCallback(t *testing.T) {
	e := newEnv(t)
	e.start()
	db := openDB(t, "node.db")
	type behindCall struct{ committed, acked uint64 }
	calls := make(chan behindCall, 4)
	mod := func(c *NodeLinkConfig) {
		c.OnHubBehind = func(_ context.Context, committed, acked uint64) { calls <- behindCall{committed, acked} }
	}
	link := e.newLink(db, &countingHandler{}, mod)
	stop := runLink(t, link)
	ctx := context.Background()
	for i := 1; i <= 3; i++ {
		if _, err := link.Enqueue(ctx, contracts.ReportEvent, map[string]int{"n": i}); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "reports committed", func() bool { n, _ := link.OutboxLen(ctx); return n == 0 })
	select {
	case c := <-calls:
		t.Fatalf("callback on a healthy hub: %+v", c)
	default:
	}
	e.stop()
	stop()
	// The hub comes back from a backup that only had report 1.
	e.hub.snapshot(func(h *fakeHub) {
		delete(h.committed, 2)
		delete(h.committed, 3)
	})
	e.start()
	link2 := e.newLink(db, &countingHandler{}, mod)
	runLink(t, link2)
	select {
	case c := <-calls:
		if c.committed != 1 || c.acked != 3 {
			t.Fatalf("behind call = %+v, want committed 1 acked 3", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OnHubBehind not called after the hub regressed")
	}
	seq, err := link2.Enqueue(ctx, contracts.ReportEvent, map[string]int{"n": 4})
	if err != nil || seq != 4 {
		t.Fatalf("next seq = %d, %v", seq, err)
	}
	waitFor(t, "report 4 committed by the restored hub", func() bool {
		var ok bool
		e.hub.snapshot(func(h *fakeHub) { _, ok = h.committed[4] })
		return ok
	})
}
