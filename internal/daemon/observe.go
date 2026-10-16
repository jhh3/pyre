package daemon

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/doing"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
	"github.com/jhh3/pyre/internal/supervisor"
)

// fingerprint covers the reportable observation fields; a report is sent
// only when it changes (bounded report volume).
func fingerprint(r contracts.Run) string {
	b, _ := json.Marshal(struct {
		S  contracts.RunState
		R  string
		N  *contracts.NativeRef
		E  *int
		Sa []contracts.SubagentObservation
		P  []string
		L  *contracts.InputLease
		St bool
		A  string
		Ac string
		C  []string
		D  string
	}{r.State, r.StateReason, r.NativeRef, r.ExitCode, r.Subagents, r.PendingApprovalIDs, r.Lease, r.Freshness.Stale, r.AdapterData["pyre.attached"], r.AccountID, r.Capabilities, activityText(r.Activity)})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

var secretRe = regexp.MustCompile(`(?i)(pyre[a-z]*_[A-Za-z0-9_-]{16,}|bearer\s+\S+|token[=:]\s*\S+)`)

func activityText(a *contracts.RunActivity) string {
	if a == nil {
		return ""
	}
	return a.Text
}

// nextActivity keeps the "doing now" phrase only while the run is running,
// restarting its clock when the phrase changes.
func nextActivity(cur *contracts.RunActivity, state contracts.RunState, phrase string, at time.Time) *contracts.RunActivity {
	phrase = doing.Clip(redact(phrase))
	if state != contracts.RunRunning || phrase == "" {
		return nil
	}
	if cur != nil && cur.Text == phrase {
		return cur
	}
	return &contracts.RunActivity{Text: phrase, Since: at}
}

// redact removes credential-looking substrings from diagnostic text.
func redact(s string) string { return secretRe.ReplaceAllString(s, "[redacted]") }

// pollLoop observes live runs with bounded concurrency. Each run has its
// own interval: reset to Poll.Interval when something changes, doubled up
// to Poll.IdleMax while nothing does.
func (d *Daemon) pollLoop(ctx context.Context) {
	tick := d.cfg.Poll.Interval / 2
	if tick > 500*time.Millisecond {
		tick = 500 * time.Millisecond
	}
	if tick < 10*time.Millisecond {
		tick = 10 * time.Millisecond
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	sem := d.pollSem
	for {
		now := d.now()
		for _, lr := range d.liveRuns() {
			lr.mu.Lock()
			due := !lr.polling && !now.Before(lr.nextPoll)
			if due {
				lr.polling = true
			}
			lr.mu.Unlock()
			if !due {
				d.checkStamp(lr, now)
				continue
			}
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			d.wg.Add(1)
			go func(lr *liveRun) {
				defer d.wg.Done()
				defer func() { <-sem }()
				d.observeRun(ctx, lr)
			}(lr)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-d.pollKick:
		}
	}
}

// PollNow forces a fresh observation of a run (after a command, or in
// tests). If a poll is in flight it waits for it and then observes again,
// so the result reflects state after the call.
func (d *Daemon) PollNow(ctx context.Context, runID string) {
	lr, ok := d.getLive(runID)
	if !ok {
		return
	}
	deadline := time.Now().Add(d.cfg.Poll.Timeout + time.Second)
	for {
		lr.mu.Lock()
		if !lr.polling {
			lr.polling = true
			lr.mu.Unlock()
			break
		}
		lr.mu.Unlock()
		if time.Now().After(deadline) || ctx.Err() != nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, still := d.getLive(runID); !still {
		return
	}
	select { // PollNow shares the observation concurrency bound
	case d.pollSem <- struct{}{}:
	case <-ctx.Done():
		d.schedule(lr, false)
		return
	}
	defer func() { <-d.pollSem }()
	d.observeRun(ctx, lr)
}

// changeStamper is implemented by adapters whose runs leave cheap traces
// between observations (e.g. the Claude adapter's hook-event and transcript
// files). ChangeStamp must be cheap (a few stats) and never block on the
// run: the poll loop calls it for every waiting run on each tick.
type changeStamper interface {
	ChangeStamp(h contracts.RunHandle) string
}

// stampOf returns the run's current change stamp ("" when the adapter has
// none or the run has no handle yet).
func stampOf(lr *liveRun) string {
	cs, ok := lr.adapter.(changeStamper)
	if !ok {
		return ""
	}
	lr.mu.Lock()
	h := lr.rec.Handle
	lr.mu.Unlock()
	if h == nil {
		return ""
	}
	return cs.ChangeStamp(*h)
}

// checkStamp makes a backed-off run due when its change stamp moved since
// its last observation began: something happened that no poll has seen
// yet (a hook fired, the transcript grew). A native-terminal interrupt
// fires no hook, so without this the state stayed stale until
// Poll.IdleMax. Observations stay at least Poll.Interval apart, so a busy
// run is not observed more often than before.
func (d *Daemon) checkStamp(lr *liveRun, now time.Time) {
	st := stampOf(lr)
	if st == "" {
		return
	}
	lr.mu.Lock()
	defer lr.mu.Unlock()
	if lr.polling || st == lr.stamp {
		return
	}
	next := lr.observedAt.Add(d.cfg.Poll.Interval)
	if next.Before(now) {
		next = now
	}
	if next.Before(lr.nextPoll) {
		lr.nextPoll = next
		lr.interval = d.cfg.Poll.Interval
	}
}

// pollSoon makes a run's next observation due within Poll.Interval (after
// an accepted delivery the runtime turns busy and will go idle again; a
// backed-off observer would notice only after Poll.IdleMax).
func (d *Daemon) pollSoon(sessionID, runID string) {
	lr, ok := d.getLive(runID)
	if !ok {
		return
	}
	lr.mu.Lock()
	lr.interval = d.cfg.Poll.Interval
	if next := d.now().Add(d.cfg.Poll.Interval); !lr.polling && lr.nextPoll.After(next) {
		lr.nextPoll = next
	}
	lr.mu.Unlock()
	select {
	case d.pollKick <- struct{}{}:
	default:
	}
}

// hasQueuedMail reports whether a session has mail waiting on this node.
func (d *Daemon) hasQueuedMail(ctx context.Context, sessionID string) bool {
	if d.inbox == nil || sessionID == "" {
		return false
	}
	ms, err := d.inbox.Queued(ctx, sessionID)
	return err == nil && len(ms) > 0
}

func (d *Daemon) schedule(lr *liveRun, changed bool) {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	lr.polling = false
	if changed || lr.interval <= 0 {
		lr.interval = d.cfg.Poll.Interval
	} else {
		lr.interval *= 2
		if lr.interval > d.cfg.Poll.IdleMax {
			lr.interval = d.cfg.Poll.IdleMax
		}
	}
	lr.nextPoll = d.now().Add(lr.interval)
}

func (d *Daemon) observeRun(ctx context.Context, lr *liveRun) {
	rec, _ := lr.snapshot()
	a := lr.adapter
	if a == nil || rec.Handle == nil {
		d.schedule(lr, false)
		return
	}
	if !lr.recovered {
		rctx, cancel := context.WithTimeout(ctx, d.cfg.Poll.Timeout)
		h, err := a.Recover(rctx, *rec.Handle)
		cancel()
		if contracts.IsCode(err, contracts.ErrNotFound) {
			d.endRun(ctx, lr, contracts.RunExited, "run process no longer present (exit not observed)", nil)
			return
		}
		if err != nil {
			d.schedule(lr, false)
			return
		}
		lr.mu.Lock()
		lr.recovered, lr.rec.Handle = true, &h
		lr.mu.Unlock()
		rec.Handle = &h
	}
	stamp := stampOf(lr) // before observing: a change during Observe wakes the next poll
	lr.mu.Lock()
	cursor := lr.cursor
	lr.stamp, lr.observedAt = stamp, d.now()
	lr.mu.Unlock()
	octx, cancel := context.WithTimeout(ctx, d.cfg.Poll.Timeout)
	obs, err := a.Observe(octx, *rec.Handle, cursor)
	cancel()
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		if contracts.IsCode(err, contracts.ErrNotFound) {
			d.endRun(ctx, lr, contracts.RunExited, "run process no longer present (exit not observed)", nil)
			return
		}
		// Lost contact is unknown, never failure.
		changed := d.applyState(ctx, lr, func(r *contracts.Run) {
			r.State = contracts.RunUnknown
			r.StateReason = "observe failed: " + redact(err.Error())
			r.Freshness = d.freshness(true, "observation failed")
			r.Activity = nil
		})
		d.schedule(lr, changed)
		return
	}
	attached := d.attachedClients(ctx, *rec.Handle)
	lr.mu.Lock()
	lr.cursor = obs.NextCursor
	wasAttached := lr.attached
	lr.attached = attached
	lr.mu.Unlock()

	pending, apprChanged := d.applyApprovals(ctx, lr, rec.Run, obs.Approvals)
	if len(obs.Receipts) > 0 {
		for i := range obs.Receipts {
			if obs.Receipts[i].RunID == "" {
				obs.Receipts[i].RunID = rec.Run.RunID
			}
		}
		d.engine.ApplyEvidence(ctx, obs.Receipts)
	}
	d.applyActivity(ctx, lr, obs.Activity)
	d.applyLimit(ctx, lr, obs.LimitEvent)

	if obs.State.Terminal() {
		d.endRun(ctx, lr, obs.State, obs.StateReason, obs.ExitCode)
		return
	}
	prevState := rec.Run.State
	changed := d.applyState(ctx, lr, func(r *contracts.Run) {
		r.State, r.StateReason = obs.State, obs.StateReason
		if obs.NativeRef != nil {
			r.NativeRef = obs.NativeRef
		}
		r.Subagents = obs.Subagents
		r.ExitCode = obs.ExitCode
		r.PendingApprovalIDs = pending
		at := obs.ObservedAt
		if at.IsZero() {
			at = d.now()
		}
		r.Freshness = contracts.Freshness{ObservedAt: at, Source: "node:" + d.nodeID}
		r.Activity = nextActivity(r.Activity, obs.State, obs.Doing, at)
		if r.AdapterData == nil {
			r.AdapterData = map[string]string{}
		}
		if attached {
			r.AdapterData["pyre.attached"] = "true"
		} else {
			delete(r.AdapterData, "pyre.attached")
		}
	})
	queued := d.hasQueuedMail(ctx, rec.Run.SessionID)
	idle := obs.State == contracts.RunWaitingInput || obs.State == contracts.RunWaitingBackground
	if idle {
		d.maybeAutoTitle(ctx, rec, prevState != obs.State) // W-names
	}
	if (changed && (prevState != obs.State || wasAttached != attached)) || (queued && idle) {
		// A state change, or mail waiting while the run looks idle (the
		// busy period may have been shorter than the poll interval): try
		// delivery now instead of after the engine's backoff. An adapter
		// with delivery.InputReadiness answers "not ready" without an
		// attempt, so a run that only looks idle (live: a startup modal)
		// costs no receipts per poll.
		d.engine.Kick(rec.Run.SessionID)
	}
	// While mail waits, keep observing at the active interval so the run
	// going idle is noticed within Poll.Interval.
	d.schedule(lr, changed || apprChanged || queued)
}

// attachedClients reports whether a terminal client is attached to the
// run's tmux session (an implicit raw-input lease).
func (d *Daemon) attachedClients(ctx context.Context, h contracts.RunHandle) bool {
	if h.TmuxSocket == "" || h.TmuxTarget == "" {
		return false
	}
	if host := termhostFor(h); host != nil {
		cs, err := host.ListClients(ctx, h.TmuxTarget)
		return err == nil && len(cs) > 0
	}
	n, err := supervisor.Tmux{Socket: h.TmuxSocket, Timeout: 3 * time.Second}.Clients(ctx, h.TmuxTarget)
	return err == nil && n > 0
}

// applyState mutates the run under its lock and persists/reports it if the
// fingerprint changed.
func (d *Daemon) applyState(ctx context.Context, lr *liveRun, fn func(r *contracts.Run)) bool {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	next := lr.rec
	run := next.Run
	if run.AdapterData != nil {
		cp := make(map[string]string, len(run.AdapterData))
		for k, v := range run.AdapterData {
			cp[k] = v
		}
		run.AdapterData = cp
	}
	fn(&run)
	next.Run = run
	fp := fingerprint(run)
	if fp == lr.rec.Fingerprint {
		lr.rec.Run = run // freshness refresh in memory only
		return false
	}
	next.Fingerprint = fp
	err := sqlitex.Tx(ctx, d.db, func(tx *sql.Tx) error {
		if err := putRunTx(ctx, tx, next, d.now()); err != nil {
			return err
		}
		_, err := d.rep.EnqueueTx(ctx, tx, contracts.ReportRun, d.now(), next.Run)
		return err
	})
	if err != nil {
		d.noteErr("persist run %s: %v", run.RunID, err)
		return false
	}
	d.searchTurnSettled(lr.rec.Run.State, next.Run.State)
	lr.rec = next
	d.rep.Kick()
	return true
}

// applyApprovals assigns stable approval IDs by (run, request ID), reports
// new/changed approvals, and withdraws pending ones that disappeared.
func (d *Daemon) applyApprovals(ctx context.Context, lr *liveRun, run contracts.Run, seen []contracts.Approval) ([]string, bool) {
	now := d.now()
	var pendingIDs []string
	changed := false
	err := sqlitex.Tx(ctx, d.db, func(tx *sql.Tx) error {
		present := map[string]bool{}
		for _, a := range seen {
			a.RunID, a.SessionID = run.RunID, run.SessionID
			if a.RequestID == "" {
				continue
			}
			if a.State == "" {
				a.State = contracts.ApprovalPending
			}
			old, ok, err := getApprovalByRequest(ctx, tx, run.RunID, a.RequestID)
			if err != nil {
				return err
			}
			if ok {
				a.ApprovalID = old.ApprovalID
				if a.CreatedAt.IsZero() {
					a.CreatedAt = old.CreatedAt
				}
				if a.State == contracts.ApprovalPending && old.State != contracts.ApprovalPending {
					a.State, a.Resolution = old.State, old.Resolution // never reopen
				}
				if a.Resolution == nil && old.Resolution != nil {
					a.Resolution = old.Resolution
				}
			} else if a.ApprovalID == "" {
				a.ApprovalID = contracts.NewID(contracts.PrefixApproval)
			}
			if a.CreatedAt.IsZero() {
				a.CreatedAt = now
			}
			present[a.ApprovalID] = true
			if a.State == contracts.ApprovalPending {
				pendingIDs = append(pendingIDs, a.ApprovalID)
			}
			ob, _ := json.Marshal(old)
			nb, _ := json.Marshal(a)
			if ok && string(ob) == string(nb) {
				continue
			}
			if err := putApprovalTx(ctx, tx, a, now); err != nil {
				return err
			}
			if _, err := d.rep.EnqueueTx(ctx, tx, contracts.ReportApproval, now, a); err != nil {
				return err
			}
			changed = true
		}
		pend, err := pendingApprovals(ctx, tx, run.RunID)
		if err != nil {
			return err
		}
		for _, a := range pend {
			if present[a.ApprovalID] {
				continue
			}
			a.State = contracts.ApprovalWithdrawn
			if err := putApprovalTx(ctx, tx, a, now); err != nil {
				return err
			}
			if _, err := d.rep.EnqueueTx(ctx, tx, contracts.ReportApproval, now, a); err != nil {
				return err
			}
			changed = true
		}
		return nil
	})
	if err != nil {
		d.noteErr("approvals for run %s: %v", run.RunID, err)
	}
	if changed {
		d.rep.Kick()
	}
	return pendingIDs, changed
}

func (d *Daemon) applyActivity(ctx context.Context, lr *liveRun, line string) {
	if line == "" {
		return
	}
	lr.mu.Lock()
	if line == lr.lastActivity || d.now().Sub(lr.activityAt) < d.cfg.ActivityMinGap {
		lr.mu.Unlock()
		return
	}
	lr.lastActivity, lr.activityAt = line, d.now()
	run := lr.rec.Run
	lr.mu.Unlock()
	if len(line) > 500 {
		line = line[:500]
	}
	act := contracts.SessionActivity{SessionID: run.SessionID, RunID: run.RunID, Line: redact(line), At: d.now()}
	if _, err := d.rep.Enqueue(ctx, contracts.ReportSessionActivity, act); err != nil && ctx.Err() == nil {
		d.noteErr("activity report: %v", err)
	}
}

func (d *Daemon) applyLimit(ctx context.Context, lr *liveRun, ev *contracts.LimitEvent) {
	if ev == nil {
		return
	}
	lr.mu.Lock()
	if !ev.At.IsZero() && !ev.At.After(lr.lastLimitAt) {
		lr.mu.Unlock()
		return
	}
	run := lr.rec.Run
	// A resumed run replays the previous run's history (transcript records,
	// screen): a limit observed before this run started belongs to the run it
	// replaced and must not trigger another switch.
	if !ev.At.IsZero() && !run.StartedAt.IsZero() && ev.At.Before(run.StartedAt) {
		lr.mu.Unlock()
		return
	}
	lr.lastLimitAt = ev.At
	lr.mu.Unlock()
	d.cfg.Logger.Warn("run reported a limit event", "run", run.RunID, "kind", ev.Kind)
	lh, ok := d.cfg.Accounts.(LimitHandler)
	if !ok {
		return
	}
	// Planned outside this poll: the fallback quiesces this very run (stop,
	// then polls it until it has ended), which cannot happen while this
	// poll holds the run's observation slot. WaitLaunches waits for it.
	ev2 := *ev
	d.asyncWG.Add(1)
	go func() {
		defer d.asyncWG.Done()
		fctx, cancel := context.WithTimeout(ctx, limitPlanTimeout)
		defer cancel()
		plan, err := lh.OnLimit(fctx, run, ev2)
		if err != nil {
			d.noteErr("fallback planning for run %s: %v", run.RunID, err)
			return
		}
		if plan == nil {
			return
		}
		if plan.PlannedAt.IsZero() {
			plan.PlannedAt = d.now()
		}
		if _, err := d.rep.Enqueue(fctx, contracts.ReportFallback, plan); err != nil {
			d.noteErr("fallback report: %v", err)
		}
	}()
}

// limitPlanTimeout bounds one limit fallback (quiesce + successor launch).
const limitPlanTimeout = 3 * time.Minute
