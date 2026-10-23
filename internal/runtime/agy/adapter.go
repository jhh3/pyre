// Package agy is the runtime adapter for Google Antigravity's CLI (agy),
// validated with agy 1.4.2 on Linux. agy runs with its real TUI inside the
// node's isolated tmux server, like the Claude adapter:
//
//   - Per-run customizations travel in a Pyre-owned workspace folder given
//     with --add-dir (agy loads .agents/ from every workspace folder):
//     hooks.json records lifecycle hooks into the run state dir (HookMain),
//     mcp_config.json registers the pyre MCP server (agent tools as the
//     session), and rules/pyre.md carries the agent guide. Nothing is
//     written to the owner's ~/.gemini config or the project.
//   - Input is bracketed paste into the idle prompt with a [pyre-msg:<id>]
//     marker; acceptance is the marker in a USER_INPUT step of the
//     conversation's transcript_full.jsonl. A busy run queues (Pyre retries
//     when the turn ends); agy's own input queue is not used.
//   - Run state comes from the hooks (PreInvocation ... Stop) and the
//     screen footer ("esc to cancel" / "? for shortcuts"): an interrupt
//     (Escape) fires no hook.
//   - The native id is the agy conversation id (hook payloads); resume is
//     `agy --conversation <id>`.
//
// agy has no config-dir switch: runs use the node's own login (~/.gemini),
// so account.isolated_profile is not advertised. Tool permission prompts
// are answered in the terminal only (no approvals broker); a bypass run
// (--dangerously-skip-permissions) shows none.
package agy

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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/hostsel"
	"github.com/jhh3/pyre/internal/runtime/sessionhost"
	"github.com/jhh3/pyre/internal/runtime/tmux"
)

// AdapterName is the RunHandle.Adapter value.
const AdapterName = "agy-tmux"

// NativeKind is the NativeRef kind of an agy conversation.
const NativeKind = "agy_conversation"

// TestedVersions lists agy versions this adapter was validated with.
var TestedVersions = []string{"1.4.2"}

// PermissionsEnv carries the run's permission mode (daemon.PermissionsEnv).
const PermissionsEnv = "PYRE_PERMISSIONS"

// Options configures the agy adapter (one per node).
type Options struct {
	NodeID     string
	TmuxBin    string
	TmuxSocket string // the node's isolated tmux server socket (required)
	SSHAlias   string
	// AgyBin is used when ResolvedLaunch.Executable is empty (default "agy"
	// on PATH).
	AgyBin string
	// AgyHome is agy's state dir (default ~/.gemini/antigravity-cli): its
	// sign-in token's presence, conversation transcripts and settings are
	// read from it (never written, the token never read).
	AgyHome string
	// WorkspaceRoot holds the per-session customization folders passed with
	// --add-dir (<root>/<session-id>; required). One folder per session keeps
	// a resumed conversation's workspace path stable across runs.
	WorkspaceRoot string
	// HookCommand is the argv prefix of the hook commands; the adapter
	// appends "<Event> <state-dir>". Typically {pyred, "hook", "agy"}.
	// Required.
	HookCommand []string
	// MCPCommand registers the pyre MCP server ("pyre" key) in every run,
	// typically {pyre, "mcp"}; it inherits the session's environment. Nil:
	// no agent tools over MCP.
	MCPCommand []string
	// TrustWorkspace reports whether the owner trusts cwd, letting the
	// adapter accept agy's workspace-trust dialog. Nil: never.
	TrustWorkspace func(cwd string) bool
	// OnAutomation is told when the adapter answers a dialog by itself.
	OnAutomation func(contracts.NodeAutomation)
	// DeliverTimeout bounds the wait for acceptance evidence (default 20s).
	DeliverTimeout time.Duration
	// StartupWait bounds Launch's wait for the prompt (default 20s).
	StartupWait time.Duration
	// StopWait bounds the graceful /exit before kill-session (default 10s).
	StopWait time.Duration
	// ModelsTTL is how long `agy models` is cached (default 1h).
	ModelsTTL time.Duration
	Now       func() time.Time
}

// Adapter implements contracts.RuntimeAdapter for agy.
type Adapter struct {
	o   Options
	srv sessionhost.Host

	mu   sync.Mutex
	runs map[string]*runEntry

	automated  sync.Map
	modelCache modelCache
}

var _ contracts.RuntimeAdapter = (*Adapter)(nil)

type runEntry struct {
	mu        sync.Mutex // guards rs/meta
	deliverMu sync.Mutex // serializes input to the run
	rs        *runState
	meta      *runMeta
	tpath     atomic.Value // string: transcript path last read (ChangeStamp)
}

// runMeta is <state>/run.json.
type runMeta struct {
	RunID          string    `json:"run_id"`
	SessionID      string    `json:"session_id"`
	Cwd            string    `json:"cwd"`
	Workspace      string    `json:"workspace"`
	ConversationID string    `json:"conversation_id,omitempty"` // resumed conversation (new ones arrive with the hooks)
	Model          string    `json:"model,omitempty"`
	Effort         string    `json:"effort,omitempty"`
	Bypass         bool      `json:"bypass,omitempty"`
	LaunchedAt     time.Time `json:"launched_at"`
}

