// Package headless is the state machine of headless runs the caller
// executes itself (pyre run). The CLI runs the harness's one-shot command
// (`claude -p ...`, `codex exec ...`) as its own direct child with stdio
// passed through; the node only binds the account (profile env) and records
// the run, so it shows in sessions, search and read like any other run.
// The adapter keeps the run's state in <run dir>/exec.json, written through
// the daemon's local-socket endpoints (claimed, started, exited). It never
// starts a process, has no input, attach or approvals, and stops a run by
// signalling the child the CLI reported. A harness supplies its transcript
// readers and how its native conversation id is found (Harness).
package headless

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/osutil"
)

const fileExec = "exec.json"

// Exec phases (exec.json).
const (
	phasePending = "pending" // recorded; the CLI has not claimed the launch
	phaseClaimed = "claimed" // the CLI has the binding; the child is starting
	phaseRunning = "running" // the child runs (pid known)
	phaseExited  = "exited"
)

// ClaimTimeout is how long a recorded headless run waits for its CLI to
// claim the launch before it is ended (the CLI fell back or died).
var ClaimTimeout = 2 * time.Minute

// Capabilities are a headless run's capabilities: it can be stopped,
// read, and resumed (interactively, by the harness's own adapter).
var Capabilities = []string{contracts.CapStop, contracts.CapTranscript, contracts.CapResumeNative, contracts.CapAccountProfile}

// State is exec.json (no secrets).
type State struct {
	Phase     string    `json:"phase"`
	CreatedAt time.Time `json:"created_at"`
	Cwd       string    `json:"cwd"`
	// Home is the run's config home (CLAUDE_CONFIG_DIR / CODEX_HOME).
	Home      string `json:"config_dir"`
	ClientPID int    `json:"client_pid,omitempty"` // the pyre run process
	ChildPID  int    `json:"child_pid,omitempty"`  // the harness process
	// ChildGroup: the child leads its own process group (signals reach the
	// tools it started too). The start times guard against pid reuse.
	ChildGroup  bool      `json:"child_group,omitempty"`
	ClientStart string    `json:"client_start,omitempty"`
	ChildStart  string    `json:"child_start,omitempty"`
	StartedAt   time.Time `json:"started_at,omitempty"`
	ExitCode    *int      `json:"exit_code,omitempty"`
	Signal      string    `json:"signal,omitempty"`
	NativeID    string    `json:"native_id,omitempty"`
	EndedAt     time.Time `json:"ended_at,omitempty"`
	EndReason   string    `json:"end_reason,omitempty"`
	StopAsked   bool      `json:"stop_asked,omitempty"`
	ResumeFrom  string    `json:"resume_from,omitempty"` // the conversation the caller's command resumes
}

// Harness is what a harness's headless adapter delegates to.
type Harness interface {
	Probe(ctx context.Context) (contracts.ProbeResult, error)
	ReadTranscript(ctx context.Context, h contracts.RunHandle, req contracts.ReadSessionRequest) (contracts.ReadSessionResponse, error)
	TranscriptFiles(h contracts.RunHandle) (string, []string)
	ResumeSpec(ctx context.Context, prev contracts.Run, sameAccount bool) (contracts.ContinuationKind, *contracts.NativeRef, error)
}

// Spec describes one harness's headless adapter.
type Spec struct {
	Name    string            // adapter name (RunHandle.Adapter)
	Harness contracts.Harness // claude | codex
	Base    Harness
	// Child names the harness process in end reasons ("claude").
	Child string
	// NativeKind is the NativeRef kind of the harness's conversations.
	NativeKind string
	// Home is the config home a launch runs on (its profile, else the
	// machine's own login).
	Home func(l contracts.ResolvedLaunch) string
	// ValidNative accepts a conversation id the CLI saw in the output.
	ValidNative func(id string) bool
	// FindNative finds the conversation a run wrote when the CLI saw no
	// id ("" when it cannot tell).
	FindNative func(st State) string
	// NativeKnown records the conversation for the harness's transcript
	// readers (its run meta in dir).
	NativeKnown func(runID, dir string, st State, id string)
	Now         func() time.Time
}

