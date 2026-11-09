// Package shell is the runtime adapter for plain shell sessions inside the
// node's isolated tmux server. A shell has no acceptance signal, busy state or
// turn concept, so agent actions (deliver, steer, answer, interrupt, resume)
// are unsupported: Pyre never types a message into a shell prompt. The one
// exception is the session's initial prompt (pyre start --harness shell
// --prompt CMD): it is the launcher's own command, typed once the shell is
// ready, as if the user had typed it and pressed Enter. Humans attach to the
// native terminal (raw typing there is governed by the input lease).
package shell

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/hostsel"
	"github.com/jhh3/pyre/internal/runtime/sessionhost"
	"github.com/jhh3/pyre/internal/runtime/tmux"
)

// AdapterName is the RunHandle.Adapter value.
const AdapterName = "shell-tmux"

// Options configures the shell adapter.
type Options struct {
	NodeID string
	// TmuxBin is the tmux executable (default: tmux on PATH).
	TmuxBin string
	// TmuxSocket is the node's isolated tmux server socket (required), in a
	// 0700 Pyre state directory, e.g. <pyre-state>/tmux/pyre.sock.
	TmuxSocket string
	// SSHAlias lets other machines attach with ssh -t (optional).
	SSHAlias string
	// Shell is used when ResolvedLaunch.Executable is empty (default $SHELL,
	// then /bin/bash) and is started as a login shell.
	Shell string
	// ReadyTimeout bounds the wait for a shell to be ready for its initial
	// prompt (default 30s).
	ReadyTimeout time.Duration
	Now          func() time.Time
}

// Adapter implements contracts.RuntimeAdapter for plain shells.
type Adapter struct {
	o   Options
	srv sessionhost.Host
}

var _ contracts.RuntimeAdapter = (*Adapter)(nil)

// New validates options and prepares the tmux server handle (the server
// itself starts lazily on the first launch).
func New(o Options) (*Adapter, error) {
	if o.TmuxSocket == "" {
		return nil, errors.New("shell: TmuxSocket is required")
	}
	srv, err := hostsel.New(o.TmuxBin, o.TmuxSocket)
	if err != nil {
		return nil, err
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.ReadyTimeout <= 0 {
		o.ReadyTimeout = 30 * time.Second
	}
	if o.Shell == "" {
		o.Shell = os.Getenv("SHELL")
	}
	if o.Shell == "" {
		o.Shell = defaultShell()
	}
	return &Adapter{o: o, srv: srv}, nil
}

// Server exposes the session host (for daemon housekeeping).
func (a *Adapter) Server() sessionhost.Host { return a.srv }

func (a *Adapter) Name() string               { return AdapterName }
func (a *Adapter) Harness() contracts.Harness { return contracts.HarnessShell }

// Capabilities advertised by this adapter.
func Capabilities() []string {
	return []string{
		contracts.CapAttachNative, contracts.CapAttachReadOnly,
		contracts.CapStop, contracts.CapScreen, contracts.CapLaunchEnvFile,
		contracts.CapShellInitialPrompt,
	}
}

func (a *Adapter) Probe(ctx context.Context) (contracts.ProbeResult, error) {
	pr := contracts.ProbeResult{Harness: contracts.HarnessShell, Adapter: AdapterName, Executable: a.o.Shell, Capabilities: Capabilities()}
	desc, err := a.srv.Check(ctx)
	pr.Version = desc
	if err != nil {
		pr.Reason = err.Error()
		return pr, nil
	}
	if _, err := exec.LookPath(a.o.Shell); err != nil {
		pr.Reason = "shell not found: " + a.o.Shell
		return pr, nil
	}
	pr.Available = true
	return pr, nil
}

func (a *Adapter) Launch(ctx context.Context, l contracts.ResolvedLaunch) (contracts.RunHandle, error) {
	if !tmux.ValidName(l.RunID) {
		return contracts.RunHandle{}, contracts.Errorf(contracts.ErrInvalid, "run id %q is not a valid tmux session name", l.RunID)
	}
	if l.StateDir == "" {
		return contracts.RunHandle{}, contracts.Errorf(contracts.ErrInvalid, "state dir required")
	}
	if err := tmux.PrepareDir(l.StateDir); err != nil {
		return contracts.RunHandle{}, err
	}
	if ok, err := a.srv.HasSession(ctx, l.RunID); err != nil {
		return contracts.RunHandle{}, err
	} else if ok {
		return contracts.RunHandle{}, contracts.Errorf(contracts.ErrConflict, "run %s already has a live tmux session", l.RunID)
	}
	argv := []string{a.o.Shell, "-l"}
	if !loginShells[strings.TrimSuffix(filepath.Base(a.o.Shell), ".exe")] {
		argv = argv[:1] // PowerShell/cmd: no login-shell flag
	}
	if l.Executable != "" {
		argv = append([]string{l.Executable}, l.Args...)
		if len(l.Args) == 0 && loginShells[filepath.Base(l.Executable)] {
			argv = append(argv, "-l") // a plain shell starts as a login shell
		}
	}
	if err := a.srv.NewSession(ctx, tmux.SessionSpec{Name: l.RunID, Cwd: l.Cwd, Env: l.Env, Unset: l.Unset, Argv: argv}); err != nil {
		return contracts.RunHandle{}, contracts.Errorf(contracts.ErrUnavailable, "start shell: %v", err)
	}
	if cmd := initialCommand(l.InitialPrompt); cmd != "" {
		if err := a.typeInitial(ctx, l.RunID, argv[0], cmd); err != nil {
			_ = a.srv.KillSession(context.Background(), l.RunID)
			return contracts.RunHandle{}, err
		}
	}
	h := contracts.RunHandle{
		RunID: l.RunID, SessionID: l.SessionID, Adapter: AdapterName, Harness: contracts.HarnessShell,
		TmuxSocket: a.srv.SocketPath(), TmuxTarget: l.RunID, StateDir: l.StateDir,
		NativeRef: &contracts.NativeRef{Kind: "tmux_session", ID: l.RunID},
	}
	if p, err := a.srv.Pane(ctx, l.RunID); err == nil {
		h.PID = p.PID
	}
	return h, nil
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
	return h, nil
}

type exitRecord struct {
	ExitCode *int      `json:"exit_code,omitempty"`
	Reason   string    `json:"reason"`
	At       time.Time `json:"at"`
}

func writeExit(dir string, r exitRecord) {
	b, _ := json.Marshal(r)
	tmp := filepath.Join(dir, ".exit.json.tmp")
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, filepath.Join(dir, "exit.json"))
	}
}

