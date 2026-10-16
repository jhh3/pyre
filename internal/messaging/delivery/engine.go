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
//     once; the message is never pasted afterwards (decision I16). The hub
//     reports the recipient session's own reads, acks and replies the same
//     way (ApplyRead, evidence pyre.read; W-delivery).
//   - No stale backlog flush: mail that was already waiting when this
//     engine started and that the run's doorbell announced more than
//     StaleBacklog ago is not pasted; it stays in the inbox for the agent
//     (the doorbell re-notifies with one summary line) and never blocks
//     newer mail.
//   - No stale held paste (W-delivery2): mail the doorbell announced that
//     was held (input lease, attach, background, busy) for HeldStale or
//     longer is not pasted when the hold releases; it stays in the inbox
//     and the doorbell rings one summary line instead.
//   - Final gate (W-delivery2): immediately before every paste attempt the
//     engine re-reads the local fact and asks the hub (MessageFacts, short
//     timeout, falling back to the local fact) whether the message was
//     already accepted by any evidence (read, ack, reply), cancelled or
//     expired; such mail is settled locally and never pasted.
//   - Shutdown never cuts a paste on purpose: Drain stops new attempts and
//     waits for in-flight ones. An attempt cut anyway is reconciled with a
//     fresh context; provably unsubmitted input stays queued, only a truly
//     unknown outcome becomes indeterminate (reason "pyred shutdown").
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

// DoorbellNotifier is optionally implemented by runtime adapters with a
// doorbell (Claude input.monitor): when the run's agent was told about a
// message (ok=false: never, or the doorbell is not running).
type DoorbellNotifier interface {
	DoorbellNotifiedAt(h contracts.RunHandle, messageID string) (time.Time, bool)
}

// DefaultStaleBacklog is Config.StaleBacklog's default.
const DefaultStaleBacklog = 10 * time.Minute

// DefaultHeldStale is Config.HeldStale's default.
const DefaultHeldStale = 30 * time.Minute

// DefaultFactsTimeout bounds the pre-paste hub lookup.
const DefaultFactsTimeout = 2 * time.Second

// MessageFacts is the pre-paste gate's view of the hub (implemented by the
// daemon). Settled reports a hub-side terminal fact about the message:
// accepted (by any evidence: the recipient read, acked or replied, or a
// pull), cancelled, or expired. ok=false means not settled or unknown (no
// hub, timeout); the local inbox state is then the fact.
type MessageFacts interface {
	Settled(ctx context.Context, sessionID, messageID string) (state contracts.RuntimeDeliveryState, detail string, ok bool)
}

// StaleReminder is optionally implemented by runtime adapters with a
// doorbell: ring one summary line ("N unread Pyre messages — use inbox")
// for mail left in the inbox instead of being pasted.
type StaleReminder interface {
	RemindUnread(h contracts.RunHandle, messageIDs []string)
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
	// StaleBacklog: mail already waiting when the engine starts whose
	// doorbell notification is older than this is left in the inbox instead
	// of being pasted (default DefaultStaleBacklog; negative disables).
	StaleBacklog time.Duration
	// HeldStale: doorbell-announced mail held at least this long (since it
	// was received) is not pasted when the hold releases (default
	// DefaultHeldStale; negative disables).
	HeldStale time.Duration
	// Facts, if set, is asked right before every paste attempt (bounded by
	// FactsTimeout, default DefaultFactsTimeout).
	Facts        MessageFacts
	FactsTimeout time.Duration

	// OnAccepted, if set, is called after an accepted delivery is recorded
	// (the runtime is now likely busy; the host should observe the run soon
	// so queued follow-ups go out as soon as it is idle again).
	OnAccepted func(sessionID, runID string)
}

// Engine delivers inbox messages to runs.
type Engine struct {
	cfg Config
	log *slog.Logger

	mu       sync.Mutex
	locks    map[string]*sync.Mutex // per session
	force    map[string]bool        // sessions kicked explicitly
	kick     chan struct{}
	sem      chan struct{}
	wg       sync.WaitGroup
	started  time.Time // engine start: mail received before it is backlog
	stopping bool      // Drain began: no new attempts (under mu)
	inflight int       // attempts between claim and finish (under mu)
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
	if cfg.StaleBacklog == 0 {
		cfg.StaleBacklog = DefaultStaleBacklog
	}
	if cfg.HeldStale == 0 {
		cfg.HeldStale = DefaultHeldStale
	}
	if cfg.FactsTimeout <= 0 {
		cfg.FactsTimeout = DefaultFactsTimeout
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

		started: cfg.Clock.Now(),
	}
}

// Drain stops new delivery attempts and waits up to timeout for attempts in
// flight to finish (a paste is never cut on purpose: the caller cancels the
// engine only afterwards). It returns the number still in flight.
func (e *Engine) Drain(timeout time.Duration) int {
	e.mu.Lock()
	e.stopping = true
	n := e.inflight
	e.mu.Unlock()
	deadline := time.Now().Add(timeout)
	for n > 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		e.mu.Lock()
		n = e.inflight
		e.mu.Unlock()
	}
	return n
}

