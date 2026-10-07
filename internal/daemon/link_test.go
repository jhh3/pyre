package daemon

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/mock"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
	"github.com/jhh3/pyre/internal/transport"
)

// linkHub is a minimal hub around the real transport.NodeServer.
type linkHub struct {
	t       *testing.T
	auth    *transport.AuthStore
	cert    *transport.HubCert
	addr    string
	ns      *transport.NodeServer
	srv     *httptest.Server
	mu      sync.Mutex
	reports map[uint64]contracts.NodeReport
	hellos  []contracts.NodeHello
	acks    map[string]bool
}

func newLinkHub(t *testing.T) *linkHub {
	db, err := sqlitex.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	auth, err := transport.NewAuthStore(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := transport.EnsureHubCert(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	h := &linkHub{t: t, auth: auth, cert: cert, reports: map[uint64]contracts.NodeReport{}, acks: map[string]bool{}}
	t.Cleanup(h.stop)
	return h
}

func (h *linkHub) start() {
	h.t.Helper()
	ns, err := transport.NewNodeServer(transport.NodeServerConfig{Auth: h.auth, HubID: "hub_test", HeartbeatInterval: time.Second,
		OnHello: func(_ context.Context, _ string, hello contracts.NodeHello, _ transport.ConnInfo) (transport.WelcomeInfo, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.hellos = append(h.hellos, hello)
			var max uint64
			for s := range h.reports {
				if s > max {
					max = s
				}
			}
			return transport.WelcomeInfo{CommittedReportSeq: max}, nil
		},
		OnReport: func(_ context.Context, _ string, r contracts.NodeReport) error {
			if r.OriginSeq == 0 {
				return nil
			}
			h.mu.Lock()
			h.reports[r.OriginSeq] = r
			h.mu.Unlock()
			return nil
		},
		OnCommandAck: func(_ context.Context, _ string, a contracts.CommandAck) error {
			h.mu.Lock()
			h.acks[a.CommandID] = true
			h.mu.Unlock()
			return nil
		},
	})
	if err != nil {
		h.t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle(contracts.PathNodeConnect, ns)
	addr := h.addr
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	var l net.Listener
	waitFor(h.t, "listen", func() bool { l, err = net.Listen("tcp", addr); return err == nil })
	srv := httptest.NewUnstartedServer(mux)
	srv.Listener = l
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{h.cert.Certificate}}
	srv.StartTLS()
	h.addr, h.ns, h.srv = l.Addr().String(), ns, srv
}

func (h *linkHub) stop() {
	if h.ns != nil {
		h.ns.Close()
		h.srv.Close()
		h.ns, h.srv = nil, nil
	}
}

func (h *linkHub) byKind(kind contracts.ReportKind) []contracts.NodeReport {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []contracts.NodeReport
	for i := uint64(1); i <= uint64(len(h.reports))+100; i++ {
		if r, ok := h.reports[i]; ok && r.Kind == kind {
			out = append(out, r)
		}
	}
	return out
}

func (h *linkHub) send(nodeID string, kind contracts.CommandKind, seq int64, payload any) string {
	h.t.Helper()
	cmd := command(kind, payload)
	cmd.Seq = seq
	waitFor(h.t, "node connected", func() bool { return h.ns != nil && h.ns.Connected(nodeID) })
	if err := h.ns.Send(context.Background(), nodeID, cmd); err != nil {
		h.t.Fatal(err)
	}
	return cmd.CommandID
}

func TestDaemonOverRealNodeLinkSurvivesHubOutage(t *testing.T) {
	hub := newLinkHub(t)
	hub.start()
	nodeID := contracts.NewID(contracts.PrefixNode)
	tok, err := hub.auth.Issue(context.Background(), contracts.PrincipalNode, nodeID, "n1")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	db, err := sqlitex.Open(filepath.Join(dir, "pyred.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ma := mock.NewAdapter()
	d, err := New(Config{StateDir: dir, DB: db, NodeID: nodeID, Adapters: []contracts.RuntimeAdapter{ma},
		Poll: PollConfig{Interval: 20 * time.Millisecond, IdleMax: 100 * time.Millisecond}, HeartbeatInterval: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	tlsCfg, _ := transport.PinnedTLSConfig(hub.cert.Fingerprint)
	link, err := transport.NewNodeLink(transport.NodeLinkConfig{HubURL: "https://" + hub.addr, NodeID: nodeID, Token: tok, TLS: tlsCfg, DB: db,
		Hello: d.Hello, Handler: transport.CommandHandlerFunc(d.HandleCommand),
		Backoff: transport.BackoffConfig{Initial: 20 * time.Millisecond, Max: 200 * time.Millisecond}, StableAfter: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	d.SetReporter(link)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatal(err)
	}
	linkDone := make(chan struct{})
	go func() { link.Run(ctx); close(linkDone) }()
	defer func() { cancel(); <-linkDone; d.Close() }()

	// start_run through the hub command channel.
	cwd := t.TempDir()
	cmdID := hub.send(nodeID, contracts.CmdStartRun, 1, startCmd("ses_h", "run_h", contracts.HarnessMock, cwd))
	waitFor(t, "final start result at hub", func() bool {
		for _, r := range hub.byKind(contracts.ReportCommandResult) {
			var cr contracts.CommandResult
			json.Unmarshal(r.Payload, &cr)
			if cr.CommandID == cmdID && cr.Outcome == contracts.OutcomeSucceeded {
				return true
			}
		}
		return false
	})
	waitFor(t, "run observed waiting_input at hub", func() bool {
		for _, r := range hub.byKind(contracts.ReportRun) {
			var run contracts.Run
			json.Unmarshal(r.Payload, &run)
			if run.RunID == "run_h" && run.State == contracts.RunWaitingInput {
				return true
			}
		}
		return false
	})

	// Hub outage: the run keeps going and local state keeps working.
	hub.stop()
	waitFor(t, "link down", func() bool { return !link.Connected() })
	ma.SetState("run_h", contracts.RunRunning)
	waitFor(t, "local observation during outage", func() bool {
		return d.Diagnostics(ctx).Resources.ActiveRuns == 1 && runState(d, "run_h") == contracts.RunRunning
	})
	before := len(hub.byKind(contracts.ReportRun))

	// Hub returns: queued reports flush; Hello carries LiveRuns.
	hub.start()
	waitFor(t, "reconnect", link.Connected)
	waitFor(t, "outage-time run report delivered", func() bool { return len(hub.byKind(contracts.ReportRun)) > before })
	hub.mu.Lock()
	last := hub.hellos[len(hub.hellos)-1]
	hub.mu.Unlock()
	if len(last.LiveRuns) != 1 || last.LiveRuns[0].RunID != "run_h" {
		t.Fatalf("reconnect hello live runs = %+v", last.LiveRuns)
	}

	// Deliver through the hub: receipts reach the hub.
	ma.SetState("run_h", contracts.RunWaitingInput)
	d.PollNow(ctx, "run_h")
	hub.send(nodeID, contracts.CmdDeliver, 2, contracts.DeliverCommand{SessionID: "ses_h", Message: contracts.MessageEnvelope{SchemaVersion: 1,
		MessageID: "msg_h", OperationID: "op_h", Sender: contracts.Principal{Kind: contracts.PrincipalOwner, ID: "o"},
		Recipient: contracts.Address{Kind: contracts.AddrSession, ID: "ses_h"}, Mode: contracts.ModeQueue, Body: contracts.MessageBody{Text: "hi"}}})
	waitFor(t, "accepted receipt at hub", func() bool {
		for _, r := range hub.byKind(contracts.ReportReceipt) {
			var rc contracts.DeliveryReceipt
			json.Unmarshal(r.Payload, &rc)
			if rc.MessageID == "msg_h" && rc.State == contracts.DeliveryAccepted && rc.RunID == "run_h" {
				return true
			}
		}
		return false
	})
}

func runState(d *Daemon, runID string) contracts.RunState {
	if lr, ok := d.getLive(runID); ok {
		r, _ := lr.snapshot()
		return r.Run.State
	}
	return ""
}