// Adapter is a harness's headless (pyre run) adapter.
type Adapter struct {
	s  Spec
	mu sync.Mutex // serializes exec.json updates
}

// New returns the headless adapter of s.
func New(s Spec) *Adapter {
	if s.Now == nil {
		s.Now = time.Now
	}
	return &Adapter{s: s}
}

var _ contracts.RuntimeAdapter = (*Adapter)(nil)

func (x *Adapter) Name() string               { return x.s.Name }
func (x *Adapter) Harness() contracts.Harness { return x.s.Harness }

func (x *Adapter) Probe(ctx context.Context) (contracts.ProbeResult, error) {
	pr, err := x.s.Base.Probe(ctx)
	pr.Adapter, pr.Capabilities = x.s.Name, append([]string(nil), Capabilities...)
	return pr, err
}

// ReadState reads a run's exec.json.
func ReadState(dir string) (State, error) {
	var st State
	b, err := os.ReadFile(filepath.Join(dir, fileExec))
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal(b, &st)
}

func writeState(dir string, st State) error {
	b, _ := json.MarshalIndent(st, "", "  ")
	return WriteFileAtomic(filepath.Join(dir, fileExec), b)
}

// update applies f to the run's state under the adapter lock.
func (x *Adapter) update(h contracts.RunHandle, f func(*State) error) (State, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	st, err := ReadState(h.StateDir)
	if err != nil {
		return st, contracts.Errorf(contracts.ErrNotFound, "headless run %s has no state: %v", h.RunID, err)
	}
	if err := f(&st); err != nil {
		return st, err
	}
	return st, writeState(h.StateDir, st)
}

// Launch records the run; nothing is started (the CLI runs the command).
func (x *Adapter) Launch(ctx context.Context, l contracts.ResolvedLaunch) (contracts.RunHandle, error) {
	if l.StateDir == "" || l.Cwd == "" {
		return contracts.RunHandle{}, contracts.Errorf(contracts.ErrInvalid, "state dir and cwd are required")
	}
	if err := os.MkdirAll(l.StateDir, 0o700); err != nil {
		return contracts.RunHandle{}, err
	}
	st := State{Phase: phasePending, CreatedAt: x.s.Now().UTC(), Cwd: l.Cwd, Home: x.s.Home(l)}
	h := contracts.RunHandle{RunID: l.RunID, SessionID: l.SessionID, Adapter: x.s.Name, Harness: x.s.Harness,
		StateDir: l.StateDir, Data: map[string]string{"mode": "headless"}}
	if l.Resume != nil && l.Resume.Kind == x.s.NativeKind {
		st.ResumeFrom = l.Resume.ID
		h.NativeRef = &contracts.NativeRef{Kind: x.s.NativeKind, ID: l.Resume.ID}
		x.s.NativeKnown(l.RunID, l.StateDir, st, l.Resume.ID)
	}
	x.mu.Lock()
	err := writeState(l.StateDir, st)
	x.mu.Unlock()
	return h, err
}

// Claimed records that the CLI took the binding (clientPID: the pyre run
// process, watched until it reports the exit).
func (x *Adapter) Claimed(h contracts.RunHandle, clientPID int) error {
	_, err := x.update(h, func(st *State) error {
		if st.Phase != phasePending {
			return contracts.Errorf(contracts.ErrConflict, "headless run %s was already claimed", h.RunID)
		}
		st.Phase, st.ClientPID, st.ClientStart = phaseClaimed, clientPID, procStart(clientPID)
		return nil
	})
	return err
}

// Started records the child process (group: it leads its own process
// group).
func (x *Adapter) Started(h contracts.RunHandle, childPID int, group bool) error {
	_, err := x.update(h, func(st *State) error {
		if st.Phase == phaseExited || st.StopAsked {
			return contracts.Errorf(contracts.ErrConflict, "headless run %s was stopped", h.RunID)
		}
		st.Phase, st.ChildPID, st.StartedAt = phaseRunning, childPID, x.s.Now().UTC()
		st.ChildGroup, st.ChildStart = group, procStart(childPID)
		return nil
	})
	return err
}

