package mock

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/buildinfo"
	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/messaging/header"
)

// AdapterCaps are the capabilities the mock adapter advertises.
var AdapterCaps = []string{
	c.CapAttachReadOnly, c.CapInputQueue, c.CapInputIdempotent, c.CapInputReconcile, c.CapApprovals, c.CapQuestions,
	c.CapInterrupt, c.CapStop, c.CapResumeNative, c.CapResumeHandoff, c.CapTranscript, c.CapScreen,
}

// AdapterOptions configures the mock RuntimeAdapter.
type AdapterOptions struct {
	Clock c.Clock
	// BusyObserves is how many Observe calls a run stays "running" after
	// accepting input or an answer (default 1).
	BusyObserves int
}

// Adapter is a deterministic contracts.RuntimeAdapter for daemon tests.
//
// Input acceptance: Deliver records the input in the run's transcript with
// its marker and returns accepted with EvidenceMock. Busy handling: while a
// run is running/starting/waiting_approval, Deliver returns State=queued
// and consumes nothing. Idempotency: a repeated message ID returns the first
// result without re-submitting (input.idempotent). Crash window: after
// CrashAfterSubmit(runID), the next Deliver submits the input and then
// returns an error, as if the daemon crashed before recording a receipt;
// Reconcile then finds the evidence (unless LoseEvidence was called).
// Markers "[mock:reject]" and "[mock:indeterminate]" in the text force those
// outcomes; "[mock:approval]" and "[mock:question]" make the accepted input
// raise a pending permission request or question (for end-to-end client
// tests against a real pyred started with --dev-mock). Nothing here is runtime validation.
type Adapter struct {
	opts AdapterOptions

	mu   sync.Mutex
	runs map[string]*adapterRun
}

type adapterRun struct {
	handle     c.RunHandle
	state      c.RunState
	reason     string
	busy       int
	exitCode   *int
	approvals  map[string]*c.Approval
	transcript []c.TranscriptEntry
	delivered  map[string]c.DeliveryResult // message id -> result (idempotency)
	submitted  map[string]bool             // message id -> input reached the runtime
	crashNext  bool
	loseEvid   bool
	limit      *c.LimitEvent
	seq        int64
	gone       bool
}

var _ c.RuntimeAdapter = (*Adapter)(nil)

// NewAdapter returns a mock adapter.
func NewAdapter(opts ...AdapterOptions) *Adapter {
	var o AdapterOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	if o.Clock == nil {
		o.Clock = c.SystemClock{}
	}
	if o.BusyObserves <= 0 {
		o.BusyObserves = 1
	}
	return &Adapter{opts: o, runs: map[string]*adapterRun{}}
}

func (a *Adapter) Name() string       { return "mock" }
func (a *Adapter) Harness() c.Harness { return c.HarnessMock }

func (a *Adapter) Probe(ctx context.Context) (c.ProbeResult, error) {
	// Version is the pyred build (the mock harness ships inside it); the
	// harness name already says "mock".
	return c.ProbeResult{Harness: c.HarnessMock, Adapter: "mock", Version: buildinfo.Version, Executable: "(in-process mock)", Available: true,
		Capabilities: append([]string(nil), AdapterCaps...)}, nil
}

func (a *Adapter) get(h c.RunHandle) (*adapterRun, error) {
	r, ok := a.runs[h.RunID]
	if !ok || r.gone {
		return nil, notFound(c.KindRun, h.RunID)
	}
	return r, nil
}

func (r *adapterRun) add(at time.Time, kind, text, msgID string) {
	r.seq++
	r.transcript = append(r.transcript, c.TranscriptEntry{Seq: r.seq, RunID: r.handle.RunID, At: at, Kind: kind, Text: text, MessageID: msgID})
}

func (a *Adapter) Launch(ctx context.Context, l c.ResolvedLaunch) (c.RunHandle, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if l.RunID == "" || l.SessionID == "" {
		return c.RunHandle{}, c.Errorf(c.ErrInvalid, "run_id and session_id are required")
	}
	if r, ok := a.runs[l.RunID]; ok && !r.gone {
		return c.RunHandle{}, c.Errorf(c.ErrConflict, "run %s already has a live handle; use Recover", l.RunID)
	}
	native := &c.NativeRef{Kind: "mock_session", ID: "mock-" + l.RunID}
	if l.Resume != nil {
		native = &c.NativeRef{Kind: l.Resume.Kind, ID: l.Resume.ID}
	}
	h := c.RunHandle{RunID: l.RunID, SessionID: l.SessionID, Adapter: "mock", Harness: c.HarnessMock, PID: 40000 + len(a.runs), StateDir: l.StateDir, NativeRef: native, Data: map[string]string{"mock": "true"}}
	r := &adapterRun{handle: h, state: c.RunWaitingInput, reason: "idle at prompt (mock)", approvals: map[string]*c.Approval{}, delivered: map[string]c.DeliveryResult{}, submitted: map[string]bool{}}
	now := a.opts.Clock.Now()
	r.add(now, "system", "[mock] launched in "+l.Cwd, "")
	if l.InitialPrompt != "" {
		r.add(now, "user", l.InitialPrompt, "")
		r.state, r.reason, r.busy = c.RunRunning, "working on initial prompt (mock)", a.opts.BusyObserves
	}
	a.runs[l.RunID] = r
	return h, nil
}

