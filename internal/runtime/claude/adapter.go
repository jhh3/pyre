// Package claude is the native Claude Code runtime adapter (WP01 report §8,
// decision I07). Claude runs with its real TUI inside the node's isolated
// tmux server; per-run hooks passed with --settings record state and evidence
// into the run state dir; input is bracketed paste gated on hook state, with
// a [pyre-msg:<id>] marker whose arrival in UserPromptSubmit or the native
// transcript is the acceptance evidence. The PermissionRequest hook is a
// structured approval broker that races the TUI dialog.
package claude

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jhh3/pyre/internal/buildinfo"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/osutil"
	"github.com/jhh3/pyre/internal/runtime/claudeplugin"
	"github.com/jhh3/pyre/internal/runtime/hostsel"
	"github.com/jhh3/pyre/internal/runtime/sessionhost"
	"github.com/jhh3/pyre/internal/runtime/tmux"
	"github.com/jhh3/pyre/internal/winenv"
)

// AdapterName is the RunHandle.Adapter value.
const AdapterName = "claude-tmux"

// TestedVersions lists Claude Code versions this adapter was validated with.
var TestedVersions = []string{"2.1.289"}

// Options configures the Claude adapter. The daemon supplies one instance per
// node.
type Options struct {
	NodeID string
	// TmuxBin is the tmux executable (default tmux on PATH).
	TmuxBin string
	// TmuxSocket is the node's isolated tmux server socket (required), in a
	// 0700 Pyre state dir, e.g. <pyre-state>/tmux/pyre.sock (<=100 bytes).
	TmuxSocket string
	// SSHAlias lets other machines attach with `ssh -t <alias>` (optional).
	SSHAlias string
	// ClaudeBin is used when ResolvedLaunch.Executable is empty (default
	// "claude" on PATH).
	ClaudeBin string
	// TmpDir becomes CLAUDE_CODE_TMPDIR for runs whose environment does not
	// set one (created 0700 at launch). Claude keeps per-session scratch and
	// background task output there; on hosts where /tmp is RAM-backed tmpfs
	// with a per-user quota, a disk directory keeps long sessions from filling
	// memory and failing every command with EDQUOT. Empty: Claude's default.
	TmpDir string
	// HookCommand is the argv prefix of the hook command written into
	// pyre-hooks.json; the adapter appends "<Event> <state-dir>". Typically
	// []string{"/path/to/pyred", "hook", "claude"}, whose handler calls
	// HookMain. Required.
	HookCommand []string
	// PermissionMode is passed explicitly with --permission-mode (default
	// "default"; Claude shows it as "manual mode").
	PermissionMode string
	// OwnerHome is the home whose Claude settings record the owner's
	// one-time acceptance of the bypass-permissions warning (default
	// $HOME). A bypass run is refused unless the owner accepted it there.
	OwnerHome string
	// TrustWorkspace reports whether the owner trusts cwd, allowing the
	// adapter to accept Claude's workspace-trust dialog. Nil: never accept;
	// the run then reports "needs human: workspace trust".
	TrustWorkspace func(cwd string) bool
	// OnAutomation, when set, is told each time the adapter answers a
	// harness dialog by itself (workspace trust; the "background work is
	// running" exit dialog on stop) so the node can report it to the
	// owner's audit log (W-audit). Called at most once per run and action,
	// off the hot path; it must not block.
	OnAutomation func(contracts.NodeAutomation)
	// ProfileAsConfigDir sets CLAUDE_CONFIG_DIR=ResolvedLaunch.ProfileDir
	// when ProfileDir is set (account isolation mechanism under WP02
	// validation; off by default, in which case ProfileDir is ignored and the
	// node's default login is used).
	ProfileAsConfigDir bool
	// TranscriptHomes are further Claude config homes (globs allowed) where
	// a resumed session's transcript may live besides the default login,
	// e.g. the accounts' shared store and every profile. A resume imports
	// it into the new run's config home (resumeimport.go).
	TranscriptHomes []string
	// StopHookSteer enables ModeSteer via Stop-hook injection (delivered when
	// the current turn ends; the TUI shows "Stop hook error").
	StopHookSteer bool
	// DoorbellCommand enables the plugin-monitor doorbell (S-D,
	// capability input.monitor): argv prefix of the monitor, typically
	// []string{pyred, "inbox-watch"}; the adapter appends
	// "--run-dir <state-dir>". The run gets a generated plugin
	// (--plugin-dir=<state>/pyre-plugin) and pre-approved pyre inbox/ack
	// tools. Requires the pyre MCP server (key "pyre") in the run.
	DoorbellCommand []string
	// DoorbellPasteHold is how long Deliver leaves a message the doorbell
	// already announced to the agent's inbox read before pasting it anyway
	// (default 3m). Without the hold, a paste after a dialog closes or a
	// human detaches races the queued notification and the agent would see
	// the message twice.
	DoorbellPasteHold time.Duration
	// DisablePluginDelivery turns off plugin prompt delivery (capability
	// input.plugin_prompt): by default every run's generated plugin carries
	// a hooks module that submits Pyre messages with $.prompt.submit between
	// turns, and Deliver prefers it whenever the module reports it is
	// loaded; paste remains the fallback (older Claude Code, module not
	// loaded, steering mid-turn).
	DisablePluginDelivery bool
	// DisableApprovalBroker leaves permission prompts to the TUI only.
	DisableApprovalBroker bool
	// ApprovalWait is how long the PermissionRequest hook waits for a Pyre
	// answer before falling back to the TUI dialog (default 120s).
	ApprovalWait time.Duration
	// DeliverTimeout bounds the wait for acceptance evidence (default 20s).
	DeliverTimeout time.Duration
	// StartupWait bounds Launch's wait for SessionStart (default 20s).
	StartupWait time.Duration
	// StopWait bounds the graceful /exit before kill-session (default 10s).
	StopWait time.Duration
	// FirstRunScreenWait is how long after launch a run that has not
	// started and shows no input box (an unrecognised first-run screen) is
	// reported as needing a human (default 90s).
	FirstRunScreenWait time.Duration
	Now                func() time.Time

	testHook func(point string) error // crash-window injection in tests
}

// Adapter implements contracts.RuntimeAdapter for Claude Code.
type Adapter struct {
	o   Options
	srv sessionhost.Host

	mu   sync.Mutex
	runs map[string]*runEntry

	automated sync.Map // action+run already reported through OnAutomation

	tails tailCache // sessions.read's per-transcript state (transcript_tail.go)
}

var _ contracts.RuntimeAdapter = (*Adapter)(nil)

type runEntry struct {
	mu        sync.Mutex // guards rs/meta
	deliverMu sync.Mutex // serializes input to the run
	rs        *runState
	meta      *runMeta
	tpath     atomic.Value // string: transcript path last read (for ChangeStamp, lock-free)

	// startupMu serializes startup-dialog key presses (Launch's
	// waitStartup and Observe both drive them) and guards onb.
	startupMu sync.Mutex
	onb       onboardingPresses
}

// onboardingPresses tracks Enter presses on Claude's onboarding screens.
type onboardingPresses struct {
	step  string    // screen last pressed ("theme", "security")
	at    time.Time // when
	n     int       // presses on that screen
	total int       // presses on all onboarding screens of the run
}

// runMeta is <state>/run.json.
type runMeta struct {
	RunID       string    `json:"run_id"`
	SessionUUID string    `json:"claude_session_id"`
	ConfigDir   string    `json:"config_dir"`
	ConfigMech  string    `json:"config_mechanism"` // "default" | "claude_config_dir"
	Cwd         string    `json:"cwd"`
	Resumed     bool      `json:"resumed"`
	LaunchedAt  time.Time `json:"launched_at"`
	// ManagedSettings is the environment generation's settings overlay that
	// was merged into this run's pyre-hooks.json ("" = none).
	ManagedSettings string `json:"managed_settings,omitempty"`
	// InitialPrompt: launched with a prompt, so the run's first
	// UserPromptSubmit is that prompt, not the owner typing (askNudge).
	InitialPrompt bool `json:"initial_prompt,omitempty"`
	// DevChannels are the development channels the session's launch
	// arguments load (--dangerously-load-development-channels): the owner
	// asked for them, so the startup warning is confirmed for this run.
	DevChannels []string `json:"dev_channels,omitempty"`
	// Bypass: launched with --dangerously-skip-permissions. The owner chose
	// to let the run do anything, so its folder is trusted (startup dialog).
	Bypass bool `json:"bypass,omitempty"`
}