// Exited records the child's end: its exit code (nil when it was killed
// by a signal), the signal, and the conversation id when the CLI saw it.
// Without one the conversation the run resumed, else the harness's lookup
// (FindNative), names it.
func (x *Adapter) Exited(h contracts.RunHandle, code *int, signal, nativeID string) error {
	st, err := x.update(h, func(st *State) error {
		if st.Phase == phaseExited {
			return nil
		}
		st.Phase, st.ExitCode, st.Signal, st.EndedAt = phaseExited, code, signal, x.s.Now().UTC()
		switch {
		case nativeID != "" && x.s.ValidNative(nativeID):
			st.NativeID = nativeID
		case st.ResumeFrom != "":
			st.NativeID = st.ResumeFrom
		case x.s.FindNative != nil:
			st.NativeID = x.s.FindNative(*st)
		}
		switch {
		case signal != "":
			st.EndReason = "killed by " + signal
		case code != nil:
			st.EndReason = "exited"
		}
		return nil
	})
	if err == nil && st.NativeID != "" {
		x.s.NativeKnown(h.RunID, h.StateDir, st, st.NativeID)
	}
	return err
}

func (x *Adapter) Recover(ctx context.Context, h contracts.RunHandle) (contracts.RunHandle, error) {
	if _, err := ReadState(h.StateDir); err != nil {
		return h, contracts.Errorf(contracts.ErrNotFound, "headless run %s: %v", h.RunID, err)
	}
	return h, nil
}

func (x *Adapter) Observe(ctx context.Context, h contracts.RunHandle, cursor string) (contracts.Observation, error) {
	now := x.s.Now()
	obs := contracts.Observation{ObservedAt: now, NativeRef: h.NativeRef, NextCursor: cursor}
	st, err := ReadState(h.StateDir)
	if err != nil {
		obs.State, obs.StateReason = contracts.RunUnknown, "headless run state unreadable"
		return obs, nil
	}
	// A CLI that vanished without reporting (killed) ends the run.
	gone := ""
	switch st.Phase {
	case phasePending:
		if now.Sub(st.CreatedAt) > ClaimTimeout {
			gone = "the pyre run command never took the launch (it ran without Pyre or ended)"
		}
	case phaseClaimed, phaseRunning:
		if st.ClientPID > 0 && !sameProcess(st.ClientPID, st.ClientStart) {
			gone = "the pyre run command ended without reporting the exit"
			if st.ChildPID > 0 && sameProcess(st.ChildPID, st.ChildStart) {
				// The wrapper was killed (SIGKILL: nothing forwarded) and
				// its caller can no longer read the output: stop the child
				// it left behind (Linux also has the parent-death signal).
				signalChild(st)
				gone = "the pyre run command was killed; its " + x.s.Child + " was stopped"
			}
		}
	}
	if gone != "" {
		st, _ = x.update(h, func(s *State) error {
			if s.Phase != phaseExited {
				s.Phase, s.EndedAt, s.EndReason = phaseExited, now.UTC(), gone
			}
			return nil
		})
	}
	if st.NativeID != "" {
		obs.NativeRef = &contracts.NativeRef{Kind: x.s.NativeKind, ID: st.NativeID}
	}
	switch st.Phase {
	case phasePending, phaseClaimed:
		obs.State, obs.StateReason = contracts.RunStarting, "headless (pyre run): starting"
	case phaseRunning:
		obs.State, obs.StateReason = contracts.RunRunning, "headless (pyre run)"
	default:
		obs.State, obs.ExitCode = contracts.RunExited, st.ExitCode
		obs.StateReason = "headless (pyre run): " + st.EndReason
		if st.EndReason == "" {
			obs.StateReason = "headless (pyre run) ended"
		}
	}
	return obs, nil
}

