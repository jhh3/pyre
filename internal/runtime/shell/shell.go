// Package shell is the runtime adapter for plain shell sessions inside the
// node's isolated tmux server. A shell has no acceptance signal, busy state or
// turn concept, so agent actions (deliver, steer, answer, interrupt, resume)
// are unsupported: Pyre never types into a shell prompt. Humans attach to the
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
	"strconv"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
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
	Now   func() time.Time
}

// Adapter implements contracts.RuntimeAdapter for plain shells.
type Adapter struct {
	o   Options
	srv *tmux.Server
}

var _ contracts.RuntimeAdapter = (*Adapter)(nil)

// New validates options and prepares the tmux server handle (the server
// itself starts lazily on the first launch).
func New(o Options) (*Adapter, error) {
	if o.TmuxSocket == "" {
		return nil, errors.New("shell: TmuxSocket is required")
	}
	srv, err := tmux.NewServer(o.TmuxBin, o.TmuxSocket)
	if err != nil {
		return nil, err
	}
	if err := srv.WriteConfig(); err != nil {
		return nil, err
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Shell == "" {
		o.Shell = os.Getenv("SHELL")
	}
	if o.Shell == "" {
		o.Shell = "/bin/bash"
	}
	return &Adapter{o: o, srv: srv}, nil
}

// Server exposes the tmux server (for daemon housekeeping).
func (a *Adapter) Server() *tmux.Server { return a.srv }

func (a *Adapter) Name() string               { return AdapterName }
func (a *Adapter) Harness() contracts.Harness { return contracts.HarnessShell }

// Capabilities advertised by this adapter.
func Capabilities() []string {
	return []string{
		contracts.CapAttachNative, contracts.CapAttachReadOnly,
		contracts.CapStop, contracts.CapScreen,
	}
}

func (a *Adapter) Probe(ctx context.Context) (contracts.ProbeResult, error) {
	pr := contracts.ProbeResult{Harness: contracts.HarnessShell, Adapter: AdapterName, Executable: a.o.Shell, Capabilities: Capabilities()}
	v, err := a.srv.Version(ctx)
	if err != nil {
		pr.Reason = err.Error()
		return pr, nil
	}
	pr.Version = "tmux " + strconv.Itoa(v.Major) + "." + strconv.Itoa(v.Minor)
	if !v.AtLeast(3, 2) {
		pr.Reason = "tmux >= 3.2 required, found " + v.Raw
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
	if l.Executable != "" {
		argv = append([]string{l.Executable}, l.Args...)
		if len(l.Args) == 0 && loginShells[filepath.Base(l.Executable)] {
			argv = append(argv, "-l") // a plain shell starts as a login shell
		}
	}
	if err := a.srv.NewSession(ctx, tmux.SessionSpec{Name: l.RunID, Cwd: l.Cwd, Env: l.Env, Unset: l.Unset, Argv: argv}); err != nil {
		return contracts.RunHandle{}, contracts.Errorf(contracts.ErrUnavailable, "start shell: %v", err)
	}
	h := contracts.RunHandle{
		RunID: l.RunID, SessionID: l.SessionID, Adapter: AdapterName, Harness: contracts.HarnessShell,
		TmuxSocket: a.o.TmuxSocket, TmuxTarget: l.RunID, StateDir: l.StateDir,
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
	return contracts.Errorf(contracts.ErrUnsupported, "shell sessions do not support %s (Pyre never types into a shell prompt; attach instead)", what)
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
	return tmux.BuildAttach(tmux.AttachConfig{NodeID: a.o.NodeID, SSHAlias: a.o.SSHAlias, TmuxBin: a.srv.Bin, Socket: a.o.TmuxSocket},
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