// New validates options. The tmux server starts lazily on first launch.
func New(o Options) (*Adapter, error) {
	if o.TmuxSocket == "" {
		return nil, errors.New("claude: TmuxSocket is required")
	}
	if len(o.HookCommand) == 0 {
		return nil, errors.New("claude: HookCommand is required")
	}
	srv, err := hostsel.New(o.TmuxBin, o.TmuxSocket)
	if err != nil {
		return nil, err
	}
	if o.PermissionMode == "" {
		o.PermissionMode = "default"
	}
	if o.ApprovalWait <= 0 {
		o.ApprovalWait = 120 * time.Second
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
	if o.FirstRunScreenWait <= 0 {
		o.FirstRunScreenWait = 90 * time.Second
	}
	if o.DoorbellPasteHold <= 0 {
		o.DoorbellPasteHold = 3 * time.Minute
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Adapter{o: o, srv: srv, runs: map[string]*runEntry{}}, nil
}

// Server exposes the session host (for daemon housekeeping).
func (a *Adapter) Server() sessionhost.Host { return a.srv }

func (a *Adapter) Name() string               { return AdapterName }
func (a *Adapter) Harness() contracts.Harness { return contracts.HarnessClaude }

// Capabilities advertised with the given options.
func (a *Adapter) Capabilities() []string {
	caps := []string{
		contracts.CapAttachNative, contracts.CapAttachReadOnly,
		contracts.CapInputQueue, contracts.CapInputReconcile, contracts.CapInputPaste,
		contracts.CapInterrupt, contracts.CapStop,
		contracts.CapResumeNative, contracts.CapResumeHandoff,
		contracts.CapSubagentsObserve, contracts.CapTranscript, contracts.CapScreen,
		contracts.CapHooks, contracts.CapRequiresTrustDialog,
		contracts.CapLaunchHarnessArgs, contracts.CapResumeOutside, contracts.CapLaunchEnvFile,
	}
	if !a.o.DisableApprovalBroker {
		caps = append(caps, contracts.CapApprovals, contracts.CapApprovalsRaceTUI)
	}
	if a.o.StopHookSteer {
		caps = append(caps, contracts.CapInputSteer, contracts.CapInputStopHook)
	}
	if a.o.ProfileAsConfigDir {
		// Cross-account resume: every Pyre Claude profile on a node links
		// its conversation store (projects/, todos/, ...) to one shared
		// store, so `claude --resume <uuid>` under another account's
		// CLAUDE_CONFIG_DIR finds the same transcript. The daemon still
		// checks both profiles resolve to the same store before resuming.
		caps = append(caps, contracts.CapAccountProfile, contracts.CapAccountCrossResume)
	}
	if len(a.o.DoorbellCommand) > 0 {
		caps = append(caps, contracts.CapInputMonitor)
	}
	if !a.o.DisablePluginDelivery {
		caps = append(caps, contracts.CapInputPlugin)
	}
	return caps
}

var claudeVersionRe = regexp.MustCompile(`(\d+\.\d+\.\d+)`)

func (a *Adapter) claudeBin(l *contracts.ResolvedLaunch) string {
	return nativeClaude(a.claudeBinRaw(l))
}

func (a *Adapter) claudeBinRaw(l *contracts.ResolvedLaunch) string {
	if l != nil && l.Executable != "" {
		return l.Executable
	}
	if a.o.ClaudeBin != "" {
		return a.o.ClaudeBin
	}
	if p, err := exec.LookPath("claude"); err == nil {
		return p
	}
	return "claude"
}

// nativeClaude runs the native claude.exe instead of npm's claude.cmd on
// Windows: cmd.exe truncates the command line at the first newline, which
// dropped the agent guide and the initial prompt.
func nativeClaude(p string) string {
	if runtime.GOOS != "windows" {
		return p
	}
	if n := winenv.Default().NativeClaude(p); n != "" {
		return n
	}
	return p
}

// monitorCommand is the plugin monitor's shell command line. On Windows it
// is parsed by Git Bash, so paths use forward slashes (which Win32 accepts)
// and POSIX quoting.
func monitorCommand(argv []string) string {
	if runtime.GOOS == "windows" {
		for i, a := range argv {
			if filepath.IsAbs(a) {
				argv[i] = winenv.PosixPath(a)
			}
		}
	}
	return tmux.ShellJoin(argv)
}

func (a *Adapter) Probe(ctx context.Context) (contracts.ProbeResult, error) {
	bin := a.claudeBin(nil)
	pr := contracts.ProbeResult{Harness: contracts.HarnessClaude, Adapter: AdapterName, Executable: bin, Capabilities: a.Capabilities()}
	if _, err := a.srv.Check(ctx); err != nil {
		pr.Reason = err.Error()
		return pr, nil
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, bin, "--version").Output()
	if err != nil {
		pr.Reason = "claude --version failed: " + err.Error()
		return pr, nil
	}
	if m := claudeVersionRe.FindString(string(out)); m != "" {
		pr.Version = m
	} else {
		pr.Version = strings.TrimSpace(string(out))
	}
	pr.Available = true
	pr.Models, pr.Efforts = Models(), append([]string(nil), Efforts...)
	tested := false
	for _, t := range TestedVersions {
		if t == pr.Version {
			tested = true
		}
	}
	if !tested {
		pr.Reason = "untested Claude Code version (tested: " + strings.Join(TestedVersions, ", ") + "); paste/screen parsing may differ"
	}
	if runtime.GOOS == "windows" && winenv.Default().GitBash() == "" {
		note := "Git for Windows (bash.exe) not found: Claude Code uses PowerShell for its shell and hooks and the Pyre doorbell monitor is off (install Git for Windows or set CLAUDE_CODE_GIT_BASH_PATH)"
		if pr.Reason != "" {
			note = pr.Reason + "; " + note
		}
		pr.Reason = note
	}
	return pr, nil
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// reservedArgs are controlled by the adapter and rejected in ResolvedLaunch.Args.
var reservedArgs = map[string]bool{
	"--session-id": true, "--resume": true, "-r": true, "--continue": true, "-c": true,
	"--fork-session": true, "--settings": true, "-p": true, "--print": true, "--bg": true, "--background": true,
}

func (a *Adapter) Launch(ctx context.Context, l contracts.ResolvedLaunch) (contracts.RunHandle, error) {
	if !tmux.ValidName(l.RunID) {
		return contracts.RunHandle{}, contracts.Errorf(contracts.ErrInvalid, "run id %q is not a valid tmux session name", l.RunID)
	}
	if l.StateDir == "" || l.Cwd == "" {
		return contracts.RunHandle{}, contracts.Errorf(contracts.ErrInvalid, "state dir and cwd are required")
	}
	if st, err := os.Stat(l.Cwd); err != nil || !st.IsDir() {
		return contracts.RunHandle{}, contracts.Errorf(contracts.ErrInvalid, "cwd %q is not a directory", l.Cwd)
	}
	ownMode := true
	// The environment generation's managed settings overlay arrives as one
	// --settings argument; it is merged into the run's own settings file
	// (Claude Code takes a single --settings) instead of being passed on.
	managed := ""
	var args, appendPrompt, devChannels []string
	for i := 0; i < len(l.Args); i++ {
		arg := l.Args[i]
		name, val, hasVal := strings.Cut(arg, "=")
		switch {
		case name == "--append-system-prompt" && hasVal:
			// A session's own appended prompt joins Pyre's agent guide:
			// Claude keeps only the last --append-system-prompt.
			appendPrompt = append(appendPrompt, val)
			continue
		case name == "--dangerously-load-development-channels" && hasVal:
			devChannels = append(devChannels, val)
		}
		if name == "--settings" {
			if !hasVal {
				if i+1 >= len(l.Args) {
					return contracts.RunHandle{}, contracts.Errorf(contracts.ErrInvalid, "argument --settings needs a file")
				}
				i++
				val = l.Args[i]
			}
			if managed != "" {
				return contracts.RunHandle{}, contracts.Errorf(contracts.ErrInvalid, "only one managed --settings file can be merged into a run (got %s and %s)", managed, val)
			}
			if !filepath.IsAbs(val) {
				return contracts.RunHandle{}, contracts.Errorf(contracts.ErrInvalid, "managed --settings file %q must be an absolute path", val)
			}
			managed = val
			continue
		}
		args = append(args, arg)
	}
	l.Args = args
	for _, arg := range l.Args {
		name, _, _ := strings.Cut(arg, "=")
		if reservedArgs[name] {
			return contracts.RunHandle{}, contracts.Errorf(contracts.ErrInvalid, "argument %s is controlled by the adapter", name)
		}
		if name == "--permission-mode" {
			ownMode = false
		}
	}
	if ok, err := a.srv.HasSession(ctx, l.RunID); err != nil {
		return contracts.RunHandle{}, contracts.Errorf(contracts.ErrUnavailable, "tmux: %v", err)
	} else if ok {
		return contracts.RunHandle{}, contracts.Errorf(contracts.ErrConflict, "run %s already has a live tmux session", l.RunID)
	}
	if err := tmux.PrepareDir(l.StateDir); err != nil {
		return contracts.RunHandle{}, err
	}
	for _, d := range []string{dirPending, dirAnswers, dirInject, claudeplugin.SpoolDir} {
		if err := tmux.PrepareDir(filepath.Join(l.StateDir, d)); err != nil {
			return contracts.RunHandle{}, err
		}
	}
	doorbell := len(a.o.DoorbellCommand) > 0
	doorbellOff := ""
	if doorbell {
		if k := doorbellBlocked(l.Env, l.Unset); k != "" {
			doorbell, doorbellOff = false, "disabled: "+k+" is set (Claude Code runs no plugin monitors)"
		} else if runtime.GOOS == "windows" && winenv.Default().GitBash() == "" {
			// Plugin monitors are shell command lines; Pyre writes them for
			// Git Bash only (PowerShell would need different quoting).
			doorbell, doorbellOff = false, "disabled: Git for Windows (bash.exe) not found; Claude Code runs plugin monitors in its shell"
		}
	}
	bypass := l.Env[PermissionsEnv] == "bypass"
	if bypass && !OwnerAcceptedBypass(a.ownerHome()) {
		return contracts.RunHandle{}, contracts.Errorf(contracts.ErrUnavailable,
			"permissions bypass: the owner never accepted Claude's bypass-permissions warning on this node (no skipDangerousModePermissionPrompt in %s); run `claude --dangerously-skip-permissions` once and accept it, or launch with --permissions ask",
			filepath.Join(a.ownerHome(), ".claude", "settings.json"))
	}
	settings, err := writeRunSettings(l.StateDir, a.o.HookCommand, a.o.ApprovalWait, runAllowedTools(), managed)
	if err != nil {
		return contracts.RunHandle{}, err
	}
	if bypass {
		// Carry the owner's acceptance into the run so the one-time warning
		// dialog never blocks (an isolated profile's settings may predate it).
		if err := setSettingsKey(settings, settingSkipBypassPrompt, true); err != nil {
			return contracts.RunHandle{}, err
		}
	}
	brokerPath := filepath.Join(l.StateDir, fileBroker)
	if a.o.DisableApprovalBroker {
		_ = os.Remove(brokerPath)
	} else {
		b, _ := json.Marshal(brokerConfig{WaitSeconds: int(a.o.ApprovalWait / time.Second)})
		if err := writeFileAtomic(brokerPath, b); err != nil {
			return contracts.RunHandle{}, err
		}
	}

	sid, resumed := newUUID(), false
	if l.Resume != nil {
		if l.Resume.Kind != "claude_session" || !uuidRe.MatchString(l.Resume.ID) {
			return contracts.RunHandle{}, contracts.Errorf(contracts.ErrInvalid, "resume ref %+v is not a claude session id", *l.Resume)
		}
		sid, resumed = l.Resume.ID, true
	}
	argv := []string{a.claudeBin(&l), "--settings", settings}
	if resumed {
		argv = append(argv, "--resume", sid)
	} else {
		argv = append(argv, "--session-id", sid)
	}
	switch {
	case bypass:
		argv = append(argv, "--dangerously-skip-permissions")
	case ownMode:
		argv = append(argv, "--permission-mode", a.o.PermissionMode)
	}
	if g := strings.Join(append(nonEmpty(l.AgentGuide), appendPrompt...), "\n\n"); g != "" {
		argv = append(argv, "--append-system-prompt", g)
	}
	// A chosen model and effort (pyre start --model/--effort; "" keeps
	// Claude's default). Claude Code takes aliases (opus, sonnet, ...) or
	// full model names, and --effort low|medium|high|xhigh|max.
	if l.Model != "" {
		argv = append(argv, "--model", l.Model)
	}
	if l.Effort != "" {
		argv = append(argv, "--effort", l.Effort)
	}
	var spec claudeplugin.Spec
	if doorbell {
		spec.MonitorCommand = monitorCommand(append(append([]string{}, a.o.DoorbellCommand...), "--run-dir", l.StateDir))
	}
	if !a.o.DisablePluginDelivery {
		spec.Module = &claudeplugin.Module{RunDir: l.StateDir, HookArgv: a.o.HookCommand}
		a.resetPluginSpool(l.StateDir)
	}
	if spec.MonitorCommand != "" || spec.Module != nil {
		dir, err := claudeplugin.Write(filepath.Join(l.StateDir, "pyre-plugin"), spec)
		if err != nil {
			return contracts.RunHandle{}, err
		}
		argv = append(argv, "--plugin-dir="+dir) // "=" form: the flag is repeatable
	}
	argv = append(argv, l.Args...)
	if l.InitialPrompt != "" {
		argv = append(argv, l.InitialPrompt)
	}

	env := map[string]string{}
	for k, v := range l.Env {
		env[k] = v
	}
	env[StateDirEnv] = l.StateDir
	if a.o.TmpDir != "" && env[TmpDirEnv] == "" {
		if err := os.MkdirAll(a.o.TmpDir, 0o700); err != nil {
			return contracts.RunHandle{}, fmt.Errorf("claude: run temp dir: %w", err)
		}
		env[TmpDirEnv] = a.o.TmpDir
	}
	if _, ok := env["PYRE_RUN_ID"]; !ok {
		env["PYRE_RUN_ID"] = l.RunID
	}
	if _, ok := env["PYRE_SESSION_ID"]; !ok && l.SessionID != "" {
		env["PYRE_SESSION_ID"] = l.SessionID
	}
	meta := &runMeta{RunID: l.RunID, SessionUUID: sid, Cwd: l.Cwd, Resumed: resumed, Bypass: bypass, LaunchedAt: a.o.Now().UTC(), ConfigMech: "default", ManagedSettings: managed, InitialPrompt: l.InitialPrompt != "",
		DevChannels: devChannels}
	if a.o.ProfileAsConfigDir && l.ProfileDir != "" {
		env["CLAUDE_CONFIG_DIR"] = l.ProfileDir
		meta.ConfigDir, meta.ConfigMech = l.ProfileDir, "claude_config_dir"
	} else if d := env["CLAUDE_CONFIG_DIR"]; d != "" {
		meta.ConfigDir = d
	} else {
		meta.ConfigDir = defaultConfigDir()
	}
	if meta.ConfigMech == "claude_config_dir" {
		ensureFirstRunAnswers(meta.ConfigDir)
	}
	installSkills(meta.ConfigDir)
	if resumed {
		if err := a.importTranscript(meta.ConfigDir, l.Cwd, sid); err != nil {
			return contracts.RunHandle{}, contracts.Errorf(contracts.ErrUnavailable, "resume: bring the conversation into %s: %v", meta.ConfigDir, err)
		}
	}
	mb, _ := json.MarshalIndent(meta, "", "  ")
	if err := writeFileAtomic(filepath.Join(l.StateDir, fileRun), mb); err != nil {
		return contracts.RunHandle{}, err
	}
	_ = os.Remove(filepath.Join(l.StateDir, fileExit))

	if err := a.srv.NewSession(ctx, tmux.SessionSpec{Name: l.RunID, Cwd: l.Cwd, Env: env, Unset: l.Unset, Argv: argv, Width: launchWidth, Height: launchHeight}); err != nil {
		return contracts.RunHandle{}, contracts.Errorf(contracts.ErrUnavailable, "start claude in tmux: %v", err)
	}
	h := contracts.RunHandle{
		RunID: l.RunID, SessionID: l.SessionID, Adapter: AdapterName, Harness: contracts.HarnessClaude,
		TmuxSocket: a.srv.SocketPath(), TmuxTarget: l.RunID, StateDir: l.StateDir,
		NativeRef: &contracts.NativeRef{Kind: "claude_session", ID: sid},
		Data:      map[string]string{"claude_session_id": sid, "config_mechanism": meta.ConfigMech},
	}
	if bypass {
		h.Data["permissions"] = "bypass"
	}
	if !a.o.DisablePluginDelivery {
		h.Data["plugin_delivery"] = "on"
	}
	switch {
	case doorbell:
		h.Data["doorbell"] = "on"
	case doorbellOff != "":
		h.Data["doorbell"] = doorbellOff
		h.Data[contracts.HandleCapsUnavailable] = contracts.CapInputMonitor
	}
	if p, err := a.srv.Pane(ctx, l.RunID); err == nil {
		h.PID = p.PID
	}
	a.waitStartup(ctx, h)
	return h, nil
}

// skillNotes dedupes installSkills log lines (config dir + skill name).
var skillNotes sync.Map

// installSkills writes the agent slash commands into the session's Claude
// config dir (bare /<name>; plugin skills need /pyre-session:<name>). A
// failure or an owner's same-named skill is logged once and never blocks
// the launch.
func installSkills(configDir string) {
	skipped, err := claudeplugin.MaterializeSkills(configDir, buildinfo.Version)
	for _, name := range skipped {
		if _, seen := skillNotes.LoadOrStore(configDir+"\x00"+name, true); !seen {
			log.Printf("claude: %s/skills/%s is the owner's own skill (no %s); leaving it, /%s runs it", configDir, name, claudeplugin.ManagedMarker, name)
		}
	}
	if err != nil {
		if _, seen := skillNotes.LoadOrStore(configDir+"\x00error", true); !seen {
			log.Printf("claude: install agent slash commands into %s: %v", configDir, err)
		}
	}
}

// waitStartup waits (bounded) for SessionStart while driving known startup
// dialogs. Unknown dialogs are left for a human (reported by Observe).
func (a *Adapter) waitStartup(ctx context.Context, h contracts.RunHandle) {
	deadline := time.Now().Add(a.o.StartupWait)
	var dialogSince time.Time
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return
		}
		e := a.entry(h)
		e.mu.Lock()
		a.refresh(h, e)
		started := e.rs.hasStart
		e.mu.Unlock()
		if started {
			return
		}
		if p, err := a.srv.Pane(ctx, h.TmuxTarget); err != nil || p.Dead {
			return
		}
		si, _ := a.screen(ctx, h.TmuxTarget)
		if a.handleStartupDialog(ctx, h, si) {
			dialogSince = time.Time{}
		} else if si.Dialog != "" {
			if dialogSince.IsZero() {
				dialogSince = time.Now()
			} else if time.Since(dialogSince) > 3*time.Second {
				return // needs a human
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// handleStartupDialog drives the workspace-trust dialog when the owner
// trusts cwd, the development-channels warning of a run launched to load
// them, and Claude's first-run onboarding screens. Returns true if it
// pressed a key.
func (a *Adapter) handleStartupDialog(ctx context.Context, h contracts.RunHandle, si screenInfo) bool {
	switch si.DialogKind {
	case "devchannels", "onboarding", "trust":
	default:
		return false
	}
	// One caller at a time, judging a screen read after any earlier press:
	// a key meant for one dialog must never land on the next.
	e := a.entry(h)
	e.startupMu.Lock()
	defer e.startupMu.Unlock()
	fresh, err := a.screen(ctx, h.TmuxTarget)
	if err != nil {
		return false
	}
	si = fresh
	if si.DialogKind == "devchannels" {
		return a.acceptDevChannels(ctx, h, si)
	}
	if si.DialogKind == "onboarding" {
		return a.answerOnboarding(ctx, h, e, si)
	}
	if si.DialogKind != "trust" {
		return false
	}
	meta := a.meta(h)
	if meta == nil || !(meta.Bypass || (a.o.TrustWorkspace != nil && a.o.TrustWorkspace(meta.Cwd))) {
		return false
	}
	if si.TrustYes {
		if a.srv.SendKeys(ctx, h.TmuxTarget, tmux.KeyEnter) == nil {
			a.automation(h, contracts.AutomationTrustAccepted, meta.Cwd)
		}
	} else {
		_ = a.srv.SendKeys(ctx, h.TmuxTarget, tmux.KeyDown)
	}
	time.Sleep(300 * time.Millisecond)
	return true
}

// acceptDevChannels confirms Claude's "Loading development channels"
// warning for a run whose launch arguments load development channels (the
// owner passed --dangerously-load-development-channels for the session);
// any other run leaves it for a human.
func (a *Adapter) acceptDevChannels(ctx context.Context, h contracts.RunHandle, si screenInfo) bool {
	meta := a.meta(h)
	if meta == nil || len(meta.DevChannels) == 0 || a.o.Now().Sub(meta.LaunchedAt) > devChannelsWindow {
		return false // a startup dialog: never answered later in the run
	}
	if si.DevAccept {
		if a.srv.SendKeys(ctx, h.TmuxTarget, tmux.KeyEnter) == nil {
			a.automation(h, contracts.AutomationDevChannelsAccepted, strings.Join(meta.DevChannels, ", "))
		}
	} else {
		_ = a.srv.SendKeys(ctx, h.TmuxTarget, tmux.KeyUp)
	}
	time.Sleep(300 * time.Millisecond)
	return true
}

// devChannelsWindow bounds when the development-channels warning is
// answered: it is a startup dialog (shown before the input box).
const devChannelsWindow = 2 * time.Minute

// answerOnboarding presses Enter on Claude's first-run onboarding screens
// (the theme picker, keeping the highlighted theme, and the security notes)
// of a run that has not started: Claude shows them whenever the profile's
// config lacks hasCompletedOnboarding, and an unattended run otherwise sits
// there and never runs its prompt. Neither screen is a decision (the theme
// is cosmetic and changeable with /theme; the notes only continue).
//
// Each screen gets one press. A screen still shown is pressed again only
// after onboardingRepress (a slow redraw, e.g. on Windows, otherwise turns
// the extra Enter into an answer to the next screen: the workspace-trust
// dialog, a login method). After maxOnboardingPresses the screen is left
// for a human (Observe reports it as needs human). Caller holds
// e.startupMu.
func (a *Adapter) answerOnboarding(ctx context.Context, h contracts.RunHandle, e *runEntry, si screenInfo) bool {
	now := time.Now()
	o := &e.onb
	if o.step == si.Onboarding {
		if now.Sub(o.at) < onboardingRepress {
			return true // pressed; waiting for the next screen to be drawn
		}
		if o.n >= maxOnboardingPresses {
			return false
		}
	} else {
		o.step, o.n = si.Onboarding, 0
	}
	if o.total >= 2*maxOnboardingPresses {
		return false
	}
	if a.srv.SendKeys(ctx, h.TmuxTarget, tmux.KeyEnter) != nil {
		return false // not pressed: not counted
	}
	o.at, o.n, o.total = now, o.n+1, o.total+1
	a.automation(h, contracts.AutomationOnboardingAnswered, "onboarding (theme picker / security notes)")
	time.Sleep(300 * time.Millisecond)
	return true
}

// onboardingRepress is how long a still-shown onboarding screen waits
// before another Enter; maxOnboardingPresses bounds presses per screen.
const (
	onboardingRepress    = 3 * time.Second
	maxOnboardingPresses = 3
)

// notStarted reports a run with no hook event at all (not even
// SessionStart), no input box and no recognised dialog longer than
// FirstRunScreenWait after launch. firstRun: the screen looks like Claude's
// first-run onboarding (its "Welcome to Claude Code" banner), a screen that
// blocks the run until a human answers it.
func (a *Adapter) notStarted(h contracts.RunHandle, noEvents bool, si screenInfo, now time.Time) (stuck, firstRun bool) {
	if !noEvents || si.HasInputBox || si.Dialog != "" {
		return false, false
	}
	meta := a.meta(h)
	if meta == nil || meta.LaunchedAt.IsZero() || now.Sub(meta.LaunchedAt) <= a.o.FirstRunScreenWait {
		return false, false
	}
	all := strings.Join(si.Lines, "\n")
	return true, strings.Contains(all, "Welcome to Claude Code") || strings.Contains(all, "Let's get started")
}

// State reasons for notStarted.
const (
	firstRunScreenReason = "needs human: Claude first-run screen (attach to answer it)"
	notStartedReason     = "Claude has not started (no SessionStart after %d s)"
)

func nonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}

func (a *Adapter) screen(ctx context.Context, name string) (screenInfo, error) {
	plain, err := a.srv.CapturePane(ctx, name, tmux.CaptureOptions{Join: true})
	if err != nil {
		return screenInfo{}, err
	}
	esc, _ := a.srv.CapturePane(ctx, name, tmux.CaptureOptions{Join: true, Escapes: true})
	return parseScreen(plain, esc), nil
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

// refresh folds new hook events and transcript lines into e.rs (e.mu held).
func (a *Adapter) refresh(h contracts.RunHandle, e *runEntry) {
	rs := e.rs
	recs, next, err := readEvents(h.StateDir, rs.eventsOff)
	if err == nil {
		for _, r := range recs {
			rs.applyHook(r)
		}
		rs.eventsOff = next
	}
	path := rs.transcriptPath
	if path == "" {
		if m := a.loadMeta(h, e); m != nil {
			path = guessTranscriptPath(m.ConfigDir, m.Cwd, m.SessionUUID)
		}
	}
	if path == "" {
		return
	}
	if path != rs.tPath {
		rs.tPath, rs.tOff = path, 0
		e.tpath.Store(path)
	}
	next, err = readJSONLFrom(path, rs.tOff, 8<<20, func(line []byte, off int64) {
		var t transcriptRecord
		if json.Unmarshal(line, &t) == nil {
			rs.applyTranscript(t, off)
		}
	})
	if err == nil {
		rs.tOff = next
	}
}

// ChangeStamp is a cheap fingerprint of the run's hook-event file and
// native transcript (size and modification time). The daemon compares it
// between observations so a backed-off observer wakes as soon as Claude
// writes something. A native-terminal interrupt fires no hook; its only
// trace is the transcript's system/turn_duration record. It never takes the
// run lock.
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

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return osutil.ProcessAlive(pid)
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

// approvalID derives a stable Pyre approval id from run + request id.
func approvalID(runID, requestID string) string {
	sum := sha256.Sum256([]byte(runID + "\x00" + requestID))
	return "apr_" + strings.ToUpper(hex.EncodeToString(sum[:13]))
}

type answerState struct {
	submitted bool
	option    string
	at        time.Time
}

func readAnswerState(stateDir, rid string) answerState {
	for _, suffix := range []string{".consumed", ".json"} {
		b, err := os.ReadFile(filepath.Join(stateDir, dirAnswers, rid+suffix))
		if err != nil {
			continue
		}
		var af answerFile
		if json.Unmarshal(b, &af) == nil {
			return answerState{submitted: true, option: af.OptionID, at: af.At}
		}
	}
	return answerState{}
}

func readPending(stateDir, rid string) *pendingRequest {
	b, err := os.ReadFile(filepath.Join(stateDir, dirPending, rid+".json"))
	if err != nil {
		return nil
	}
	var p pendingRequest
	if json.Unmarshal(b, &p) != nil {
		return nil
	}
	return &p
}

// evalApprovals turns broker tracks into contract approvals. The TUI races
// the broker, so a Pyre answer is "effective" only when runtime evidence
// follows (PostToolUse after an allow; the turn moving on after a deny).
func (a *Adapter) evalApprovals(h contracts.RunHandle, rs *runState, derived contracts.RunState) []contracts.Approval {
	var out []contracts.Approval
	order := rs.approvalOrder
	if len(order) > 50 {
		order = order[len(order)-50:]
	}
	for _, rid := range order {
		t := rs.approvals[rid]
		ap := contracts.Approval{
			ApprovalID: approvalID(h.RunID, rid), SessionID: h.SessionID, RunID: h.RunID, RequestID: rid,
			Kind: contracts.ApprovalPermission, Title: "Allow " + t.ToolName + "?", Body: redact(t.Summary),
			Options: []contracts.ApprovalOption{
				{ID: "allow", Label: "Allow", Kind: "allow"},
				{ID: "deny", Label: "Deny", Kind: "deny"},
			},
			AllowText: true, CreatedAt: t.CreatedAt, State: contracts.ApprovalPending,
		}
		if pr := readPending(h.StateDir, rid); pr != nil {
			exp := pr.ExpiresAt
			ap.ExpiresAt = &exp
		}
		ans := readAnswerState(h.StateDir, rid)
		postAt, hasPost := time.Time{}, false
		if t.ToolUseID != "" {
			postAt, hasPost = rs.toolPostAt[t.ToolUseID]
		}
		movedOn := rs.lastTurnDurationAt.After(t.CreatedAt) ||
			(rs.lastStateAt.After(t.CreatedAt) && derived != contracts.RunWaitingApproval && derived != contracts.RunRunning) ||
			rs.ended
		resolve := func(source, effect string, at time.Time, option string) {
			ap.State = contracts.ApprovalResolved
			ap.Resolution = &contracts.ApprovalResolution{OptionID: option, ResolvedAt: at, Source: source, Effect: effect}
		}
		superseded := func() string {
			if ans.submitted {
				return "superseded"
			}
			return ""
		}
		switch {
		case !t.ScreenAnsweredAt.IsZero() && (t.AnsweredAt.IsZero() || t.AnsweredAt.After(t.ScreenAnsweredAt)):
			// The dialog closed and the tool runs: answered in the terminal
			// (a broker answer consumed later had no effect: C14, the hook
			// keeps waiting after a TUI "Yes").
			resolve("native", superseded(), t.ScreenAnsweredAt, "")
		case !t.AnsweredAt.IsZero():
			option := ans.option
			if option == "" {
				option = t.Behavior
			}
			switch {
			case hasPost && postAt.Before(t.AnsweredAt):
				resolve("native", "superseded", postAt, "")
			case t.Behavior == "allow" && hasPost:
				resolve("pyre", "effective", t.AnsweredAt, option)
			case t.Behavior == "deny" && (movedOn || rs.lastEventAt.After(t.AnsweredAt)):
				resolve("pyre", "effective", t.AnsweredAt, option)
			default:
				resolve("pyre", "submitted", t.AnsweredAt, option)
			}
		case hasPost:
			resolve("native", superseded(), postAt, "")
		case t.ToolUseID != "" && !rs.toolDeniedAt[t.ToolUseID].IsZero():
			// auto mode denied it (PermissionDenied) before anyone answered
			resolve("native", superseded(), rs.toolDeniedAt[t.ToolUseID], "deny")
		case t.Withdrawn:
			resolve("native", superseded(), rs.lastEventAt, "")
		case t.TimedOut:
			if movedOn {
				resolve("native", superseded(), rs.lastEventAt, "")
			} else {
				ap.State = contracts.ApprovalExpired // TUI dialog still up; a human must answer there
			}
		case t.HookPID > 0 && !pidAlive(t.HookPID):
			// Claude killed the waiting hook: answered "No" in the TUI or interrupted.
			resolve("native", superseded(), rs.lastEventAt, "")
		case movedOn:
			ap.State = contracts.ApprovalWithdrawn
		}
		out = append(out, ap)
	}
	return out
}

// permDialogGrace: a permission request younger than this whose dialog no
// observation has shown yet is never judged answered by the screen (Claude
// may not have drawn the dialog yet).
const permDialogGrace = 3 * time.Second

// observeApprovals evaluates the broker requests against the current screen
// (si, nil when unavailable) and returns them with the run state (e.mu
// held). Claude fires no hook when a permission dialog is answered "Yes" in
// the terminal (C14: the broker hook keeps waiting) until the tool finishes,
// so without the screen a long tool kept the run waiting_approval and its
// expired request flagged "answer in its terminal". Every observation
// re-checks the screen: a request is answered in the terminal once the
// dialog is gone and the turn works ("esc to interrupt"), or, after the
// remote answer window expired, once no dialog covers the input box. A
// visible dialog keeps it waiting; nothing persists from a past screen
// except the answered mark.
func (a *Adapter) observeApprovals(h contracts.RunHandle, rs *runState, state contracts.RunState, reason string, si *screenInfo, now time.Time) ([]contracts.Approval, contracts.RunState, string) {
	aps := a.evalApprovals(h, rs, state)
	answered := false
	if si != nil {
		open := func(ap contracts.Approval) *approvalTrack {
			if ap.State != contracts.ApprovalPending && ap.State != contracts.ApprovalExpired {
				return nil
			}
			t := rs.approvals[ap.RequestID]
			if t == nil || !t.AnsweredAt.IsZero() {
				return nil
			}
			return t
		}
		for _, ap := range aps {
			t := open(ap)
			if t == nil {
				continue
			}
			switch {
			case si.DialogKind == "permission":
				if t.DialogSeenAt.IsZero() {
					t.DialogSeenAt = now
				}
			case !si.Clear():
			case t.DialogSeenAt.IsZero() && now.Sub(t.CreatedAt) < permDialogGrace:
			case si.Working() || ap.State == contracts.ApprovalExpired:
				t.ScreenAnsweredAt = now
				rs.activity = t.ToolName + ": " + oneLine(t.Summary, 120)
				answered = true
			}
		}
		if answered {
			aps = a.evalApprovals(h, rs, state)
		}
	}
	waiting := false
	for _, ap := range aps {
		if ap.State == contracts.ApprovalPending || ap.State == contracts.ApprovalExpired {
			waiting = true
			state = contracts.RunWaitingApproval
			if ap.State == contracts.ApprovalExpired {
				reason = "needs human: permission prompt waiting in its terminal (remote answer window expired; attach to answer it, or interrupt to cancel)"
			} else {
				reason = "permission request pending (" + ap.Title + ")"
			}
		}
	}
	// A permission prompt reported by a hook is waiting only while the
	// screen shows it: answered in the terminal, the tool runs.
	if !waiting && state == contracts.RunWaitingApproval && rs.permPrompt && si != nil && si.Clear() &&
		(answered || (si.Working() && now.Sub(rs.lastStateAt) >= permDialogGrace)) {
		rs.set(contracts.RunRunning, "permission answered in the terminal; tool running", now)
		state, reason = rs.state, rs.reason
	}
	return aps, state, reason
}

func (a *Adapter) Observe(ctx context.Context, h contracts.RunHandle, cursor string) (contracts.Observation, error) {
	now := a.o.Now()
	obs := contracts.Observation{ObservedAt: now, NativeRef: h.NativeRef}
	pane, perr := a.srv.Pane(ctx, h.TmuxTarget)
	gone := perr != nil && (errors.Is(perr, tmux.ErrNoSession) || errors.Is(perr, tmux.ErrNoServer))

	// The screen is read before the hook events so a permission request
	// folded in below is never judged by an older screen (the grace in
	// observeApprovals covers a request whose dialog is not drawn yet).
	var si *screenInfo
	if perr == nil && !pane.Dead {
		if s, err := a.screen(ctx, h.TmuxTarget); err == nil {
			si = &s
		}
	}
	if si != nil && si.DialogKind == "devchannels" && a.handleStartupDialog(ctx, h, *si) {
		si = nil // answered (or moving to the answer); the next poll reads the screen again
	}

	e := a.entry(h)
	e.mu.Lock()
	a.refresh(h, e)
	rs := e.rs
	state, reason := rs.derive()
	obs.Approvals, state, reason = a.observeApprovals(h, rs, state, reason, si, now)
	if rs.sessionID != "" {
		obs.NativeRef = &contracts.NativeRef{Kind: "claude_session", ID: rs.sessionID}
	}
	obs.Activity = rs.activity
	if state == contracts.RunRunning {
		obs.Doing = rs.doingNow()
	}
	obs.LimitEvent = rs.limit
	for _, id := range rs.subOrder {
		s := rs.subagents[id]
		so := contracts.SubagentObservation{ID: s.ID, Kind: s.Kind, State: "running"}
		st := s.Start
		so.StartedAt = &st
		if !s.End.IsZero() {
			en := s.End
			so.EndedAt, so.State = &en, "stopped"
		}
		obs.Subagents = append(obs.Subagents, so)
	}
	if len(obs.Subagents) > 50 {
		obs.Subagents = obs.Subagents[len(obs.Subagents)-50:]
	}
	obs.Receipts, obs.NextCursor = a.receipts(h, rs, cursor)
	hasStart, ended, noEvents := rs.hasStart, rs.ended, rs.lastEventAt.IsZero()
	e.mu.Unlock()

	switch {
	case gone:
		state, reason = contracts.RunExited, "tmux session gone"
		if r := readExit(h.StateDir); r != nil {
			obs.ExitCode, reason = r.ExitCode, r.Reason
		}
	case perr != nil:
		state, reason = contracts.RunUnknown, "tmux unavailable: "+perr.Error()
	case pane.Dead:
		state, reason = contracts.RunExited, "claude exited"
		if ended {
			reason = "claude exited after SessionEnd"
		}
		obs.ExitCode = pane.ExitStatus
		writeExit(h.StateDir, exitRecord{ExitCode: pane.ExitStatus, Reason: reason, At: now})
	default:
		if si != nil {
			si := *si
			// A dialog covering the input box blocks the run until a human
			// answers it in the terminal: report it as waiting on a prompt
			// (waiting_approval, "needs human: ..."), never as idle at the
			// prompt, which would invite delivery retries.
			if !hasStart {
				if a.handleStartupDialog(ctx, h, si) {
					reason = "accepting workspace trust (owner-trusted directory)"
					if si.DialogKind == "onboarding" {
						reason = "answering " + si.Dialog
					}
				} else if si.Dialog != "" {
					state, reason = contracts.RunWaitingApproval, "needs human: "+si.Dialog+" (attach to answer it in the terminal)"
				} else if stuck, firstRun := a.notStarted(h, noEvents, si, now); firstRun {
					state, reason = contracts.RunWaitingApproval, firstRunScreenReason
				} else if stuck {
					reason = fmt.Sprintf(notStartedReason, int(a.o.FirstRunScreenWait.Seconds()))
				}
			} else if state == contracts.RunWaitingInput && si.Dialog != "" && !si.HasInputBox {
				state, reason = contracts.RunWaitingApproval, "needs human: "+si.Dialog+" (attach to answer it in the terminal)"
			}
			// No screen-based limit detection: a resumed session's screen still
			// shows the previous run's "You've hit your limit", which re-triggered
			// account switches. The StopFailure hook and timestamped transcript
			// API-error records report real limits.
			// A paste Pyre could not submit (indeterminate) may sit unsent in
			// the input box: say so, and what a human does about it.
			if state == contracts.RunWaitingInput && si.HasInputBox && !si.InputEmpty() {
				if id, _ := a.leftoverDraft(h, si); id != "" {
					if cs, err := a.srv.ListClients(ctx, h.TmuxTarget); err == nil && tmux.WritableClients(cs) == 0 {
						reason = "needs human: Claude's input box holds Pyre's unsent draft of " + id + " (its delivery is indeterminate). " + draftAction(id)
					}
				}
			}
		}
	}
	obs.State, obs.StateReason = state, reason
	return obs, nil
}

// cursor: "e<eventsOffset>.t<transcriptOffset>.p<pathHash>"
func parseObsCursor(c string) (eOff, tOff int64, pathHash string) {
	for _, part := range strings.Split(c, ".") {
		if len(part) < 2 {
			continue
		}
		var n int64
		switch part[0] {
		case 'e':
			fmt.Sscanf(part[1:], "%d", &n)
			eOff = n
		case 't':
			fmt.Sscanf(part[1:], "%d", &n)
			tOff = n
		case 'p':
			pathHash = part[1:]
		}
	}
	return
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:4])
}

// receipts reports evidence discovered since the cursor for messages this
// adapter attempted (acceptance found after an indeterminate result, and
// consumption of natively queued input).
func (a *Adapter) receipts(h contracts.RunHandle, rs *runState, cursor string) ([]contracts.DeliveryReceipt, string) {
	eOff, tOff, ph := parseObsCursor(cursor)
	if ph != shortHash(rs.tPath) {
		tOff = 0
	}
	next := fmt.Sprintf("e%d.t%d.p%s", rs.eventsOff, rs.tOff, shortHash(rs.tPath))
	attempts := lastAttempts(h.StateDir)
	isNew := func(e evidence) bool {
		if e.Source == 'e' {
			return e.Offset >= eOff
		}
		return e.Offset >= tOff
	}
	var out []contracts.DeliveryReceipt
	for msg, evs := range rs.accepted {
		att, attempted := attempts[msg]
		for _, e := range evs {
			// Doorbell reads need no paste attempt; other evidence is only
			// reported for messages this adapter attempted.
			if !isNew(e) || (!attempted && e.Kind != contracts.EvidenceClaudeInboxRead) {
				continue
			}
			detail := "accepted"
			if c, ok := rs.consumed[msg]; ok && c.Ref == e.Ref {
				detail = "consumed_at=" + c.At.UTC().Format(time.RFC3339Nano)
			}
			if e.Kind == contracts.EvidenceClaudeInboxRead {
				detail = "doorbell: read with the pyre inbox tool (consumed_at=" + e.At.UTC().Format(time.RFC3339Nano) + ")"
				if at, ok := rs.acked[msg]; ok {
					detail += "; acked_at=" + at.UTC().Format(time.RFC3339Nano)
				}
			}
			out = append(out, contracts.DeliveryReceipt{
				MessageID: msg, AttemptID: att.AttemptID, SessionID: h.SessionID, RunID: h.RunID, NodeID: a.o.NodeID,
				State: contracts.DeliveryAccepted, Evidence: e.Kind, EvidenceRef: e.Ref, Detail: detail, ObservedAt: e.At,
			})
		}
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
	e := a.entry(h)
	e.mu.Lock()
	a.refresh(h, e)
	if e.rs.sessionID != "" {
		h.NativeRef = &contracts.NativeRef{Kind: "claude_session", ID: e.rs.sessionID}
	}
	e.mu.Unlock()
	return h, nil
}

func (a *Adapter) Attach(ctx context.Context, h contracts.RunHandle, req contracts.AttachCommand) (contracts.AttachDescriptor, error) {
	if ok, err := a.srv.HasSession(ctx, h.TmuxTarget); err != nil || !ok {
		return contracts.AttachDescriptor{}, contracts.Errorf(contracts.ErrNotFound, "tmux session %s is gone", h.TmuxTarget)
	}
	d, err := a.srv.AttachDescriptor(sessionhost.AttachConfig{NodeID: a.o.NodeID, SSHAlias: a.o.SSHAlias},
		h.RunID, h.TmuxTarget, req, a.o.Now())
	if err == nil && !d.ReadOnly {
		d.Notes += "; while attached, Pyre queues structured input instead of pasting"
	}
	return d, err
}

func (a *Adapter) receipt(outcome contracts.CommandOutcome, accepted bool, detail string) contracts.CommandReceipt {
	return contracts.CommandReceipt{Accepted: accepted, Outcome: outcome, Detail: detail, ObservedAt: a.o.Now()}
}

func (a *Adapter) derived(h contracts.RunHandle) (contracts.RunState, string, *runState) {
	e := a.entry(h)
	e.mu.Lock()
	defer e.mu.Unlock()
	a.refresh(h, e)
	s, r := e.rs.derive()
	return s, r, e.rs
}

// Interrupt sends Escape to the TUI. It is a raw terminal action: the daemon
// must hold the run's input lease before calling it.
func (a *Adapter) Interrupt(ctx context.Context, h contracts.RunHandle) (contracts.CommandReceipt, error) {
	p, err := a.srv.Pane(ctx, h.TmuxTarget)
	if err != nil || p.Dead {
		return a.receipt(contracts.OutcomeFailed, false, "run is not live"), nil
	}
	state, _, _ := a.derived(h)
	if state != contracts.RunRunning && state != contracts.RunWaitingApproval {
		return a.receipt(contracts.OutcomeSucceeded, true, "nothing to interrupt (state "+string(state)+")"), nil
	}
	if err := a.srv.SendKeys(ctx, h.TmuxTarget, tmux.KeyEscape); err != nil {
		return a.receipt(contracts.OutcomeFailed, false, err.Error()), nil
	}
	end := time.Now().Add(5 * time.Second)
	for time.Now().Before(end) {
		time.Sleep(250 * time.Millisecond)
		if s, _, _ := a.derived(h); s != contracts.RunRunning && s != contracts.RunWaitingApproval {
			return a.receipt(contracts.OutcomeSucceeded, true, "turn ended after Escape (state "+string(s)+")"), nil
		}
	}
	return a.receipt(contracts.OutcomeDelivered, true, "Escape sent; turn end not yet observed"), nil
}

// Stop types /exit when the prompt is idle and empty (waiting for
// SessionEnd), and falls back to kill-session. The tmux session is removed
// after the exit status is recorded.
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
	if !p.Dead {
		if a.gracefulExit(ctx, h) {
			method = "/exit"
		}
	}
	if p2, err := a.srv.Pane(ctx, h.TmuxTarget); err == nil {
		reason := "stopped by pyre (" + method + ")"
		writeExit(h.StateDir, exitRecord{ExitCode: p2.ExitStatus, Reason: reason, At: now})
	}
	if err := a.srv.TerminateSession(ctx, h.TmuxTarget, 5*time.Second); err != nil {
		return a.receipt(contracts.OutcomeFailed, false, err.Error()), nil
	}
	if ok, _ := a.srv.HasSession(ctx, h.TmuxTarget); ok {
		return a.receipt(contracts.OutcomeIndeterminate, true, "tmux session still present"), nil
	}
	return a.receipt(contracts.OutcomeSucceeded, true, "stopped via "+method), nil
}

func (a *Adapter) gracefulExit(ctx context.Context, h contracts.RunHandle) bool {
	e := a.entry(h)
	e.deliverMu.Lock()
	defer e.deliverMu.Unlock()
	if cs, err := a.srv.ListClients(ctx, h.TmuxTarget); err != nil || tmux.WritableClients(cs) > 0 {
		return false
	}
	state, _, _ := a.derived(h)
	if state == contracts.RunRunning || state == contracts.RunWaitingApproval {
		_ = a.srv.SendKeys(ctx, h.TmuxTarget, tmux.KeyEscape)
		time.Sleep(800 * time.Millisecond)
	}
	si, err := a.screen(ctx, h.TmuxTarget)
	if err != nil || !si.InputEmpty() {
		return false
	}
	if err := a.srv.Paste(ctx, h.TmuxTarget, "pyre-exit", []byte("/exit")); err != nil {
		return false
	}
	ok := false
	for i := 0; i < 10; i++ {
		time.Sleep(150 * time.Millisecond)
		if si, err := a.screen(ctx, h.TmuxTarget); err == nil && si.HasInputBox && strings.TrimSpace(si.InputText()) == "/exit" {
			ok = true
			break
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
	answered := false
	for time.Now().Before(end) {
		time.Sleep(200 * time.Millisecond)
		if p, err := a.srv.Pane(ctx, h.TmuxTarget); err != nil || p.Dead {
			return true
		}
		// A background task (e.g. Pyre's own message monitor) makes Claude
		// ask before exiting. Stopping was requested, so confirm "Exit and
		// stop tasks" — only while that option is the selected one.
		if !answered {
			if si, err := a.screen(ctx, h.TmuxTarget); err == nil && exitStopsTasksSelected(si.Lines) {
				answered = a.srv.SendKeys(ctx, h.TmuxTarget, tmux.KeyEnter) == nil
				if answered {
					a.automation(h, contracts.AutomationExitConfirmed, "")
				}
			}
		}
	}
	return false
}

// automation reports a dialog the adapter answered by itself (once per run
// and action).
func (a *Adapter) automation(h contracts.RunHandle, action, detail string) {
	if a.o.OnAutomation == nil {
		return
	}
	if _, dup := a.automated.LoadOrStore(action+"\x00"+h.RunID, true); dup {
		return
	}
	go a.o.OnAutomation(contracts.NodeAutomation{RunID: h.RunID, SessionID: h.SessionID, Harness: contracts.HarnessClaude, Action: action, Detail: detail})
}

// exitStopsTasksSelected reports Claude's "Background work is running"
// exit dialog with the cursor on "Exit and stop tasks".
func exitStopsTasksSelected(lines []string) bool {
	dialog := false
	for _, l := range lines {
		if strings.Contains(l, "Background work is running") {
			dialog = true
		}
		if dialog && strings.Contains(l, "❯") && strings.Contains(l, "Exit and stop tasks") {
			return true
		}
	}
	return false
}

// Answer submits a broker answer for a pending PermissionRequest. The
// receipt reports Effect "submitted"; Observe later reports "effective" or
// "superseded" (answered in the TUI first).
func (a *Adapter) Answer(ctx context.Context, h contracts.RunHandle, ap contracts.Approval, optionID, text string) (contracts.CommandReceipt, error) {
	if a.o.DisableApprovalBroker {
		return a.receipt(contracts.OutcomeUnsupported, false, ""), contracts.Errorf(contracts.ErrUnsupported, "approval broker disabled; answer in the terminal")
	}
	if ap.RunID != "" && ap.RunID != h.RunID {
		return a.receipt(contracts.OutcomeFailed, false, ""), contracts.Errorf(contracts.ErrConflict, "approval belongs to run %s, not %s", ap.RunID, h.RunID)
	}
	rid := ap.RequestID
	if !safeID(rid) || readPending(h.StateDir, rid) == nil {
		return a.receipt(contracts.OutcomeFailed, false, ""), contracts.Errorf(contracts.ErrNotFound, "no pending request %q for run %s", rid, h.RunID)
	}
	if ap.ApprovalID != "" && ap.ApprovalID != approvalID(h.RunID, rid) {
		return a.receipt(contracts.OutcomeFailed, false, ""), contracts.Errorf(contracts.ErrConflict, "approval id does not match request %s", rid)
	}
	behavior := ""
	switch optionID {
	case "allow":
		behavior = "allow"
	case "deny":
		behavior = "deny"
	default:
		return a.receipt(contracts.OutcomeFailed, false, ""), contracts.Errorf(contracts.ErrInvalid, "option %q is not allow|deny", optionID)
	}
	e := a.entry(h)
	e.mu.Lock()
	a.refresh(h, e)
	state, _ := e.rs.derive()
	var current *contracts.Approval
	for _, x := range a.evalApprovals(h, e.rs, state) {
		if x.RequestID == rid {
			x := x
			current = &x
		}
	}
	e.mu.Unlock()
	if current == nil {
		return a.receipt(contracts.OutcomeFailed, false, ""), contracts.Errorf(contracts.ErrNotFound, "request %s not observed", rid)
	}
	if current.State != contracts.ApprovalPending {
		return a.receipt(contracts.OutcomeFailed, false, "already "+string(current.State)),
			contracts.Errorf(contracts.ErrConflict, "request %s is already %s", rid, current.State).WithDetail("state", string(current.State))
	}
	lock := filepath.Join(h.StateDir, dirAnswers, rid+".lock")
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return a.receipt(contracts.OutcomeFailed, false, "already answered"), contracts.Errorf(contracts.ErrConflict, "request %s already answered through Pyre", rid)
	}
	f.Close()
	af := answerFile{RequestID: rid, RunID: h.RunID, OptionID: optionID, Behavior: behavior, Message: text, At: a.o.Now().UTC()}
	b, _ := json.Marshal(af)
	if err := writeFileAtomic(filepath.Join(h.StateDir, dirAnswers, rid+".json"), b); err != nil {
		return a.receipt(contracts.OutcomeFailed, false, err.Error()), nil
	}
	// Brief wait for the broker hook to hand the decision to Claude.
	end := time.Now().Add(3 * time.Second)
	for time.Now().Before(end) {
		if _, err := os.Stat(filepath.Join(h.StateDir, dirAnswers, rid+".consumed")); err == nil {
			return a.receipt(contracts.OutcomeDelivered, true, "effect=submitted; decision handed to Claude by the broker hook"), nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return a.receipt(contracts.OutcomeDelivered, true, "effect=submitted; broker hook has not consumed it yet"), nil
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
	if limit > 500 {
		limit = 500
	}
	var start int64
	if req.Cursor != "" {
		fmt.Sscanf(req.Cursor, "%d", &start)
	}
	var sel []contracts.TranscriptEntry
	if path != "" {
		sel, _ = a.tails.read(path, h.RunID, start, limit, req.Tail)
	}
	resp.Entries = sel
	if len(sel) > 0 {
		resp.NextCursor = fmt.Sprintf("%d", sel[len(sel)-1].Seq)
	} else {
		resp.NextCursor = req.Cursor
	}
	if path == "" {
		resp.Freshness.Reason = "transcript not located yet"
	}
	if screen, err := a.srv.CapturePane(ctx, h.TmuxTarget, tmux.CaptureOptions{Join: true}); err == nil {
		resp.Screen = strings.TrimRight(screen, "\n")
	}
	return resp, nil
}

// ResumeSpec: native `--resume <id>` keeps the same Claude session id when
// the previous thread is reachable from the new binding: the same account,
// or (daemon-checked, CapAccountCrossResume) another account whose profile
// resolves to the same shared transcript store. Otherwise a handoff.
func (a *Adapter) ResumeSpec(ctx context.Context, prev contracts.Run, sameAccount bool) (contracts.ContinuationKind, *contracts.NativeRef, error) {
	if prev.NativeRef == nil || prev.NativeRef.Kind != "claude_session" || !uuidRe.MatchString(prev.NativeRef.ID) {
		return contracts.ContinuationHandoff, nil, nil
	}
	if !sameAccount {
		return contracts.ContinuationHandoff, nil, nil
	}
	ref := *prev.NativeRef
	return contracts.ContinuationNativeResume, &ref, nil
}

// ensureFirstRunAnswers adds Claude's non-secret first-run answers to a Pyre
// profile's .claude.json when they are missing, at every launch: a profile
// composed before the defaults existed (or rewritten by Claude) otherwise
// stops at the onboarding theme picker and never runs the initial prompt
// (first GCP Windows node; win-home 2026-10-20, whose .claude.json held only
// "projects"). hasCompletedOnboarding alone gates the onboarding screens
// (Claude Code 2.1.299-2.1.304). theme is kept for older versions; Claude
// 2.1.30x keeps the theme in settings.json and drops a default-valued
// "dark" from .claude.json when it saves, so its absence there is normal.
// Existing values are never changed. A file Pyre cannot read, parse or
// write, or a symlinked one, is left alone and logged once (the screen fallback in
// answerOnboarding still gets the run started).
func ensureFirstRunAnswers(dir string) {
	p := filepath.Join(dir, ".claude.json")
	if fi, err := os.Lstat(p); err == nil && fi.Mode()&osutil.LinkModes != 0 {
		// The rename would replace the link (and whatever it points to may
		// be the owner's own config): left alone.
		firstRunNote(p, "a symlink; left alone")
		return
	}
	cfg := map[string]json.RawMessage{}
	if b, err := os.ReadFile(p); err == nil {
		// A UTF-8 byte-order mark (Windows editors, PowerShell 5) is not JSON.
		if json.Unmarshal(bytes.TrimPrefix(b, utf8BOM), &cfg) != nil || cfg == nil {
			firstRunNote(p, "unparseable; left alone")
			return
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		firstRunNote(p, "read: "+err.Error())
		return
	}
	changed := false
	for k, v := range map[string]string{"hasCompletedOnboarding": "true", "theme": `"dark"`} {
		if raw, ok := cfg[k]; !ok || string(raw) == "null" {
			cfg[k] = json.RawMessage(v)
			changed = true
		}
	}
	if !changed {
		return
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err == nil {
		err = os.MkdirAll(dir, 0o700) // a profile Claude has not run in yet
	}
	if err == nil {
		err = writeFileAtomic(p, b)
	}
	if err != nil {
		firstRunNote(p, "write: "+err.Error())
	}
}

var utf8BOM = []byte("\xef\xbb\xbf")

// firstRunNotes dedupes ensureFirstRunAnswers log lines (path + problem).
var firstRunNotes sync.Map

func firstRunNote(p, problem string) {
	if _, seen := firstRunNotes.LoadOrStore(p+"\x00"+problem, true); !seen {
		log.Printf("claude: first-run answers not added to %s (%s); an onboarding screen at launch is answered on screen instead", p, problem)
	}
}