func readExit(dir string) *exitRecord {
	b, err := os.ReadFile(filepath.Join(dir, "exit.json"))
	if err != nil {
		return nil
	}
	var r exitRecord
	if json.Unmarshal(b, &r) != nil {
		return nil
	}
	return &r
}

func (a *Adapter) Observe(ctx context.Context, h contracts.RunHandle, cursor string) (contracts.Observation, error) {
	now := a.o.Now()
	obs := contracts.Observation{ObservedAt: now, NativeRef: h.NativeRef, NextCursor: cursor}
	p, err := a.srv.Pane(ctx, h.TmuxTarget)
	if err != nil {
		if errors.Is(err, tmux.ErrNoSession) || errors.Is(err, tmux.ErrNoServer) {
			obs.State = contracts.RunExited
			obs.StateReason = "tmux session gone"
			if r := readExit(h.StateDir); r != nil {
				obs.ExitCode = r.ExitCode
				obs.StateReason = r.Reason
			}
			return obs, nil
		}
		obs.State = contracts.RunUnknown
		obs.StateReason = "tmux unavailable: " + err.Error()
		return obs, nil
	}
	if p.Dead {
		obs.State = contracts.RunExited
		obs.ExitCode = p.ExitStatus
		obs.StateReason = "shell exited"
		writeExit(h.StateDir, exitRecord{ExitCode: p.ExitStatus, Reason: "shell exited", At: now})
	}
	screen, err := a.srv.CapturePane(ctx, h.TmuxTarget, tmux.CaptureOptions{Join: true})
	if err == nil {
		obs.Activity = lastLine(screen)
		sum := sha256.Sum256([]byte(screen))
		hash := hex.EncodeToString(sum[:8])
		prevHash, prevAt := parseCursor(cursor)
		at := now
		changed := prevHash != hash
		if !changed && !prevAt.IsZero() {
			at = prevAt
		}
		obs.NextCursor = hash + ":" + strconv.FormatInt(at.Unix(), 10)
		if !p.Dead {
			obs.State = contracts.RunRunning
			if changed {
				obs.StateReason = "shell alive; screen changed (no turn semantics)"
			} else {
				obs.StateReason = "shell alive; screen unchanged since " + at.UTC().Format(time.RFC3339)
			}
		}
	} else if !p.Dead {
		obs.State = contracts.RunRunning
		obs.StateReason = "shell alive (screen unavailable)"
	}
	return obs, nil
}

