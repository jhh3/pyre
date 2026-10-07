// Package delivery is the recipient node's delivery engine (contracts.md §5):
// it moves messages from the durable inbox into a run's supported input
// interface and reports evidence-backed DeliveryReceipts.
//
// Rules implemented:
//   - Delivery is serialized per session (and therefore per run); queued
//     mail keeps hub-accepted order; explicit steer messages may go first.
//   - An attempt is claimed durably (attempt ID, bound run) and an
//     "attempting" receipt is queued in the same transaction BEFORE
//     adapter.Deliver is called.
//   - Adapter capabilities are respected: without input.queue mail stays
//     queued with a reason; steering requires input.steer or
//     input.stop_hook, otherwise it is rejected as unsupported.
//   - A human input lease or an attached terminal keeps mail queued unless
//     the adapter declares a structured path that is safe alongside a human
//     (Codex app-server, Claude channel, Stop-hook steering).
//   - Busy runtimes (State=queued from the adapter) keep mail queued with
//     bounded exponential backoff; no attempt is consumed. An adapter that
//     implements InputReadiness is asked first, so a run that cannot take
//     input (busy, a dialog on screen, still starting) gets no attempt at all:
//     mail stays queued with one "queued" receipt per reason change, never an
//     attempting/queued pair per retry.
//   - After a crash, attempts left "attempting" are reconciled through the
//     adapter; without evidence they become indeterminate, which is never
//     retried automatically.
//   - Messages bound to a run never move to another run; a replacement run
//     only receives clearly unattempted mail.
//   - Expired queued mail receives an "expired" receipt; cancel works only
//     before any attempt.
//   - Pull evidence (the run fetched the message itself, e.g. the Claude
//     doorbell's inbox-tool read) for a message without an open attempt
//     mints an attempt bound to the reporting run and records "accepted"
//     once; the message is never pasted afterwards (decision I16).
package delivery

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/messaging/inbox"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// DefaultMaxConcurrent is Config.MaxConcurrent's default.
const DefaultMaxConcurrent = 4

// Reporter queues durable node reports inside the caller's transaction
// (implemented by transport.NodeLink; the daemon supplies a local
// implementation when no hub is configured).
type Reporter interface {
	EnqueueTx(ctx context.Context, tx *sql.Tx, kind contracts.ReportKind, at time.Time, payload any) (uint64, error)
	Kick()
}

// Target is a run that may receive input.
type Target struct {
	Run      contracts.Run // latest observation: State, Capabilities, Lease
	Handle   contracts.RunHandle
	Adapter  contracts.RuntimeAdapter
	Attached bool // a human terminal client is attached
}

// InputReadiness is optionally implemented by runtime adapters: a check,
// without side effects on the runtime, whether the run can take input in
// mode now. Not ready keeps the message queued without claiming an attempt.
type InputReadiness interface {
	InputReady(ctx context.Context, h contracts.RunHandle, mode contracts.MessageMode) (ready bool, reason string)
}

// Runs resolves delivery targets (implemented by the daemon).
type Runs interface {
	// Current returns the session's current run on this node (possibly
	// ended). ok=false if the session has no run here.
	Current(ctx context.Context, sessionID string) (Target, bool)
	// Get returns a run by ID. ok=false if this node does not know it.
	Get(ctx context.Context, runID string) (Target, bool)
}

// Config configures an Engine.
type Config struct {
	NodeID   string
	Inbox    *inbox.Store
	Runs     Runs
	Reporter Reporter
	Clock    contracts.Clock
	// HubClock judges hub-issued deadlines (input lease expiry). The node
	// passes its hub clock estimate so a skewed node clock does not drop or
	// prolong leases. Default Clock.
	HubClock contracts.Clock
	Logger   *slog.Logger

	MinBackoff     time.Duration // default 1s
	MaxBackoff     time.Duration // default 60s
	DeliverTimeout time.Duration // per adapter call, default 30s
	MaxConcurrent  int           // sessions processed in parallel, default DefaultMaxConcurrent
	Tick           time.Duration // scheduler tick, default 1s
	Retention      time.Duration // terminal inbox records, default 7d

	// OnAccepted, if set, is called after an accepted delivery is recorded
	// (the runtime is now likely busy; the host should observe the run soon
	// so queued follow-ups go out as soon as it is idle again).
	OnAccepted func(sessionID, runID string)
}

