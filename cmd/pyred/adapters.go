package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/jhh3/pyre/internal/stablepath"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/agy"
	"github.com/jhh3/pyre/internal/runtime/claude"
	"github.com/jhh3/pyre/internal/runtime/codex"
	"github.com/jhh3/pyre/internal/runtime/mock"
	"github.com/jhh3/pyre/internal/runtime/shell"
)

// maxSocketPath keeps unix socket paths under the macOS sun_path limit.
const maxSocketPath = 100

// builtAdapters are the runtime adapters of this pyred plus handles other
// integrations need (the Codex adapter reads rate limits for accounts).
type builtAdapters struct {
	list  []contracts.RuntimeAdapter
	exec  []contracts.RuntimeAdapter // headless pyre run adapters (daemon.Config.ExecAdapters)
	codex *codex.Adapter
	// rates relays the Codex adapter's rate-limit pushes to the accounts
	// service once it exists (own-login usage, accounts/ownlogin.go).
	rates *rateRelay
	// auto relays dialogs an adapter answered by itself (trust prompt,
	// Claude's exit dialog) to the hub's audit log once reporting is up.
	auto *autoRelay
	// wake answers the Codex adapter's "may this session be woken?" once
	// the hub client exists (bgwake.go; allowed before then).
	wake *wakeRelay
	// doorbell is the Claude doorbell monitor argv (nil when disabled) and
	// doorbellOff why it is disabled.
	doorbell    []string
	doorbellOff string
}

// EnvDoorbell disables the Claude doorbell when set to 0/false/off
// (same as pyred run --no-doorbell).
const EnvDoorbell = "PYRE_DOORBELL"

// doorbellCommand returns the Claude doorbell monitor argv (S-D, decision
// I16): this pyred's absolute path plus "inbox-watch". The agent reads the
// mail with the pyre MCP inbox tool, so the doorbell is off without a pyre
// CLI (no MCP server in the run), with --no-doorbell, or PYRE_DOORBELL=0.
func doorbellCommand(o runOpts, exe, pyreCLI string) ([]string, string) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvDoorbell))) {
	case "0", "false", "off", "no":
		return nil, EnvDoorbell + " disables it"
	}
	if o.noDoorbell {
		return nil, "--no-doorbell"
	}
	if pyreCLI == "" {
		return nil, "no pyre CLI next to pyred or on PATH (the agent needs the pyre MCP inbox tool)"
	}
	return []string{exe, "inbox-watch"}, ""
}