// beginAttempt counts an attempt in flight unless a drain began.
func (e *Engine) beginAttempt() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stopping {
		return false
	}
	e.inflight++
	return true
}

func (e *Engine) endAttempt() {
	e.mu.Lock()
	e.inflight--
	e.mu.Unlock()
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
	var parked staleParks
	defer parked.remind()
	for _, m := range steer {
		e.processOne(ctx, m, force, nil)
	}
	for _, m := range queue {
		if ctx.Err() != nil {
			return
		}
		if blocked := e.processOne(ctx, m, force, &parked); blocked {
			return // preserve order behind a temporarily blocked head
		}
	}
}

// staleParks collects mail newly left in the inbox by the held-stale rule
// in one pass, so the run's doorbell rings a single summary line for it.
type staleParks struct {
	t   Target
	ids []string
}

func (p *staleParks) add(t Target, id string) {
	p.t = t
	p.ids = append(p.ids, id)
}

func (p *staleParks) remind() {
	if len(p.ids) == 0 {
		return
	}
	if r, ok := p.t.Adapter.(StaleReminder); ok {
		r.RemindUnread(p.t.Handle, p.ids)
	}
}

// processOne handles one queued message. It returns blocked=true when the
// message remains queued for a temporary reason (later mail must wait).
func (e *Engine) processOne(ctx context.Context, m inbox.Message, force bool, parked *staleParks) (blocked bool) {
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
	if why, stale, held := e.staleBacklog(m, t); stale {
		// Left in the inbox for the agent; never blocks newer mail.
		if held && m.Reason != why && parked != nil {
			parked.add(t, m.Envelope.MessageID)
		}
		e.deferMsg(ctx, m, why, e.cfg.MaxBackoff)
		return false
	}
	if rc, ok := t.Adapter.(InputReadiness); ok {
		if ready, why := rc.InputReady(ctx, t.Handle, m.Envelope.Mode); !ready {
			e.deferMsg(ctx, m, why, e.nextBackoff(m))
			return true
		}
	}
	if e.settledElsewhere(ctx, m, t) {
		return false
	}
	return e.attempt(ctx, m, t)
}

// settledElsewhere is the final gate before a paste (caller holds the
// session lock): the local fact must still be "queued, never attempted
// here", and the hub must not know the message as accepted (the recipient
// read, acked or replied to it, or any other evidence), cancelled or
// expired. Hub-settled mail is settled locally and never pasted.
func (e *Engine) settledElsewhere(ctx context.Context, m inbox.Message, t Target) bool {
	id := m.Envelope.MessageID
	cur, err := e.cfg.Inbox.Get(ctx, id)
	if err != nil {
		e.log.Warn("pre-paste check: inbox read failed", "message", id, "err", err)
		return true // retried on the next pass
	}
	if cur.State != contracts.DeliveryQueued {
		return true // settled locally since this pass began (e.g. a read withdrawal)
	}
	if e.cfg.Facts == nil {
		return false
	}
	fctx, cancel := context.WithTimeout(ctx, e.cfg.FactsTimeout)
	state, detail, ok := e.cfg.Facts.Settled(fctx, m.SessionID, id)
	cancel()
	if !ok {
		return false
	}
	switch state {
	case contracts.DeliveryAccepted:
		if detail == "" {
			detail = "the hub already records it as accepted"
		}
		detail = "not pasted: " + detail
		runID := t.Run.RunID
		if cur.BoundRunID != "" {
			runID = cur.BoundRunID
		}
		r := contracts.DeliveryReceipt{MessageID: id, SessionID: m.SessionID, RunID: runID,
			State: contracts.DeliveryAccepted, Evidence: contracts.EvidencePyreRead, Detail: detail, Consumed: true}
		changed, _, err := e.applyOneLocked(ctx, r)
		if err != nil {
			e.log.Warn("pre-paste check: could not record hub acceptance", "message", id, "err", err)
		} else if changed {
			e.kickReporter()
		}
		return true
	case contracts.DeliveryCancelled, contracts.DeliveryExpired:
		if detail == "" {
			detail = "the hub already records it as " + string(state)
		}
		e.closeQueued(ctx, cur, state, "not pasted: "+detail)
		return true
	}
	return false
}

