//go:build !windows

package stack

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/jhh3/pyre/internal/client"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/daemon"
	"github.com/jhh3/pyre/internal/enrollment"
	"github.com/jhh3/pyre/internal/runtime/mock"
	"github.com/jhh3/pyre/internal/runtime/shell"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
	"github.com/jhh3/pyre/internal/transport"
)

// RoleEnv selects the test-binary daemon role.
const RoleEnv = "PYRE_RESILIENCE_ROLE"

// RoleDaemon is the value of RoleEnv for RunTestDaemon.
const RoleDaemon = "pyred-test"

// CrashMarker in a delivered message makes the test daemon SIGKILL itself
// right after the runtime accepted the input and before the daemon can
// record the receipt (the A06 crash window).
const CrashMarker = "[wp18:crash-after-submit]"

// TmuxSocket is where the test daemon's shell adapter keeps its isolated
// tmux server.
func TmuxSocket(state string) string { return filepath.Join(state, "tmux", "pyre.sock") }

// RunTestDaemon mirrors cmd/pyred `run` wiring (daemon + NodeLink + local
// socket) and additionally registers the WP07 shell adapter on an isolated
// tmux socket (--shell) and/or the mock adapter (--mock, with the
// crash-after-submit hook). Scenarios that do not need the crash hook use
// the real pyred instead (it registers the native shell adapter).
// args are the arguments after the program name, starting with "run".
func RunTestDaemon(args []string) int {
	if len(args) == 0 || args[0] != "run" {
		fmt.Fprintln(os.Stderr, "pyred-test: expected 'run'")
		return 2
	}
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	state := fs.String("state", "", "")
	hub := fs.String("hub", "", "")
	credFile := fs.String("credential", "", "")
	ownerCred := fs.String("owner-credential", "", "")
	name := fs.String("name", "", "")
	_ = fs.Bool("no-integrations", true, "")
	withShell := fs.Bool("shell", false, "")
	withMock := fs.Bool("mock", false, "")
	_ = fs.Bool("dev-mock", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if err := run(*state, *hub, *credFile, *ownerCred, *name, *withShell, *withMock); err != nil {
		fmt.Fprintln(os.Stderr, "pyred-test:", err)
		return 1
	}
	return 0
}

// crashingMock wraps the mock adapter: a delivery whose text contains
// CrashMarker is submitted to the runtime, then the process dies.
type crashingMock struct{ *mock.Adapter }

func (c crashingMock) Deliver(ctx context.Context, h contracts.RunHandle, in contracts.DeliveryInput) (contracts.DeliveryResult, error) {
	res, err := c.Adapter.Deliver(ctx, h, in)
	if strings.Contains(in.Text, CrashMarker) {
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		select {}
	}
	return res, err
}

func run(state, hubURL, credFile, ownerCredPath, name string, withShell, withMock bool) error {
	if err := os.MkdirAll(state, 0o700); err != nil {
		return err
	}
	logger := transport.NewRedactingLogger(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cred, err := enrollment.LoadNodeCredential(credFile)
	if err != nil {
		return err
	}
	if hubURL == "" {
		hubURL = cred.HubURL
	}
	owner, err := enrollment.LoadClientCredential(ownerCredPath)
	if err != nil {
		return err
	}
	dbPath := filepath.Join(state, "pyred.db")
	db, err := sqlitex.Open(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		_ = os.Chmod(p, 0o600) // as cmd/pyred does (sqlitex.Open honours the umask)
	}
	var adapters []contracts.RuntimeAdapter
	if withShell {
		if err := os.MkdirAll(filepath.Dir(TmuxSocket(state)), 0o700); err != nil {
			return err
		}
		sa, err := shell.New(shell.Options{NodeID: cred.NodeID, TmuxSocket: TmuxSocket(state), Shell: "/bin/sh"})
		if err != nil {
			return err
		}
		adapters = append(adapters, sa)
	}
	if withMock {
		adapters = append(adapters, crashingMock{mock.NewAdapter()})
	}
	pin := cred.HubCertSHA256
	cfg := daemon.Config{StateDir: state, DB: db, NodeID: cred.NodeID, NodeName: name, OwnerToken: owner.Token, Logger: logger, Adapters: adapters,
		Upstream: func(bearer string) contracts.API {
			return client.New(hubURL, bearer, client.Options{CertSHA256: pin, UserAgent: "pyred-test-proxy"})
		}}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	d, err := daemon.New(cfg)
	if err != nil {
		return err
	}
	tlsCfg, err := transport.ClientTLSConfig(pin)
	if err != nil {
		return err
	}
	link, err := transport.NewNodeLink(transport.NodeLinkConfig{HubURL: hubURL, NodeID: cred.NodeID, Token: cred.Token, TLS: tlsCfg, DB: db,
		Logger: logger.With("component", "nodelink"), Hello: d.Hello, Handler: transport.CommandHandlerFunc(d.HandleCommand),
		RecoverCommand: d.RecoverCommand,
		Backoff:        transport.BackoffConfig{Initial: 100 * 1e6, Max: 2 * 1e9},
		OnStateChange: func(s transport.LinkStatus) {
			if s.State == transport.LinkConnected {
				d.KickOutbox()
			}
		}})
	if err != nil {
		return err
	}
	d.SetReporter(link)
	if err := d.Start(ctx); err != nil {
		return err
	}
	errc := make(chan error, 2)
	go func() { errc <- d.ServeLocal(ctx) }()
	go func() { errc <- link.Run(ctx) }()
	logger.Info("pyred-test running", "node", d.NodeID(), "shell", withShell, "mock", withMock)
	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errc:
		cancel()
	}
	d.Close()
	return runErr
}