// Engine delivers inbox messages to runs.
type Engine struct {
	cfg Config
	log *slog.Logger

	mu    sync.Mutex
	locks map[string]*sync.Mutex // per session
	force map[string]bool        // sessions kicked explicitly
	kick  chan struct{}
	sem   chan struct{}
	wg    sync.WaitGroup
}

// New returns an engine. Call Recover then Run.
func New(cfg Config) *Engine {
	if cfg.Clock == nil {
		cfg.Clock = contracts.SystemClock{}
	}
	if cfg.HubClock == nil {
		cfg.HubClock = cfg.Clock
	}
	if cfg.MinBackoff <= 0 {
		cfg.MinBackoff = time.Second
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = 60 * time.Second
	}
	if cfg.DeliverTimeout <= 0 {
		cfg.DeliverTimeout = 30 * time.Second
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = DefaultMaxConcurrent
	}
	if cfg.Tick <= 0 {
		cfg.Tick = time.Second
	}
	if cfg.Retention <= 0 {
		cfg.Retention = 7 * 24 * time.Hour
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Engine{
		cfg:   cfg,
		log:   log,
		locks: map[string]*sync.Mutex{},
		force: map[string]bool{},
		kick:  make(chan struct{}, 1),
		sem:   make(chan struct{}, cfg.MaxConcurrent),
	}
}

func (e *Engine) lock(sessionID string) func() {
	e.mu.Lock()
	l, ok := e.locks[sessionID]
	if !ok {
		l = &sync.Mutex{}
		e.locks[sessionID] = l
	}
	e.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// Kick asks the engine to process a session now (ignoring backoff), e.g.
// after a new message, a lease release, or a run state change. An empty
// sessionID processes every session with queued mail.
func (e *Engine) Kick(sessionID string) {
	e.mu.Lock()
	if sessionID != "" {
		e.force[sessionID] = true
	}
	e.mu.Unlock()
	select {
	case e.kick <- struct{}{}:
	default:
	}
}

// Run processes queued mail until ctx is cancelled.
func (e *Engine) Run(ctx context.Context) {
	t := time.NewTicker(e.cfg.Tick)
	defer t.Stop()
	lastPrune := time.Time{}
	for {
		e.scan(ctx)
		if now := e.cfg.Clock.Now(); now.Sub(lastPrune) > time.Hour {
			lastPrune = now
			if _, err := e.cfg.Inbox.Prune(ctx, now.Add(-e.cfg.Retention)); err != nil && ctx.Err() == nil {
				e.log.Warn("inbox prune failed", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			e.wg.Wait()
			return
		case <-e.kick:
		case <-t.C:
		}
	}
}

func (e *Engine) scan(ctx context.Context) {
	sessions, err := e.cfg.Inbox.SessionsWithQueued(ctx)
	if err != nil {
		if ctx.Err() == nil {
			e.log.Warn("inbox scan failed", "err", err)
		}
		return
	}
	e.mu.Lock()
	force := e.force
	e.force = map[string]bool{}
	e.mu.Unlock()
	for _, s := range sessions {
		s, f := s, force[s]
		select {
		case e.sem <- struct{}{}:
		case <-ctx.Done():
			return
		}
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			defer func() { <-e.sem }()
			e.ProcessSession(ctx, s, f)
		}()
	}
	e.wg.Wait()
}

func (e *Engine) receipt(m inbox.Message, attemptID, runID string, state contracts.RuntimeDeliveryState, ev contracts.EvidenceCategory, ref, detail string) contracts.DeliveryReceipt {
	if ev == "" {
		ev = contracts.EvidenceNone
	}
	return contracts.DeliveryReceipt{
		MessageID: m.Envelope.MessageID, AttemptID: attemptID, SessionID: m.SessionID, RunID: runID,
		NodeID: e.cfg.NodeID, State: state, Evidence: ev, EvidenceRef: ref, Detail: detail,
		ObservedAt: e.cfg.Clock.Now(),
	}
}

func (e *Engine) report(ctx context.Context, tx *sql.Tx, r contracts.DeliveryReceipt) error {
	if e.cfg.Reporter == nil {
		return nil
	}
	_, err := e.cfg.Reporter.EnqueueTx(ctx, tx, contracts.ReportReceipt, r.ObservedAt, r)
	return err
}

func (e *Engine) kickReporter() {
	if e.cfg.Reporter != nil {
		e.cfg.Reporter.Kick()
	}
}

// liveForInput reports whether a run state can take input at all.
func liveForInput(s contracts.RunState) bool {
	switch s {
	case contracts.RunRunning, contracts.RunWaitingInput, contracts.RunWaitingApproval, contracts.RunWaitingBackground:
		return true
	}
	return false
}

// structuredSafe reports whether the adapter has an input path that does
// not type into the terminal, and so may proceed while a human holds the
// raw-input lease or is attached.
func structuredSafe(caps []string, mode contracts.MessageMode) bool {
	if contracts.HasCap(caps, contracts.CapInputAppServer) || contracts.HasCap(caps, contracts.CapInputChannel) {
		return true
	}
	return mode == contracts.ModeSteer && contracts.HasCap(caps, contracts.CapInputStopHook)
}

func capsOf(t Target) []string {
	if len(t.Run.Capabilities) > 0 {
		return t.Run.Capabilities
	}
	return nil
}

// gate decides what to do with a message for a target. Exactly one of
// deliver, reject, or a non-empty deferReason is returned.
func (e *Engine) gate(t Target, mode contracts.MessageMode) (deliver bool, reject string, deferReason string) {
	caps := capsOf(t)
	if t.Adapter == nil {
		return false, "", "run has no runtime adapter on this node"
	}
	switch mode {
	case contracts.ModeSteer:
		if !contracts.HasCap(caps, contracts.CapInputSteer) && !contracts.HasCap(caps, contracts.CapInputStopHook) {
			return false, "unsupported: adapter " + t.Run.Adapter + " does not support steering (input.steer/input.stop_hook)", ""
		}
	default:
		if !contracts.HasCap(caps, contracts.CapInputQueue) {
			return false, "", "adapter " + t.Run.Adapter + " has no structured input (input.queue); message stays queued"
		}
	}
	if !liveForInput(t.Run.State) {
		return false, "", "run " + t.Run.RunID + " is " + string(t.Run.State)
	}
	safe := structuredSafe(caps, mode)
	hubNow := e.cfg.HubClock.Now() // leases expire on the hub clock
	note := ""
	if mode != contracts.ModeSteer && contracts.HasCap(caps, contracts.CapInputMonitor) {
		note = "; doorbell (input.monitor) notifies the agent, acceptance follows when it reads the message with the pyre inbox tool"
	}
	if t.Run.Lease != nil && t.Run.Lease.Active(hubNow) && !safe {
		return false, "", "input lease held by " + string(t.Run.Lease.Holder.Kind) + ":" + t.Run.Lease.Holder.ID + "; raw delivery blocked" + note
	}
	if t.Attached && !safe {
		return false, "", "a terminal client is attached; raw delivery blocked until detach or lease" + note
	}
	if t.Run.State == contracts.RunWaitingApproval && !safe {
		return false, "", "run is waiting for an approval; raw delivery blocked" + note
	}
	return true, "", ""
}

func (e *Engine) nextBackoff(m inbox.Message) time.Duration {
	b := m.Backoff * 2
	if b < e.cfg.MinBackoff {
		b = e.cfg.MinBackoff
	}
	if b > e.cfg.MaxBackoff {
		b = e.cfg.MaxBackoff
	}
	return b
}

// ProcessSession attempts delivery of a session's queued mail. force
// ignores per-message backoff.
func (e *Engine) ProcessSession(ctx context.Context, sessionID string, force bool) {
	unlock := e.lock(sessionID)
	defer unlock()
	msgs, err := e.cfg.Inbox.Queued(ctx, sessionID)
	if err != nil {
		e.log.Warn("inbox read failed", "session", sessionID, "err", err)
		return
	}
	// Explicit steering may overtake queued mail; queued mail keeps order.
	var steer, queue []inbox.Message
	for _, m := range msgs {
		if m.Envelope.Mode == contracts.ModeSteer {
			steer = append(steer, m)
		} else {
			queue = append(queue, m)
		}
	}
	for _, m := range steer {
		e.processOne(ctx, m, force)
	}
	for _, m := range queue {
		if ctx.Err() != nil {
			return
		}
		if blocked := e.processOne(ctx, m, force); blocked {
			return // preserve order behind a temporarily blocked head
		}
	}
}

// processOne handles one queued message. It returns blocked=true when the
// message remains queued for a temporary reason (later mail must wait).
func (e *Engine) processOne(ctx context.Context, m inbox.Message, force bool) (blocked bool) {
	now := e.cfg.Clock.Now()
	if !force && !m.NextAt.IsZero() && m.NextAt.After(now) {
		return true
	}
	if m.ExpiresAt != nil && !now.Before(*m.ExpiresAt) {
		e.closeQueued(ctx, m, contracts.DeliveryExpired, "retention ended before delivery (expired "+m.ExpiresAt.UTC().Format(time.RFC3339)+")")
		return false
	}
	if m.Envelope.Mode == contracts.ModeInfo {
		e.closeQueued(ctx, m, contracts.DeliveryNotApplicable, "informational message; never injected into a runtime")
		return false
	}
	var t Target
	var ok bool
	if m.BoundRunID != "" {
		t, ok = e.cfg.Runs.Get(ctx, m.BoundRunID)
		if !ok || t.Run.State.Terminal() {
			// Never moves to another run; does not block unrelated mail.
			e.deferMsg(ctx, m, "bound to run "+m.BoundRunID+" which is not live on this node; requires reconciliation or explicit reissue", e.cfg.MaxBackoff)
			return false
		}
	} else {
		t, ok = e.cfg.Runs.Current(ctx, m.SessionID)
		if !ok || t.Run.State.Terminal() {
			e.deferMsg(ctx, m, "recipient session has no live run on this node", e.cfg.MaxBackoff)
			return true
		}
	}
	deliver, reject, why := e.gate(t, m.Envelope.Mode)
	if reject != "" {
		e.closeQueued(ctx, m, contracts.DeliveryRejected, reject)
		return false
	}
	if !deliver {
		e.deferMsg(ctx, m, why, e.nextBackoff(m))
		return true
	}
	if rc, ok := t.Adapter.(InputReadiness); ok {
		if ready, why := rc.InputReady(ctx, t.Handle, m.Envelope.Mode); !ready {
			e.deferMsg(ctx, m, why, e.nextBackoff(m))
			return true
		}
	}
	return e.attempt(ctx, m, t)
}

func (e *Engine) closeQueued(ctx context.Context, m inbox.Message, state contracts.RuntimeDeliveryState, reason string) {
	err := sqlitex.Tx(ctx, e.cfg.Inbox.DB(), func(tx *sql.Tx) error {
		mm, err := e.cfg.Inbox.CloseQueuedTx(ctx, tx, m.Envelope.MessageID, state, reason, e.cfg.Clock.Now())
		if err != nil {
			return err
		}
		return e.report(ctx, tx, e.receipt(mm, "", mm.BoundRunID, state, contracts.EvidenceNone, "", reason))
	})
	if err != nil {
		e.log.Warn("could not close message", "message", m.Envelope.MessageID, "state", state, "err", err)
		return
	}
	e.kickReporter()
}

func (e *Engine) deferMsg(ctx context.Context, m inbox.Message, reason string, backoff time.Duration) {
	now := e.cfg.Clock.Now()
	reported := false
	err := sqlitex.Tx(ctx, e.cfg.Inbox.DB(), func(tx *sql.Tx) error {
		changed, err := e.cfg.Inbox.DeferTx(ctx, tx, m.Envelope.MessageID, reason, now.Add(backoff), backoff, now)
		if err != nil || !changed {
			return err
		}
		// Report a queued reason only when it changes (visible, not noisy).
		if err := e.report(ctx, tx, e.receipt(m, "", m.BoundRunID, contracts.DeliveryQueued, contracts.EvidenceNone, "", reason)); err != nil {
			return err
		}
		reported = true
		return e.cfg.Inbox.MarkReportedTx(ctx, tx, m.Envelope.MessageID, reason)
	})
	if err != nil {
		e.log.Warn("could not defer message", "message", m.Envelope.MessageID, "err", err)
		return
	}
	if reported {
		e.kickReporter()
	}
}

// attempt claims, delivers, and records one attempt.
func (e *Engine) attempt(ctx context.Context, m inbox.Message, t Target) (blocked bool) {
	attemptID := contracts.NewID(contracts.PrefixAttempt)
	runID := t.Run.RunID
	err := sqlitex.Tx(ctx, e.cfg.Inbox.DB(), func(tx *sql.Tx) error {
		mm, err := e.cfg.Inbox.ClaimTx(ctx, tx, m.Envelope.MessageID, runID, attemptID, e.cfg.Clock.Now())
		if err != nil {
			return err
		}
		return e.report(ctx, tx, e.receipt(mm, attemptID, runID, contracts.DeliveryAttempting, contracts.EvidenceNone, "", ""))
	})
	if err != nil {
		e.log.Warn("could not claim delivery attempt", "message", m.Envelope.MessageID, "run", runID, "err", err)
		return true
	}
	e.kickReporter()

	in := contracts.DeliveryInput{MessageID: m.Envelope.MessageID, AttemptID: attemptID, Mode: m.Envelope.Mode, Text: Render(m.Envelope)}
	dctx, cancel := context.WithTimeout(ctx, e.cfg.DeliverTimeout)
	res, derr := t.Adapter.Deliver(dctx, t.Handle, in)
	cancel()
	if derr != nil {
		res = e.resultFromError(ctx, t, m.Envelope.MessageID, derr)
	}
	return e.finish(ctx, m, attemptID, runID, res)
}

// resultFromError converts an adapter error into an evidence-backed result.
// Only errors that prove nothing was submitted become rejected; anything
// else is reconciled once and otherwise indeterminate.
func (e *Engine) resultFromError(ctx context.Context, t Target, messageID string, derr error) contracts.DeliveryResult {
	switch contracts.CodeOf(derr) {
	case contracts.ErrUnsupported, contracts.ErrInvalid:
		return contracts.DeliveryResult{State: contracts.DeliveryRejected, Evidence: contracts.EvidenceNone, Detail: "adapter refused before submission: " + derr.Error()}
	}
	if ctx.Err() != nil {
		return contracts.DeliveryResult{State: contracts.DeliveryIndeterminate, Evidence: contracts.EvidenceNone, Detail: "node stopping during delivery: " + derr.Error()}
	}
	if contracts.HasCap(capsOf(t), contracts.CapInputReconcile) {
		rctx, cancel := context.WithTimeout(ctx, e.cfg.DeliverTimeout)
		r, err := t.Adapter.Reconcile(rctx, t.Handle, messageID)
		cancel()
		if err == nil && (r.State == contracts.DeliveryAccepted || r.State == contracts.DeliveryRejected) {
			return r
		}
	}
	return contracts.DeliveryResult{State: contracts.DeliveryIndeterminate, Evidence: contracts.EvidenceNone, Detail: "delivery error with unknown outcome: " + derr.Error()}
}

func (e *Engine) finish(ctx context.Context, m inbox.Message, attemptID, runID string, res contracts.DeliveryResult) (blocked bool) {
	if res.State == contracts.DeliveryAccepted && !contracts.AcceptanceEvidence(res.Evidence) {
		// Bytes written to a PTY (or an informational notice) are not
		// acceptance evidence.
		res.State = contracts.DeliveryIndeterminate
		res.Detail = "only " + string(res.Evidence) + " evidence; acceptance unverified. " + res.Detail
	}
	now := e.cfg.Clock.Now()
	backoff := e.nextBackoff(m)
	// Finishing must survive caller cancellation: the attempt already
	// happened and its evidence must be recorded.
	fctx := context.WithoutCancel(ctx)
	var deferred bool
	err := sqlitex.Tx(fctx, e.cfg.Inbox.DB(), func(tx *sql.Tx) error {
		mm, a, err := e.cfg.Inbox.FinishTx(fctx, tx, attemptID, res, now, now.Add(backoff), backoff)
		if err != nil {
			return err
		}
		if a.State == inbox.AttemptDeferred {
			deferred = true
			reason := res.Detail
			if reason == "" {
				reason = "runtime busy"
			}
			if err := e.report(fctx, tx, e.receipt(mm, attemptID, runID, contracts.DeliveryQueued, res.Evidence, res.EvidenceRef, reason)); err != nil {
				return err
			}
			return e.cfg.Inbox.MarkReportedTx(fctx, tx, mm.Envelope.MessageID, reason)
		}
		return e.report(fctx, tx, e.receipt(mm, attemptID, runID, a.State, res.Evidence, res.EvidenceRef, res.Detail))
	})
	if err != nil {
		// The attempt stays "attempting" and is reconciled on recovery.
		e.log.Error("could not record delivery result", "message", m.Envelope.MessageID, "attempt", attemptID, "err", err)
		return true
	}
	e.kickReporter()
	if !deferred && res.State == contracts.DeliveryAccepted && e.cfg.OnAccepted != nil {
		e.cfg.OnAccepted(m.SessionID, runID)
	}
	return deferred
}

// Recover reconciles attempts left in the attempting state by a crash.
// Evidence of acceptance/non-submission resolves them; otherwise they
// become indeterminate and are never retried automatically.
func (e *Engine) Recover(ctx context.Context) error {
	atts, err := e.cfg.Inbox.Attempting(ctx)
	if err != nil {
		return err
	}
	for _, a := range atts {
		m, err := e.cfg.Inbox.Get(ctx, a.MessageID)
		if err != nil {
			return err
		}
		unlock := e.lock(m.SessionID)
		res := contracts.DeliveryResult{State: contracts.DeliveryIndeterminate, Evidence: contracts.EvidenceNone,
			Detail: "node stopped between attempt claim and receipt; no reconcilable evidence"}
		t, ok := e.cfg.Runs.Get(ctx, a.RunID)
		switch {
		case !ok || t.Adapter == nil:
			res.Detail = "node stopped between attempt claim and receipt; run " + a.RunID + " is not recoverable here"
		default:
			rctx, cancel := context.WithTimeout(ctx, e.cfg.DeliverTimeout)
			r, rerr := t.Adapter.Reconcile(rctx, t.Handle, a.MessageID)
			cancel()
			if rerr == nil && (r.State == contracts.DeliveryAccepted || r.State == contracts.DeliveryRejected) {
				res = r
				if res.Detail == "" {
					res.Detail = "reconciled after restart"
				}
			} else if rerr != nil {
				res.Detail += "; reconcile: " + rerr.Error()
			} else if r.Detail != "" {
				res.Detail += "; reconcile: " + r.Detail
			}
		}
		if res.State == contracts.DeliveryQueued {
			res.State = contracts.DeliveryIndeterminate
		}
		e.finish(ctx, m, a.AttemptID, a.RunID, res)
		unlock()
	}
	return nil
}

// ApplyEvidence applies receipts discovered asynchronously by the
// observation loop (e.g. hook receipts for marked input, or consumption).
func (e *Engine) ApplyEvidence(ctx context.Context, receipts []contracts.DeliveryReceipt) {
	for _, r := range receipts {
		m, err := e.cfg.Inbox.Get(ctx, r.MessageID)
		if err != nil {
			continue // evidence for a message this node never held
		}
		unlock := e.lock(m.SessionID)
		now := e.cfg.Clock.Now()
		changed, pulled := false, false
		err = sqlitex.Tx(ctx, e.cfg.Inbox.DB(), func(tx *sql.Tx) error {
			changed, pulled = false, false
			cur, err := e.cfg.Inbox.GetTx(ctx, tx, r.MessageID)
			if err != nil {
				return err
			}
			if r.RunID != "" && cur.BoundRunID != "" && r.RunID != cur.BoundRunID {
				return nil // evidence from another run never rebinds a message
			}
			if r.SessionID != "" && r.SessionID != cur.SessionID {
				return nil // evidence from another session's run
			}
			if r.State == contracts.DeliveryAccepted && contracts.PullEvidence(r.Evidence) && r.RunID != "" {
				// The run fetched the message itself: no attempt exists (or
				// the last one closed without consuming). Mint one, bound to
				// that run, so acceptance is recorded once and the message
				// is never pasted afterwards (decision I16).
				attemptID := contracts.NewID(contracts.PrefixAttempt)
				res := contracts.DeliveryResult{State: contracts.DeliveryAccepted, Evidence: r.Evidence, EvidenceRef: r.EvidenceRef, Detail: r.Detail}
				mm, ok, err := e.cfg.Inbox.RecordPullTx(ctx, tx, r.MessageID, r.RunID, attemptID, res, now)
				if err != nil || ok {
					if err != nil {
						return err
					}
					changed, pulled = true, true
					rc := e.receipt(mm, attemptID, r.RunID, contracts.DeliveryAccepted, r.Evidence, r.EvidenceRef, r.Detail)
					rc.Consumed = contracts.ConsumptionEvidence(r.Evidence)
					return e.report(ctx, tx, rc)
				}
			}
			if cur.State == contracts.DeliveryAccepted && (r.Consumed || isConsumption(r.Evidence)) {
				ok, err := e.cfg.Inbox.MarkConsumedTx(ctx, tx, r.MessageID, now)
				if err != nil || !ok {
					return err
				}
				changed = true
				rc := e.receipt(cur, r.AttemptID, cur.BoundRunID, contracts.DeliveryAccepted, r.Evidence, r.EvidenceRef, r.Detail)
				rc.Consumed = true
				return e.report(ctx, tx, rc)
			}
			mm, ok, err := e.cfg.Inbox.ResolveByEvidenceTx(ctx, tx, r.MessageID, r.AttemptID,
				contracts.DeliveryResult{State: r.State, Evidence: r.Evidence, EvidenceRef: r.EvidenceRef, Detail: r.Detail}, now)
			if err != nil || !ok {
				return err
			}
			changed = true
			return e.report(ctx, tx, e.receipt(mm, r.AttemptID, mm.BoundRunID, mm.State, r.Evidence, r.EvidenceRef, r.Detail))
		})
		unlock()
		if err != nil {
			e.log.Warn("could not apply delivery evidence", "message", r.MessageID, "err", err)
		}
		if changed {
			e.kickReporter()
		}
		if pulled && err == nil && e.cfg.OnAccepted != nil {
			e.cfg.OnAccepted(m.SessionID, r.RunID)
		}
	}
}

func isConsumption(ev contracts.EvidenceCategory) bool {
	return contracts.ConsumptionEvidence(ev)
}

// Cancel cancels a queued message that was never attempted.
func (e *Engine) Cancel(ctx context.Context, messageID string) (inbox.Message, error) {
	m, err := e.cfg.Inbox.Get(ctx, messageID)
	if err != nil {
		return m, err
	}
	unlock := e.lock(m.SessionID)
	defer unlock()
	var out inbox.Message
	err = sqlitex.Tx(ctx, e.cfg.Inbox.DB(), func(tx *sql.Tx) error {
		var err error
		out, err = e.cfg.Inbox.CloseQueuedTx(ctx, tx, messageID, contracts.DeliveryCancelled, "cancelled before any attempt", e.cfg.Clock.Now())
		if err != nil {
			return err
		}
		return e.report(ctx, tx, e.receipt(out, "", out.BoundRunID, contracts.DeliveryCancelled, contracts.EvidenceNone, "", "cancelled before any attempt"))
	})
	if err == nil {
		e.kickReporter()
	}
	return out, err
}