// buildAdapters creates the native adapters (WP07) on one shared,
// Pyre-owned tmux server at <state>/tmux/pyre.sock. An adapter that cannot
// be constructed is logged and skipped; harness availability (installed,
// logged in, tmux version) is reported by the daemon's probes, never fatal.
func buildAdapters(o runOpts, nodeID string, log *slog.Logger) (builtAdapters, error) {
	var b builtAdapters
	sock := filepath.Join(o.state, "tmux", "pyre.sock")
	if len(sock) > maxSocketPath {
		return b, fmt.Errorf("tmux socket path %s is %d bytes (max %d); use a shorter --state", sock, len(sock), maxSocketPath)
	}
	if o.devMock {
		b.list = append(b.list, mock.NewAdapter())
	}
	exe, err := os.Executable()
	if err != nil {
		return b, err
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	exe = stablepath.Of(exe) // hooks outlive this version's install dir
	b.auto = &autoRelay{}
	b.doorbell, b.doorbellOff = doorbellCommand(o, exe, findPyreCLI())
	if b.doorbellOff != "" {
		log.Info("claude doorbell (input.monitor) disabled", "reason", b.doorbellOff)
	}
	cl, err := claude.New(claude.Options{NodeID: nodeID, TmuxSocket: sock, SSHAlias: o.sshAlias,
		HookCommand: []string{exe, "hook", "claude"}, ProfileAsConfigDir: true, TrustWorkspace: trustFunc(o.trustDirs),
		TranscriptHomes: claudeTranscriptHomes(profileRoot(o.state, os.Getenv, userHome())),
		TmpDir:          claudeTmpDir(o.state),
		PermissionMode:  "default", ApprovalWait: 120 * time.Second, DoorbellCommand: b.doorbell, OnAutomation: b.auto.call})
	if err != nil {
		log.Warn("claude adapter unavailable", "err", err)
	} else {
		b.list = append(b.list, cl)
		b.exec = append(b.exec, claude.NewExec(cl))
	}
	b.rates = &rateRelay{}
	b.wake = &wakeRelay{}
	cx, err := codex.New(codex.Options{NodeID: nodeID, TmuxSocket: sock, SSHAlias: o.sshAlias, StateRoot: filepath.Join(o.state, "rt"),
		ApprovalPolicy: "on-request", Sandbox: "workspace-write", TrustWorkspace: trustFunc(o.trustDirs), OnRateLimits: b.rates.call, OnAutomation: b.auto.call,
		WakeAllowed: b.wake.call, TranscriptHomes: codexTranscriptHomes(profileRoot(o.state, os.Getenv, userHome()))})
	if err != nil {
		log.Warn("codex adapter unavailable", "err", err)
	} else {
		b.list = append(b.list, cx)
		b.exec = append(b.exec, codex.NewExec(cx))
		b.codex = cx
	}
	// agy (Google Antigravity): offered wherever the agy binary is found and
	// signed in (its Probe says why not otherwise). Runs use the node's own
	// login; hooks and the pyre MCP server travel in a per-session
	// workspace folder under <state>/agy.
	var agyMCP []string
	if cli := findPyreCLI(); cli != "" {
		agyMCP = []string{cli, "mcp"}
	}
	ag, err := agy.New(agy.Options{NodeID: nodeID, TmuxSocket: sock, SSHAlias: o.sshAlias, WorkspaceRoot: filepath.Join(o.state, "agy"),
		HookCommand: []string{exe, "hook", "agy"}, MCPCommand: agyMCP, TrustWorkspace: trustFunc(o.trustDirs), OnAutomation: b.auto.call})
	if err != nil {
		log.Warn("agy adapter unavailable", "err", err)
	} else {
		b.list = append(b.list, ag)
	}
	sh, err := shell.New(shell.Options{NodeID: nodeID, TmuxSocket: sock, SSHAlias: o.sshAlias})
	if err != nil {
		log.Warn("shell adapter unavailable", "err", err)
	} else {
		b.list = append(b.list, sh)
	}
	return b, nil
}

// trustFunc trusts a workspace (letting the Claude adapter accept the
// workspace-trust dialog, and the Codex TUI start trusted) only inside
// directories the owner configured with --trust-dir. Default: trust
// nothing; the run reports
// "needs human: workspace trust".
func trustFunc(dirs []string) func(cwd string) bool {
	var roots []string
	for _, d := range dirs {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		if strings.HasPrefix(d, "~/") {
			if h, err := os.UserHomeDir(); err == nil {
				d = filepath.Join(h, d[2:])
			}
		}
		if abs, err := filepath.Abs(d); err == nil {
			if r, err := filepath.EvalSymlinks(abs); err == nil {
				abs = r
			}
			roots = append(roots, filepath.Clean(abs))
		}
	}
	return func(cwd string) bool {
		c := filepath.Clean(cwd)
		if r, err := filepath.EvalSymlinks(c); err == nil {
			c = r
		}
		for _, r := range roots {
			if c == r || strings.HasPrefix(c, r+string(filepath.Separator)) {
				return true
			}
		}
		return false
	}
}

// rateRelay forwards rate-limit pushes to a handler set after start-up.
type rateRelay struct{ f atomic.Value }

func (r *rateRelay) set(f func(home string, raw json.RawMessage)) { r.f.Store(f) }

func (r *rateRelay) call(home string, raw json.RawMessage) {
	if f, ok := r.f.Load().(func(string, json.RawMessage)); ok && f != nil {
		f(home, raw)
	}
}

// autoRelay forwards adapter automation notices to a handler set after
// start-up (dropped before then).
type autoRelay struct{ f atomic.Value }

func (r *autoRelay) set(f func(contracts.NodeAutomation)) { r.f.Store(f) }

func (r *autoRelay) call(a contracts.NodeAutomation) {
	if f, ok := r.f.Load().(func(contracts.NodeAutomation)); ok && f != nil {
		f(a)
	}
}

// wakeRelay forwards the Codex adapter's wake check to a handler set after
// start-up (every wake is allowed before then).
type wakeRelay struct{ f atomic.Value }

func (r *wakeRelay) set(f func(context.Context, string) bool) { r.f.Store(f) }

func (r *wakeRelay) call(ctx context.Context, sessionID string) bool {
	if f, ok := r.f.Load().(func(context.Context, string) bool); ok && f != nil {
		return f(ctx, sessionID)
	}
	return true
}

// wakeAllowed is the hub's answer to "may this session be woken by a
// finished background command?": not when it is archived or scheduled to
// archive when done (its run is about to stop). A hub that cannot answer
// allows it; one that no longer knows the session does not.
func wakeAllowed(get func(context.Context, contracts.GetRequest) (contracts.SessionView, error)) func(context.Context, string) bool {
	return func(ctx context.Context, sessionID string) bool {
		v, err := get(ctx, contracts.GetRequest{ID: sessionID})
		if err != nil {
			return !contracts.IsCode(err, contracts.ErrNotFound)
		}
		return !v.Session.Archived && !slices.Contains(v.Session.Tags, contracts.TagArchiveWhenDone)
	}
}

// claudeTranscriptHomes are the Claude config homes a resume searches for a
// conversation besides the default login: the accounts' shared store and
// every account profile (one not yet linked to the store keeps its own).
func claudeTranscriptHomes(profiles string) []string {
	return []string{
		filepath.Join(filepath.Dir(filepath.Clean(profiles)), "shared", "claude"),
		filepath.Join(profiles, "*", "claude*"),
	}
}

// codexTranscriptHomes are the CODEX_HOMEs besides ~/.codex that hold
// rollouts: the accounts' shared store and every account profile.
func codexTranscriptHomes(profiles string) []string {
	return []string{
		filepath.Join(filepath.Dir(filepath.Clean(profiles)), "shared", "codex"),
		filepath.Join(profiles, "*", "codex*"),
	}
}

// claudeTmpDir keeps Claude's per-session scratch on disk under the node
// state instead of /tmp, which is RAM-backed tmpfs (with a per-user quota) on
// some hosts. Windows' %TEMP% is already on disk.
func claudeTmpDir(state string) string {
	if runtime.GOOS == "windows" {
		return ""
	}
	return filepath.Join(state, "tmp", "claude")
}