func parseCursor(c string) (string, time.Time) {
	h, ts, ok := strings.Cut(c, ":")
	if !ok {
		return "", time.Time{}
	}
	n, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return h, time.Time{}
	}
	return h, time.Unix(n, 0)
}

func lastLine(screen string) string {
	lines := strings.Split(screen, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			if len(l) > 160 {
				l = l[:160]
			}
			return l
		}
	}
	return ""
}

// loginShells are executables started with -l when launched without args.
var loginShells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true}

func unsupported(what string) error {
	return contracts.Errorf(contracts.ErrUnsupported, "shell sessions do not support %s (Pyre types only the initial prompt into a shell; attach instead)", what)
}

// lineEditors are shells that always read an interactive command line in
// non-canonical mode (readline, ZLE, fish's reader), so that mode alone
// says they are ready.
var lineEditors = map[string]bool{"bash": true, "zsh": true, "fish": true}

// shellQuiet is how long the screen of a shell whose terminal mode says
// nothing (no line editor, as dash; or no tty to inspect) must stay
// unchanged, with something on it, before the shell counts as ready.
const shellQuiet = 1500 * time.Millisecond

// initialCommand is the text typed for an initial prompt: trailing line
// breaks are dropped (Enter is sent once); whitespace alone is nothing.
func initialCommand(p string) string {
	p = strings.TrimRight(p, "\r\n")
	if strings.TrimSpace(p) == "" {
		return ""
	}
	return p
}

// typeInitial types cmd into the shell of session name, then Enter, once
// the shell has read its rc files and waits for input. Ready means the
// terminal is in non-canonical mode (a line editor: readline, ZLE, fish,
// whichever shell the rc files exec'd); for bash, zsh and fish on a
// terminal Pyre can inspect, only that. Where the mode cannot be seen or the
// shell has no line editor (sh, dash), ready is the shell in the foreground
// (on tmux, which names the foreground program) with something on its
// screen, unchanged for shellQuiet; typed early, the text waits in the
// terminal's input queue until the shell reads it. The text goes through a
// paste buffer (tmux load-buffer reads it from stdin), never a command line,
// so its length is not limited; quotes, "$", and line breaks arrive as
// typed, and a shell that asked for bracketed paste gets a multi-line
// command as one edit. A shell that exits or never becomes
// ready within ReadyTimeout fails the launch: the command is never typed
// into an rc file's own program.
func (a *Adapter) typeInitial(ctx context.Context, name, shellPath, cmd string) error {
	ctx, cancel := context.WithTimeout(ctx, a.o.ReadyTimeout)
	defer cancel()
	want := commandName(shellPath)
	var last string
	var since time.Time
	for {
		p, err := a.srv.Pane(ctx, name)
		switch {
		case ctx.Err() != nil:
			return contracts.Errorf(contracts.ErrUnavailable, "the shell was not ready for its initial prompt within %s (an rc file still running or waiting for input?); the prompt was not typed", a.o.ReadyTimeout)
		case err != nil:
			return contracts.Errorf(contracts.ErrUnavailable, "shell: %v; the initial prompt was not typed", err)
		case p.Dead:
			return contracts.Errorf(contracts.ErrUnavailable, "the shell exited before its initial prompt could be typed")
		}
		raw, known := lineEditing(p.TTY)
		if raw {
			break
		}
		now := time.Now()
		if known && lineEditors[want] {
			// Its line editor is the only sign: while a login shell reads
			// its rc files, its children often share its process group,
			// so tmux names the shell as the foreground program.
		} else if a.srv.Kind() != hostsel.KindTmux || commandName(p.CurrentCommand) == want {
			screen, err := a.srv.CapturePane(ctx, name, tmux.CaptureOptions{})
			if err == nil && screen != last {
				last, since = screen, now
			} else if err == nil && strings.TrimSpace(screen) != "" && now.Sub(since) >= shellQuiet {
				break
			}
		} else {
			last, since = "", now
		}
		select {
		case <-ctx.Done():
		case <-time.After(50 * time.Millisecond):
		}
	}
	if err := a.srv.Paste(ctx, name, "pyre-init-"+name, []byte(cmd)); err != nil {
		return contracts.Errorf(contracts.ErrUnavailable, "type the initial prompt: %v", err)
	}
	if err := a.srv.SendKeys(ctx, name, sessionhost.KeyEnter); err != nil {
		return contracts.Errorf(contracts.ErrUnavailable, "type the initial prompt: %v", err)
	}
	return nil
}