func unsupported(what string) error {
	return contracts.Errorf(contracts.ErrUnsupported, "a headless pyre run has no %s", what)
}

func (x *Adapter) Deliver(ctx context.Context, h contracts.RunHandle, in contracts.DeliveryInput) (contracts.DeliveryResult, error) {
	return contracts.DeliveryResult{}, unsupported("input")
}
func (x *Adapter) Reconcile(ctx context.Context, h contracts.RunHandle, messageID string) (contracts.DeliveryResult, error) {
	return contracts.DeliveryResult{}, unsupported("input")
}
func (x *Adapter) Attach(ctx context.Context, h contracts.RunHandle, req contracts.AttachCommand) (contracts.AttachDescriptor, error) {
	return contracts.AttachDescriptor{}, unsupported("terminal (its output goes to the caller)")
}
func (x *Adapter) Interrupt(ctx context.Context, h contracts.RunHandle) (contracts.CommandReceipt, error) {
	return contracts.CommandReceipt{}, unsupported("interrupt (stop ends it)")
}
func (x *Adapter) Answer(ctx context.Context, h contracts.RunHandle, a contracts.Approval, optionID, text string) (contracts.CommandReceipt, error) {
	return contracts.CommandReceipt{}, unsupported("approvals")
}

// Stop signals the child the CLI reported; the CLI reports its exit.
func (x *Adapter) Stop(ctx context.Context, h contracts.RunHandle) (contracts.CommandReceipt, error) {
	st, err := x.update(h, func(s *State) error {
		s.StopAsked = true
		if s.Phase == phasePending || s.Phase == phaseClaimed {
			// Not started yet: the CLI's start report is refused and it
			// ends the child at once.
			s.Phase, s.EndedAt, s.EndReason = phaseExited, x.s.Now().UTC(), "stopped before the command started"
		}
		return nil
	})
	if err != nil {
		return contracts.CommandReceipt{}, err
	}
	rc := contracts.CommandReceipt{Accepted: true, Outcome: contracts.OutcomeSucceeded, ObservedAt: x.s.Now()}
	if st.ChildPID == 0 || !sameProcess(st.ChildPID, st.ChildStart) {
		return rc, nil
	}
	if err := signalChild(st); err != nil && !errors.Is(err, os.ErrProcessDone) && osutil.ProcessAlive(st.ChildPID) {
		return contracts.CommandReceipt{}, contracts.Errorf(contracts.ErrUnavailable, "stop headless run %s: %v", h.RunID, err)
	}
	return rc, nil
}

// sameProcess: pid is alive and, when its start time was recorded, still
// the same process.
func sameProcess(pid int, start string) bool {
	if !osutil.ProcessAlive(pid) {
		return false
	}
	return start == "" || procStart(pid) == "" || procStart(pid) == start
}

// signalChild asks the headless child (and its process group, when it
// leads one) to stop.
func signalChild(st State) error {
	if st.ChildGroup {
		if err := terminateGroup(st.ChildPID); err == nil {
			return nil
		}
	}
	return osutil.Terminate(st.ChildPID)
}

func (x *Adapter) ReadTranscript(ctx context.Context, h contracts.RunHandle, req contracts.ReadSessionRequest) (contracts.ReadSessionResponse, error) {
	resp, err := x.s.Base.ReadTranscript(ctx, h, req)
	resp.Screen = "" // no terminal
	return resp, err
}

// TranscriptFiles (daemon search) delegates to the harness adapter.
func (x *Adapter) TranscriptFiles(h contracts.RunHandle) (string, []string) {
	return x.s.Base.TranscriptFiles(h)
}

func (x *Adapter) ResumeSpec(ctx context.Context, prev contracts.Run, sameAccount bool) (contracts.ContinuationKind, *contracts.NativeRef, error) {
	return x.s.Base.ResumeSpec(ctx, prev, sameAccount)
}

// WriteFileAtomic writes via a unique temp file in the same directory,
// synced before the rename (0600).
func WriteFileAtomic(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}
