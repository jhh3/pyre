package assemble

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/client"
	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/daemon"
	"github.com/jhh3/pyre/internal/runtime/mock"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
	"github.com/jhh3/pyre/internal/transport"
)

// Interrupted children end to end (issue iss_…):
// a real hub, node transport and daemon over the mock runtime. A lead
// session starts a child; the machine reboots under the child (its process
// is gone, and the machine booted after the run started). The restarted
// pyred resumes the child natively, and the lead gets exactly one
// interrupted report naming the machine, the cause, the resume and what
// was lost, and no "has exited" report. A later plain pyred restart, with
// every process alive, sends the lead nothing.
func TestInterruptedChildReportEndToEnd(t *testing.T) {
	base := t.TempDir()
	h, addr := serveHub(t, filepath.Join(base, "hub"), "127.0.0.1:0")
	defer h.stop()
	owner := c.WithPrincipal(context.Background(), c.Principal{Kind: c.PrincipalOwner, ID: "cli_synth-interrupted"})
	tk, err := h.st.Fleet.CreateEnrollment(owner, c.EnrollmentRequest{Mutation: c.Mutation{OperationID: c.NewID(c.PrefixOperation)}, Name: "synth-rebooter"})
	if err != nil {
		t.Fatal(err)
	}
	cred, err := h.st.Enrollment.RedeemNode(context.Background(), c.NodeEnrollExchange{NodeID: tk.NodeID, Secret: tk.Secret, Hostname: "synth-rebooter", Platform: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	state, err := os.MkdirTemp("/tmp", "pyre-interrupted-")
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
	ad := mock.NewAdapter()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	var boot atomic.Value
	boot.Store(time.Now().Add(-time.Hour)) // booted before anything here started
	startPyred := func() func() {
		d, err := daemon.New(daemon.Config{StateDir: state, DB: db, NodeID: cred.NodeID, NodeName: "synth-rebooter", HeartbeatInterval: time.Hour,
			Adapters: []c.RuntimeAdapter{ad}, Poll: daemon.PollConfig{Interval: 20 * time.Millisecond, IdleMax: 100 * time.Millisecond},
			Logger: quiet, NodeToken: cred.Token, BootTime: func() (time.Time, error) { return boot.Load().(time.Time), nil },
			Upstream: func(bearer string) c.API {
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
		return func() { cancel(); <-done; d.Close() }
	}
	stop := startPyred()

	view := func(sid string) c.SessionView {
		v, err := h.st.Hub.GetSession(owner, c.GetRequest{ID: sid})
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	idle := func(sid string) bool { v := view(sid); return v.Run != nil && v.Run.State == c.RunWaitingInput }
	pr, err := h.st.Hub.StartSession(owner, c.StartSessionRequest{Mutation: c.Mutation{OperationID: c.NewID(c.PrefixOperation)}, NodeID: cred.NodeID,
		Harness: c.HarnessMock, Cwd: state, Title: "synthetic lead"})
	if err != nil {
		t.Fatal(err)
	}
	lead := pr.Session.Session.SessionID
	waitFor(t, "lead idle", func() bool { return idle(lead) })
	asLead := c.WithPrincipal(context.Background(), c.Principal{Kind: c.PrincipalSession, ID: lead})
	cr, err := h.st.Hub.StartSession(asLead, c.StartSessionRequest{Mutation: c.Mutation{OperationID: c.NewID(c.PrefixOperation)}, NodeID: cred.NodeID,
		Harness: c.HarnessMock, Cwd: state, Title: "synthetic worker"})
	if err != nil {
		t.Fatal(err)
	}
	child := cr.Session.Session.SessionID
	waitFor(t, "worker idle", func() bool { return idle(child) })
	oldRun := view(child).Run.RunID

	// Reports the lead received about the worker, by text.
	reports := func() []string {
		page, err := h.st.Hub.ListMessages(owner, c.ListMessagesRequest{Box: "inbox", Address: &c.Address{Kind: c.AddrSession, ID: lead}})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, m := range page.Items {
			if strings.Contains(m.Envelope.Body.Text, child) {
				out = append(out, m.Envelope.Body.Text)
			}
		}
		return out
	}
	interrupted := func() []string {
		var out []string
		for _, r := range reports() {
			if strings.Contains(r, "was interrupted") {
				out = append(out, r)
			}
		}
		return out
	}

	// The machine reboots under the worker: pyred stops, the worker's
	// process dies, and the machine boots (now) after the run started.
	time.Sleep(2500 * time.Millisecond) // the boot must clear the run's start by bootSkew
	stop()
	ad.Vanish(oldRun)
	boot.Store(time.Now())
	stop = startPyred()

	waitFor(t, "worker resumed", func() bool {
		v := view(child)
		return v.Run != nil && v.Run.RunID != oldRun && v.Run.PreviousRunID == oldRun && !v.Run.State.Terminal()
	})
	newRun := view(child).Run.RunID
	waitFor(t, "lead told of the interruption", func() bool { return len(interrupted()) > 0 })
	got := interrupted()[0]
	for _, want := range []string{"synthetic worker was interrupted: synth-rebooter restarted; Pyre resumed it", "Cause: machine restarted",
		"resumed it as a new run " + newRun, "Lost: tool calls that were in flight and its background jobs"} {
		if !strings.Contains(got, want) {
			t.Fatalf("interrupted report lacks %q:\n%s", want, got)
		}
	}
	if v := view(lead); v.Run == nil || v.Run.State.Terminal() {
		t.Fatalf("the lead's own run did not survive: %+v", v.Run)
	}

	// A plain pyred restart: every process survives. Nothing more.
	waitFor(t, "worker idle after its resume note", func() bool { return idle(child) })
	time.Sleep(300 * time.Millisecond) // the resume note's turn reports settle
	before := len(reports())
	stop()
	stop = startPyred()
	defer stop()
	waitFor(t, "worker still current and live", func() bool { v := view(child); return v.Run != nil && v.Run.RunID == newRun && !v.Run.State.Terminal() })
	time.Sleep(500 * time.Millisecond)
	if n := len(interrupted()); n != 1 {
		t.Fatalf("lead got %d interrupted reports, want exactly 1:\n%s", n, strings.Join(interrupted(), "\n---\n"))
	}
	if n := len(reports()); n != before {
		t.Fatalf("a plain pyred restart sent the lead %d more reports:\n%s", n-before, strings.Join(reports(), "\n---\n"))
	}
	for _, r := range reports() {
		if strings.Contains(r, "has exited: run process not found") {
			t.Fatalf("an ended report went out besides the interrupted one:\n%s", r)
		}
	}
}
