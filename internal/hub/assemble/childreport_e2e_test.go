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

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/daemon"
	"github.com/jhh3/pyre/internal/runtime/mock"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
	"github.com/jhh3/pyre/internal/transport"
)

// W-childnotify end to end: a real hub, node transport and daemon with the
// mock runtime. An owner session (the parent) starts a child with a prompt;
// the child works a turn and goes idle; the parent receives exactly one
// Pyre message from the child carrying its last assistant message, and the
// message is accepted by the parent's runtime (the normal delivery path).
func TestChildReportsToParentEndToEnd(t *testing.T) {
	base := t.TempDir()
	h, addr := serveHub(t, filepath.Join(base, "hub"), "127.0.0.1:0")
	defer h.stop()
	owner := c.WithPrincipal(context.Background(), c.Principal{Kind: c.PrincipalOwner, ID: "cli_synth-childnotify"})
	tk, err := h.st.Fleet.CreateEnrollment(owner, c.EnrollmentRequest{Mutation: c.Mutation{OperationID: c.NewID(c.PrefixOperation)}, Name: "synth-childnotify"})
	if err != nil {
		t.Fatal(err)
	}
	cred, err := h.st.Enrollment.RedeemNode(context.Background(), c.NodeEnrollExchange{NodeID: tk.NodeID, Secret: tk.Secret, Hostname: "synth-childnotify", Platform: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	state, err := os.MkdirTemp("/tmp", "pyre-childnotify-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(state) })
	db, err := sqlitex.Open(filepath.Join(state, "pyred.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ad := mock.NewAdapter(mock.AdapterOptions{BusyObserves: 2})
	d, err := daemon.New(daemon.Config{StateDir: state, DB: db, NodeID: cred.NodeID, HeartbeatInterval: time.Hour,
		Adapters: []c.RuntimeAdapter{ad}, Poll: daemon.PollConfig{Interval: 20 * time.Millisecond, IdleMax: 100 * time.Millisecond},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	tlsCfg, err := transport.PinnedTLSConfig(h.st.Cert.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	link, err := transport.NewNodeLink(transport.NodeLinkConfig{HubURL: "https://" + addr, NodeID: cred.NodeID, Token: cred.Token, TLS: tlsCfg, DB: db,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Hello: d.Hello, Handler: transport.CommandHandlerFunc(d.HandleCommand),
		Backoff: transport.BackoffConfig{Initial: 20 * time.Millisecond, Max: 200 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	d.SetReporter(link)
	ctx, cancel := context.WithCancel(context.Background())
	if err := d.Start(ctx); err != nil {
		t.Fatal(err)
	}
	linkDone := make(chan struct{})
	go func() { link.Run(ctx); close(linkDone) }()
	defer func() { cancel(); <-linkDone; d.Close() }()
	waitFor(t, "node connected", func() bool { return h.st.NodeServer.Connected(cred.NodeID) })

	state0 := func(sid string) c.RunState {
		v, err := h.st.Hub.GetSession(owner, c.GetRequest{ID: sid})
		if err != nil || v.Run == nil {
			return ""
		}
		return v.Run.State
	}
	pr, err := h.st.Hub.StartSession(owner, c.StartSessionRequest{Mutation: c.Mutation{OperationID: "op_synth-cn-parent"}, NodeID: cred.NodeID,
		Harness: c.HarnessMock, Cwd: state, Title: "synthetic parent"})
	if err != nil {
		t.Fatal(err)
	}
	parent := pr.Session.Session.SessionID
	waitFor(t, "parent idle", func() bool { return state0(parent) == c.RunWaitingInput })

	asParent := c.WithPrincipal(context.Background(), c.Principal{Kind: c.PrincipalSession, ID: parent})
	cr, err := h.st.Hub.StartSession(asParent, c.StartSessionRequest{Mutation: c.Mutation{OperationID: "op_synth-cn-child"}, NodeID: cred.NodeID,
		Harness: c.HarnessMock, Cwd: state, Title: "synthetic child", InitialPrompt: "Synthetic: run the tests and report to owner."})
	if err != nil {
		t.Fatal(err)
	}
	child := cr.Session.Session.SessionID
	if cr.Session.Session.Lineage.ParentSessionID != parent || !strings.Contains(strings.Join(cr.Session.Session.Tags, ","), c.TagReportParent) {
		t.Fatalf("child lineage/tags: %+v", cr.Session.Session)
	}

	fromChild := func() []c.MessageView {
		page, err := h.st.Hub.ListMessages(owner, c.ListMessagesRequest{Box: "inbox", Address: &c.Address{Kind: c.AddrSession, ID: parent}})
		if err != nil {
			t.Fatal(err)
		}
		var out []c.MessageView
		for _, m := range page.Items {
			if m.Envelope.Sender.Kind == c.PrincipalSession && m.Envelope.Sender.ID == child {
				out = append(out, m)
			}
		}
		return out
	}
	waitFor(t, "report delivered to the parent", func() bool {
		ms := fromChild()
		return len(ms) == 1 && ms[0].Status.Runtime == c.DeliveryAccepted
	})
	m := fromChild()[0]
	for _, want := range []string{"finished its turn", "synthetic child", child, "Done (synthetic mock turn).", "synth-childnotify"} {
		if !strings.Contains(m.Envelope.Body.Text, want) {
			t.Fatalf("report lacks %q:\n%s", want, m.Envelope.Body.Text)
		}
	}
	if state0(child) != c.RunWaitingInput {
		t.Fatalf("child state %s", state0(child))
	}
	// The parent works the delivered report and goes idle; nothing more
	// arrives (no second report, no report about the owner-started parent).
	waitFor(t, "parent idle after the report", func() bool { return state0(parent) == c.RunWaitingInput })
	time.Sleep(500 * time.Millisecond)
	if n := len(fromChild()); n != 1 {
		t.Fatalf("parent received %d reports, want exactly 1", n)
	}
	page, err := h.st.Hub.ListMessages(owner, c.ListMessagesRequest{Box: "inbox", Address: &c.Address{Kind: c.AddrOwner, ID: "owner"}})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("owner inbox: %v %d", err, len(page.Items))
	}
}
