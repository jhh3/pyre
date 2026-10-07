package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/jhh3/pyre/internal/accounts"
	"github.com/jhh3/pyre/internal/agentguide"
	"github.com/jhh3/pyre/internal/buildinfo"
	"github.com/jhh3/pyre/internal/client"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/daemon"
	"github.com/jhh3/pyre/internal/enrollment"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
	"github.com/jhh3/pyre/internal/supervisor"
	"github.com/jhh3/pyre/internal/terminal"
	"github.com/jhh3/pyre/internal/transport"
	"github.com/jhh3/pyre/internal/updates"
)

type runOpts struct {
	state, hub, credential, ownerCred, name, sshAlias, logLevel string
	maxRuns                                                     int
	devMock, noIntegrations, noDoorbell                         bool
	trustDirs                                                   []string
	rotaCheckout                                                bool
	rotaIndependent                                             string
	rotaHome                                                    string
	credBackend, credKeychain                                   string
	accountsTestMode                                            bool
	permissions, defaultAccount                                 string
	toolPaths                                                   []string
	envHome                                                     string
	devClockOffset                                              time.Duration
	devCommandRetention                                         time.Duration
	// Background work limits (A20): see docs/operations.md.
	hiveCaptureQueue, hiveBackgroundSlots, readCacheQueue int
}

// noIntegrationsReason explains the node actions that pyred run
// --no-integrations leaves out (pyre update, pyre env, accounts,
// connections, memory).
const noIntegrationsReason = "pyred runs with --no-integrations, which leaves out accounts, connections, environment profiles (pyre env), tool updates (pyre update) and Hive/Outfit; restart the node without that flag to use them"

func cmdRun(args []string) error {
	var o runOpts
	fs := runFlags(&o)
	if err := fs.Parse(args); err != nil {
		return err
	}
	err := runDaemon(o)
	var ex *execRequest
	if errors.As(err, &ex) {
		return doExec(ex) // every deferred close in runDaemon has run
	}
	return err
}

// runFlags defines `pyred run` flags into o.
func runFlags(o *runOpts) *flag.FlagSet {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.StringVar(&o.state, "state", daemon.DefaultStateDir(), "state directory (db, socket, runs, logs)")
	fs.StringVar(&o.hub, "hub", "", "hub URL (overrides the credential file)")
	fs.StringVar(&o.credential, "credential", "", "node credential file (default <state>/node-credential.json if present)")
	fs.StringVar(&o.ownerCred, "owner-credential", "", "owner client credential used for local socket requests without a token")
	fs.StringVar(&o.name, "name", "", "node display name")
	fs.StringVar(&o.sshAlias, "ssh-alias", "", "ssh alias other machines use to reach this node (remote attach)")
	fs.IntVar(&o.maxRuns, "max-runs", 0, "advertised run capacity (0 = unlimited)")
	fs.StringVar(&o.logLevel, "log-level", "info", "debug|info|warn|error")
	fs.BoolVar(&o.devMock, "dev-mock", false, "register the deterministic mock runtime adapter (development only)")
	fs.DurationVar(&o.devClockOffset, "dev-clock-offset", 0, "TESTING ONLY: shift the daemon and hub-link clock by this much (simulates node clock skew)")
	fs.DurationVar(&o.devCommandRetention, "dev-command-retention", 0, "TESTING ONLY: keep finished hub-link command records this long instead of 30 days (soak checks of the hourly prune)")
	fs.Func("trust-dir", "directory whose workspaces Claude runs may trust automatically (repeatable; default none)", func(v string) error {
		o.trustDirs = append(o.trustDirs, v)
		return nil
	})
	fs.Func("tool-path", "extra directory to search for tmux/claude/codex/node (repeatable; also PYRE_TOOL_PATH)", func(v string) error {
		o.toolPaths = append(o.toolPaths, v)
		return nil
	})
	fs.IntVar(&o.hiveCaptureQueue, "hive-capture-queue", 32, "background limit: transcripts that may wait for Hive capture (more are dropped; Hive's sweep and the next capture pick them up)")
	fs.IntVar(&o.hiveBackgroundSlots, "hive-background-slots", 1, "background limit: concurrent background Hive subprocesses (capture + note bridge); interactive memory calls never wait for a slot")
	fs.IntVar(&o.readCacheQueue, "read-cache-queue", 256, "background limit: hub reads waiting to be written to the node read cache (more are not cached)")
	fs.BoolVar(&o.noIntegrations, "no-integrations", false, "do not wire the integrations: Hive memory, Outfit plugins, accounts and rota checkout, connections, environment profiles (pyre env) and tool updates (pyre update); their node actions answer unsupported. Runs, messages, attach, documents and artifacts keep working")
	fs.BoolVar(&o.noDoorbell, "no-doorbell", false, "do not give Claude runs the inbox doorbell monitor (input.monitor; also "+EnvDoorbell+"=0)")
	fs.StringVar(&o.envHome, "env-home", "", "home whose .claude/.codex the environment manager adopts from, materializes managed home items into and checks for drift (default $HOME; isolated test nodes)")
	fs.BoolVar(&o.rotaCheckout, "rota-checkout", false, "run sessions on rota's accounts by checking snapshots out into isolated profiles (requires the lock-aware rota; hub fleet lease)")
	fs.StringVar(&o.permissions, "permissions", daemon.PermissionsAsk, "default permission mode of every Pyre-launched run on this node: ask (harness approval prompts, relayed by Pyre) or bypass (Claude --dangerously-skip-permissions, Codex approval never + danger-full-access sandbox); pyre start --permissions overrides it per launch")
	fs.StringVar(&o.defaultAccount, "default-account", accounts.DefaultAccountAuto, "account for launches without an explicit account: auto (eligible Pyre subscription when this node has Pyre accounts for the provider, else your own login), subscription (always an eligible Pyre subscription; waits when exhausted) or ambient (always your own login)")
	fs.StringVar(&o.rotaHome, "rota-home", "", "home whose rota snapshots (.claude/accounts, .codex/accounts) and live default logins (.claude.json, .codex/auth.json) the rota checkout uses (default $HOME; for a node running under an isolated HOME)")
	fs.StringVar(&o.rotaIndependent, "rota-independent", "", "comma-separated account IDs whose rota login on this machine is independent (checked out without a hub lease)")
	fs.StringVar(&o.credBackend, "credential-backend", "", "store for Pyre-provisioned connection credentials: auto (default; macOS Keychain, Linux Secret Service), keychain or secret-service (also "+EnvCredentialBackend+"); never a plaintext fallback")
	fs.StringVar(&o.credKeychain, "credential-keychain", "", "macOS: keep Pyre-provisioned credentials in this keychain file instead of the default search list (also "+EnvCredentialKeychain+"; e.g. a throwaway test keychain)")
	addRepoFlags(fs) // W-quicklaunch
	fs.BoolVar(&o.accountsTestMode, "accounts-test-mode", false, "TESTING ONLY: enable account.inject_limit and sub-5-minute usage polls (same as PYRE_ACCOUNTS_TEST_MODE=1); never on a production node")
	return fs
}

