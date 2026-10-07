// Package daemontest provides a deterministic fake contracts.RuntimeAdapter
// for daemon and delivery tests. Its "processes" live in a World that
// outlives adapter and daemon instances, so tests can simulate a daemon
// restart with runs still alive. It is labelled mock and never counts as
// runtime evidence. It is replaced by internal/runtime/mock (WP03) where
// that suffices.
package daemontest

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// AgentCaps is a capability set like a structured agent adapter.
var AgentCaps = []string{
	contracts.CapAttachNative, contracts.CapInputQueue, contracts.CapInputReconcile,
	contracts.CapInputStopHook, contracts.CapApprovals, contracts.CapQuestions,
	contracts.CapInterrupt, contracts.CapStop, contracts.CapResumeNative,
	contracts.CapTranscript, contracts.CapSubagentsObserve, contracts.CapAgentTools,
	contracts.CapInputPaste,
}

// Proc is a simulated live runtime process.
type Proc struct {
	Launch      contracts.ResolvedLaunch
	Handle      contracts.RunHandle
	State       contracts.RunState
	Reason      string
	Delivered   []contracts.DeliveryInput // inputs the runtime accepted
	Interrupts  int
	Answers     []string
	Activity    string
	Approvals   []contracts.Approval
	Receipts    []contracts.DeliveryReceipt // pending async evidence
	Limit       *contracts.LimitEvent
	ExitCode    *int
	Subagents   []contracts.SubagentObservation
	ObserveHits int
}

// World holds simulated processes across adapter/daemon restarts.
type World struct {
	mu       sync.Mutex
	procs    map[string]*Proc // run ID -> proc
	launches map[string]int   // run ID -> Launch calls
	recovers map[string]int
}

// NewWorld returns an empty world.
func NewWorld() *World {
	return &World{procs: map[string]*Proc{}, launches: map[string]int{}, recovers: map[string]int{}}
}

// Proc returns a copy of a run's process state.
func (w *World) Proc(runID string) (Proc, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	p, ok := w.procs[runID]
	if !ok {
		return Proc{}, false
	}
	return *p, true
}

// Mutate edits a run's process under the world lock.
func (w *World) Mutate(runID string, fn func(p *Proc)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if p, ok := w.procs[runID]; ok {
		fn(p)
	}
}

// Kill removes a process (it disappears without an observed exit).
func (w *World) Kill(runID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.procs, runID)
}

// Launches returns how many times Launch was called for a run.
func (w *World) Launches(runID string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.launches[runID]
}

// TotalLaunches counts Launch calls across runs.
func (w *World) TotalLaunches() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, c := range w.launches {
		n += c
	}
	return n
}

// Recovers returns how many times Recover was called for a run.
func (w *World) Recovers(runID string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.recovers[runID]
}

// Adapter is the fake runtime adapter.
type Adapter struct {
	World     *World
	AdapterID string
	H         contracts.Harness
	Caps      []string

	// DeliverFunc, if set, decides delivery; return accept=false with a
	// result to override. It runs before the default behavior.
	DeliverFunc func(p *Proc, in contracts.DeliveryInput) (contracts.DeliveryResult, error, bool)
	// ReconcileUnknown makes Reconcile find no evidence even if delivered.
	ReconcileUnknown bool

	mu          sync.Mutex
	DeliverHits int
}

// New returns a fake adapter for harness h with the agent capability set.
func New(w *World, h contracts.Harness) *Adapter {
	return &Adapter{World: w, AdapterID: "fake-" + string(h), H: h, Caps: append([]string(nil), AgentCaps...)}
}

func (a *Adapter) Name() string               { return a.AdapterID }
func (a *Adapter) Harness() contracts.Harness { return a.H }

func (a *Adapter) Probe(context.Context) (contracts.ProbeResult, error) {
	return contracts.ProbeResult{Harness: a.H, Adapter: a.AdapterID, Version: "0.0.0-fake", Executable: "/fake/" + string(a.H),
		Available: true, Capabilities: append([]string(nil), a.Caps...)}, nil
}