func (a *Adapter) Recover(ctx context.Context, h c.RunHandle) (c.RunHandle, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r, err := a.get(h)
	if err != nil {
		return c.RunHandle{}, err
	}
	if r.state.Terminal() {
		return c.RunHandle{}, notFound(c.KindRun, h.RunID)
	}
	return r.handle, nil
}

func (a *Adapter) Observe(ctx context.Context, h c.RunHandle, cursor string) (c.Observation, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r, err := a.get(h)
	if err != nil {
		return c.Observation{}, err
	}
	now := a.opts.Clock.Now()
	if r.state == c.RunRunning && r.busy > 0 {
		r.busy--
		if r.busy == 0 {
			r.add(now, "assistant", "Done (synthetic mock turn).", "")
			r.state, r.reason = c.RunWaitingInput, "turn finished (mock)"
		}
	}
	after, _ := strconv.ParseInt(cursor, 10, 64)
	obs := c.Observation{State: r.state, StateReason: r.reason, ObservedAt: now, NativeRef: r.handle.NativeRef, ExitCode: r.exitCode, LimitEvent: r.limit, NextCursor: strconv.FormatInt(r.seq, 10)}
	for _, ap := range r.approvals {
		obs.Approvals = append(obs.Approvals, *ap)
	}
	if n := len(r.transcript); n > 0 && r.transcript[n-1].Seq > after {
		obs.Activity = truncate(r.transcript[n-1].Text, 200)
	}
	r.limit = nil
	return obs, nil
}

func (a *Adapter) Deliver(ctx context.Context, h c.RunHandle, in c.DeliveryInput) (c.DeliveryResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r, err := a.get(h)
	if err != nil {
		return c.DeliveryResult{}, err
	}
	if res, ok := r.delivered[in.MessageID]; ok && res.State != c.DeliveryQueued {
		return res, nil // idempotent: never re-submit the same message
	}
	if r.submitted[in.MessageID] {
		// Input reached the runtime before a crash; don't submit again.
		return a.reconcileLocked(r, in.MessageID), nil
	}
	if r.state.Terminal() {
		res := c.DeliveryResult{State: c.DeliveryRejected, Evidence: c.EvidenceNone, Detail: "run has exited (mock)"}
		r.delivered[in.MessageID] = res
		return res, nil
	}
	if in.Mode == c.ModeSteer {
		res := c.DeliveryResult{State: c.DeliveryRejected, Evidence: c.EvidenceNone, Detail: "unsupported: mock adapter does not advertise " + c.CapInputSteer}
		r.delivered[in.MessageID] = res
		return res, nil
	}
	switch r.state {
	case c.RunWaitingInput, c.RunWaitingBackground:
	default:
		return c.DeliveryResult{State: c.DeliveryQueued, Evidence: c.EvidenceNone, Detail: "runtime busy (" + string(r.state) + "); retry later"}, nil
	}
	if strings.Contains(in.Text, MarkerReject) {
		res := c.DeliveryResult{State: c.DeliveryRejected, Evidence: c.EvidenceNone, Detail: "simulated runtime refusal (mock)"}
		r.delivered[in.MessageID] = res
		return res, nil
	}
	now := a.opts.Clock.Now()
	text := in.Text
	if !header.HasMarker(text, in.MessageID) {
		text = c.MessageMarker(in.MessageID) + " " + text
	}
	r.submitted[in.MessageID] = true
	r.add(now, "pyre_message", text, in.MessageID)
	r.state, r.reason, r.busy = c.RunRunning, "working on delivered message (mock)", a.opts.BusyObserves
	if strings.Contains(in.Text, MarkerApproval) {
		a.requestApprovalLocked(r, c.Approval{RequestID: "mock-perm-" + in.MessageID, Kind: c.ApprovalPermission,
			Title: "Run synthetic command `echo pyre-mock`? (mock)", Body: "Synthetic permission request raised by the mock adapter; nothing runs.",
			Options: []c.ApprovalOption{{ID: "allow", Label: "Allow", Kind: "allow"}, {ID: "deny", Label: "Deny", Kind: "deny"}}})
	}
	if strings.Contains(in.Text, MarkerQuestion) {
		a.requestApprovalLocked(r, c.Approval{RequestID: "mock-q-" + in.MessageID, Kind: c.ApprovalQuestion,
			Title: "Which synthetic colour should the mock use?", Body: "Synthetic question raised by the mock adapter.",
			Options: []c.ApprovalOption{{ID: "red", Label: "Red", Kind: "choice"}, {ID: "blue", Label: "Blue", Kind: "choice"}}, AllowText: true})
	}
	if strings.Contains(in.Text, MarkerIndeterminate) {
		r.loseEvid = true
		res := c.DeliveryResult{State: c.DeliveryIndeterminate, Evidence: c.EvidenceNone, Detail: "simulated lost receipt (mock)"}
		r.delivered[in.MessageID] = res
		return res, nil
	}
	if r.crashNext {
		r.crashNext = false
		return c.DeliveryResult{}, c.Errorf(c.ErrOutcomeIndeterminate, "simulated crash after submitting input and before recording a receipt (mock)")
	}
	res := c.DeliveryResult{State: c.DeliveryAccepted, Evidence: c.EvidenceMock, EvidenceRef: "mock:" + in.AttemptID, Detail: "mock acceptance; not runtime validation"}
	r.delivered[in.MessageID] = res
	return res, nil
}

