package assemble

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/hub"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
	"github.com/jhh3/pyre/internal/transport"
)

// servedHub is a composed hub behind a real TLS listener.
type servedHub struct {
	st  *Stack
	srv *http.Server
}

func serveHub(t *testing.T, dir, addr string) (*servedHub, string) {
	t.Helper()
	st, err := Open(context.Background(), Config{DataDir: dir, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Hub: hub.Options{PollInterval: 20 * time.Millisecond, MaintenanceInterval: 200 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	st.Start(context.Background())
	var ln net.Listener
	for i := 0; i < 50; i++ { // the port may linger briefly after a restart
		if ln, err = net.Listen("tcp", addr); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: st.Handler, TLSConfig: st.Cert.ServerTLSConfig()}
	go srv.ServeTLS(ln, "", "")
	return &servedHub{st: st, srv: srv}, ln.Addr().String()
}

func (s *servedHub) stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.st.NodeServer.Close() // drop node sockets (Shutdown does not track hijacked conns)
	s.srv.Shutdown(ctx)
	s.st.Close()
}

// fakeDaemon is a minimal node runtime behind a real transport.NodeLink.
type fakeDaemon struct {
	mu        sync.Mutex
	link      *transport.NodeLink
	starts    int
	delivered []string
	runs      map[string]c.Run
}

func (d *fakeDaemon) HandleCommand(ctx context.Context, cmd c.NodeCommand) (c.CommandResult, error) {
	switch cmd.Kind {
	case c.CmdStartRun:
		var sr c.StartRunCommand
		if err := json.Unmarshal(cmd.Payload, &sr); err != nil {
			return c.CommandResult{}, err
		}
		if sr.SessionToken == "" {
			return c.CommandResult{}, errors.New("no session token")
		}
		run := c.Run{RunID: sr.RunID, SessionID: sr.Session.SessionID, Adapter: "mock", Harness: c.HarnessMock, State: c.RunWaitingInput, StartedAt: time.Now().UTC(), Capabilities: mockCaps}
		d.mu.Lock()
		d.starts++
		d.runs[run.RunID] = run
		d.mu.Unlock()
		b, _ := json.Marshal(run)
		return c.CommandResult{Outcome: c.OutcomeSucceeded, Result: b}, nil
	case c.CmdDeliver:
		var dc c.DeliverCommand
		if err := json.Unmarshal(cmd.Payload, &dc); err != nil {
			return c.CommandResult{}, err
		}
		d.mu.Lock()
		d.delivered = append(d.delivered, dc.Message.MessageID)
		var runID string
		for id, r := range d.runs {
			if r.SessionID == dc.SessionID {
				runID = id
			}
		}
		d.mu.Unlock()
		// Durable receipt through the node outbox.
		_, err := d.link.Enqueue(ctx, c.ReportReceipt, c.DeliveryReceipt{MessageID: dc.Message.MessageID, AttemptID: c.NewID(c.PrefixAttempt), SessionID: dc.SessionID,
			RunID: runID, State: c.DeliveryAccepted, Evidence: c.EvidenceMock, EvidenceRef: "mock-turn", ObservedAt: time.Now().UTC()})
		return c.CommandResult{Outcome: c.OutcomeSucceeded}, err
	}
	return c.CommandResult{Outcome: c.OutcomeSucceeded}, nil
}

func (d *fakeDaemon) liveRuns() []c.Run {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []c.Run
	for _, r := range d.runs {
		out = append(out, r)
	}
	return out
}

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

// TestEndToEndRestartAndResendOverTransport exercises A05/A19 through the
// real node WebSocket transport: launch, node disconnect, message stored
// while the recipient is offline, hub restart, node reconnect, resend of
// the queued delivery exactly once, durable receipt, and no duplicate
// launch.
func TestEndToEndRestartAndResendOverTransport(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "hub")
	h, addr := serveHub(t, dir, "127.0.0.1:0")
	owner := c.WithPrincipal(context.Background(), c.Principal{Kind: c.PrincipalOwner, ID: "cli_synth-e2e"})
	tk, err := h.st.Fleet.CreateEnrollment(owner, c.EnrollmentRequest{Mutation: c.Mutation{OperationID: c.NewID(c.PrefixOperation)}, Name: "synth-node"})
	if err != nil {
		t.Fatal(err)
	}
	cred, err := h.st.Enrollment.RedeemNode(context.Background(), c.NodeEnrollExchange{NodeID: tk.NodeID, Secret: tk.Secret, Hostname: "synth-node", Platform: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	pin := h.st.Cert.Fingerprint
	nodeDB, err := sqlitex.Open(filepath.Join(base, "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer nodeDB.Close()
	d := &fakeDaemon{runs: map[string]c.Run{}}
	startLink := func() (context.CancelFunc, chan struct{}) {
		tlsCfg, err := transport.PinnedTLSConfig(pin)
		if err != nil {
			t.Fatal(err)
		}
		link, err := transport.NewNodeLink(transport.NodeLinkConfig{HubURL: "https://" + addr, NodeID: cred.NodeID, Token: cred.Token, TLS: tlsCfg, DB: nodeDB,
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
			Hello: func(ctx context.Context) (c.NodeHello, error) {
				return c.NodeHello{Hostname: "synth-node", Platform: "linux", Arch: "amd64", PyredVersion: "0.1.0-synth",
					Harnesses: []c.HarnessInfo{{Harness: c.HarnessMock, Version: "mock", Available: true, Capabilities: mockCaps}},
					Resources: c.NodeResources{CPUCount: 4, MaxRuns: 8}, LiveRuns: d.liveRuns()}, nil
			},
			Handler: d, Backoff: transport.BackoffConfig{Initial: 20 * time.Millisecond, Max: 200 * time.Millisecond}})
		if err != nil {
			t.Fatal(err)
		}
		d.mu.Lock()
		d.link = link
		d.mu.Unlock()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { link.Run(ctx); close(done) }()
		return cancel, done
	}
	stopLink, linkDone := startLink()
	waitFor(t, "node connected", func() bool { return h.st.NodeServer.Connected(cred.NodeID) })

	resp, err := h.st.Hub.StartSession(owner, c.StartSessionRequest{Mutation: c.Mutation{OperationID: "op_e2e-start"}, NodeID: cred.NodeID, Harness: c.HarnessMock, Cwd: "/tmp/synth"})
	if err != nil {
		t.Fatal(err)
	}
	sid := resp.Session.Session.SessionID
	waitFor(t, "run waiting_input", func() bool {
		v, err := h.st.Hub.GetSession(owner, c.GetRequest{ID: sid})
		return err == nil && v.Run != nil && v.Run.State == c.RunWaitingInput
	})

	// Node goes away; the hub shows the run stale/unknown, not failed.
	stopLink()
	<-linkDone
	waitFor(t, "node disconnected", func() bool { return !h.st.NodeServer.Connected(cred.NodeID) })
	v, _ := h.st.Hub.GetSession(owner, c.GetRequest{ID: sid})
	if v.Run.State != c.RunUnknown || !v.Run.Freshness.Stale {
		t.Fatalf("disconnected view: %+v", v.Run)
	}
	m, err := h.st.Hub.SendMessage(owner, c.SendMessageRequest{Mutation: c.Mutation{OperationID: "op_e2e-msg"}, Recipient: c.Address{Kind: c.AddrSession, ID: sid}, Body: c.MessageBody{Text: "Synthetic: run the tests."}})
	if err != nil || m.Status.Storage != c.StorageStored || m.Status.Runtime != c.DeliveryQueued {
		t.Fatalf("send while offline: %v %+v", err, m.Status)
	}

	// Hub restart on the same address.
	h.stop()
	h, _ = serveHub(t, dir, addr)
	defer h.stop()
	if got, err := h.st.Hub.GetMessage(owner, c.GetRequest{ID: m.Envelope.MessageID}); err != nil || got.Status.Storage != c.StorageStored {
		t.Fatalf("message lost across hub restart: %v", err)
	}

	stopLink, linkDone = startLink()
	defer func() { stopLink(); <-linkDone }()
	waitFor(t, "runtime accepted after reconnect", func() bool {
		got, err := h.st.Hub.GetMessage(owner, c.GetRequest{ID: m.Envelope.MessageID})
		return err == nil && got.Status.Runtime == c.DeliveryAccepted
	})
	got, _ := h.st.Hub.GetMessage(owner, c.GetRequest{ID: m.Envelope.MessageID})
	if got.Status.BoundRunID != resp.Session.Run.RunID {
		t.Fatalf("bound to %s", got.Status.BoundRunID)
	}
	d.mu.Lock()
	starts, delivered := d.starts, len(d.delivered)
	d.mu.Unlock()
	if starts != 1 || delivered != 1 {
		t.Fatalf("starts=%d deliveries=%d (want exactly one each)", starts, delivered)
	}
	v, _ = h.st.Hub.GetSession(owner, c.GetRequest{ID: sid})
	if v.Run.State != c.RunWaitingInput || v.Run.Freshness.Stale {
		t.Fatalf("after reconnect: %+v", v.Run)
	}
}
