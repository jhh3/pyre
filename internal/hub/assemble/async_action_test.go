package assemble

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/daemon"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
	"github.com/jhh3/pyre/internal/transport"
)

// Async node actions (daemon.RegisterAction) acknowledge first and report
// the final result later as a second CommandResult. Through the real hub,
// node transport and daemon: InvokeNodeAction returns the final result when
// it arrives within the timeout; otherwise pending/delivered with the
// command ID, and commands.get (and a replay of the same operation) show
// the final result once it is reported.
func TestAsyncNodeActionFinalResultEndToEnd(t *testing.T) {
	base := t.TempDir()
	h, addr := serveHub(t, filepath.Join(base, "hub"), "127.0.0.1:0")
	defer h.stop()
	owner := c.WithPrincipal(context.Background(), c.Principal{Kind: c.PrincipalOwner, ID: "cli_synth-async"})
	tk, err := h.st.Fleet.CreateEnrollment(owner, c.EnrollmentRequest{Mutation: c.Mutation{OperationID: c.NewID(c.PrefixOperation)}, Name: "synth-async"})
	if err != nil {
		t.Fatal(err)
	}
	cred, err := h.st.Enrollment.RedeemNode(context.Background(), c.NodeEnrollExchange{NodeID: tk.NodeID, Secret: tk.Secret, Hostname: "synth-async", Platform: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	state, err := os.MkdirTemp("/tmp", "pyre-async-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(state) })
	db, err := sqlitex.Open(filepath.Join(state, "pyred.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	d, err := daemon.New(daemon.Config{StateDir: state, DB: db, NodeID: cred.NodeID, HeartbeatInterval: time.Hour, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	d.RegisterAction("synth.fast", func(ctx context.Context, p json.RawMessage) (any, error) {
		return map[string]any{"answer": 42}, nil
	})
	d.RegisterAction("synth.slow", func(ctx context.Context, p json.RawMessage) (any, error) {
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return map[string]any{"answer": "slow"}, nil
	})
	d.RegisterAction("synth.fail", func(ctx context.Context, p json.RawMessage) (any, error) {
		return nil, c.Errorf(c.ErrInvalid, "synthetic refusal")
	})
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
	defer func() { close(releaseOnce(release)); cancel(); <-linkDone; d.Close() }()
	waitFor(t, "node connected", func() bool { return h.st.NodeServer.Connected(cred.NodeID) })

	invoke := func(action, op string, timeout int) c.NodeActionResult {
		t.Helper()
		res, err := h.st.Hub.InvokeNodeAction(owner, c.NodeActionRequest{Mutation: c.Mutation{OperationID: op}, NodeID: cred.NodeID, Action: action, TimeoutSeconds: timeout})
		if err != nil {
			t.Fatalf("%s: %v", action, err)
		}
		return res
	}
	fast := invoke("synth.fast", "op_synth-fast", 5)
	if fast.Outcome != c.OutcomeSucceeded || string(fast.Result) != `{"answer":42}` {
		t.Fatalf("fast async action: %+v (result %s)", fast, fast.Result)
	}
	failed := invoke("synth.fail", "op_synth-fail", 5)
	if failed.Outcome != c.OutcomeFailed || failed.Error == nil || failed.Error.Code != c.ErrInvalid {
		t.Fatalf("failing async action: %+v", failed)
	}

	slow := invoke("synth.slow", "op_synth-slow", 1)
	if slow.Outcome == c.OutcomeSucceeded || slow.CommandID == "" {
		t.Fatalf("slow action finished early: %+v", slow)
	}
	got, err := h.st.Hub.GetCommandResult(owner, c.GetRequest{ID: slow.CommandID})
	if err != nil || got.Outcome == c.OutcomeSucceeded {
		t.Fatalf("commands.get before completion: %+v %v", got, err)
	}
	close(release)
	waitFor(t, "final result via commands.get", func() bool {
		got, err = h.st.Hub.GetCommandResult(owner, c.GetRequest{ID: slow.CommandID})
		return err == nil && got.Outcome == c.OutcomeSucceeded
	})
	if string(got.Result) != `{"answer":"slow"}` {
		t.Fatalf("final result %s", got.Result)
	}
	if again := invoke("synth.slow", "op_synth-slow", 1); again.CommandID != slow.CommandID || again.Outcome != c.OutcomeSucceeded || string(again.Result) != `{"answer":"slow"}` {
		t.Fatalf("replay after completion: %+v", again)
	}
	// Other principals cannot read the owner's command.
	sess := c.WithPrincipal(context.Background(), c.Principal{Kind: c.PrincipalSession, ID: "ses_synth-other"})
	if _, err := h.st.Hub.GetCommandResult(sess, c.GetRequest{ID: slow.CommandID}); !c.IsCode(err, c.ErrForbidden) {
		t.Fatalf("session reading another principal's command: %v", err)
	}
}

// releaseOnce returns a channel safe to close in defer whether or not the
// test closed release already.
func releaseOnce(release chan struct{}) chan struct{} {
	select {
	case <-release:
		return make(chan struct{})
	default:
		return release
	}
}
