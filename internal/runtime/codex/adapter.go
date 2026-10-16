// Package codex is the native Codex runtime adapter (WP01 report §9,
// decision I07). One Pyre-owned `codex app-server --listen unix://…` runs per
// CODEX_HOME binding inside the node's isolated tmux server; runs are app-
// server threads driven over JSON-RPC (turn/start with clientUserMessageId =
// Pyre message id; evidence is the userMessage item echoing that clientId).
// The human TUI is `codex resume <thread> --remote unix://<sock>` in a tmux
// session per run, so native attach shares the same thread and approvals.
package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/messaging/header"
	"github.com/jhh3/pyre/internal/osutil"
	"github.com/jhh3/pyre/internal/runtime/hostsel"
	"github.com/jhh3/pyre/internal/runtime/sessionhost"
	"github.com/jhh3/pyre/internal/runtime/tmux"
	"github.com/jhh3/pyre/internal/winenv"
)

// AdapterName is the RunHandle.Adapter value.
const AdapterName = "codex-appserver"

// TestedVersions lists Codex CLI versions this adapter was validated with.
var TestedVersions = []string{"0.160.0"}

// Options configures the Codex adapter (one instance per node).
type Options struct {
	NodeID string
	// TmuxBin / TmuxSocket: the node's isolated tmux server (required socket).
	TmuxBin    string
	TmuxSocket string
	SSHAlias   string
	// StateRoot is a node-level 0700 Pyre state dir; app-server sockets live
	// at <StateRoot>/codex/<home-key>/codex.sock (path must stay <=100 bytes).
	StateRoot string
	// CodexBin is the codex executable (default ResolvedLaunch.Executable,
	// then "codex" on PATH).
	CodexBin string
	// ApprovalPolicy / Sandbox are passed explicitly to thread/start
	// (defaults "on-request" / "workspace-write"); `-a`/`-s` in
	// ResolvedLaunch.Args override per run.
	ApprovalPolicy string
	Sandbox        string
	// AppServerArgs are appended to `codex app-server --listen …` (e.g. -c
	// overrides excluding user hooks/MCP); AppServerEnv adds environment.
	AppServerArgs []string
	AppServerEnv  map[string]string
	// AppServerCommand overrides the app-server argv (tests).
	AppServerCommand func(home, sock string) []string
	// DisableTUI skips the per-run native TUI tmux session (attach then
	// unavailable).
	DisableTUI bool
	// TrustWorkspace reports whether the native TUI may treat a run's cwd as
	// a trusted folder (the owner's --trust-dir, as for Claude). Without it
	// the attached TUI stops at Codex's "Trust this folder?" screen.
	TrustWorkspace func(cwd string) bool
	// OnAutomation, when set, is told when the adapter answers Codex's
	// folder-trust prompt by itself (W-audit; once per run, must not block).
	OnAutomation func(contracts.NodeAutomation)
	// OnRateLimits receives account/rateLimits/updated pushes (the rate
	// limits a run's turns report) with the app-server's CODEX_HOME ("" =
	// the machine's own login). Called on its own goroutine.
	OnRateLimits func(codexHome string, raw json.RawMessage)
	// DeliverTimeout bounds the wait for the userMessage item (default 10s).
	DeliverTimeout time.Duration
	// InterruptTimeout bounds turn/interrupt (default 5s).
	InterruptTimeout time.Duration
	// StartWait bounds app-server start + socket connect (default 20s).
	StartWait time.Duration
	Now       func() time.Time
}

// Adapter implements contracts.RuntimeAdapter for Codex.
type Adapter struct {
	o   Options
	srv sessionhost.Host

	mu    sync.Mutex
	homes map[string]*homeServer
	runs  map[string]*runRef

	turnsPage int // thread/turns/list page size for sessions.read (0: 50; tests)
}

var _ contracts.RuntimeAdapter = (*Adapter)(nil)

type runRef struct {
	deliverMu sync.Mutex
	meta      runMeta
	hs        *homeServer
	ts        *threadState
	subOwner  *rpcClient // client the thread is subscribed on
	// tuiEnv is the TUI pane's launch environment (may hold secrets such
	// as PYRE_SESSION_TOKEN). It lives only in memory: run.json never holds
	// it, and tmux no longer keeps it in the session environment, so a
	// respawned TUI gets it from here. Empty after a pyred restart.
	tuiEnv map[string]string
	// trustWatch is set while a watchTrust goroutine runs for this run.
	trustWatch atomic.Bool
	// trustChecked is the last recheckTrust time (unix nanos).
	trustChecked atomic.Int64
	// retrying is set while a capacity retry turn is being started.
	retrying atomic.Bool
	// readMu serializes sessions.read; turns are its kept settled pages.
	readMu sync.Mutex
	turns  *turnCache
}

// runMeta is <state>/run.json.
type runMeta struct {
	RunID          string    `json:"run_id"`
	SessionID      string    `json:"session_id"`
	ThreadID       string    `json:"thread_id"`
	Home           string    `json:"codex_home"`
	Executable     string    `json:"executable,omitempty"` // pinned codex binary ("" = adapter default)
	HomeKey        string    `json:"home_key"`
	Socket         string    `json:"socket"`
	Cwd            string    `json:"cwd"`
	ApprovalPolicy string    `json:"approval_policy"`
	Sandbox        string    `json:"sandbox"`
	Model          string    `json:"model,omitempty"`
	Unset          []string  `json:"unset,omitempty"`
	LaunchedAt     time.Time `json:"launched_at"`
}