func runDaemon(o runOpts) error {
	if !daemon.ValidPermissions(o.permissions) {
		return fmt.Errorf("--permissions %q: want ask or bypass", o.permissions)
	}
	if !accounts.ValidDefaultAccount(o.defaultAccount) {
		return fmt.Errorf("--default-account %q: want auto, subscription or ambient", o.defaultAccount)
	}
	if err := os.MkdirAll(o.state, 0o700); err != nil {
		return err
	}
	logger, closeLog := newLogger(o.state, o.logLevel)
	defer closeLog()
	// The launcher hands off to an active managed pyred (in-product update).
	if ex := daemonBoot(o.state, logger); ex != nil {
		return ex
	}
	watchHandoff := os.Getenv(envHandoffWatch) == "1"
	_ = os.Unsetenv(envHandoffWatch)

	var cred *enrollment.NodeCredentialFile
	credPath := o.credential
	if credPath == "" {
		if p := filepath.Join(o.state, "node-credential.json"); fileExists(p) {
			credPath = p
		}
	}
	if credPath != "" {
		c, err := enrollment.LoadNodeCredential(credPath)
		if err != nil {
			return err
		}
		cred = &c
	}
	hubURL, pin, nodeID, nodeToken := o.hub, "", "", ""
	if cred != nil {
		if hubURL == "" {
			hubURL = cred.HubURL
		}
		pin, nodeID, nodeToken = cred.HubCertSHA256, cred.NodeID, cred.Token
	}
	if hubURL != "" && cred == nil {
		return errors.New("--hub requires an enrolled node credential (pyred enroll)")
	}
	ownerToken := ""
	if o.ownerCred != "" {
		c, err := enrollment.LoadClientCredential(o.ownerCred)
		if err != nil {
			return err
		}
		ownerToken = c.Token
	}

	dbPath := filepath.Join(o.state, "pyred.db")
	db, err := sqlitex.Open(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		_ = os.Chmod(p, 0o600)
	}

	cfg := daemon.Config{StateDir: o.state, DB: db, NodeID: nodeID, NodeName: o.name, SSHAlias: o.sshAlias,
		MaxRuns: o.maxRuns, OwnerToken: ownerToken, Logger: logger, ReadCacheQueue: o.readCacheQueue, Permissions: o.permissions}
	cfg.RepoIndex = repoIndexConfig() // W-quicklaunch
	if o.permissions == daemon.PermissionsBypass {
		logger.Warn("permissions bypass: Pyre-launched runs on this node execute without approval prompts or sandbox (--permissions bypass)")
	}
	var clock contracts.Clock = contracts.SystemClock{}
	if o.devClockOffset != 0 {
		clock = offsetClock{o.devClockOffset}
		cfg.Clock = clock
		logger.Warn("TESTING ONLY: daemon clock shifted (--dev-clock-offset)", "offset", o.devClockOffset.String())
	}
	if hubURL != "" {
		cfg.Upstream = func(bearer string) contracts.API {
			return client.New(hubURL, bearer, client.Options{CertSHA256: pin, UserAgent: "pyred-local-proxy"})
		}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Integrations need the node ID; standalone nodes persist a local one.
	if cfg.NodeID, err = daemon.EnsureNodeID(ctx, db, cfg.NodeID); err != nil {
		return err
	}
	// Find tools the way the owner's shells do (nvm, user-local Homebrew).
	widenToolPath(ctx, o.toolPaths, logger)
	built, err := buildAdapters(o, cfg.NodeID, logger)
	if err != nil {
		return err
	}
	cfg.Adapters = built.list
	// Agent tools are core, independent of integrations: the absolute CLI
	// path goes into the guide and PYRE_CLI, and Claude and Codex get `pyre mcp`.
	cli := findPyreCLI()
	cfg.PyreCLI = cli
	if cli != "" {
		cfg.AgentMCP = []string{cli, "mcp"}
	}
	cfg.AgentGuide = func(h contracts.Harness, _ contracts.Session) string {
		// Claude gets `pyre mcp` via --mcp-config; Codex via per-thread config.
		return agentguide.Text(agentguide.Options{CLI: cli, MCP: cli != "" && h != contracts.HarnessShell})
	}
	var integ *integrations
	if !o.noIntegrations {
		integ = wireIntegrations(ctx, &cfg, o, built, hubURL, pin, nodeToken, logger)
	} else {
		cfg.ActionsUnavailable = noIntegrationsReason
	}

	if hubURL != "" { // relayed web terminals dial the hub back (WP19)
		tlsCfg, err := transport.ClientTLSConfig(pin)
		if err != nil {
			return err
		}
		bridge, err := terminal.NewNodeBridge(terminal.BridgeConfig{HubURL: hubURL, Token: nodeToken, TLS: tlsCfg, Logger: logger.With("component", "terminal")})
		if err != nil {
			return err
		}
		defer bridge.Close()
		cfg.Terminal = bridge
	}
	d, err := daemon.New(cfg)
	if err != nil {
		return err
	}
	var link *transport.NodeLink
	linkUp := make(chan struct{}, 1)
	if hubURL != "" {
		tlsCfg, err := transport.ClientTLSConfig(pin)
		if err != nil {
			return err
		}
		link, err = transport.NewNodeLink(transport.NodeLinkConfig{HubURL: hubURL, NodeID: cred.NodeID, Token: nodeToken, TLS: tlsCfg, DB: db,
			Clock: clock, Logger: logger.With("component", "nodelink"), Hello: d.Hello, Handler: transport.CommandHandlerFunc(d.HandleCommand),
			CommandRetention: o.devCommandRetention,
			OnStateChange: func(s transport.LinkStatus) {
				d.SetHubLink(s.State == transport.LinkConnected)
				if s.State == transport.LinkConnected {
					d.KickOutbox()
					select {
					case linkUp <- struct{}{}:
					default:
					}
				}
			},
			// A hub restored from an older backup (or replaced) lost reports
			// it acknowledged: send it our run records to reconcile.
			OnHubBehind: func(ctx context.Context, committed, acked uint64) {
				reason := fmt.Sprintf("hub committed report %d < acknowledged %d", committed, acked)
				if n, err := d.SendRunSnapshot(ctx, reason); err != nil {
					logger.Warn("run snapshot for a hub that lost reports failed", "err", err)
				} else {
					logger.Info("sent run snapshot to a hub that lost reports", "records", n, "reason", reason)
				}
			}})
		if err != nil {
			return err
		}
		d.SetReporter(link)
	}
	if integ != nil {
		integ.attach(ctx, d, link)
	}
	if err := d.Start(ctx); err != nil {
		return err
	}
	if integ != nil && integ.acct != nil {
		if err := recoverRota(ctx, integ.acct, d); err != nil {
			logger.Warn("rota checkout recovery failed; affected accounts stay locked until resolved", "err", err)
		}
	}
	errc := make(chan error, 2)
	go func() { errc <- d.ServeLocal(ctx) }()
	if link != nil {
		go func() { errc <- link.Run(ctx) }()
	}
	logger.Info("pyred running", "node", d.NodeID(), "state", o.state, "socket", d.SocketPath(), "hub", hubURL != "")
	fmt.Fprintf(os.Stderr, "pyred: node %s serving %s (hub: %s)\n", d.NodeID(), d.SocketPath(), orNone(hubURL))
	if watchHandoff {
		go watchFirstBoot(ctx, o.state, hubURL != "", linkUp, logger)
	}
	var runErr error
	var restart *execRequest
	select {
	case <-ctx.Done():
	case runErr = <-errc:
		cancel()
	case restart = <-selfRestart:
		logger.Info("pyred restarting through the launcher", "why", restart.why)
		cancel()
	}
	d.Close()
	logger.Info("pyred stopped; runs keep running and are recovered on next start")
	if restart != nil {
		return restart
	}
	return runErr
}

// watchFirstBoot proves a newly activated managed pyred healthy (hub link
// connected; standalone: started) within the handoff timeout, or marks the
// handoff failed and restarts through the launcher, which rolls it back.
func watchFirstBoot(ctx context.Context, state string, hub bool, linkUp <-chan struct{}, log *slog.Logger) {
	um, err := updates.New(updates.Options{StateDir: state})
	if err != nil {
		return
	}
	if !hub {
		_ = um.MarkDaemonHealthy(buildinfo.Version)
		log.Info("managed pyred first boot healthy (standalone)", "version", buildinfo.Version)
		return
	}
	timeout := handoffTimeout()
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-linkUp:
		_ = um.MarkDaemonHealthy(buildinfo.Version)
		log.Info("managed pyred first boot healthy (hub connected)", "version", buildinfo.Version)
	case <-t.C:
		detail := fmt.Sprintf("not connected to the hub within %s", timeout)
		_ = um.MarkDaemonFailed(buildinfo.Version, detail)
		log.Error("managed pyred first boot failed; restarting through the launcher, which rolls it back", "version", buildinfo.Version, "reason", detail)
		requestRestart(restartViaLauncher("first boot of " + buildinfo.Version + " failed: " + detail))
	}
}