// commandName is a program path or tmux command name reduced to its base
// name: "/bin/bash", "-zsh" and "pwsh.exe" become bash, zsh and pwsh.
func commandName(p string) string {
	p = strings.TrimPrefix(filepath.Base(strings.ReplaceAll(p, `\`, "/")), "-")
	return strings.TrimSuffix(strings.ToLower(p), ".exe")
}

func (a *Adapter) Deliver(context.Context, contracts.RunHandle, contracts.DeliveryInput) (contracts.DeliveryResult, error) {
	return contracts.DeliveryResult{}, unsupported("message delivery")
}

func (a *Adapter) Reconcile(context.Context, contracts.RunHandle, string) (contracts.DeliveryResult, error) {
	return contracts.DeliveryResult{}, unsupported("delivery reconciliation")
}

func (a *Adapter) Attach(ctx context.Context, h contracts.RunHandle, req contracts.AttachCommand) (contracts.AttachDescriptor, error) {
	if ok, err := a.srv.HasSession(ctx, h.TmuxTarget); err != nil || !ok {
		return contracts.AttachDescriptor{}, contracts.Errorf(contracts.ErrNotFound, "tmux session %s is gone", h.TmuxTarget)
	}
	return a.srv.AttachDescriptor(sessionhost.AttachConfig{NodeID: a.o.NodeID, SSHAlias: a.o.SSHAlias},
		h.RunID, h.TmuxTarget, req, a.o.Now())
}

func (a *Adapter) Interrupt(context.Context, contracts.RunHandle) (contracts.CommandReceipt, error) {
	return contracts.CommandReceipt{Outcome: contracts.OutcomeUnsupported, ObservedAt: a.o.Now()}, unsupported("interrupt")
}

func (a *Adapter) Stop(ctx context.Context, h contracts.RunHandle) (contracts.CommandReceipt, error) {
	now := a.o.Now()
	r := contracts.CommandReceipt{Accepted: true, ObservedAt: now}
	var code *int
	if p, err := a.srv.Pane(ctx, h.TmuxTarget); err == nil && p.Dead {
		code = p.ExitStatus
	}
	if err := a.srv.TerminateSession(ctx, h.TmuxTarget, 3*time.Second); err != nil {
		r.Outcome = contracts.OutcomeFailed
		r.Detail = err.Error()
		return r, nil
	}
	writeExit(h.StateDir, exitRecord{ExitCode: code, Reason: "stopped by pyre (tmux session killed)", At: now})
	if ok, _ := a.srv.HasSession(ctx, h.TmuxTarget); ok {
		r.Outcome = contracts.OutcomeIndeterminate
		r.Detail = "tmux session still present after kill-session"
		return r, nil
	}
	r.Outcome = contracts.OutcomeSucceeded
	r.Detail = "tmux session killed"
	return r, nil
}

func (a *Adapter) Answer(context.Context, contracts.RunHandle, contracts.Approval, string, string) (contracts.CommandReceipt, error) {
	return contracts.CommandReceipt{Outcome: contracts.OutcomeUnsupported, ObservedAt: a.o.Now()}, unsupported("approvals")
}

func (a *Adapter) ReadTranscript(ctx context.Context, h contracts.RunHandle, req contracts.ReadSessionRequest) (contracts.ReadSessionResponse, error) {
	resp := contracts.ReadSessionResponse{Freshness: contracts.Freshness{ObservedAt: a.o.Now(), Source: "node:" + a.o.NodeID}}
	screen, err := a.srv.CapturePane(ctx, h.TmuxTarget, tmux.CaptureOptions{History: 200, Join: true})
	if err != nil {
		resp.Freshness.Stale = true
		resp.Freshness.Reason = fmt.Sprintf("screen unavailable: %v", err)
		return resp, nil
	}
	resp.Screen = strings.TrimRight(screen, "\n")
	return resp, nil
}

func (a *Adapter) ResumeSpec(context.Context, contracts.Run, bool) (contracts.ContinuationKind, *contracts.NativeRef, error) {
	return contracts.ContinuationNone, nil, unsupported("resume")
}

// defaultShell is the shell when neither Options.Shell nor $SHELL names one:
// /bin/bash on unix; on Windows PowerShell 7 (pwsh) when installed, else
// Windows PowerShell, else %ComSpec% (cmd.exe).
func defaultShell() string {
	if runtime.GOOS != "windows" {
		return "/bin/bash"
	}
	if p, err := exec.LookPath("pwsh.exe"); err == nil {
		return p
	}
	ps := filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	if _, err := os.Stat(ps); err == nil {
		return ps
	}
	if c := os.Getenv("ComSpec"); c != "" {
		return c
	}
	return "cmd.exe"
}