// staleBacklog reports doorbell-announced mail that must not be pasted:
//   - restart backlog: it was already waiting when this engine started and
//     the doorbell told the agent more than StaleBacklog ago (incident
//     2026-10-07: a restart pasted a backlog the agent had handled);
//   - stale held mail (held=true): it was held (input lease, attach,
//     background, busy runtime) and is HeldStale or older now that the hold
//     released (incident 2026-10-07: messages held 8 h behind the owner's
//     attach were pasted when the owner left).
//
// It stays readable through the inbox; the doorbell rings a summary line.
// The reason text is stable (deferMsg reports it once).
func (e *Engine) staleBacklog(m inbox.Message, t Target) (why string, stale, held bool) {
	if m.Envelope.Mode == contracts.ModeSteer {
		return "", false, false
	}
	dn, ok := t.Adapter.(DoorbellNotifier)
	if !ok {
		return "", false, false
	}
	at, ok := dn.DoorbellNotifiedAt(t.Handle, m.Envelope.MessageID)
	if !ok {
		return "", false, false
	}
	now := e.cfg.Clock.Now()
	if e.cfg.StaleBacklog >= 0 && m.ReceivedAt.Before(e.started) && now.Sub(at) >= e.cfg.StaleBacklog {
		return "left in the inbox, not pasted: the doorbell told the agent at " + at.UTC().Format(time.RFC3339) +
			" and the message was still waiting when pyred restarted (stale backlog); the agent reads it with the pyre inbox tool", true, false
	}
	if e.cfg.HeldStale >= 0 && now.Sub(m.ReceivedAt) >= e.cfg.HeldStale {
		return "left in the inbox, not pasted: received " + m.ReceivedAt.UTC().Format(time.RFC3339) + ", announced by the doorbell at " +
			at.UTC().Format(time.RFC3339) + ", and held longer than " + e.cfg.HeldStale.String() +
			" (stale held mail); the doorbell rings a summary line and the agent reads it with the pyre inbox tool", true, true
	}
	return "", false, false
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
	if !e.beginAttempt() {
		return true // pyred is shutting down: no new attempts
	}
	defer e.endAttempt()
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
	cut := ctx.Err() != nil
	cancel()
	if derr != nil {
		res = e.resultFromError(ctx, t, m.Envelope.MessageID, derr)
	}
	if cut {
		res = e.shutdownResult(ctx, t, m.Envelope.MessageID, res)
	}
	return e.finish(ctx, m, attemptID, runID, res)
}

// shutdownResult settles an attempt that pyred's shutdown cut (the drain
// timed out). The adapter reconciles it with a fresh, short context: proof
// of acceptance is accepted; proof that nothing was submitted keeps the
// message queued for later delivery (no human needed); only a genuinely
// unknown outcome stays indeterminate, with a clear "pyred shutdown" reason.
func (e *Engine) shutdownResult(ctx context.Context, t Target, messageID string, res contracts.DeliveryResult) contracts.DeliveryResult {
	const why = "pyred shutdown cut the delivery"
	if res.State != contracts.DeliveryIndeterminate {
		return res
	}
	if !contracts.HasCap(capsOf(t), contracts.CapInputReconcile) {
		res.Detail = why + ": " + res.Detail
		return res
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	r, err := t.Adapter.Reconcile(rctx, t.Handle, messageID)
	cancel()
	switch {
	case err == nil && r.State == contracts.DeliveryAccepted:
		r.Detail = why + "; reconciled: " + r.Detail
		return r
	case err == nil && r.State == contracts.DeliveryRejected:
		return contracts.DeliveryResult{State: contracts.DeliveryQueued, Evidence: contracts.EvidenceNone,
			Detail: why + " before submission (" + r.Detail + "); not delivered, stays queued"}
	}
	res.Detail = why + ": " + res.Detail
	if err != nil {
		res.Detail += "; reconcile: " + err.Error()
	} else if r.Detail != "" {
		res.Detail += "; reconcile: " + r.Detail
	}
	return res
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
		changed, pulled, err := e.applyOneLocked(ctx, r)
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

// applyOneLocked applies one receipt; the caller holds the session lock.
func (e *Engine) applyOneLocked(ctx context.Context, r contracts.DeliveryReceipt) (changed, pulled bool, err error) {
	now := e.cfg.Clock.Now()
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
	return changed, pulled, err
}

// ApplyRead records that the recipient session read the message through
// the Pyre API (the hub's read withdrawal, evidence pyre.read): a queued
// message is accepted on its run and never pasted. A message with an
// attempt in flight finishes on its own; one that is already settled is
// left as it is.
func (e *Engine) ApplyRead(ctx context.Context, messageID, runID string, ev contracts.EvidenceCategory, detail string) (inbox.Message, error) {
	m, err := e.cfg.Inbox.Get(ctx, messageID)
	if err != nil {
		return m, err
	}
	if !contracts.PullEvidence(ev) {
		ev = contracts.EvidencePyreRead
	}
	if detail == "" {
		detail = "read by the recipient session; not pasted"
	}
	if m.BoundRunID != "" {
		runID = m.BoundRunID
	}
	if runID == "" {
		if t, ok := e.cfg.Runs.Current(ctx, m.SessionID); ok {
			runID = t.Run.RunID
		}
	}
	if runID == "" {
		if m.State == contracts.DeliveryQueued && !m.Attempted {
			e.closeQueued(ctx, m, contracts.DeliveryCancelled, detail)
		}
		return e.cfg.Inbox.Get(ctx, messageID)
	}
	e.ApplyEvidence(ctx, []contracts.DeliveryReceipt{{MessageID: messageID, SessionID: m.SessionID, RunID: runID,
		State: contracts.DeliveryAccepted, Evidence: ev, Detail: detail, Consumed: true}})
	return e.cfg.Inbox.Get(ctx, messageID)
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