func (a *Adapter) reconcileLocked(r *adapterRun, messageID string) c.DeliveryResult {
	if res, ok := r.delivered[messageID]; ok && res.State != c.DeliveryIndeterminate {
		return res
	}
	if !r.submitted[messageID] {
		return c.DeliveryResult{State: c.DeliveryIndeterminate, Evidence: c.EvidenceNone, Detail: "no evidence found; absence of evidence does not prove non-execution (mock)"}
	}
	if r.loseEvid {
		return c.DeliveryResult{State: c.DeliveryIndeterminate, Evidence: c.EvidenceNone, Detail: "evidence unavailable (mock)"}
	}
	res := c.DeliveryResult{State: c.DeliveryAccepted, Evidence: c.EvidenceMock, EvidenceRef: "mock:transcript:" + messageID, Detail: "marker found in mock transcript"}
	r.delivered[messageID] = res
	return res
}

func (a *Adapter) Reconcile(ctx context.Context, h c.RunHandle, messageID string) (c.DeliveryResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r, err := a.get(h)
	if err != nil {
		return c.DeliveryResult{}, err
	}
	return a.reconcileLocked(r, messageID), nil
}

func (a *Adapter) Attach(ctx context.Context, h c.RunHandle, req c.AttachCommand) (c.AttachDescriptor, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, err := a.get(h); err != nil {
		return c.AttachDescriptor{}, err
	}
	return c.AttachDescriptor{RunID: h.RunID, Mechanism: c.AttachReadOnly, ReadOnly: true, ExpiresAt: a.opts.Clock.Now().Add(time.Minute),
		Notes: "MOCK adapter: transcript/screen only"}, nil
}

func (a *Adapter) receipt(detail string) c.CommandReceipt {
	return c.CommandReceipt{CommandID: c.NewID(c.PrefixCommand), Accepted: true, Outcome: c.OutcomeSucceeded, Detail: detail, ObservedAt: a.opts.Clock.Now()}
}

func (a *Adapter) Interrupt(ctx context.Context, h c.RunHandle) (c.CommandReceipt, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r, err := a.get(h)
	if err != nil {
		return c.CommandReceipt{}, err
	}
	if r.state.Terminal() {
		return c.CommandReceipt{}, c.Errorf(c.ErrConflict, "run already %s", r.state)
	}
	r.state, r.reason, r.busy = c.RunWaitingInput, "interrupted (mock)", 0
	return a.receipt("interrupted (mock)"), nil
}

func (a *Adapter) Stop(ctx context.Context, h c.RunHandle) (c.CommandReceipt, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r, err := a.get(h)
	if err != nil {
		return c.CommandReceipt{}, err
	}
	if !r.state.Terminal() {
		zero := 0
		r.state, r.reason, r.exitCode, r.busy = c.RunExited, "stopped (mock)", &zero, 0
		for _, ap := range r.approvals {
			ap.State = c.ApprovalWithdrawn
		}
	}
	return a.receipt("stopped (mock)"), nil
}