// New validates options. The tmux server starts lazily on first launch.
func New(o Options) (*Adapter, error) {
	if o.TmuxSocket == "" {
		return nil, errors.New("agy: TmuxSocket is required")
	}
	if len(o.HookCommand) == 0 {
		return nil, errors.New("agy: HookCommand is required")
	}
	if o.WorkspaceRoot == "" {
		return nil, errors.New("agy: WorkspaceRoot is required")
	}
	srv, err := hostsel.New(o.TmuxBin, o.TmuxSocket)
	if err != nil {
		return nil, err
	}
	if o.AgyHome == "" {
		if h, err := os.UserHomeDir(); err == nil {
			o.AgyHome = filepath.Join(h, ".gemini", "antigravity-cli")
		}
	}
	if o.DeliverTimeout <= 0 {
		o.DeliverTimeout = 20 * time.Second
	}
	if o.StartupWait <= 0 {
		o.StartupWait = 20 * time.Second
	}
	if o.StopWait <= 0 {
		o.StopWait = 10 * time.Second
	}
	if o.ModelsTTL <= 0 {
		o.ModelsTTL = time.Hour
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Adapter{o: o, srv: srv, runs: map[string]*runEntry{}}, nil
}

// Server exposes the session host (daemon housekeeping).
func (a *Adapter) Server() sessionhost.Host { return a.srv }

func (a *Adapter) Name() string               { return AdapterName }
func (a *Adapter) Harness() contracts.Harness { return contracts.HarnessAgy }

// Capabilities advertised by this adapter. Not supported: input.steer
// (agy's "send immediately" is not driven), input.idempotent,
// approvals.answer / questions.answer (answered in the terminal),
// subagents.observe, account.isolated_profile and
// account.cross_account_resume (one own login per node).
func (a *Adapter) Capabilities() []string {
	caps := []string{
		contracts.CapAttachNative, contracts.CapAttachReadOnly,
		contracts.CapInputQueue, contracts.CapInputReconcile, contracts.CapInputPaste,
		contracts.CapInterrupt, contracts.CapStop,
		contracts.CapResumeNative, contracts.CapResumeHandoff,
		contracts.CapTranscript, contracts.CapScreen, contracts.CapHooks,
		contracts.CapRequiresTrustDialog, contracts.CapLaunchEnvFile,
	}
	if len(a.o.MCPCommand) > 0 {
		caps = append(caps, contracts.CapAgentTools)
	}
	return caps
}

func (a *Adapter) bin(l *contracts.ResolvedLaunch) string {
	if l != nil && l.Executable != "" {
		return l.Executable
	}
	if a.o.AgyBin != "" {
		return a.o.AgyBin
	}
	if p, err := exec.LookPath("agy"); err == nil {
		return p
	}
	return "agy"
}

var versionRe = regexp.MustCompile(`\d+\.\d+\.\d+`)

// tokenFile is agy's sign-in token (only its presence is checked).
const tokenFile = "antigravity-oauth-token"

// Probe reports agy available when the binary runs and a sign-in exists.
// A node without agy simply does not offer the harness.
func (a *Adapter) Probe(ctx context.Context) (contracts.ProbeResult, error) {
	bin := a.bin(nil)
	pr := contracts.ProbeResult{Harness: contracts.HarnessAgy, Adapter: AdapterName, Executable: bin, Capabilities: a.Capabilities()}
	if runtime.GOOS == "windows" {
		// Not validated on Windows: hook commands are POSIX shell lines.
		pr.Reason, pr.Absent = "agy sessions are not supported on Windows yet", true
		return pr, nil
	}
	if _, err := exec.LookPath(bin); err != nil {
		pr.Reason, pr.Absent = "agy not installed (no agy on PATH)", true
		return pr, nil
	}
	if _, err := a.srv.Check(ctx); err != nil {
		pr.Reason = err.Error()
		return pr, nil
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	out, err := exec.CommandContext(cctx, bin, "--version").Output()
	cancel()
	if err != nil {
		pr.Reason = "agy --version failed: " + err.Error()
		return pr, nil
	}
	pr.Version = versionRe.FindString(string(out))
	if _, err := os.Stat(filepath.Join(a.o.AgyHome, tokenFile)); err != nil {
		pr.Reason = "agy is not signed in on this node (run agy once and sign in)"
		return pr, nil
	}
	pr.Available = true
	pr.Efforts = append([]string(nil), Efforts...)
	pr.Models = a.models(bin)
	tested := false
	for _, t := range TestedVersions {
		tested = tested || t == pr.Version
	}
	var notes []string
	if !tested {
		notes = append(notes, "untested agy version (tested: "+strings.Join(TestedVersions, ", ")+"); screen parsing may differ")
	}
	if runtime.GOOS != "linux" {
		notes = append(notes, "agy sessions are validated on Linux only")
	}
	pr.Reason = strings.Join(notes, "; ")
	return pr, nil
}

// reservedArgs are controlled by the adapter.
var reservedArgs = map[string]bool{
	"--conversation": true, "-c": true, "--continue": true, "-p": true, "--print": true, "--prompt": true,
	"-i": true, "--prompt-interactive": true, "--add-dir": true, "--output-format": true, "--input-format": true,
	"--remote-control": true, "--dangerously-skip-permissions": true, "--model": true, "--effort": true,
}

// Launch width/height of the run window.
const (
	launchWidth  = 200
	launchHeight = 50
)

func (a *Adapter) Launch(ctx context.Context, l contracts.ResolvedLaunch) (contracts.RunHandle, error) {
	if !tmux.ValidName(l.RunID) {
		return contracts.RunHandle{}, contracts.Errorf(contracts.ErrInvalid, "run id %q is not a valid tmux session name", l.RunID)
	}
	if l.StateDir == "" || l.Cwd == "" {
		return contracts.RunHandle{}, contracts.Errorf(contracts.ErrInvalid, "state dir and cwd are required")
	}
	if !filepath.IsAbs(l.StateDir) || !filepath.IsAbs(l.Cwd) {
		return contracts.RunHandle{}, contracts.Errorf(contracts.ErrInvalid, "state dir and cwd must be absolute")
	}
	if st, err := os.Stat(l.Cwd); err != nil || !st.IsDir() {
		return contracts.RunHandle{}, contracts.Errorf(contracts.ErrInvalid, "cwd %q is not a directory", l.Cwd)
	}
	for _, arg := range l.Args {
		name, _, _ := strings.Cut(arg, "=")
		if reservedArgs[name] {
			return contracts.RunHandle{}, contracts.Errorf(contracts.ErrInvalid, "argument %s is controlled by the agy adapter", name)
		}
	}
	if err := checkModelArgs(l.Model, l.Effort); err != nil {
		return contracts.RunHandle{}, err
	}
	conv := ""
	if l.Resume != nil {
		if l.Resume.Kind != NativeKind || !safeConversationID(l.Resume.ID) {
			return contracts.RunHandle{}, contracts.Errorf(contracts.ErrInvalid, "resume ref %+v is not an agy conversation id", *l.Resume)
		}
		conv = l.Resume.ID
	}
	if ok, err := a.srv.HasSession(ctx, l.RunID); err != nil {
		return contracts.RunHandle{}, contracts.Errorf(contracts.ErrUnavailable, "tmux: %v", err)
	} else if ok {
		return contracts.RunHandle{}, contracts.Errorf(contracts.ErrConflict, "run %s already has a live tmux session", l.RunID)
	}
	if err := tmux.PrepareDir(l.StateDir); err != nil {
		return contracts.RunHandle{}, err
	}
	ws, err := a.writeWorkspace(l)
	if err != nil {
		return contracts.RunHandle{}, err
	}
	bypass := l.Env[PermissionsEnv] == "bypass"
	argv := []string{a.bin(&l), "--add-dir", ws}
	if conv != "" {
		argv = append(argv, "--conversation", conv)
	}
	if bypass {
		argv = append(argv, "--dangerously-skip-permissions")
	}
	if l.Model != "" {
		argv = append(argv, "--model", l.Model)
	}
	if l.Effort != "" {
		argv = append(argv, "--effort", l.Effort)
	}
	argv = append(argv, l.Args...)
	if l.InitialPrompt != "" {
		argv = append(argv, "--prompt-interactive", l.InitialPrompt)
	}
	env := map[string]string{}
	for k, v := range l.Env {
		env[k] = v
	}
	if _, ok := env["PYRE_RUN_ID"]; !ok {
		env["PYRE_RUN_ID"] = l.RunID
	}
	if _, ok := env["PYRE_SESSION_ID"]; !ok && l.SessionID != "" {
		env["PYRE_SESSION_ID"] = l.SessionID
	}
	meta := runMeta{RunID: l.RunID, SessionID: l.SessionID, Cwd: l.Cwd, Workspace: ws, ConversationID: conv,
		Model: l.Model, Effort: l.Effort, Bypass: bypass, LaunchedAt: a.o.Now().UTC()}
	mb, _ := json.MarshalIndent(meta, "", "  ")
	if err := writeFileAtomic(filepath.Join(l.StateDir, fileRun), mb); err != nil {
		return contracts.RunHandle{}, err
	}
	_ = os.Remove(filepath.Join(l.StateDir, fileExit))
	if err := a.srv.NewSession(ctx, tmux.SessionSpec{Name: l.RunID, Cwd: l.Cwd, Env: env, Unset: l.Unset, Argv: argv, Width: launchWidth, Height: launchHeight}); err != nil {
		return contracts.RunHandle{}, contracts.Errorf(contracts.ErrUnavailable, "start agy in tmux: %v", err)
	}
	h := contracts.RunHandle{
		RunID: l.RunID, SessionID: l.SessionID, Adapter: AdapterName, Harness: contracts.HarnessAgy,
		TmuxSocket: a.srv.SocketPath(), TmuxTarget: l.RunID, StateDir: l.StateDir,
		Data: map[string]string{"workspace": ws},
	}
	if conv != "" {
		h.NativeRef = &contracts.NativeRef{Kind: NativeKind, ID: conv}
	}
	if bypass {
		h.Data["permissions"] = "bypass"
	}
	if p, err := a.srv.Pane(ctx, l.RunID); err == nil {
		h.PID = p.PID
	}
	a.waitStartup(ctx, h, l.InitialPrompt != "")
	if id := a.conversationID(h); id != "" {
		h.NativeRef = &contracts.NativeRef{Kind: NativeKind, ID: id}
	}
	return h, nil
}

func checkModelArgs(model, effort string) error {
	if model != "" && !contracts.ValidModelName(model) {
		return contracts.Errorf(contracts.ErrInvalid, "model %q is not a model name", model)
	}
	if effort != "" && !contracts.ValidEffort(effort) {
		return contracts.Errorf(contracts.ErrInvalid, "effort %q is not an effort level", effort)
	}
	return nil
}

// writeWorkspace (re)writes the session's customization folder:
// .agents/hooks.json, .agents/mcp_config.json and .agents/rules/pyre.md.
func (a *Adapter) writeWorkspace(l contracts.ResolvedLaunch) (string, error) {
	name := l.SessionID
	if !tmux.ValidName(name) {
		name = l.RunID
	}
	ws := filepath.Join(a.o.WorkspaceRoot, name)
	dir := filepath.Join(ws, ".agents")
	for _, d := range []string{a.o.WorkspaceRoot, ws, dir, filepath.Join(dir, "rules")} {
		if err := tmux.PrepareDir(d); err != nil {
			return "", err
		}
	}
	hb, err := hooksFile(a.o.HookCommand, l.StateDir)
	if err != nil {
		return "", err
	}
	if err := writeFileAtomic(filepath.Join(dir, "hooks.json"), hb); err != nil {
		return "", err
	}
	servers := map[string]any{}
	if len(a.o.MCPCommand) > 0 {
		servers["pyre"] = map[string]any{"command": a.o.MCPCommand[0], "args": a.o.MCPCommand[1:]}
	}
	mb, _ := json.MarshalIndent(map[string]any{"mcpServers": servers}, "", "  ")
	if err := writeFileAtomic(filepath.Join(dir, "mcp_config.json"), mb); err != nil {
		return "", err
	}
	if err := writeFileAtomic(filepath.Join(dir, "rules", "pyre.md"), []byte(rulesText(l.Cwd, ws, l.AgentGuide))); err != nil {
		return "", err
	}
	return ws, nil
}

// rulesText is the always-on rule carrying the agent guide. agy lists
// workspace folders sorted by path and treats the first as the default
// directory for commands, so the rule names the real working directory and
// keeps the agent out of the configuration folder (validated on 1.4.2).
func rulesText(cwd, ws, guide string) string {
	var b strings.Builder
	b.WriteString("---\ntrigger: always_on\n---\n# Pyre session\n\n")
	fmt.Fprintf(&b, "Your working directory is %s: run commands there and treat it as the project.\n", cwd)
	fmt.Fprintf(&b, "The workspace folder %s only holds Pyre's configuration for this session (hooks, MCP servers, these rules). "+
		"Never run commands in it, and never read or edit files there.\n", ws)
	b.WriteString("Pyre's MCP tools (server \"pyre\") are MCP tools: call them like any other MCP tool.\n")
	if g := strings.TrimSpace(guide); g != "" {
		b.WriteString("\n" + g + "\n")
	}
	return b.String()
}

// waitStartup waits (bounded) for agy's prompt (or, with an initial prompt,
// its first hook) while accepting the workspace-trust dialog in an
// owner-trusted directory. Other dialogs are left for a human.
func (a *Adapter) waitStartup(ctx context.Context, h contracts.RunHandle, prompt bool) {
	deadline := time.Now().Add(a.o.StartupWait)
	var dialogSince time.Time
	for time.Now().Before(deadline) && ctx.Err() == nil {
		if p, err := a.srv.Pane(ctx, h.TmuxTarget); err != nil || p.Dead {
			return
		}
		e := a.entry(h)
		e.mu.Lock()
		a.refresh(h, e)
		started := e.rs.started
		e.mu.Unlock()
		si, err := a.screen(ctx, h.TmuxTarget)
		if err == nil {
			switch {
			case a.handleTrust(ctx, h, si):
				dialogSince = time.Time{}
			case started || (!prompt && si.HasInputBox && si.Dialog == ""):
				return
			case si.Dialog != "":
				if dialogSince.IsZero() {
					dialogSince = time.Now()
				} else if time.Since(dialogSince) > 3*time.Second {
					return // needs a human
				}
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// handleTrust accepts agy's workspace-trust dialog when the owner trusts
// cwd (agy then records it in its settings.json, as when a person answers).
func (a *Adapter) handleTrust(ctx context.Context, h contracts.RunHandle, si screenInfo) bool {
	if si.DialogKind != "trust" || a.o.TrustWorkspace == nil {
		return false
	}
	m := a.meta(h)
	if m == nil || !a.o.TrustWorkspace(m.Cwd) {
		return false
	}
	if si.TrustYes {
		if a.srv.SendKeys(ctx, h.TmuxTarget, tmux.KeyEnter) == nil {
			a.automation(h, contracts.AutomationTrustAccepted, m.Cwd)
		}
	} else {
		_ = a.srv.SendKeys(ctx, h.TmuxTarget, tmux.KeyUp)
	}
	time.Sleep(300 * time.Millisecond)
	return true
}

func (a *Adapter) automation(h contracts.RunHandle, action, detail string) {
	if a.o.OnAutomation == nil {
		return
	}
	if _, dup := a.automated.LoadOrStore(action+"\x00"+h.RunID, true); dup {
		return
	}
	go a.o.OnAutomation(contracts.NodeAutomation{RunID: h.RunID, SessionID: h.SessionID, Harness: contracts.HarnessAgy, Action: action, Detail: detail})
}

func (a *Adapter) screen(ctx context.Context, name string) (screenInfo, error) {
	plain, err := a.srv.CapturePane(ctx, name, tmux.CaptureOptions{Join: true})
	if err != nil {
		return screenInfo{}, err
	}
	return parseScreen(plain), nil
}

func (a *Adapter) entry(h contracts.RunHandle) *runEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	e := a.runs[h.RunID]
	if e == nil {
		e = &runEntry{rs: newRunState()}
		a.runs[h.RunID] = e
	}
	return e
}

func (a *Adapter) meta(h contracts.RunHandle) *runMeta {
	e := a.entry(h)
	e.mu.Lock()
	defer e.mu.Unlock()
	return a.loadMeta(h, e)
}

func (a *Adapter) loadMeta(h contracts.RunHandle, e *runEntry) *runMeta {
	if e.meta != nil {
		return e.meta
	}
	b, err := os.ReadFile(filepath.Join(h.StateDir, fileRun))
	if err != nil {
		return nil
	}
	var m runMeta
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	e.meta = &m
	return e.meta
}

// refresh folds new hook records and transcript lines into e.rs (e.mu held).
func (a *Adapter) refresh(h contracts.RunHandle, e *runEntry) {
	rs := e.rs
	recs, next := readEvents(h.StateDir, rs.eventsOff)
	for _, r := range recs {
		rs.applyHook(r)
	}
	rs.eventsOff = next
	if rs.conversationID == "" {
		if m := a.loadMeta(h, e); m != nil && m.ConversationID != "" {
			rs.conversationID = m.ConversationID
		}
	}
	path := rs.transcriptPath
	if path == "" {
		path = transcriptPathFor(a.o.AgyHome, rs.conversationID)
	}
	if path == "" {
		return
	}
	if path != rs.tPath {
		rs.tPath, rs.tOff, rs.tSeq = path, 0, 0
		e.tpath.Store(path)
	}
	rs.tOff, _ = readJSONLFrom(path, rs.tOff, rs.applyTranscript)
}

// conversationID is the run's agy conversation ("" until its first turn).
func (a *Adapter) conversationID(h contracts.RunHandle) string {
	e := a.entry(h)
	e.mu.Lock()
	defer e.mu.Unlock()
	a.refresh(h, e)
	return e.rs.conversationID
}

// ChangeStamp fingerprints the hook record file and transcript (size and
// mtime) so a backed-off observer wakes when agy writes something.
func (a *Adapter) ChangeStamp(h contracts.RunHandle) string {
	stamp := fileStamp(filepath.Join(h.StateDir, fileEvents))
	if p, _ := a.entry(h).tpath.Load().(string); p != "" {
		stamp += "|" + fileStamp(p)
	}
	return stamp
}

func fileStamp(path string) string {
	st, err := os.Stat(path)
	if err != nil {
		return "-"
	}
	return fmt.Sprintf("%d.%d", st.Size(), st.ModTime().UnixNano())
}

type exitRecord struct {
	ExitCode *int      `json:"exit_code,omitempty"`
	Reason   string    `json:"reason"`
	At       time.Time `json:"at"`
}

func readExit(dir string) *exitRecord {
	b, err := os.ReadFile(filepath.Join(dir, fileExit))
	if err != nil {
		return nil
	}
	var r exitRecord
	if json.Unmarshal(b, &r) != nil {
		return nil
	}
	return &r
}

func writeExit(dir string, r exitRecord) {
	if readExit(dir) != nil {
		return
	}
	b, _ := json.Marshal(r)
	_ = writeFileAtomic(filepath.Join(dir, fileExit), b)
}

// stateGrace: hooks say a turn runs but the screen shows the idle prompt;
// after this long without a hook the screen wins (an Escape interrupt fires
// no Stop hook).
const stateGrace = 3 * time.Second

// derive combines the hook state with the screen (si nil: unavailable).
func (a *Adapter) derive(rs *runState, si *screenInfo, now time.Time) (contracts.RunState, string) {
	state, reason := rs.hookState()
	if si == nil {
		return state, reason
	}
	switch {
	case si.DialogKind == "trust":
		return contracts.RunWaitingApproval, "needs human: agy asks whether to trust this workspace (attach to answer it in the terminal)"
	case si.Dialog != "":
		return contracts.RunWaitingApproval, "needs human: " + si.Dialog + " (attach to answer it in the terminal)"
	case !si.HasInputBox:
		if !rs.started {
			return contracts.RunStarting, "agy starting (prompt not visible yet)"
		}
		return state, reason
	case si.Working():
		return contracts.RunRunning, "turn in progress"
	case si.Idle() && state == contracts.RunRunning && now.Sub(rs.lastEvent) >= stateGrace:
		return contracts.RunWaitingInput, "turn ended (interrupted; no Stop hook)"
	case state == contracts.RunRunning && !si.InputEmpty() && si.Footer != "" && !strings.Contains(si.Footer, "esc to cancel") &&
		now.Sub(rs.lastEvent) >= stateGrace:
		// A draft in the box hides "? for shortcuts" (the footer shows only
		// the model); a running turn still says "esc to cancel" over a draft
		// (verified on 1.4.2). So no "esc to cancel" after an interrupt with
		// text in the box means the turn ended.
		return contracts.RunWaitingInput, "turn ended (interrupted; no Stop hook; a draft is in the input box)"
	case si.Idle() && state != contracts.RunRunning:
		if strings.Contains(si.Footer, "task(s)") {
			return contracts.RunWaitingInput, reason + "; background task(s) still running"
		}
		return contracts.RunWaitingInput, reason
	}
	return state, reason
}

func (a *Adapter) Observe(ctx context.Context, h contracts.RunHandle, cursor string) (contracts.Observation, error) {
	now := a.o.Now()
	obs := contracts.Observation{ObservedAt: now, NativeRef: h.NativeRef}
	pane, perr := a.srv.Pane(ctx, h.TmuxTarget)
	gone := perr != nil && (errors.Is(perr, tmux.ErrNoSession) || errors.Is(perr, tmux.ErrNoServer))
	var si *screenInfo
	if perr == nil && !pane.Dead {
		if s, err := a.screen(ctx, h.TmuxTarget); err == nil {
			si = &s
		}
	}
	e := a.entry(h)
	e.mu.Lock()
	a.refresh(h, e)
	rs := e.rs
	state, reason := a.derive(rs, si, now)
	if rs.conversationID != "" {
		obs.NativeRef = &contracts.NativeRef{Kind: NativeKind, ID: rs.conversationID}
	}
	if rs.lastTool != "" {
		obs.Activity = "tool: " + rs.lastTool
	}
	if state == contracts.RunRunning && rs.lastTool != "" && rs.toolAt.After(rs.lastStop.At) {
		obs.Doing = "used " + rs.lastTool
	}
	obs.Receipts, obs.NextCursor = a.receipts(h, rs, cursor)
	e.mu.Unlock()

	switch {
	case gone:
		a.removeWorkspace(h)
		state, reason = contracts.RunExited, "tmux session gone"
		if r := readExit(h.StateDir); r != nil {
			obs.ExitCode, reason = r.ExitCode, r.Reason
		}
	case perr != nil:
		state, reason = contracts.RunUnknown, "tmux unavailable: "+perr.Error()
	case pane.Dead:
		state, reason = contracts.RunExited, "agy exited"
		obs.ExitCode = pane.ExitStatus
		writeExit(h.StateDir, exitRecord{ExitCode: pane.ExitStatus, Reason: reason, At: now})
	case si != nil && si.DialogKind == "trust":
		if a.handleTrust(ctx, h, *si) {
			reason = "accepting workspace trust (owner-trusted directory)"
		}
	}
	obs.State, obs.StateReason = state, reason
	return obs, nil
}

// receipts reports acceptance evidence found since the cursor for messages
// this adapter attempted (an indeterminate attempt resolved later).
// Cursor: "t<transcript offset>.p<path hash>".
func (a *Adapter) receipts(h contracts.RunHandle, rs *runState, cursor string) ([]contracts.DeliveryReceipt, string) {
	var tOff int64
	ph := ""
	for _, part := range strings.Split(cursor, ".") {
		if len(part) < 2 {
			continue
		}
		switch part[0] {
		case 't':
			fmt.Sscanf(part[1:], "%d", &tOff)
		case 'p':
			ph = part[1:]
		}
	}
	if ph != shortHash(rs.tPath) {
		tOff = 0
	}
	next := fmt.Sprintf("t%d.p%s", rs.tOff, shortHash(rs.tPath))
	attempts := lastAttempts(h.StateDir)
	var out []contracts.DeliveryReceipt
	for msg, ev := range rs.accepted {
		att, ok := attempts[msg]
		if !ok || ev.Off < tOff {
			continue
		}
		out = append(out, contracts.DeliveryReceipt{
			MessageID: msg, AttemptID: att.AttemptID, SessionID: h.SessionID, RunID: h.RunID, NodeID: a.o.NodeID,
			State: contracts.DeliveryAccepted, Evidence: contracts.EvidenceAgyUserInput, EvidenceRef: ev.Ref,
			Detail: "consumed_at=" + ev.At.UTC().Format(time.RFC3339Nano), ObservedAt: ev.At,
		})
	}
	return out, next
}

func (a *Adapter) Recover(ctx context.Context, h contracts.RunHandle) (contracts.RunHandle, error) {
	ok, err := a.srv.HasSession(ctx, h.TmuxTarget)
	if err != nil {
		return h, contracts.Errorf(contracts.ErrUnavailable, "tmux: %v", err)
	}
	if !ok {
		return h, contracts.Errorf(contracts.ErrNotFound, "tmux session %s is gone", h.TmuxTarget).WithRef(contracts.KindRun, h.RunID)
	}
	if p, err := a.srv.Pane(ctx, h.TmuxTarget); err == nil {
		h.PID = p.PID
	}
	if id := a.conversationID(h); id != "" {
		h.NativeRef = &contracts.NativeRef{Kind: NativeKind, ID: id}
	}
	return h, nil
}

func (a *Adapter) Attach(ctx context.Context, h contracts.RunHandle, req contracts.AttachCommand) (contracts.AttachDescriptor, error) {
	if ok, err := a.srv.HasSession(ctx, h.TmuxTarget); err != nil || !ok {
		return contracts.AttachDescriptor{}, contracts.Errorf(contracts.ErrNotFound, "tmux session %s is gone", h.TmuxTarget)
	}
	d, err := a.srv.AttachDescriptor(sessionhost.AttachConfig{NodeID: a.o.NodeID, SSHAlias: a.o.SSHAlias}, h.RunID, h.TmuxTarget, req, a.o.Now())
	if err == nil && !d.ReadOnly {
		d.Notes += "; while attached, Pyre queues input instead of pasting"
	}
	return d, err
}

func (a *Adapter) receipt(outcome contracts.CommandOutcome, accepted bool, detail string) contracts.CommandReceipt {
	return contracts.CommandReceipt{Accepted: accepted, Outcome: outcome, Detail: detail, ObservedAt: a.o.Now()}
}

// current derives the run state from hooks and the live screen.
func (a *Adapter) current(ctx context.Context, h contracts.RunHandle) (contracts.RunState, string, *screenInfo) {
	var si *screenInfo
	if s, err := a.screen(ctx, h.TmuxTarget); err == nil {
		si = &s
	}
	e := a.entry(h)
	e.mu.Lock()
	defer e.mu.Unlock()
	a.refresh(h, e)
	st, r := a.derive(e.rs, si, a.o.Now())
	return st, r, si
}

// Interrupt presses Escape while a turn runs (agy: "esc to cancel"). A raw
// terminal action: the daemon holds the run's input lease.
func (a *Adapter) Interrupt(ctx context.Context, h contracts.RunHandle) (contracts.CommandReceipt, error) {
	p, err := a.srv.Pane(ctx, h.TmuxTarget)
	if err != nil || p.Dead {
		return a.receipt(contracts.OutcomeFailed, false, "run is not live"), nil
	}
	if st, _, _ := a.current(ctx, h); st != contracts.RunRunning && st != contracts.RunWaitingApproval {
		return a.receipt(contracts.OutcomeSucceeded, true, "nothing to interrupt (state "+string(st)+")"), nil
	}
	if err := a.srv.SendKeys(ctx, h.TmuxTarget, tmux.KeyEscape); err != nil {
		return a.receipt(contracts.OutcomeFailed, false, err.Error()), nil
	}
	end := time.Now().Add(stateGrace + 3*time.Second)
	for time.Now().Before(end) {
		time.Sleep(250 * time.Millisecond)
		if st, _, _ := a.current(ctx, h); st == contracts.RunWaitingInput {
			return a.receipt(contracts.OutcomeSucceeded, true, "turn ended after Escape"), nil
		}
	}
	return a.receipt(contracts.OutcomeDelivered, true, "Escape sent; turn end not yet observed"), nil
}

// Stop types /exit at an idle, empty prompt with nobody attached (bounded),
// then removes the tmux session.
func (a *Adapter) Stop(ctx context.Context, h contracts.RunHandle) (contracts.CommandReceipt, error) {
	now := a.o.Now()
	p, err := a.srv.Pane(ctx, h.TmuxTarget)
	if err != nil {
		if errors.Is(err, tmux.ErrNoSession) || errors.Is(err, tmux.ErrNoServer) {
			return a.receipt(contracts.OutcomeSucceeded, true, "already gone"), nil
		}
		return a.receipt(contracts.OutcomeFailed, false, err.Error()), nil
	}
	method := "kill-session"
	if !p.Dead && a.gracefulExit(ctx, h) {
		method = "/exit"
	}
	if p2, err := a.srv.Pane(ctx, h.TmuxTarget); err == nil {
		writeExit(h.StateDir, exitRecord{ExitCode: p2.ExitStatus, Reason: "stopped by pyre (" + method + ")", At: now})
	}
	if err := a.srv.TerminateSession(ctx, h.TmuxTarget, 5*time.Second); err != nil {
		return a.receipt(contracts.OutcomeFailed, false, err.Error()), nil
	}
	if ok, _ := a.srv.HasSession(ctx, h.TmuxTarget); ok {
		return a.receipt(contracts.OutcomeIndeterminate, true, "tmux session still present"), nil
	}
	a.forget(h)
	return a.receipt(contracts.OutcomeSucceeded, true, "stopped via "+method), nil
}

// forget drops a stopped run's memory and its session's customization
// folder (Launch writes it again for a resume; the conversation is in
// agy's own store).
func (a *Adapter) forget(h contracts.RunHandle) {
	a.removeWorkspace(h)
	a.mu.Lock()
	delete(a.runs, h.RunID)
	a.mu.Unlock()
	for _, act := range []string{contracts.AutomationTrustAccepted} {
		a.automated.Delete(act + "\x00" + h.RunID)
	}
}

func (a *Adapter) gracefulExit(ctx context.Context, h contracts.RunHandle) bool {
	e := a.entry(h)
	e.deliverMu.Lock()
	defer e.deliverMu.Unlock()
	if cs, err := a.srv.ListClients(ctx, h.TmuxTarget); err != nil || tmux.WritableClients(cs) > 0 {
		return false
	}
	if si, err := a.screen(ctx, h.TmuxTarget); err == nil && si.Working() {
		_ = a.srv.SendKeys(ctx, h.TmuxTarget, tmux.KeyEscape)
		time.Sleep(800 * time.Millisecond)
	}
	si, err := a.screen(ctx, h.TmuxTarget)
	if err != nil || !si.InputEmpty() || si.Dialog != "" {
		return false
	}
	if err := a.srv.Paste(ctx, h.TmuxTarget, "pyre-exit", []byte("/exit")); err != nil {
		return false
	}
	ok := false
	for i := 0; i < 10 && !ok; i++ {
		time.Sleep(150 * time.Millisecond)
		if si, err := a.screen(ctx, h.TmuxTarget); err == nil && strings.TrimSpace(si.Input) == "/exit" {
			ok = true
		}
	}
	if !ok {
		a.clearInput(ctx, h.TmuxTarget)
		return false
	}
	if err := a.srv.SendKeys(ctx, h.TmuxTarget, tmux.KeyEnter); err != nil {
		return false
	}
	end := time.Now().Add(a.o.StopWait)
	for time.Now().Before(end) {
		time.Sleep(200 * time.Millisecond)
		if p, err := a.srv.Pane(ctx, h.TmuxTarget); err != nil || p.Dead {
			return true
		}
	}
	return false
}

func (a *Adapter) Answer(context.Context, contracts.RunHandle, contracts.Approval, string, string) (contracts.CommandReceipt, error) {
	return a.receipt(contracts.OutcomeUnsupported, false, ""), contracts.Errorf(contracts.ErrUnsupported,
		"agy permission prompts are answered in its terminal (attach); Pyre has no approvals broker for agy")
}

func (a *Adapter) ReadTranscript(ctx context.Context, h contracts.RunHandle, req contracts.ReadSessionRequest) (contracts.ReadSessionResponse, error) {
	resp := contracts.ReadSessionResponse{Freshness: contracts.Freshness{ObservedAt: a.o.Now(), Source: "node:" + a.o.NodeID}}
	e := a.entry(h)
	e.mu.Lock()
	a.refresh(h, e)
	path := e.rs.tPath
	e.mu.Unlock()
	limit := req.Limit
	if limit <= 0 {
		limit = 50
	}
	limit = min(limit, 500)
	var start int64
	if req.Cursor != "" {
		fmt.Sscanf(req.Cursor, "%d", &start)
	}
	var all, sel []contracts.TranscriptEntry
	if path != "" {
		all = readTranscript(path, h.RunID)
	}
	if req.Tail && req.Cursor == "" {
		sel = all[max(0, len(all)-limit):]
	} else {
		for _, en := range all {
			if en.Seq > start {
				sel = append(sel, en)
				if len(sel) == limit {
					break
				}
			}
		}
	}
	resp.Entries = sel
	resp.NextCursor = req.Cursor
	if len(sel) > 0 {
		resp.NextCursor = fmt.Sprintf("%d", sel[len(sel)-1].Seq)
	}
	if path == "" {
		resp.Freshness.Reason = "no agy conversation yet (it starts with the first prompt)"
	}
	if screen, err := a.srv.CapturePane(ctx, h.TmuxTarget, tmux.CaptureOptions{Join: true}); err == nil {
		resp.Screen = strings.TrimRight(screen, "\n")
	}
	return resp, nil
}

// ResumeSpec: `agy --conversation <id>` when the conversation exists on this
// node (agy keeps conversations per machine, under the one own login).
// Otherwise a handoff.
func (a *Adapter) ResumeSpec(ctx context.Context, prev contracts.Run, sameAccount bool) (contracts.ContinuationKind, *contracts.NativeRef, error) {
	if prev.NativeRef == nil || prev.NativeRef.Kind != NativeKind || !conversationOnNode(a.o.AgyHome, prev.NativeRef.ID) {
		return contracts.ContinuationHandoff, nil, nil
	}
	ref := *prev.NativeRef
	return contracts.ContinuationNativeResume, &ref, nil
}

// NativeThreadOnNode (daemon nativeThreadLocator): the conversation is in
// this node's agy home whatever account the resume binds (there is one
// login).
func (a *Adapter) NativeThreadOnNode(prev contracts.Run) bool {
	return prev.NativeRef != nil && prev.NativeRef.Kind == NativeKind && conversationOnNode(a.o.AgyHome, prev.NativeRef.ID)
}

// TranscriptFiles lists the run's transcript for the search index and the
// title scanner.
func (a *Adapter) TranscriptFiles(h contracts.RunHandle) (string, []string) {
	e := a.entry(h)
	e.mu.Lock()
	a.refresh(h, e)
	p := e.rs.tPath
	e.mu.Unlock()
	if p == "" {
		return TranscriptFormat, nil
	}
	if _, err := os.Stat(p); err != nil {
		return TranscriptFormat, nil
	}
	return TranscriptFormat, []string{p}
}

// removeWorkspace deletes the run's session customization folder (only
// inside WorkspaceRoot) once the run is over.
func (a *Adapter) removeWorkspace(h contracts.RunHandle) {
	m := a.meta(h)
	if m == nil || m.Workspace == "" {
		return
	}
	root := filepath.Clean(a.o.WorkspaceRoot) + string(filepath.Separator)
	if !strings.HasPrefix(filepath.Clean(m.Workspace), root) {
		return
	}
	// A later run of the session rewrote the folder for itself: keep it.
	if b, err := os.ReadFile(filepath.Join(m.Workspace, ".agents", "hooks.json")); err != nil || !strings.Contains(string(b), h.StateDir) {
		return
	}
	_ = os.RemoveAll(m.Workspace)
}
