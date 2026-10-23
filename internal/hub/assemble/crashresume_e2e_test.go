package assemble

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/client"
	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/daemon"
	"github.com/jhh3/pyre/internal/resumenote"
	"github.com/jhh3/pyre/internal/runtime/mock"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
	"github.com/jhh3/pyre/internal/transport"
)

// Crash resume end to end: a real hub, node transport and daemon (with the
// local API proxy to the hub) over the mock runtime. The session's process
// dies while pyred is down; the restarted pyred resumes it natively by
// itself, the hub makes the new run current (an ordinary resume with
// lineage), and the session receives the crash note through the hub's
// normal message path, accepted by its new run's runtime.
func TestCrashResumeEndToEnd(t *testing.T) {
	base := t.TempDir()
	h, addr := serveHub(t, filepath.Join(base, "hub"), "127.0.0.1:0")
	defer h.stop()
	owner := c.WithPrincipal(context.Background(), c.Principal{Kind: c.PrincipalOwner, ID: "cli_synth-crashresume"})
	tk, err := h.st.Fleet.CreateEnrollment(owner, c.EnrollmentRequest{Mutation: c.Mutation{OperationID: c.NewID(c.PrefixOperation)}, Name: "synth-crashresume"})
	if err != nil {
		t.Fatal(err)
	}
	cred, err := h.st.Enrollment.RedeemNode(context.Background(), c.NodeEnrollExchange{NodeID: tk.NodeID, Secret: tk.Secret, Hostname: "synth-crashresume", Platform: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	state, err := os.MkdirTemp("/tmp", "pyre-crashresume-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(state) })
	db, err := sqlitex.Open(filepath.Join(state, "pyred.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tlsCfg, err := transport.PinnedTLSConfig(h.st.Cert.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	ad := mock.NewAdapter() // in-process: outlives the pyred restarts below
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	// startPyred runs a daemon and its hub link over the shared state; the
	// returned func stops both (the runs' processes are the adapter's).
	startPyred := func() (*daemon.Daemon, func()) {
		d, err := daemon.New(daemon.Config{StateDir: state, DB: db, NodeID: cred.NodeID, HeartbeatInterval: time.Hour,
			Adapters: []c.RuntimeAdapter{ad}, Poll: daemon.PollConfig{Interval: 20 * time.Millisecond, IdleMax: 100 * time.Millisecond},
			Logger: quiet, NodeToken: cred.Token, Upstream: func(bearer string) c.API {
				return client.New("https://"+addr, bearer, client.Options{CertSHA256: h.st.Cert.Fingerprint})
			}})
		if err != nil {
			t.Fatal(err)
		}
		link, err := transport.NewNodeLink(transport.NodeLinkConfig{HubURL: "https://" + addr, NodeID: cred.NodeID, Token: cred.Token, TLS: tlsCfg, DB: db,
			Logger: quiet, Hello: d.Hello, Handler: transport.CommandHandlerFunc(d.HandleCommand),
			Backoff: transport.BackoffConfig{Initial: 20 * time.Millisecond, Max: 200 * time.Millisecond}})
		if err != nil {
			t.Fatal(err)
		}
		d.SetReporter(link)
		ctx, cancel := context.WithCancel(context.Background())
		if err := d.Start(ctx); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { link.Run(ctx); close(done) }()
		waitFor(t, "node connected", func() bool { return h.st.NodeServer.Connected(cred.NodeID) })
		return d, func() { cancel(); <-done; d.Close() }
	}
	_, stop := startPyred()

	view := func(sid string) c.SessionView {
		v, err := h.st.Hub.GetSession(owner, c.GetRequest{ID: sid})
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	start := func(title string) (string, string) {
		sr, err := h.st.Hub.StartSession(owner, c.StartSessionRequest{Mutation: c.Mutation{OperationID: c.NewID(c.PrefixOperation)}, NodeID: cred.NodeID,
			Harness: c.HarnessMock, Cwd: state, Title: title})
		if err != nil {
			t.Fatal(err)
		}
		id := sr.Session.Session.SessionID
		waitFor(t, title+" idle", func() bool { v := view(id); return v.Run != nil && v.Run.State == c.RunWaitingInput })
		return id, view(id).Run.RunID
	}
	// Session IDs sort in creation order and roots resume in ID order: the
	// victim, started last, is resumed after the other two were handled.
	archived, archivedRun := start("synthetic archived while down")
	stopped, stoppedRun := start("synthetic stopped while down")
	sid, oldRun := start("synthetic crash victim")

	// pyred dies and the sessions' processes die with it. While the node is
	// down the owner archives one session (its session token is revoked) and
	// stops another (the stop is queued for the dead run).
	stop()
	for _, r := range []string{oldRun, archivedRun, stoppedRun} {
		ad.Vanish(r)
	}
	yes := true
	if _, err := h.st.Hub.UpdateSession(owner, c.UpdateSessionRequest{Mutation: c.Mutation{OperationID: c.NewID(c.PrefixOperation)}, SessionID: archived, Archived: &yes}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.Hub.StopSession(owner, c.SessionCommandRequest{Mutation: c.Mutation{OperationID: c.NewID(c.PrefixOperation)}, SessionID: stopped}); err != nil {
		t.Fatal(err)
	}
	_, stop = startPyred()
	defer stop()

	var cur c.Run
	waitFor(t, "hub sees the crash resume", func() bool {
		v := view(sid)
		if v.Run == nil || v.Run.RunID == oldRun {
			return false
		}
		cur = *v.Run
		return cur.State == c.RunWaitingInput || cur.State == c.RunRunning
	})
	if cur.PreviousRunID != oldRun || cur.ContinuationKind != c.ContinuationNativeResume {
		t.Fatalf("resumed run = %+v", cur)
	}
	runs, err := h.st.Hub.ListRuns(owner, c.ListRunsRequest{SessionID: sid})
	if err != nil {
		t.Fatal(err)
	}
	var old *c.Run
	for i := range runs.Items {
		if runs.Items[i].RunID == oldRun {
			old = &runs.Items[i]
		}
	}
	if len(runs.Items) != 2 || old == nil || old.State != c.RunExited || !strings.Contains(old.StateReason, "exit not observed") {
		t.Fatalf("runs = %+v", runs.Items)
	}

	id := resumenote.MessageID(sid, cur.RunID)
	var m c.MessageView
	waitFor(t, "crash note accepted by the resumed run", func() bool {
		m, err = h.st.Hub.GetMessage(owner, c.GetRequest{ID: id})
		return err == nil && m.Status.Runtime == c.DeliveryAccepted
	})
	if !strings.Contains(m.Envelope.Body.Text, "Pyre restarted after a crash") || m.Envelope.Recipient.ID != sid {
		t.Fatalf("note = %+v", m.Envelope)
	}

	// The archived session stays ended; the stopped one ends up stopped
	// (its resume cancelled, or the resumed run stopped).
	waitFor(t, "stopped session has no live run", func() bool {
		v := view(stopped)
		return v.Run != nil && v.Run.State.Terminal()
	})
	if v := view(archived); v.Run == nil || v.Run.RunID != archivedRun {
		t.Fatalf("archived session resumed: %+v", v.Run)
	}
	if v := view(stopped); v.Run == nil || !v.Run.State.Terminal() {
		t.Fatalf("stopped session live again: %+v", v.Run)
	}
}