func (a *Adapter) Answer(ctx context.Context, h c.RunHandle, ap c.Approval, optionID, text string) (c.CommandReceipt, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r, err := a.get(h)
	if err != nil {
		return c.CommandReceipt{}, err
	}
	cur, ok := r.approvals[ap.RequestID]
	if !ok {
		return c.CommandReceipt{}, c.Errorf(c.ErrNotFound, "no pending request %s on this run", ap.RequestID)
	}
	if cur.State != c.ApprovalPending {
		return c.CommandReceipt{}, c.Errorf(c.ErrConflict, "request %s is %s", ap.RequestID, cur.State)
	}
	cur.State = c.ApprovalResolved
	cur.Resolution = &c.ApprovalResolution{OptionID: optionID, Text: text, ResolvedAt: a.opts.Clock.Now(), Source: "pyre", Effect: "effective"}
	pending := false
	for _, x := range r.approvals {
		if x.State == c.ApprovalPending {
			pending = true
		}
	}
	if !pending {
		r.state, r.reason, r.busy = c.RunRunning, "approval answered (mock)", a.opts.BusyObserves
	}
	return a.receipt("answer applied (mock)"), nil
}

func (a *Adapter) ReadTranscript(ctx context.Context, h c.RunHandle, req c.ReadSessionRequest) (c.ReadSessionResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r, err := a.get(h)
	if err != nil {
		return c.ReadSessionResponse{}, err
	}
	after, _ := strconv.ParseInt(req.Cursor, 10, 64)
	resp := c.ReadSessionResponse{Entries: []c.TranscriptEntry{}, Freshness: c.Freshness{ObservedAt: a.opts.Clock.Now(), Source: "mock"}}
	for _, e := range r.transcript {
		if e.Seq > after {
			resp.Entries = append(resp.Entries, e)
		}
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 100
	}
	if len(resp.Entries) > limit {
		if req.Tail {
			resp.Entries = resp.Entries[len(resp.Entries)-limit:]
		} else {
			resp.Entries = resp.Entries[:limit]
		}
	}
	resp.NextCursor = req.Cursor
	if n := len(resp.Entries); n > 0 {
		resp.NextCursor = strconv.FormatInt(resp.Entries[n-1].Seq, 10)
	}
	resp.Screen = "[mock screen] " + string(r.state) + "\n> "
	return resp, nil
}

func (a *Adapter) ResumeSpec(ctx context.Context, prev c.Run, sameAccount bool) (c.ContinuationKind, *c.NativeRef, error) {
	if prev.NativeRef == nil {
		return c.ContinuationHandoff, nil, nil
	}
	nr := *prev.NativeRef
	return c.ContinuationNativeResume, &nr, nil
}

// ---- scripting controls for tests ----

// CrashAfterSubmit makes the next Deliver to runID submit the input and then
// fail as if the caller crashed before recording a receipt.
func (a *Adapter) CrashAfterSubmit(runID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r, ok := a.runs[runID]; ok {
		r.crashNext = true
	}
}

// LoseEvidence makes Reconcile unable to find evidence for runID.
func (a *Adapter) LoseEvidence(runID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r, ok := a.runs[runID]; ok {
		r.loseEvid = true
	}
}

// SetState forces a run's observed state (e.g. running = busy).
func (a *Adapter) SetState(runID string, st c.RunState) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r, ok := a.runs[runID]; ok {
		r.state, r.reason, r.busy = st, "forced by test (mock)", 0
	}
}

// RequestApproval adds a pending runtime request to a run.
func (a *Adapter) RequestApproval(runID string, ap c.Approval) c.Approval {
	a.mu.Lock()
	defer a.mu.Unlock()
	r, ok := a.runs[runID]
	if !ok {
		return ap
	}
	return a.requestApprovalLocked(r, ap)
}

func (a *Adapter) requestApprovalLocked(r *adapterRun, ap c.Approval) c.Approval {
	if ap.RequestID == "" {
		ap.RequestID = "mock-req-" + strconv.Itoa(len(r.approvals)+1)
	}
	ap.RunID, ap.SessionID, ap.State = r.handle.RunID, r.handle.SessionID, c.ApprovalPending
	if ap.CreatedAt.IsZero() {
		ap.CreatedAt = a.opts.Clock.Now()
	}
	r.approvals[ap.RequestID] = &ap
	r.state, r.reason, r.busy = c.RunWaitingApproval, "waiting for approval (mock)", 0
	return ap
}

// Exit ends a run with an exit code (crash/exit simulation).
func (a *Adapter) Exit(runID string, code int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r, ok := a.runs[runID]; ok {
		st := c.RunExited
		if code != 0 {
			st = c.RunFailed
		}
		r.state, r.reason, r.exitCode, r.busy = st, "exited (mock)", &code, 0
	}
}

// InjectLimit reports a limit event on the next Observe.
func (a *Adapter) InjectLimit(runID, kind string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r, ok := a.runs[runID]; ok {
		r.limit = &c.LimitEvent{Kind: kind, Detail: "injected (mock)", At: a.opts.Clock.Now()}
	}
}

// Vanish simulates the runtime process disappearing (Recover → not found).
func (a *Adapter) Vanish(runID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r, ok := a.runs[runID]; ok {
		r.gone = true
	}
}