func (a *Adapter) Launch(ctx context.Context, l contracts.ResolvedLaunch) (contracts.RunHandle, error) {
	w := a.World
	w.mu.Lock()
	defer w.mu.Unlock()
	w.launches[l.RunID]++
	if _, ok := w.procs[l.RunID]; ok {
		return contracts.RunHandle{}, contracts.Errorf(contracts.ErrConflict, "fake: run %s already has a live process", l.RunID)
	}
	h := contracts.RunHandle{RunID: l.RunID, SessionID: l.SessionID, Adapter: a.AdapterID, Harness: a.H,
		TmuxSocket: l.StateDir + "/tmux.sock", TmuxTarget: "pyre", StateDir: l.StateDir,
		NativeRef: &contracts.NativeRef{Kind: "fake_session", ID: "native-" + l.RunID}}
	if l.Resume != nil {
		h.NativeRef = l.Resume
	}
	w.procs[l.RunID] = &Proc{Launch: l, Handle: h, State: contracts.RunWaitingInput}
	return h, nil
}

func (a *Adapter) proc(h contracts.RunHandle) (*Proc, error) {
	p, ok := a.World.procs[h.RunID]
	if !ok {
		return nil, contracts.Errorf(contracts.ErrNotFound, "fake: run %s has no process", h.RunID).WithRef(contracts.KindRun, h.RunID)
	}
	return p, nil
}

func (a *Adapter) Recover(ctx context.Context, h contracts.RunHandle) (contracts.RunHandle, error) {
	a.World.mu.Lock()
	defer a.World.mu.Unlock()
	a.World.recovers[h.RunID]++
	p, err := a.proc(h)
	if err != nil {
		return h, err
	}
	return p.Handle, nil
}

func (a *Adapter) Observe(ctx context.Context, h contracts.RunHandle, cursor string) (contracts.Observation, error) {
	a.World.mu.Lock()
	defer a.World.mu.Unlock()
	p, err := a.proc(h)
	if err != nil {
		return contracts.Observation{}, err
	}
	p.ObserveHits++
	o := contracts.Observation{State: p.State, StateReason: p.Reason, ObservedAt: time.Now().UTC(), NativeRef: p.Handle.NativeRef,
		Approvals: append([]contracts.Approval(nil), p.Approvals...), Activity: p.Activity, ExitCode: p.ExitCode,
		Subagents: append([]contracts.SubagentObservation(nil), p.Subagents...), LimitEvent: p.Limit,
		Receipts: p.Receipts, NextCursor: fmt.Sprintf("%d", p.ObserveHits)}
	p.Receipts = nil
	p.Limit = nil
	return o, nil
}

func (a *Adapter) Deliver(ctx context.Context, h contracts.RunHandle, in contracts.DeliveryInput) (contracts.DeliveryResult, error) {
	a.mu.Lock()
	a.DeliverHits++
	a.mu.Unlock()
	a.World.mu.Lock()
	defer a.World.mu.Unlock()
	p, err := a.proc(h)
	if err != nil {
		return contracts.DeliveryResult{}, err
	}
	if a.DeliverFunc != nil {
		if r, err, done := a.DeliverFunc(p, in); done {
			return r, err
		}
	}
	if p.State == contracts.RunRunning && in.Mode != contracts.ModeSteer {
		return contracts.DeliveryResult{State: contracts.DeliveryQueued, Evidence: contracts.EvidenceNone, Detail: "fake runtime busy"}, nil
	}
	p.Delivered = append(p.Delivered, in)
	return contracts.DeliveryResult{State: contracts.DeliveryAccepted, Evidence: contracts.EvidenceMock, EvidenceRef: "fake:" + in.AttemptID}, nil
}

// Hits returns the number of Deliver calls.
func (a *Adapter) Hits() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.DeliverHits
}

func (a *Adapter) Reconcile(ctx context.Context, h contracts.RunHandle, messageID string) (contracts.DeliveryResult, error) {
	a.World.mu.Lock()
	defer a.World.mu.Unlock()
	p, err := a.proc(h)
	if err != nil {
		return contracts.DeliveryResult{}, err
	}
	if !a.ReconcileUnknown {
		for _, d := range p.Delivered {
			if d.MessageID == messageID {
				return contracts.DeliveryResult{State: contracts.DeliveryAccepted, Evidence: contracts.EvidenceMock, EvidenceRef: "fake-reconcile:" + d.AttemptID}, nil
			}
		}
	}
	return contracts.DeliveryResult{State: contracts.DeliveryIndeterminate, Evidence: contracts.EvidenceNone, Detail: "fake: no evidence"}, nil
}