// New validates options.
func New(o Options) (*Adapter, error) {
	if o.TmuxSocket == "" || o.StateRoot == "" {
		return nil, errors.New("codex: TmuxSocket and StateRoot are required")
	}
	srv, err := hostsel.New(o.TmuxBin, o.TmuxSocket)
	if err != nil {
		return nil, err
	}
	if err := tmux.PrepareDir(o.StateRoot); err != nil {
		return nil, err
	}
	if o.ApprovalPolicy == "" {
		o.ApprovalPolicy = "on-request"
	}
	if o.Sandbox == "" {
		o.Sandbox = "workspace-write"
	}
	if o.DeliverTimeout <= 0 {
		o.DeliverTimeout = 10 * time.Second
	}
	if o.InterruptTimeout <= 0 {
		o.InterruptTimeout = 5 * time.Second
	}
	if o.StartWait <= 0 {
		o.StartWait = 20 * time.Second
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	a := &Adapter{o: o, srv: srv, homes: map[string]*homeServer{}, runs: map[string]*runRef{}}
	go a.sweepLoop(context.Background()) // unload threads no live run owns (unload.go)
	return a, nil
}

// Server exposes the session host (for daemon housekeeping).
func (a *Adapter) Server() sessionhost.Host { return a.srv }

func (a *Adapter) Name() string               { return AdapterName }
func (a *Adapter) Harness() contracts.Harness { return contracts.HarnessCodex }

// Capabilities advertised by this adapter.
func (a *Adapter) Capabilities() []string {
	caps := []string{
		contracts.CapInputQueue, contracts.CapInputSteer, contracts.CapInputReconcile, contracts.CapInputAppServer,
		contracts.CapApprovals, contracts.CapQuestions, contracts.CapInterrupt, contracts.CapStop,
		contracts.CapResumeNative, contracts.CapResumeHandoff, contracts.CapSubagentsObserve,
		contracts.CapTranscript, contracts.CapAccountProfile,
	}
	if !a.o.DisableTUI {
		caps = append(caps, contracts.CapAttachNative, contracts.CapAttachReadOnly, contracts.CapAttachCodexRemote, contracts.CapScreen)
	}
	return caps
}

func (a *Adapter) codexBin() string {
	if a.o.CodexBin != "" {
		return a.o.CodexBin
	}
	if p, err := exec.LookPath("codex"); err == nil {
		if runtime.GOOS == "windows" {
			// npm installs codex.cmd (cmd.exe + node wrapper); run the
			// native codex.exe it wraps when present.
			if n := winenv.Default().NativeCodex(p); n != "" {
				return n
			}
		}
		return p
	}
	return "codex"
}

var versionRe = regexp.MustCompile(`(\d+\.\d+\.\d+)`)

func (a *Adapter) Probe(ctx context.Context) (contracts.ProbeResult, error) {
	pr := contracts.ProbeResult{Harness: contracts.HarnessCodex, Adapter: AdapterName, Executable: a.codexBin(), Capabilities: a.Capabilities()}
	if _, err := a.srv.Check(ctx); err != nil {
		pr.Reason = err.Error()
		return pr, nil
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, pr.Executable, "--version").Output()
	if err != nil {
		pr.Reason = "codex --version failed: " + err.Error()
		return pr, nil
	}
	pr.Version = versionRe.FindString(string(out))
	pr.Available = true
	tested := false
	for _, t := range TestedVersions {
		tested = tested || t == pr.Version
	}
	if !tested {
		pr.Reason = "untested Codex version (tested: " + strings.Join(TestedVersions, ", ") + ")"
	}
	return pr, nil
}

// parseArgs accepts only -m/--model, -a/--ask-for-approval, -s/--sandbox.
func (a *Adapter) parseArgs(args []string) (model, approval, sandbox string, err error) {
	approval, sandbox = a.o.ApprovalPolicy, a.o.Sandbox
	for i := 0; i < len(args); i++ {
		name, val, hasVal := strings.Cut(args[i], "=")
		if !hasVal {
			if i+1 >= len(args) {
				return "", "", "", contracts.Errorf(contracts.ErrInvalid, "argument %s needs a value", name)
			}
			i++
			val = args[i]
		}
		switch name {
		case "-m", "--model":
			model = val
		case "-a", "--ask-for-approval":
			approval = val
		case "-s", "--sandbox":
			sandbox = val
		default:
			return "", "", "", contracts.Errorf(contracts.ErrInvalid, "argument %s not allowed for codex runs (allowed: --model, --ask-for-approval, --sandbox)", name)
		}
	}
	switch approval {
	case "untrusted", "on-request", "never":
	default:
		return "", "", "", contracts.Errorf(contracts.ErrInvalid, "approval policy %q", approval)
	}
	switch sandbox {
	case "read-only", "workspace-write", "danger-full-access":
	default:
		return "", "", "", contracts.Errorf(contracts.ErrInvalid, "sandbox %q", sandbox)
	}
	return model, approval, sandbox, nil
}

func (a *Adapter) Launch(ctx context.Context, l contracts.ResolvedLaunch) (contracts.RunHandle, error) {
	if !tmux.ValidName(l.RunID) {
		return contracts.RunHandle{}, contracts.Errorf(contracts.ErrInvalid, "run id %q is not a valid tmux session name", l.RunID)
	}
	if l.StateDir == "" || l.Cwd == "" {
		return contracts.RunHandle{}, contracts.Errorf(contracts.ErrInvalid, "state dir and cwd are required")
	}
	rest, managedCfg, err := splitConfigArgs(l.Args)
	if err != nil {
		return contracts.RunHandle{}, err
	}
	model, approval, sandbox, err := a.parseArgs(rest)
	if err != nil {
		return contracts.RunHandle{}, err
	}
	if l.Env[PermissionsEnv] == "bypass" {
		// The app-server equivalents of --dangerously-bypass-approvals-and-sandbox.
		approval, sandbox = approvalNever, sandboxFull
	}
	if err := tmux.PrepareDir(l.StateDir); err != nil {
		return contracts.RunHandle{}, err
	}
	if _, err := os.Stat(filepath.Join(l.StateDir, "run.json")); err == nil {
		return contracts.RunHandle{}, contracts.Errorf(contracts.ErrConflict, "run %s was already launched", l.RunID)
	}
	hs, err := a.homeServerFor(l.ProfileDir, l.Executable)
	if err != nil {
		return contracts.RunHandle{}, contracts.Errorf(contracts.ErrInvalid, "%v", err)
	}
	if err := hs.addRef(l.RunID); err != nil {
		return contracts.RunHandle{}, err
	}
	c, err := hs.ensure(ctx, l.Unset)
	if err != nil {
		hs.release(ctx, l.RunID)
		return contracts.RunHandle{}, contracts.Errorf(contracts.ErrUnavailable, "%v", err)
	}
	tcfg, err := mergeThreadConfig(threadConfig(l.Env), managedCfg)
	if err != nil {
		hs.release(ctx, l.RunID)
		return contracts.RunHandle{}, err
	}
	if len(tcfg) > 0 {
		b, _ := json.Marshal(tcfg)
		if err := writeFileAtomic(filepath.Join(l.StateDir, threadConfigFile), b); err != nil {
			hs.release(ctx, l.RunID)
			return contracts.RunHandle{}, err
		}
	}
	var r threadResponse
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if l.Resume != nil {
		if l.Resume.Kind != "codex_thread" || l.Resume.ID == "" {
			hs.release(ctx, l.RunID)
			return contracts.RunHandle{}, contracts.Errorf(contracts.ErrInvalid, "resume ref %+v is not a codex thread", *l.Resume)
		}
		err = c.call(cctx, "thread/resume", threadResumeParams{ThreadID: l.Resume.ID, Cwd: l.Cwd, ApprovalPolicy: approval, Sandbox: sandbox, Model: model, ExcludeTurns: true, Config: tcfg}, &r)
	} else {
		err = c.call(cctx, "thread/start", threadStartParams{Cwd: l.Cwd, ApprovalPolicy: approval, Sandbox: sandbox, Model: model, DeveloperInstructions: l.AgentGuide, Config: tcfg}, &r)
	}
	if err != nil {
		hs.release(ctx, l.RunID)
		var re *rpcError
		if errors.As(err, &re) {
			return contracts.RunHandle{}, contracts.Errorf(contracts.ErrInvalid, "codex refused the thread: %v", re)
		}
		return contracts.RunHandle{}, contracts.Errorf(contracts.ErrOutcomeIndeterminate, "thread start/resume outcome unknown: %v", err)
	}
	if l.Resume == nil {
		a.materialize(cctx, c, r.Thread.ID, l)
	}
	meta := runMeta{RunID: l.RunID, SessionID: l.SessionID, ThreadID: r.Thread.ID, Home: l.ProfileDir, Executable: hs.exe, HomeKey: hs.key, Socket: hs.sock,
		Cwd: l.Cwd, ApprovalPolicy: approval, Sandbox: sandbox, Model: model, Unset: l.Unset, LaunchedAt: a.o.Now().UTC()}
	mb, _ := json.MarshalIndent(meta, "", "  ")
	if err := writeFileAtomic(filepath.Join(l.StateDir, "run.json"), mb); err != nil {
		return contracts.RunHandle{}, err
	}
	ts := newThreadState(l.RunID, l.SessionID, r.Thread.ID)
	ts.config = tcfg
	ts.setStatus(r.Thread.Status, a.o.Now())
	hs.register(ts)
	rr := &runRef{meta: meta, hs: hs, ts: ts, subOwner: c}
	a.mu.Lock()
	a.runs[l.RunID] = rr
	a.mu.Unlock()

	h := contracts.RunHandle{
		RunID: l.RunID, SessionID: l.SessionID, Adapter: AdapterName, Harness: contracts.HarnessCodex,
		TmuxSocket: a.srv.SocketPath(), TmuxTarget: l.RunID, StateDir: l.StateDir,
		NativeRef: &contracts.NativeRef{Kind: "codex_thread", ID: r.Thread.ID},
		Data:      map[string]string{"codex_thread_id": r.Thread.ID, "home_key": hs.key, "app_server_socket": hs.sock},
	}
	if !a.o.DisableTUI {
		a.ensureTrust(ctx, c, rr)
		if err := a.startTUI(ctx, rr, l.Env, l.Unset); err == nil {
			if p, err := a.srv.Pane(ctx, l.RunID); err == nil {
				h.PID = p.PID
			}
		}
	}
	if l.InitialPrompt != "" {
		var tr turnStartResponse
		_ = c.call(cctx, "turn/start", turnStartParams{ThreadID: r.Thread.ID, Input: []userInput{{Type: "text", Text: l.InitialPrompt}},
			ClientUserMessageID: "initial-" + l.RunID}, &tr)
	}
	return h, nil
}

// threadConfigFile (0600, in the run's private state dir) keeps the
// thread's overrides (Pyre identity plus the environment generation's
// managed config) so a restarted app-server or pyred re-sends exactly the
// same config on thread/resume: a running run keeps its generation. It
// holds the run's Pyre session token (also held in the node database),
// never a provider credential.
const threadConfigFile = "thread-config.json"

// splitConfigArgs takes the environment generation's `-c key=value`
// overlays (also `--config key=value`) out of the launch arguments. Codex
// runs share one app-server per CODEX_HOME, so a process-wide -c would leak
// into every thread on that home; instead each value becomes a per-thread
// config override (thread/start and thread/resume "config"), which Codex
// merges like -c. Nothing is written to any CODEX_HOME. Values are parsed as
// TOML, falling back to a literal string, as `codex -c` does.
func splitConfigArgs(args []string) (rest []string, cfg map[string]any, err error) {
	for i := 0; i < len(args); i++ {
		name, val, hasVal := strings.Cut(args[i], "=")
		if name != "-c" && name != "--config" {
			rest = append(rest, args[i])
			continue
		}
		if !hasVal {
			if i+1 >= len(args) {
				return nil, nil, contracts.Errorf(contracts.ErrInvalid, "argument %s needs key=value", name)
			}
			i++
			val = args[i]
		}
		key, raw, ok := strings.Cut(val, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" || strings.HasPrefix(key, ".") || strings.HasSuffix(key, ".") || strings.Contains(key, "..") || strings.ContainsAny(key, " \t\"") {
			return nil, nil, contracts.Errorf(contracts.ErrInvalid, "codex config override %q is not key=value", val)
		}
		if cfg == nil {
			cfg = map[string]any{}
		}
		if _, dup := cfg[key]; dup {
			return nil, nil, contracts.Errorf(contracts.ErrInvalid, "codex config override %s given twice", key)
		}
		cfg[key] = parseOverrideValue(strings.TrimSpace(raw))
	}
	for k := range cfg {
		for o := range cfg {
			if k != o && strings.HasPrefix(o, k+".") {
				return nil, nil, contracts.Errorf(contracts.ErrInvalid, "codex config overrides %s and %s overlap", k, o)
			}
		}
	}
	return rest, cfg, nil
}

// parseOverrideValue parses a -c value as a TOML value (string, number,
// bool, array, inline table), else returns it as a literal string.
func parseOverrideValue(raw string) any {
	var doc map[string]any
	if _, err := toml.Decode("v = "+raw, &doc); err == nil {
		if v, ok := doc["v"]; ok {
			return v
		}
	}
	return raw
}

// mergeThreadConfig adds the managed overrides to the thread's Pyre identity
// config. A managed key equal to, inside or containing a Pyre identity key
// (shell_environment_policy.set.PYRE_*, mcp_servers.pyre) is refused: the
// app-server applies overrides in no defined order, so it could replace the
// run's identity.
func mergeThreadConfig(pyre, managed map[string]any) (map[string]any, error) {
	if len(managed) == 0 {
		return pyre, nil
	}
	out := map[string]any{}
	for k, v := range pyre {
		out[k] = v
	}
	for k, v := range managed {
		for p := range pyre {
			if k == p || strings.HasPrefix(p, k+".") || strings.HasPrefix(k, p+".") {
				return nil, contracts.Errorf(contracts.ErrConflict, "managed codex config %s overlaps Pyre's per-run %s; remove it from the environment profile", k, p)
			}
		}
		out[k] = v
	}
	return out, nil
}

// threadConfig gives the thread the run's Pyre identity. The app-server is
// shared by every run on a CODEX_HOME, so its process environment cannot
// carry a per-run PYRE_SESSION_TOKEN/PYRE_SOCKET; without these overrides a
// Codex agent's `pyre` would act with no identity (or fall back to the
// owner's client config). Two per-thread overrides (dotted keys, merged
// like `-c`, so user config is kept):
//   - shell_environment_policy.set.PYRE_*: commands the agent runs get the
//     run's PYRE_* variables (the agent guide names $PYRE_CLI);
//   - mcp_servers.pyre: the `pyre mcp` agent tools with that identity
//     (MCP servers run outside the command sandbox).
func threadConfig(env map[string]string) map[string]any {
	pyreEnv := map[string]string{}
	for k, v := range env {
		if strings.HasPrefix(k, "PYRE_") && v != "" {
			pyreEnv[k] = v
		}
	}
	if pyreEnv["PYRE_SESSION_ID"] == "" {
		return nil
	}
	cfg := map[string]any{}
	for k, v := range pyreEnv {
		cfg["shell_environment_policy.set."+k] = v
	}
	if cli := pyreEnv["PYRE_CLI"]; cli != "" {
		cfg["mcp_servers.pyre"] = map[string]any{"command": cli, "args": []string{"mcp"}, "env": pyreEnv}
	}
	return cfg
}

func readThreadConfig(stateDir string) map[string]any {
	b, err := os.ReadFile(filepath.Join(stateDir, threadConfigFile))
	if err != nil {
		return nil
	}
	var cfg map[string]any
	if json.Unmarshal(b, &cfg) != nil {
		return nil
	}
	return cfg
}

// materialize makes a new thread resumable by other clients. Codex 0.160.0
// writes a thread's rollout lazily (first turn), and the native TUI's
// `codex resume --remote` passes the rollout path as a consistency check, so
// attaching to a run launched without a prompt failed ("no rollout found" /
// "missing source rollout") and the TUI pane died. thread/name/set (the
// session title, shown in Codex's own thread list) does not write the file;
// appending one short developer item (thread/inject_items) does.
func (a *Adapter) materialize(ctx context.Context, c *rpcClient, threadID string, l contracts.ResolvedLaunch) {
	name := strings.TrimSpace(l.Title)
	if name == "" {
		name = "pyre " + l.SessionID
	}
	_ = c.call(ctx, "thread/name/set", threadSetNameParams{ThreadID: threadID, Name: name}, nil)
	note := fmt.Sprintf("Pyre session %s (run %s) started.", l.SessionID, l.RunID)
	_ = c.call(ctx, "thread/inject_items", threadInjectItemsParams{ThreadID: threadID, Items: []responseItem{{
		Type: "message", Role: "developer", Content: []responseContent{{Type: "input_text", Text: note}}}}}, nil)
}

// PermissionsEnv carries the run's permission mode from the daemon
// (ResolvedLaunch.Env; "bypass" => approval never + danger-full-access).
const PermissionsEnv = "PYRE_PERMISSIONS"

const (
	approvalNever = "never"
	sandboxFull   = "danger-full-access"
)

func (a *Adapter) tuiArgv(rr *runRef) []string {
	// No permission flags here: codex 0.160 refuses any permission override
	// on `resume --remote` ("Permission overrides are not supported when
	// resuming a remote task") and exits 1. Bypass is applied to the thread
	// itself on the app-server (approval_policy never, sandbox
	// danger-full-access), which the remote TUI's turns inherit.
	return []string{rr.hs.bin(), "resume", rr.meta.ThreadID, "--remote", "unix://" + rr.hs.sock, "-C", rr.meta.Cwd}
}

// ensureTrust records the run's cwd as a trusted project in the app-server's
// config before the native TUI starts. Codex 0.160.1 (verified on a Linux host)
// decides the remote
// TUI's "Trust this folder?" only from the app-server home's config.toml:
// `-c projects."<cwd>".trust_level=...` on the TUI or on the app-server, and
// a per-thread config override, are all ignored. This writes the entry
// (config/value/write, upsert into `projects`; other entries and comments
// are kept) when the owner trusted the directory (--trust-dir). Only for a
// Pyre account profile home (harness state Pyre owns): the owner's own
// ~/.codex/config.toml is never written by Pyre (I22); an ambient run's TUI
// gets the prompt answered instead (watchTrust).
func (a *Adapter) ensureTrust(ctx context.Context, c *rpcClient, rr *runRef) {
	a.ensureTUIPermissions(ctx, c, rr)
	if skipProfileTrust || a.o.TrustWorkspace == nil || rr.meta.Home == "" || !a.o.TrustWorkspace(rr.meta.Cwd) {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_ = c.call(cctx, "config/value/write", configValueWriteParams{KeyPath: "projects", MergeStrategy: "upsert",
		Value: map[string]any{rr.meta.Cwd: map[string]string{"trust_level": "trusted"}}}, nil)
}

// ensureTUIPermissions makes the native TUI's own turns use the run's
// permissions. The remote TUI sends each turn with the sandbox and approval
// policy from its home's config.toml (it does not inherit the thread's), and
// `resume --remote` refuses permission flags, so a bypass run's turns typed
// in the TUI ran in Codex's default workspace-write sandbox (Doppler could
// not reach the keyring, server 2026-10-09). For a Pyre account profile home
// (never the owner's own ~/.codex, I22) write the run's values.
func (a *Adapter) ensureTUIPermissions(ctx context.Context, c *rpcClient, rr *runRef) {
	if skipProfileTrust || rr.meta.Home == "" || rr.meta.Sandbox == "" {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_ = c.call(cctx, "config/value/write", configValueWriteParams{KeyPath: "sandbox_mode", MergeStrategy: "replace", Value: rr.meta.Sandbox}, nil)
	if rr.meta.ApprovalPolicy != "" {
		_ = c.call(cctx, "config/value/write", configValueWriteParams{KeyPath: "approval_policy", MergeStrategy: "replace", Value: rr.meta.ApprovalPolicy}, nil)
	}
}

// skipProfileTrust (tests) leaves the profile config alone so the watcher
// path can be exercised on an isolated home.
var skipProfileTrust bool

func (a *Adapter) startTUI(ctx context.Context, rr *runRef, env map[string]string, unset []string) error {
	if env != nil {
		rr.tuiEnv = env
	}
	err := a.srv.NewSession(ctx, tmux.SessionSpec{Name: rr.meta.RunID, Cwd: rr.meta.Cwd, Env: a.tuiEnv(rr), Unset: unset, Argv: a.tuiArgv(rr), Width: 200, Height: 50})
	if err == nil {
		a.watchTrust(rr)
	}
	return err
}

// respawnTUI restarts a TUI pane the human quit.
func (a *Adapter) respawnTUI(ctx context.Context, rr *runRef) error {
	err := a.srv.RespawnPane(ctx, rr.meta.RunID, tmux.SessionSpec{Cwd: rr.meta.Cwd, Env: a.tuiEnv(rr), Unset: rr.meta.Unset, Argv: a.tuiArgv(rr)})
	if err == nil {
		a.watchTrust(rr)
	}
	return err
}

// tuiEnv is the TUI pane environment: the run's launch environment (when
// still known) plus CODEX_HOME.
func (a *Adapter) tuiEnv(rr *runRef) map[string]string {
	e := map[string]string{}
	for k, v := range rr.tuiEnv {
		e[k] = v
	}
	if rr.meta.Home != "" {
		e["CODEX_HOME"] = rr.meta.Home
	}
	return e
}

// release drops a run's ref and stops the app-server when unused.
func (hs *homeServer) release(ctx context.Context, ref string) {
	if hs.dropRef(ref) == 0 {
		hs.stop(ctx)
	}
}

// ChangeStamp is a cheap fingerprint of the run's thread activity: a
// counter of app-server notifications and requests for its thread. The
// daemon compares it between observations, so a backed-off observer wakes
// as soon as a turn starts or completes or an approval arrives. It never
// takes the run lock ("" for a run this adapter has not loaded).
func (a *Adapter) ChangeStamp(h contracts.RunHandle) string {
	a.mu.Lock()
	rr := a.runs[h.RunID]
	a.mu.Unlock()
	if rr == nil || rr.ts == nil {
		return ""
	}
	return strconv.FormatInt(rr.ts.changes.Load(), 10)
}

// run returns the run's live reference, loading run.json after a restart.
func (a *Adapter) run(h contracts.RunHandle) (*runRef, error) {
	a.mu.Lock()
	rr := a.runs[h.RunID]
	a.mu.Unlock()
	if rr != nil {
		return rr, nil
	}
	b, err := os.ReadFile(filepath.Join(h.StateDir, "run.json"))
	if err != nil {
		return nil, contracts.Errorf(contracts.ErrNotFound, "run %s has no codex state", h.RunID)
	}
	var meta runMeta
	if err := json.Unmarshal(b, &meta); err != nil {
		return nil, err
	}
	hs, err := a.homeServerFor(meta.Home, meta.Executable)
	if err != nil {
		return nil, err
	}
	ts := newThreadState(meta.RunID, meta.SessionID, meta.ThreadID)
	ts.config = readThreadConfig(h.StateDir)
	hs.register(ts)
	rr = &runRef{meta: meta, hs: hs, ts: ts}
	a.mu.Lock()
	if existing := a.runs[h.RunID]; existing != nil {
		rr = existing
	} else {
		a.runs[h.RunID] = rr
	}
	a.mu.Unlock()
	return rr, nil
}

// client returns a connected client with the run's thread subscribed. With
// start=false it never starts an app-server (only reconnects to a live one).
func (a *Adapter) client(ctx context.Context, rr *runRef, start bool) (*rpcClient, error) {
	c := rr.hs.connected()
	if c == nil {
		if !start {
			if ok, _ := rr.hs.running(ctx); !ok {
				return nil, errNoServer
			}
		}
		var err error
		if c, err = rr.hs.ensure(ctx, rr.meta.Unset); err != nil {
			return nil, err
		}
	}
	rr.ts.mu.Lock()
	owner := rr.subOwner
	rr.ts.mu.Unlock()
	if owner != c {
		var r threadResponse
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		rr.ts.mu.Lock()
		cfg := rr.ts.config
		rr.ts.mu.Unlock()
		if err := c.call(cctx, "thread/resume", threadResumeParams{ThreadID: rr.meta.ThreadID, ExcludeTurns: true, Config: cfg}, &r); err != nil {
			return nil, err
		}
		rr.ts.mu.Lock()
		rr.ts.setStatus(r.Thread.Status, a.o.Now())
		rr.subOwner = c
		rr.ts.mu.Unlock()
	}
	return c, nil
}

func stopped(stateDir string) bool {
	_, err := os.Stat(filepath.Join(stateDir, "stopped.json"))
	return err == nil
}

func (a *Adapter) Recover(ctx context.Context, h contracts.RunHandle) (contracts.RunHandle, error) {
	if stopped(h.StateDir) {
		return h, contracts.Errorf(contracts.ErrNotFound, "run %s was stopped", h.RunID)
	}
	rr, err := a.run(h)
	if err != nil {
		return h, err
	}
	_ = rr.hs.addRef(h.RunID)
	c, err := a.client(ctx, rr, true)
	if err != nil {
		var re *rpcError
		if errors.As(err, &re) {
			return h, contracts.Errorf(contracts.ErrNotFound, "codex thread %s: %v", rr.meta.ThreadID, re)
		}
		return h, contracts.Errorf(contracts.ErrUnavailable, "%v", err)
	}
	// Rebuild acceptance evidence from the thread (no runtime dedupe).
	a.scanTurns(ctx, c, rr, 5, "")
	if !a.o.DisableTUI {
		if ok, _ := a.srv.HasSession(ctx, h.RunID); !ok {
			a.ensureTrust(ctx, c, rr)
			_ = a.startTUI(ctx, rr, nil, rr.meta.Unset)
		}
		if p, err := a.srv.Pane(ctx, h.RunID); err == nil {
			h.PID = p.PID
		}
	}
	return h, nil
}

// scanTurns walks thread/turns/list (newest first) recording userMessage
// clientIds; returns true if want ("" = none) was found.
func (a *Adapter) scanTurns(ctx context.Context, c *rpcClient, rr *runRef, pages int, want string) (evidence, bool) {
	cursor := ""
	for i := 0; i < pages; i++ {
		var resp turnsListResponse
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := c.call(cctx, "thread/turns/list", turnsListParams{ThreadID: rr.meta.ThreadID, Cursor: cursor, Limit: 50, ItemsView: "full", SortDirection: "desc"}, &resp)
		cancel()
		if err != nil {
			return evidence{}, false
		}
		rr.ts.mu.Lock()
		for _, t := range resp.Data {
			for _, raw := range t.Items {
				var it item
				if json.Unmarshal(raw, &it) == nil {
					rr.ts.addEvidence(it, t.ID, a.o.Now())
				}
			}
		}
		ev, ok := rr.ts.evidence[want]
		rr.ts.mu.Unlock()
		if want != "" && ok {
			return ev, true
		}
		if resp.NextCursor == "" || len(resp.Data) == 0 {
			break
		}
		cursor = resp.NextCursor
	}
	return evidence{}, false
}

func (a *Adapter) Observe(ctx context.Context, h contracts.RunHandle, cursor string) (contracts.Observation, error) {
	now := a.o.Now()
	obs := contracts.Observation{ObservedAt: now, NativeRef: h.NativeRef, NextCursor: cursor}
	if stopped(h.StateDir) {
		obs.State, obs.StateReason = contracts.RunExited, "stopped by pyre"
		return obs, nil
	}
	rr, err := a.run(h)
	if err != nil {
		obs.State, obs.StateReason = contracts.RunUnknown, err.Error()
		return obs, nil
	}
	if _, err := a.client(ctx, rr, false); err != nil {
		obs.State, obs.StateReason = contracts.RunUnknown, "codex app-server unreachable: "+err.Error()
		return obs, nil
	}
	a.recheckTrust(rr, now)
	ts := rr.ts
	ts.mu.Lock()
	defer ts.mu.Unlock()
	a.maybeRetry(h, rr, now)
	obs.NativeRef = &contracts.NativeRef{Kind: "codex_thread", ID: ts.threadID}
	obs.Activity = ts.activity
	obs.Doing = ts.doingNow() // the daemon keeps it only while the run is running
	obs.LimitEvent = ts.limit
	pending := false
	order := ts.order
	if len(order) > 50 {
		order = order[len(order)-50:]
	}
	for _, rid := range order {
		ap := ts.approvals[rid].approval
		if ap.State == contracts.ApprovalPending {
			pending = true
		}
		obs.Approvals = append(obs.Approvals, ap)
	}
	switch st := ts.status; st.Type {
	case "idle":
		obs.State, obs.StateReason = contracts.RunWaitingInput, "thread idle"
	case "notLoaded":
		obs.State, obs.StateReason = contracts.RunWaitingInput, "thread idle (unloaded by app-server; resumed on next input)"
	case "active":
		obs.State, obs.StateReason = contracts.RunRunning, "turn active"
		for _, f := range st.ActiveFlags {
			if f == "waitingOnApproval" {
				obs.State, obs.StateReason = contracts.RunWaitingApproval, "waiting on approval"
			}
			if f == "waitingOnUserInput" {
				obs.State, obs.StateReason = contracts.RunWaitingApproval, "waiting on user input (question)"
			}
		}
	case "systemError":
		// The last turn failed; the thread is idle and takes input.
		obs.State, obs.StateReason = contracts.RunWaitingInput, "last turn failed"
		if e := ts.lastErr; e != nil {
			obs.StateReason = "last turn failed: " + oneLine(e.Message, 160)
		}
		if !ts.retryAt.IsZero() {
			obs.StateReason += fmt.Sprintf(" · retry %d/%d at %s", ts.capRetries+1, len(capBackoff), ts.retryAt.Local().Format("15:04"))
		}
	default:
		obs.State, obs.StateReason = contracts.RunUnknown, "thread status not yet known"
	}
	if pending && obs.State != contracts.RunWaitingApproval {
		obs.State, obs.StateReason = contracts.RunWaitingApproval, "approval pending"
	}
	for _, id := range ts.subOrder {
		s := ts.subagents[id]
		so := contracts.SubagentObservation{ID: s.ThreadID, Kind: s.Kind, State: "running"}
		st := s.Start
		so.StartedAt = &st
		if !s.End.IsZero() {
			en := s.End
			so.EndedAt, so.State = &en, "stopped"
		}
		obs.Subagents = append(obs.Subagents, so)
	}
	// Receipts: userMessage evidence newer than the cursor for attempted messages.
	var since int64
	fmt.Sscanf(cursor, "s%d", &since)
	attempts := lastAttempts(h.StateDir)
	for _, ev := range ts.evidence {
		att, ok := attempts[ev.MessageID]
		if !ok || ev.Seq <= since {
			continue
		}
		obs.Receipts = append(obs.Receipts, contracts.DeliveryReceipt{
			MessageID: ev.MessageID, AttemptID: att.AttemptID, SessionID: h.SessionID, RunID: h.RunID, NodeID: a.o.NodeID,
			State: contracts.DeliveryAccepted, Evidence: contracts.EvidenceCodexUserMessage, EvidenceRef: ev.ItemID,
			Detail: "turn " + ev.TurnID, ObservedAt: ev.At,
		})
	}
	obs.NextCursor = fmt.Sprintf("s%d", ts.seq)
	return obs, nil
}

func (a *Adapter) evidenceOf(rr *runRef, msgID string) (evidence, bool) {
	rr.ts.mu.Lock()
	defer rr.ts.mu.Unlock()
	ev, ok := rr.ts.evidence[msgID]
	return ev, ok
}

func renderInput(in contracts.DeliveryInput) string {
	if header.HasMarker(in.Text, in.MessageID) {
		return in.Text
	}
	return contracts.MessageMarker(in.MessageID) + " " + in.Text
}

var safeIDRe = regexp.MustCompile(`^[A-Za-z0-9_.:\-]{1,200}$`)

// Deliver: idle → turn/start (clientUserMessageId = message id); busy →
// queued, or turn/steer with expectedTurnId for ModeSteer. Accepted only
// with the userMessage item echoing the clientId, or the turn id returned by
// turn/start/turn/steer (protocol ack). Codex does not dedupe the clientId,
// so the thread is searched before every send.
func (a *Adapter) Deliver(ctx context.Context, h contracts.RunHandle, in contracts.DeliveryInput) (contracts.DeliveryResult, error) {
	if !safeIDRe.MatchString(in.MessageID) {
		return contracts.DeliveryResult{}, contracts.Errorf(contracts.ErrInvalid, "invalid message id %q", in.MessageID)
	}
	if in.Mode == contracts.ModeInfo {
		return contracts.DeliveryResult{}, contracts.Errorf(contracts.ErrInvalid, "info messages are never injected into a runtime")
	}
	if stopped(h.StateDir) {
		return result(contracts.DeliveryRejected, "", "", "run stopped"), nil
	}
	rr, err := a.run(h)
	if err != nil {
		return contracts.DeliveryResult{}, err
	}
	rr.deliverMu.Lock()
	defer rr.deliverMu.Unlock()
	c, err := a.client(ctx, rr, true)
	if err != nil {
		return contracts.DeliveryResult{}, contracts.Errorf(contracts.ErrUnavailable, "%v", err)
	}
	if ev, ok := a.evidenceOf(rr, in.MessageID); ok {
		return result(contracts.DeliveryAccepted, contracts.EvidenceCodexUserMessage, ev.ItemID, "already accepted (not re-sent)"), nil
	}
	if last, ok := lastAttempts(h.StateDir)[in.MessageID]; ok && last.State != jRejected {
		if ev, found := a.scanTurns(ctx, c, rr, 10, in.MessageID); found {
			return result(contracts.DeliveryAccepted, contracts.EvidenceCodexUserMessage, ev.ItemID, "found earlier attempt in thread (not re-sent)"), nil
		}
		if last.State == jAccepted {
			return result(contracts.DeliveryAccepted, contracts.EvidenceCategory(last.Evidence), last.Ref, "accepted earlier (not re-sent)"), nil
		}
		if last.State != jQueued {
			return result(contracts.DeliveryIndeterminate, "", "", "an earlier attempt has unknown outcome; reconcile/resolve before resending"), nil
		}
	}
	rr.ts.mu.Lock()
	status, active := rr.ts.status, rr.ts.activeTurn
	pendingApproval := false
	for _, pa := range rr.ts.approvals {
		if pa.approval.State == contracts.ApprovalPending {
			pendingApproval = true
		}
	}
	rr.ts.mu.Unlock()
	text := renderInput(in)
	input := []userInput{{Type: "text", Text: text}}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if status.Type == "active" || pendingApproval {
		if in.Mode != contracts.ModeSteer || active == "" || pendingApproval {
			why := "thread busy (turn " + active + " active)"
			if pendingApproval {
				why = "approval pending"
			}
			return result(contracts.DeliveryQueued, "", "", why), nil
		}
		if err := a.journal(h, journalRec{MessageID: in.MessageID, AttemptID: in.AttemptID, State: jAttempting, Mode: "turn/steer", Ref: active}); err != nil {
			return contracts.DeliveryResult{}, err
		}
		var sr turnSteerResponse
		err := c.call(cctx, "turn/steer", turnSteerParams{ThreadID: rr.meta.ThreadID, ExpectedTurnID: active, Input: input, ClientUserMessageID: in.MessageID}, &sr)
		return a.afterSend(ctx, h, rr, in, err, sr.TurnID, "steered into turn")
	}
	if err := a.journal(h, journalRec{MessageID: in.MessageID, AttemptID: in.AttemptID, State: jAttempting, Mode: "turn/start"}); err != nil {
		return contracts.DeliveryResult{}, err
	}
	var tr turnStartResponse
	err = c.call(cctx, "turn/start", turnStartParams{ThreadID: rr.meta.ThreadID, Input: input, ClientUserMessageID: in.MessageID}, &tr)
	detail := "turn started"
	if active != "" && tr.Turn.ID == active {
		detail = "joined the active turn (turn/start on a busy thread steers)"
	}
	return a.afterSend(ctx, h, rr, in, err, tr.Turn.ID, detail)
}

func result(s contracts.RuntimeDeliveryState, ev contracts.EvidenceCategory, ref, detail string) contracts.DeliveryResult {
	if ev == "" {
		ev = contracts.EvidenceNone
	}
	return contracts.DeliveryResult{State: s, Evidence: ev, EvidenceRef: ref, Detail: detail}
}

func (a *Adapter) afterSend(ctx context.Context, h contracts.RunHandle, rr *runRef, in contracts.DeliveryInput, err error, turnID, detail string) (contracts.DeliveryResult, error) {
	if err != nil {
		var re *rpcError
		if errors.As(err, &re) {
			_ = a.journal(h, journalRec{MessageID: in.MessageID, AttemptID: in.AttemptID, State: jRejected, Detail: re.Error()})
			return result(contracts.DeliveryRejected, "", "", "refused by codex: "+re.Message+" (safe to retry)"), nil
		}
		_ = a.journal(h, journalRec{MessageID: in.MessageID, AttemptID: in.AttemptID, State: jIndeterminate, Detail: err.Error()})
		return result(contracts.DeliveryIndeterminate, "", "", "request sent but outcome unknown: "+err.Error()+"; reconcile before any retry"), nil
	}
	end := time.Now().Add(a.o.DeliverTimeout)
	for time.Now().Before(end) && ctx.Err() == nil {
		if ev, ok := a.evidenceOf(rr, in.MessageID); ok {
			_ = a.journal(h, journalRec{MessageID: in.MessageID, AttemptID: in.AttemptID, State: jAccepted, Evidence: string(contracts.EvidenceCodexUserMessage), Ref: ev.ItemID})
			return result(contracts.DeliveryAccepted, contracts.EvidenceCodexUserMessage, ev.ItemID, detail), nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = a.journal(h, journalRec{MessageID: in.MessageID, AttemptID: in.AttemptID, State: jAccepted, Evidence: string(contracts.EvidenceProtocolAck), Ref: turnID})
	return result(contracts.DeliveryAccepted, contracts.EvidenceProtocolAck, turnID, detail+"; userMessage item not yet seen (receipt follows via Observe)"), nil
}

// Reconcile searches the thread for the clientId before any retry.
func (a *Adapter) Reconcile(ctx context.Context, h contracts.RunHandle, messageID string) (contracts.DeliveryResult, error) {
	if !safeIDRe.MatchString(messageID) {
		return contracts.DeliveryResult{}, contracts.Errorf(contracts.ErrInvalid, "invalid message id %q", messageID)
	}
	rr, err := a.run(h)
	if err != nil {
		return contracts.DeliveryResult{}, err
	}
	rr.deliverMu.Lock()
	defer rr.deliverMu.Unlock()
	if ev, ok := a.evidenceOf(rr, messageID); ok {
		return result(contracts.DeliveryAccepted, contracts.EvidenceCodexUserMessage, ev.ItemID, "reconciled"), nil
	}
	c, err := a.client(ctx, rr, true)
	if err != nil {
		return result(contracts.DeliveryIndeterminate, "", "", "app-server unavailable: "+err.Error()), nil
	}
	if ev, ok := a.scanTurns(ctx, c, rr, 20, messageID); ok {
		_ = a.journal(h, journalRec{MessageID: messageID, State: jAccepted, Evidence: string(contracts.EvidenceCodexUserMessage), Ref: ev.ItemID, Detail: "reconciled"})
		return result(contracts.DeliveryAccepted, contracts.EvidenceCodexUserMessage, ev.ItemID, "reconciled from thread/turns/list"), nil
	}
	last, ok := lastAttempts(h.StateDir)[messageID]
	switch {
	case !ok, last.State == jQueued:
		return result(contracts.DeliveryRejected, "", "", "no attempt recorded (never sent); safe to deliver"), nil
	case last.State == jRejected:
		return result(contracts.DeliveryRejected, "", "", "earlier attempt refused by codex: "+last.Detail), nil
	}
	return result(contracts.DeliveryIndeterminate, "", "", "attempt "+last.AttemptID+" has no userMessage with this clientId yet; may still land — resolve explicitly"), nil
}

func (a *Adapter) Attach(ctx context.Context, h contracts.RunHandle, req contracts.AttachCommand) (contracts.AttachDescriptor, error) {
	if a.o.DisableTUI {
		return contracts.AttachDescriptor{}, contracts.Errorf(contracts.ErrUnsupported, "native TUI disabled for this node")
	}
	rr, err := a.run(h)
	if err != nil {
		return contracts.AttachDescriptor{}, err
	}
	c, err := a.client(ctx, rr, true)
	if err != nil {
		return contracts.AttachDescriptor{}, contracts.Errorf(contracts.ErrUnavailable, "%v", err)
	}
	p, err := a.srv.Pane(ctx, h.RunID)
	if err != nil || p.Dead {
		a.ensureTrust(ctx, c, rr)
	}
	switch {
	case err != nil:
		if err := a.startTUI(ctx, rr, nil, rr.meta.Unset); err != nil {
			return contracts.AttachDescriptor{}, contracts.Errorf(contracts.ErrUnavailable, "start TUI: %v", err)
		}
	case p.Dead: // the human quit the TUI; the thread lives on in the app-server
		if err := a.respawnTUI(ctx, rr); err != nil {
			return contracts.AttachDescriptor{}, contracts.Errorf(contracts.ErrUnavailable, "respawn TUI: %v", err)
		}
	}
	d, err := a.srv.AttachDescriptor(sessionhost.AttachConfig{NodeID: a.o.NodeID, SSHAlias: a.o.SSHAlias},
		h.RunID, h.RunID, req, a.o.Now())
	if err == nil {
		d.Notes = "Codex TUI on the Pyre app-server (codex resume --remote); quitting it or detaching (Ctrl-b d) leaves the thread running"
	}
	return d, err
}

func (a *Adapter) receipt(o contracts.CommandOutcome, accepted bool, detail string) contracts.CommandReceipt {
	return contracts.CommandReceipt{Accepted: accepted, Outcome: o, Detail: detail, ObservedAt: a.o.Now()}
}

// Interrupt: turn/interrupt (bounded; interrupting a finished turn never
// answers), then thread/backgroundTerminals/clean because commands keep
// running after an interrupt.
func (a *Adapter) Interrupt(ctx context.Context, h contracts.RunHandle) (contracts.CommandReceipt, error) {
	rr, err := a.run(h)
	if err != nil {
		return a.receipt(contracts.OutcomeFailed, false, err.Error()), nil
	}
	c, err := a.client(ctx, rr, false)
	if err != nil {
		return a.receipt(contracts.OutcomeFailed, false, err.Error()), nil
	}
	rr.ts.mu.Lock()
	active, done := rr.ts.activeTurn, rr.ts.turnDone
	rr.ts.mu.Unlock()
	if active == "" {
		return a.receipt(contracts.OutcomeSucceeded, true, "nothing to interrupt"), nil
	}
	ictx, cancel := context.WithTimeout(ctx, a.o.InterruptTimeout)
	ierr := c.call(ictx, "turn/interrupt", turnInterruptParams{ThreadID: rr.meta.ThreadID, TurnID: active}, nil)
	cancel()
	completed := false
	select {
	case <-done:
		completed = true
	case <-time.After(a.o.InterruptTimeout):
	}
	cctx, cancel2 := context.WithTimeout(ctx, a.o.InterruptTimeout)
	cerr := c.call(cctx, "thread/backgroundTerminals/clean", threadIDParams{ThreadID: rr.meta.ThreadID}, nil)
	cancel2()
	detail := fmt.Sprintf("turn/interrupt err=%v; backgroundTerminals/clean err=%v", ierr, cerr)
	if completed {
		return a.receipt(contracts.OutcomeSucceeded, true, "turn interrupted; "+detail), nil
	}
	return a.receipt(contracts.OutcomeIndeterminate, true, "turn completion not observed; "+detail), nil
}

// Stop interrupts an active turn, unsubscribes, closes the TUI and releases
// the home's app-server ref (SIGTERM when no other run uses it).
func (a *Adapter) Stop(ctx context.Context, h contracts.RunHandle) (contracts.CommandReceipt, error) {
	rr, err := a.run(h)
	if err != nil {
		return a.receipt(contracts.OutcomeFailed, false, err.Error()), nil
	}
	c, cerr := a.client(ctx, rr, false)
	if cerr == nil {
		rr.ts.mu.Lock()
		active := rr.ts.activeTurn
		rr.ts.mu.Unlock()
		if active != "" {
			_, _ = a.Interrupt(ctx, h)
		}
		uctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_ = c.call(uctx, "thread/unsubscribe", threadIDParams{ThreadID: rr.meta.ThreadID}, nil)
		cancel()
	}
	_ = a.srv.TerminateSession(ctx, h.RunID, 3*time.Second)
	if cerr == nil {
		// Unsubscribing leaves the thread loaded with its MCP servers and
		// background terminals; with the TUI gone, unload it (unload.go).
		_ = unloadThread(ctx, c, rr.meta.ThreadID)
	}
	b, _ := json.Marshal(map[string]any{"at": a.o.Now().UTC(), "thread_id": rr.meta.ThreadID})
	_ = writeFileAtomic(filepath.Join(h.StateDir, "stopped.json"), b)
	_ = os.Remove(filepath.Join(h.StateDir, threadConfigFile)) // the run's session token is no longer needed
	rr.hs.unregister(rr.meta.ThreadID)
	remaining := rr.hs.dropRef(h.RunID)
	detail := "thread unsubscribed; TUI closed"
	if remaining == 0 {
		rr.hs.stop(ctx)
		detail += "; app-server stopped (no other runs on this home)"
	} else {
		detail += fmt.Sprintf("; app-server kept (%d other ref(s))", remaining)
	}
	a.mu.Lock()
	delete(a.runs, h.RunID)
	a.mu.Unlock()
	return a.receipt(contracts.OutcomeSucceeded, true, detail), nil
}

// Answer responds once to the app-server request. serverRequest/resolved is
// the truth: if it arrives first the approval is superseded.
func (a *Adapter) Answer(ctx context.Context, h contracts.RunHandle, ap contracts.Approval, optionID, text string) (contracts.CommandReceipt, error) {
	if ap.RunID != "" && ap.RunID != h.RunID {
		return a.receipt(contracts.OutcomeFailed, false, ""), contracts.Errorf(contracts.ErrConflict, "approval belongs to run %s", ap.RunID)
	}
	rr, err := a.run(h)
	if err != nil {
		return a.receipt(contracts.OutcomeFailed, false, ""), err
	}
	c, err := a.client(ctx, rr, false)
	if err != nil {
		return a.receipt(contracts.OutcomeFailed, false, ""), contracts.Errorf(contracts.ErrUnavailable, "%v", err)
	}
	rr.ts.mu.Lock()
	pa := rr.ts.approvals[ap.RequestID]
	if pa == nil {
		rr.ts.mu.Unlock()
		return a.receipt(contracts.OutcomeFailed, false, ""), contracts.Errorf(contracts.ErrNotFound, "no request %q on run %s (app-server restarted? pending requests are re-sent with new ids)", ap.RequestID, h.RunID)
	}
	if pa.approval.State != contracts.ApprovalPending || !pa.submitted.IsZero() {
		st := pa.approval.State
		rr.ts.mu.Unlock()
		return a.receipt(contracts.OutcomeFailed, false, "already "+string(st)), contracts.Errorf(contracts.ErrConflict, "request already %s", st)
	}
	var resp json.RawMessage
	if pa.method == "item/tool/requestUserInput" {
		if optionID == "" && text == "" {
			rr.ts.mu.Unlock()
			return a.receipt(contracts.OutcomeFailed, false, ""), contracts.Errorf(contracts.ErrInvalid, "an option or text is required")
		}
		resp = questionAnswer(pa.params, optionID, text)
	} else {
		switch optionID { // accept Claude-style aliases too
		case "allow":
			optionID = "accept"
		case "deny":
			optionID = "decline"
		}
		resp = pa.decisions[optionID]
		if resp == nil {
			rr.ts.mu.Unlock()
			return a.receipt(contracts.OutcomeFailed, false, ""), contracts.Errorf(contracts.ErrInvalid, "option %q not offered", optionID)
		}
	}
	pa.submitted, pa.option = a.o.Now(), optionID
	rpcID := pa.rpcID
	rr.ts.mu.Unlock()
	if err := c.respond(ctx, rpcID, resp); err != nil {
		rr.ts.mu.Lock()
		pa.submitted = time.Time{}
		rr.ts.mu.Unlock()
		return a.receipt(contracts.OutcomeIndeterminate, true, "send failed: "+err.Error()), nil
	}
	end := time.Now().Add(3 * time.Second)
	for time.Now().Before(end) {
		rr.ts.mu.Lock()
		st, res := pa.approval.State, pa.approval.Resolution
		rr.ts.mu.Unlock()
		if st == contracts.ApprovalResolved && res != nil {
			return a.receipt(contracts.OutcomeSucceeded, true, "effect="+res.Effect+"; serverRequest/resolved observed"), nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return a.receipt(contracts.OutcomeDelivered, true, "effect=submitted; serverRequest/resolved not yet observed"), nil
}

func (a *Adapter) ReadTranscript(ctx context.Context, h contracts.RunHandle, req contracts.ReadSessionRequest) (contracts.ReadSessionResponse, error) {
	resp := contracts.ReadSessionResponse{Freshness: contracts.Freshness{ObservedAt: a.o.Now(), Source: "node:" + a.o.NodeID}}
	rr, err := a.run(h)
	if err != nil {
		return resp, err
	}
	c, err := a.client(ctx, rr, false)
	if err != nil {
		resp.Freshness.Stale, resp.Freshness.Reason = true, err.Error()
		return resp, nil
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	var start int64
	fmt.Sscanf(req.Cursor, "%d", &start)
	rr.readMu.Lock()
	sel, gap, stale := a.readTurns(ctx, c, rr, h.RunID, start, limit, req.Tail)
	rr.readMu.Unlock()
	resp.Entries, resp.Gap = sel, gap
	if stale != nil {
		resp.Freshness.Stale, resp.Freshness.Reason = true, stale.Error()
	}
	resp.NextCursor = req.Cursor
	if len(sel) > 0 {
		resp.NextCursor = fmt.Sprintf("%d", sel[len(sel)-1].Seq)
	}
	if !a.o.DisableTUI {
		if s, err := a.srv.CapturePane(ctx, h.RunID, tmux.CaptureOptions{Join: true}); err == nil {
			resp.Screen = strings.TrimRight(s, "\n")
		}
	}
	return resp, nil
}

// Settled turn pages for sessions.read. The thread's turns are listed
// oldest first in pages of turnsPage; entries are numbered from the first
// turn (Seq counts items), so a read used to fetch every page (up to 20,
// full items) each time. A page whose turns have all finished does not
// change, and the app-server's cursor is anchored to a turn, so the read
// keeps the cursor after the last such page, the item count before it and
// the newest tailWindow entries before it, and fetches only the pages
// after. The kept state belongs to one app-server connection and is
// dropped when a fetch from its cursor fails.
const tailWindow = 500 // newest settled entries kept per run: the largest page sessions.read serves

type turnCache struct {
	client *rpcClient
	thread string
	cursor string                      // after the last settled page
	seq    int64                       // items before cursor
	win    []contracts.TranscriptEntry // newest entries before cursor, ascending Seq
	floor  int64                       // every entry with Seq <= seq and > floor is in win
}

func (a *Adapter) pageSize() int {
	if a.turnsPage > 0 {
		return a.turnsPage
	}
	return 50
}

// readTurns returns the entries with Seq > start (the newest limit when
// tail, else the oldest limit), whether pages were left unread, and the
// error that cut the listing short. Callers hold rr.readMu.
func (a *Adapter) readTurns(ctx context.Context, c *rpcClient, rr *runRef, runID string, start int64, limit int, tail bool) ([]contracts.TranscriptEntry, bool, error) {
	tc := rr.turns
	if tc != nil && (tc.client != c || tc.thread != rr.meta.ThreadID) {
		tc, rr.turns = nil, nil
	}
	// The kept entries answer when they hold everything after start, or
	// (tail) at least limit entries newer than start.
	if tc != nil && start < tc.floor && (!tail || len(tc.win) < limit) {
		tc = nil
	}
	next := &turnCache{client: c, thread: rr.meta.ThreadID}
	var all []contracts.TranscriptEntry
	if tc != nil {
		*next = *tc
		all = append(all, tc.win...)
	}
	cursor, seq := next.cursor, next.seq
	settled := true // every page so far was full and finished
	var stale error
	gap := false
	for i := 0; i < 20; i++ {
		var r turnsListResponse
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := c.call(cctx, "thread/turns/list", turnsListParams{ThreadID: rr.meta.ThreadID, Cursor: cursor, Limit: a.pageSize(), ItemsView: "full", SortDirection: "asc"}, &r)
		cancel()
		if err != nil {
			if tc != nil && i == 0 {
				// The kept cursor may no longer exist: list from the start.
				rr.turns = nil
				return a.readTurns(ctx, c, rr, runID, start, limit, tail)
			}
			stale = err
			break
		}
		page := a.turnEntries(runID, r.Data, &seq)
		all = append(all, page...)
		if settled && r.NextCursor != "" && len(r.Data) > 0 && allSettled(r.Data) {
			next.cursor, next.seq = r.NextCursor, seq
			next.win = append(next.win, page...)
			if n := len(next.win); n > tailWindow {
				next.floor = next.win[n-tailWindow-1].Seq
				next.win = append([]contracts.TranscriptEntry(nil), next.win[n-tailWindow:]...)
			}
		} else {
			settled = false
		}
		if r.NextCursor == "" || len(r.Data) == 0 {
			break
		}
		if i == 19 {
			gap = true
		}
		cursor = r.NextCursor
	}
	if next.cursor != "" {
		rr.turns = next
	}
	var sel []contracts.TranscriptEntry
	for _, e := range all {
		if e.Seq > start {
			sel = append(sel, e)
		}
	}
	if tail && len(sel) > limit {
		sel = sel[len(sel)-limit:]
	} else if len(sel) > limit {
		sel = sel[:limit]
	}
	return sel, gap, stale
}

func allSettled(ts []turn) bool {
	for _, t := range ts {
		if t.Status == "" || t.Status == "inProgress" {
			return false
		}
	}
	return true
}

// turnEntries converts a page of turns; seq counts every decodable item.
func (a *Adapter) turnEntries(runID string, turns []turn, seq *int64) []contracts.TranscriptEntry {
	var out []contracts.TranscriptEntry
	for _, t := range turns {
		// Entries are stamped with their turn's start (items have no time
		// of their own), not with the read time.
		at := a.o.Now()
		if t.StartedAt != nil && *t.StartedAt > 0 {
			at = time.Unix(*t.StartedAt, 0)
		}
		for _, raw := range t.Items {
			var it item
			if json.Unmarshal(raw, &it) != nil {
				continue
			}
			*seq++
			e := contracts.TranscriptEntry{Seq: *seq, RunID: runID, At: at}
			switch it.Type {
			case "userMessage":
				e.Kind, e.Text = "user", it.userText()
				if strings.HasPrefix(e.Text, "[pyre-msg:") || it.ClientID != "" && safeIDRe.MatchString(it.ClientID) && header.HasMarker(e.Text, it.ClientID) {
					e.Kind, e.MessageID = "pyre_message", it.ClientID
				}
			case "agentMessage":
				e.Kind, e.Text = "assistant", it.Text
			case "commandExecution":
				e.Kind, e.Text = "tool", "command: "+it.Command
				if it.ExitCode != nil {
					e.Text += fmt.Sprintf(" (exit %d)", *it.ExitCode)
				}
			case "fileChange":
				var ps []string
				for _, ch := range it.Changes {
					ps = append(ps, ch.Path)
				}
				e.Kind, e.Text = "tool", "file change: "+strings.Join(ps, ", ")
			case "subAgentActivity":
				e.Kind, e.Text = "system", "subagent "+it.Kind+": "+it.AgentThreadID
			case "mcpToolCall": // e.g. the pyre MCP tools (arguments not shown)
				e.Kind, e.Text = "tool", "mcp: "+it.Server+"."+it.Tool
				if it.Status != "" {
					e.Text += " (" + it.Status + ")"
				}
			default:
				continue
			}
			if len(e.Text) > 2000 {
				e.Text, e.Truncated = oneLine(e.Text, 2000), true
			}
			out = append(out, e)
		}
	}
	return out
}

// ResumeSpec: thread ids are stable across app-server restarts, so the same
// account resumes natively. Another CODEX_HOME may not hold the rollout
// (cross-account resume unproven) → handoff.
func (a *Adapter) ResumeSpec(ctx context.Context, prev contracts.Run, sameAccount bool) (contracts.ContinuationKind, *contracts.NativeRef, error) {
	if prev.NativeRef == nil || prev.NativeRef.Kind != "codex_thread" || prev.NativeRef.ID == "" || !sameAccount {
		return contracts.ContinuationHandoff, nil, nil
	}
	ref := *prev.NativeRef
	return contracts.ContinuationNativeResume, &ref, nil
}

// ReadRateLimits implements accounts.CodexRateLimitReader: it calls
// account/rateLimits/read through the single Pyre app-server for codexHome
// (starting it under a temporary ref if no run uses the home, and stopping
// it again afterwards). It never asks for a token refresh and returns the
// raw result object.
func (a *Adapter) ReadRateLimits(ctx context.Context, codexHome string) (json.RawMessage, error) {
	var raw json.RawMessage
	err := a.withHome(ctx, codexHome, func(c *rpcClient) error {
		return c.call(ctx, "account/rateLimits/read", nil, &raw)
	})
	return raw, err
}

// RefreshLogin asks the Pyre app-server for codexHome to refresh its own
// ChatGPT login (account/read {refreshToken:true}: "requests a proactive
// token refresh before returning" in managed auth mode). Call it only for a
// home that currently owns the login (a checked-out rota profile): the
// refresh rotates the refresh token, which accounts writes back to rota.
func (a *Adapter) RefreshLogin(ctx context.Context, codexHome string) error {
	return a.withHome(ctx, codexHome, func(c *rpcClient) error {
		var r json.RawMessage
		return c.call(ctx, "account/read", map[string]bool{"refreshToken": true}, &r)
	})
}

// AccountInfo is the credential-free subset of account/read.
type AccountInfo struct {
	Type               string `json:"type,omitempty"`      // chatgpt | apiKey | amazonBedrock
	PlanType           string `json:"plan_type,omitempty"` // e.g. pro, plus
	RequiresOpenaiAuth bool   `json:"requires_openai_auth"`
	LoggedIn           bool   `json:"logged_in"`
}

// ReadAccount calls account/read {refreshToken:false} and drops identity
// fields (email) and anything token-like.
func (a *Adapter) ReadAccount(ctx context.Context, codexHome string) (AccountInfo, error) {
	var r struct {
		Account *struct {
			Type     string `json:"type"`
			PlanType string `json:"planType"`
		} `json:"account"`
		RequiresOpenaiAuth bool `json:"requiresOpenaiAuth"`
	}
	err := a.withHome(ctx, codexHome, func(c *rpcClient) error {
		return c.call(ctx, "account/read", map[string]bool{"refreshToken": false}, &r)
	})
	if err != nil {
		return AccountInfo{}, err
	}
	ai := AccountInfo{RequiresOpenaiAuth: r.RequiresOpenaiAuth}
	if r.Account != nil {
		ai.Type, ai.PlanType, ai.LoggedIn = r.Account.Type, r.Account.PlanType, true
	}
	return ai, nil
}

// HomeBusy reports whether Pyre's app-server for the CODEX_HOME at
// profileDir is running. It may outlive the last run briefly, so a rota
// check-in waits until it reports false before moving the login back.
func (a *Adapter) HomeBusy(ctx context.Context, profileDir string) (bool, error) {
	hs, err := a.homeServer(profileDir)
	if err != nil {
		return false, err
	}
	if ok, _ := hs.running(ctx); ok {
		return true, nil
	}
	// App-servers of other pinned codex versions on the same home.
	root := filepath.Join(a.o.StateRoot, "codex")
	ents, _ := os.ReadDir(root)
	for _, e := range ents {
		if !e.IsDir() || e.Name() == hs.key {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, e.Name(), homeFile))
		if err != nil || string(b) != profileDir {
			continue
		}
		if p, err := a.srv.Pane(ctx, "codex-as-"+e.Name()); err == nil && !p.Dead {
			return true, nil
		}
	}
	return false, nil
}

func (a *Adapter) withHome(ctx context.Context, home string, fn func(*rpcClient) error) error {
	hs, err := a.homeServer(home)
	if err != nil {
		return err
	}
	ref := fmt.Sprintf("probe-%d", time.Now().UnixNano())
	if err := hs.addRef(ref); err != nil {
		return err
	}
	defer hs.release(context.WithoutCancel(ctx), ref)
	c, err := hs.ensure(ctx, nil)
	if err != nil {
		return err
	}
	return fn(c)
}

// Journal (fsync before every send).
const (
	jAttempting    = "attempting"
	jAccepted      = "accepted"
	jRejected      = "rejected"
	jIndeterminate = "indeterminate"
	jQueued        = "queued"
)

type journalRec struct {
	At        time.Time `json:"at"`
	MessageID string    `json:"message_id"`
	AttemptID string    `json:"attempt_id,omitempty"`
	RunID     string    `json:"run_id"`
	State     string    `json:"state"`
	Mode      string    `json:"mode,omitempty"`
	Evidence  string    `json:"evidence,omitempty"`
	Ref       string    `json:"ref,omitempty"`
	Detail    string    `json:"detail,omitempty"`
}

func (a *Adapter) journal(h contracts.RunHandle, r journalRec) error {
	r.At, r.RunID = a.o.Now().UTC(), h.RunID
	b, _ := json.Marshal(r)
	// O_RDWR, not O_WRONLY: Go opens an O_APPEND file on Windows with
	// append-only access, and LockFileEx then fails (access denied), so the
	// lock below never succeeded and nothing was written (GCP Windows node).
	f, err := os.OpenFile(filepath.Join(h.StateDir, "deliveries.jsonl"), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := osutil.Lock(f); err != nil {
		return err
	}
	defer osutil.Unlock(f)
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

func lastAttempts(stateDir string) map[string]journalRec {
	out := map[string]journalRec{}
	b, err := os.ReadFile(filepath.Join(stateDir, "deliveries.jsonl"))
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		var r journalRec
		if json.Unmarshal([]byte(line), &r) != nil {
			continue
		}
		if r.AttemptID == "" {
			r.AttemptID = out[r.MessageID].AttemptID
		}
		out[r.MessageID] = r
	}
	return out
}

func writeFileAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