func orNone(s string) string {
	if s == "" {
		return "none (standalone)"
	}
	return s
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func newLogger(state, level string) (*slog.Logger, func()) {
	return newLoggerTo(state, level, os.Stderr)
}

// newLoggerTo logs at the configured level to the size-rotated
// <state>/logs/pyred.log, and only warnings and errors to stderr. Every
// line used to go to both: under launchd (StandardErrorPath) and with
// redirected stderr the copy grew without rotation; in the A20 soak it grew
// as fast as the rotated file (about 270 KB/h per node at one message per
// second, one INFO line per local API request).
func newLoggerTo(state, level string, stderr io.Writer) (*slog.Logger, func()) {
	var lv slog.Level
	_ = lv.UnmarshalText([]byte(level))
	rf := &supervisor.RotatingFile{Path: filepath.Join(state, "logs", "pyred.log"), MaxBytes: 5 << 20, Backups: 3}
	errLv := lv
	if errLv < slog.LevelWarn {
		errLv = slog.LevelWarn
	}
	h := fanout{slog.NewTextHandler(rf, &slog.HandlerOptions{Level: lv}), slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: errLv})}
	return slog.New(h), func() { rf.Close() }
}

// fanout sends each record to every handler that is enabled for its level.
type fanout []slog.Handler

func (f fanout) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range f {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (f fanout) Handle(ctx context.Context, r slog.Record) error {
	var first error
	for _, h := range f {
		if h.Enabled(ctx, r.Level) {
			if err := h.Handle(ctx, r.Clone()); err != nil && first == nil {
				first = err
			}
		}
	}
	return first
}

func (f fanout) WithAttrs(as []slog.Attr) slog.Handler {
	out := make(fanout, len(f))
	for i, h := range f {
		out[i] = h.WithAttrs(as)
	}
	return out
}

func (f fanout) WithGroup(name string) slog.Handler {
	out := make(fanout, len(f))
	for i, h := range f {
		out[i] = h.WithGroup(name)
	}
	return out
}

// offsetClock is the system clock shifted by a fixed offset
// (--dev-clock-offset, node clock skew tests).
type offsetClock struct{ d time.Duration }

func (c offsetClock) Now() time.Time { return time.Now().Add(c.d) }