func (a *Adapter) Attach(ctx context.Context, h contracts.RunHandle, req contracts.AttachCommand) (contracts.AttachDescriptor, error) {
	argv := []string{"tmux", "-S", h.TmuxSocket, "attach-session", "-t", h.TmuxTarget}
	if req.ReadOnly {
		argv = append(argv, "-r")
	}
	return contracts.AttachDescriptor{RunID: h.RunID, Mechanism: contracts.AttachLocalTmux, Command: argv,
		ExpiresAt: time.Now().Add(time.Minute).UTC(), ReadOnly: req.ReadOnly, Notes: "detach with Ctrl-b d"}, nil
}

func (a *Adapter) Interrupt(ctx context.Context, h contracts.RunHandle) (contracts.CommandReceipt, error) {
	a.World.mu.Lock()
	defer a.World.mu.Unlock()
	p, err := a.proc(h)
	if err != nil {
		return contracts.CommandReceipt{}, err
	}
	p.Interrupts++
	p.State = contracts.RunWaitingInput
	return contracts.CommandReceipt{Accepted: true, Outcome: contracts.OutcomeSucceeded, ObservedAt: time.Now().UTC()}, nil
}

func (a *Adapter) Stop(ctx context.Context, h contracts.RunHandle) (contracts.CommandReceipt, error) {
	a.World.mu.Lock()
	defer a.World.mu.Unlock()
	p, err := a.proc(h)
	if err != nil {
		return contracts.CommandReceipt{}, err
	}
	zero := 0
	p.State, p.ExitCode, p.Reason = contracts.RunExited, &zero, "stopped"
	return contracts.CommandReceipt{Accepted: true, Outcome: contracts.OutcomeSucceeded, ObservedAt: time.Now().UTC()}, nil
}

func (a *Adapter) Answer(ctx context.Context, h contracts.RunHandle, ap contracts.Approval, optionID, text string) (contracts.CommandReceipt, error) {
	a.World.mu.Lock()
	defer a.World.mu.Unlock()
	p, err := a.proc(h)
	if err != nil {
		return contracts.CommandReceipt{}, err
	}
	p.Answers = append(p.Answers, ap.RequestID+"="+optionID+text)
	return contracts.CommandReceipt{Accepted: true, Outcome: contracts.OutcomeDelivered, ObservedAt: time.Now().UTC()}, nil
}

func (a *Adapter) ReadTranscript(ctx context.Context, h contracts.RunHandle, req contracts.ReadSessionRequest) (contracts.ReadSessionResponse, error) {
	a.World.mu.Lock()
	defer a.World.mu.Unlock()
	p, err := a.proc(h)
	if err != nil {
		return contracts.ReadSessionResponse{}, err
	}
	var out contracts.ReadSessionResponse
	for i, d := range p.Delivered {
		out.Entries = append(out.Entries, contracts.TranscriptEntry{Seq: int64(i + 1), RunID: h.RunID, Kind: "pyre_message", Text: d.Text, MessageID: d.MessageID})
	}
	out.Freshness = contracts.Freshness{ObservedAt: time.Now().UTC(), Source: "mock"}
	out.Screen = strings.TrimSpace(p.Activity)
	return out, nil
}

func (a *Adapter) ResumeSpec(ctx context.Context, prev contracts.Run, sameAccount bool) (contracts.ContinuationKind, *contracts.NativeRef, error) {
	if !contracts.HasCap(a.Caps, contracts.CapResumeNative) || prev.NativeRef == nil {
		return "", nil, contracts.Errorf(contracts.ErrUnsupported, "fake: native resume unsupported")
	}
	return contracts.ContinuationNativeResume, prev.NativeRef, nil
}

var _ contracts.RuntimeAdapter = (*Adapter)(nil)
