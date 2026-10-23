package accounts

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/usage"
)

// Proactive moves (W-rebalance). Sessions used to keep their account until
// it hit a limit mid-turn: 39 of 155 automatic switches cut the agent's last
// step off ("check its effects before repeating"). Between turns, the
// rebalancer moves an idle session off an account whose weekly window is at
// MoveWeeklyPercent or whose 5-hour window is at MoveFivePercent, onto the
// best eligible same-provider account that is not itself near a limit
// (nearLimit). It also drains idle runs off a reserve account (reserve.go)
// onto a normal account with comfortable room, unless the starter named the
// account (assignment named: explicit or Prefer). Selection never offers a
// reserve account while a normal one is comfortable, so no move goes onto a
// reserve account then. The move is the fallback's native resume (same session, same
// transcript, another profile) with no prompt: the session stays idle and
// the next message continues on the new account.
//
// Only safe moves happen:
//   - the run has read waiting_input for at least IdleBeforeMove across
//     sweeps, and no message for the session is queued or was delivered in
//     the last minute (the state lags the harness);
//   - the starter did not pin the account strictly (assignment pinned; a
//     fallback successor's internal strict bind does not count);
//   - the harness advertises cross-account resume, both profiles share the
//     transcript store, and the successor may only be a native resume (a
//     handoff would lose the conversation);
//   - at most one proactive move per session per RebalanceCooldown, none
//     within MoveFailCooldown of a failed one, and none while the limit
//     fallback's circuit breaker is open for the session. Proactive plans do
//     not count toward that breaker.
//
// At most MaxMovesPerSweep sessions move per sweep, so a hot account drains
// over a few minutes instead of herding every session onto the next account
// at once (selection re-ranks after every move).
//
// Locking: a move claims the run with a pending fallback row under fbmu and
// releases the lock before stopping and restarting anything, so a slow stop
// never blocks limit fallbacks; each move runs under moveTimeout.
const (
	MoveWeeklyPercent = 85.0
	MoveFivePercent   = 90.0
	MaxMovesPerSweep  = 3
	// MaxDrainsPerSweep bounds moves off a reserve account (inside
	// MaxMovesPerSweep), so they never crowd out moves off hot accounts.
	MaxDrainsPerSweep = 1
	IdleBeforeMove    = time.Minute
	RebalanceCooldown = 30 * time.Minute
	MoveFailCooldown  = time.Hour
	rebalanceEvery    = 2 * time.Minute
	moveTimeout       = 3 * time.Minute
)

// livePoll spaces the checks after an unconfirmed stop (tests shorten it).
var livePoll = time.Second

// ProactivePrefix starts the reason of a fallback plan the rebalancer made.
const ProactivePrefix = "proactive move between turns"

// outcomePending marks a run claimed by an in-progress proactive move.
const outcomePending contracts.FallbackOutcome = "pending"

// rebalanceState is the rebalancer's memory between sweeps.
type rebalanceState struct {
	mu        sync.Mutex
	idleSince map[string]time.Time // run -> first sweep that saw it idle
	failed    map[string]time.Time // session -> last failed move (also read from recorded plans)
	deferred  map[string]deferredLimit
}

// deferredLimit is a limit event that arrived while a move held the run.
type deferredLimit struct {
	session string
	ev      contracts.LimitEvent
}

func (s *Service) deferLimit(sessionID, runID string, ev contracts.LimitEvent) {
	s.rb.mu.Lock()
	defer s.rb.mu.Unlock()
	if s.rb.deferred == nil {
		s.rb.deferred = map[string]deferredLimit{}
	}
	s.rb.deferred[runID] = deferredLimit{sessionID, ev}
}

func (s *Service) takeDeferred(runID string) (deferredLimit, bool) {
	s.rb.mu.Lock()
	defer s.rb.mu.Unlock()
	d, ok := s.rb.deferred[runID]
	delete(s.rb.deferred, runID)
	return d, ok
}

// RunRebalancer sweeps every rebalanceEvery until ctx ends; the first thing
// it does is settle claims a previous pyred left behind.
func (s *Service) RunRebalancer(ctx context.Context) {
	s.settleStaleClaims(ctx)
	tick := time.NewTicker(rebalanceEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		s.Rebalance(ctx)
	}
}

// hotAccount: a fresh observation with a window at the move thresholds.
func (s *Service) hotAccount(ctx context.Context, accountID string) (usage.Headroom, bool) {
	obs, err := getObservation(ctx, s.db, accountID)
	if err != nil || obs == nil {
		return usage.Headroom{}, false
	}
	cd, _ := getCooldown(ctx, s.db, accountID)
	class, _, _, _, _ := s.classify(obs, cd, s.now())
	if class != classFresh {
		return usage.Headroom{}, false // at a limit already: the limit fallback handles it
	}
	h := usage.HeadroomOf(obs.Windows)
	return h, h.WeekOK && h.Weekly >= MoveWeeklyPercent || h.FiveOK && h.FiveHour >= MoveFivePercent
}

// idleLongEnough records that run reads idle now and reports whether it has
// read idle since at least IdleBeforeMove ago.
func (s *Service) idleLongEnough(runID string, idle bool) bool {
	s.rb.mu.Lock()
	defer s.rb.mu.Unlock()
	if s.rb.idleSince == nil {
		s.rb.idleSince = map[string]time.Time{}
	}
	if !idle {
		delete(s.rb.idleSince, runID)
		return false
	}
	since, ok := s.rb.idleSince[runID]
	if !ok {
		s.rb.idleSince[runID] = s.now()
		return false
	}
	return s.now().Sub(since) >= IdleBeforeMove
}

func (s *Service) moveFailedRecently(ctx context.Context, sessionID string) bool {
	s.rb.mu.Lock()
	t, ok := s.rb.failed[sessionID]
	s.rb.mu.Unlock()
	if ok && s.now().Sub(t) < MoveFailCooldown {
		return true
	}
	return failedMoveSince(ctx, s.db, sessionID, s.now().Add(-MoveFailCooldown))
}

// pruneIdle forgets idle sightings of runs that are no longer active.
func (s *Service) pruneIdle(active map[string]bool) {
	s.rb.mu.Lock()
	defer s.rb.mu.Unlock()
	for run := range s.rb.idleSince {
		if !active[run] {
			delete(s.rb.idleSince, run)
		}
	}
}

// settleStaleClaims finishes proactive-move claims older than the move
// deadline (pyred stopped or crashed mid-move): a claimed run that is still
// the session's live run just drops the claim; one that ended without a
// successor is resumed on its original account (an intervention plan when
// that fails); either way the outcome is recorded and reported.
func (s *Service) settleStaleClaims(ctx context.Context) {
	if s.cfg.Controller == nil {
		return
	}
	claims, err := staleClaims(ctx, s.db, s.now().Add(-moveTimeout-time.Minute))
	if err != nil {
		s.log.Warn("accounts: stale proactive claims", "err", err)
		return
	}
	for _, c := range claims {
		info, err := s.cfg.Controller.CurrentRun(ctx, c.SessionID)
		switch {
		case err != nil && !contracts.IsCode(err, contracts.ErrNotFound):
			continue // try again next sweep
		case err == nil && (info.RunID != c.FromRunID || info.Live):
			// Still running, or a successor exists: nothing was lost.
			s.unclaim(ctx, c.FromRunID)
			continue
		}
		plan := c
		plan.PlannedAt = s.now()
		if err != nil || info.NativeRef == nil {
			plan.Outcome = contracts.FallbackIntervention
			plan.Reason = ProactivePrefix + ": interrupted (pyred restarted mid-move); the session is stopped, resume it with pyre resume"
		} else {
			ref := *info.NativeRef
			res, berr := s.cfg.Controller.StartContinuation(ctx, Continuation{SessionID: c.SessionID, PreviousRunID: c.FromRunID, Harness: info.Harness,
				AccountID: c.FromAccountID, Kind: contracts.ContinuationNativeResume, Resume: &ref, Reason: "proactive move interrupted; resumed on the original account"})
			if berr != nil {
				plan.Outcome = contracts.FallbackIntervention
				plan.Reason = fmt.Sprintf("%s: interrupted (pyred restarted mid-move) and resuming on the original account failed (%s); the session is stopped, resume it with pyre resume", ProactivePrefix, berr)
			} else {
				plan.Outcome, plan.ToAccountID, plan.NewRunID = contracts.FallbackNativeResume, c.FromAccountID, res.NewRunID
				if res.Kind == contracts.ContinuationHandoff {
					plan.Outcome = contracts.FallbackHandoff
				}
				plan.Reason = ProactivePrefix + ": interrupted (pyred restarted mid-move); the move failed and the session resumed on its original account"
				if err := carryPinned(ctx, s.db, c.FromRunID, res.NewRunID); err != nil {
					s.log.Warn("accounts: carry pin to successor", "run", res.NewRunID, "err", err)
				}
			}
		}
		plan.Reason = redact(plan.Reason)
		if err := setFallback(ctx, s.db, plan); err != nil {
			s.log.Warn("accounts: record settled claim", "run", c.FromRunID, "err", err)
		}
		s.report(ctx, contracts.ReportFallback, plan)
	}
}

// unclaim drops a run's proactive claim and handles a limit event that
// arrived meanwhile.
func (s *Service) unclaim(ctx context.Context, runID string) {
	ctx = context.WithoutCancel(ctx)
	if err := deleteFallback(ctx, s.db, runID); err != nil {
		s.log.Warn("accounts: drop proactive claim", "run", runID, "err", err)
	}
	if d, ok := s.takeDeferred(runID); ok {
		if _, err := s.HandleLimitEvent(ctx, d.session, runID, d.ev); err != nil {
			s.log.Warn("accounts: deferred limit event", "run", runID, "err", err)
		}
	}
}

func (s *Service) markMoveFailed(sessionID string) {
	s.rb.mu.Lock()
	defer s.rb.mu.Unlock()
	if s.rb.failed == nil {
		s.rb.failed = map[string]time.Time{}
	}
	s.rb.failed[sessionID] = s.now()
}

// Rebalance moves idle sessions off hot accounts (one sweep) and returns the
// plans of the moves it made or attempted.
func (s *Service) Rebalance(ctx context.Context) []contracts.FallbackPlan {
	if s.cfg.Controller == nil {
		return nil
	}
	s.settleStaleClaims(ctx)
	// Strict pins stay (successors carry their predecessor's flag).
	rows, err := s.db.QueryContext(ctx, `SELECT a.run_id, a.session_id, a.account_id, a.named FROM accounts_assignment a
		WHERE a.released_at='' AND a.account_id<>'' AND a.credential_kind<>? AND a.session_id<>'' AND a.pinned=0
		ORDER BY a.assigned_at`, string(contracts.CredAmbient))
	if err != nil {
		s.log.Warn("accounts: rebalance query", "err", err)
		return nil
	}
	type active struct {
		run, session, account string
		named                 bool
	}
	var list []active
	for rows.Next() {
		var a active
		if rows.Scan(&a.run, &a.session, &a.account, &a.named) == nil {
			list = append(list, a)
		}
	}
	rows.Close()
	live := map[string]bool{}
	for _, a := range list {
		live[a.run] = true
	}
	s.pruneIdle(live)
	var out []contracts.FallbackPlan
	hot := map[string]*usage.Headroom{}
	ids := make([]string, 0, len(list))
	for _, a := range list {
		ids = append(ids, a.account)
	}
	reserve := s.reserveAccounts(ctx, ids) // account -> provider, reserve ones only
	room := map[contracts.Provider]bool{}  // a normal account can take a drain (once per sweep)
	for _, a := range list {
		if p, ok := reserve[a.account]; ok && !a.named {
			if _, seen := room[p]; !seen {
				room[p] = s.normalRoom(ctx, p)
			}
		}
	}
	drains := 0
	for _, a := range list {
		if len(out) >= MaxMovesPerSweep || ctx.Err() != nil {
			break
		}
		h, seen := hot[a.account]
		if !seen {
			if room, ok := s.hotAccount(ctx, a.account); ok {
				h = &room
			}
			hot[a.account] = h
		}
		p, isReserve := reserve[a.account]
		drain := h == nil && isReserve && !a.named && room[p] && drains < MaxDrainsPerSweep
		if h == nil && !drain {
			continue
		}
		from := usage.Headroom{}
		if h != nil {
			from = *h
		}
		mctx, cancel := context.WithTimeout(ctx, moveTimeout)
		plan, ok := s.moveIdle(mctx, a.session, a.run, a.account, from, drain)
		cancel()
		if ok {
			out = append(out, plan)
			if drain {
				drains++
			}
		}
	}
	return out
}

// claimForMove checks a run under fbmu and, when it may move, claims it with
// a pending fallback row (a limit event on it meanwhile returns the claim).
func (s *Service) claimForMove(ctx context.Context, sessionID, runID, fromID string) (RunInfo, bool) {
	s.fbmu.Lock()
	defer s.fbmu.Unlock()
	if p, err := getFallback(ctx, s.db, runID); err != nil || p != nil {
		return RunInfo{}, false
	}
	info, err := s.cfg.Controller.CurrentRun(ctx, sessionID)
	idle := err == nil && info.RunID == runID && info.State == contracts.RunWaitingInput && !info.TurnInFlight && !info.RecentInput
	if !s.idleLongEnough(runID, idle) {
		return RunInfo{}, false
	}
	if contracts.ProviderFor(info.Harness) == "" || !contracts.HasCap(info.Capabilities, contracts.CapAccountCrossResume) || info.NativeRef == nil {
		return RunInfo{}, false
	}
	if s.moveFailedRecently(ctx, sessionID) {
		return RunInfo{}, false
	}
	if last, err := lastProactive(ctx, s.db, sessionID, s.now().Add(-RebalanceCooldown)); err != nil || !last.IsZero() {
		return RunInfo{}, false
	}
	if why, err := s.switchPaused(ctx, sessionID, fromID); err != nil || why != "" {
		return RunInfo{}, false
	}
	claim := contracts.FallbackPlan{SessionID: sessionID, FromRunID: runID, FromAccountID: fromID, Outcome: outcomePending,
		PlannedAt: s.now(), Reason: ProactivePrefix + ": in progress"}
	if err := putFallback(ctx, s.db, claim); err != nil {
		return RunInfo{}, false
	}
	return info, true
}

// moveIdle moves one idle session to an account with headroom. ok is true
// when a plan was recorded (moved, or a failed move reported). drain: the
// run is on a reserve account that is not hot (reserve.go); it moves only
// to a normal account with comfortable room.
func (s *Service) moveIdle(ctx context.Context, sessionID, runID, fromID string, from usage.Headroom, drain bool) (contracts.FallbackPlan, bool) {
	info, ok := s.claimForMove(ctx, sessionID, runID, fromID)
	if !ok {
		return contracts.FallbackPlan{}, false
	}
	unclaim := func() (contracts.FallbackPlan, bool) {
		s.unclaim(ctx, runID)
		return contracts.FallbackPlan{}, false
	}
	h := info.Harness
	asg, err := s.assignmentForRun(ctx, s.db, runID)
	if err != nil || asg.ReleasedAt != nil {
		return unclaim()
	}
	fromGrant, err := s.grantByID(ctx, s.db, asg.GrantID)
	if err != nil {
		return unclaim()
	}
	req := ResolveRequest{Harness: h, Policy: contracts.AccountPolicy{Mode: contracts.AccountEligible}, RunID: "rebalance-plan", Exclude: []string{fromID}}
	cands, notes, err := s.gather(ctx, req)
	if err != nil {
		return unclaim()
	}
	c, err := s.choose(ctx, s.db, req, cands, notes, s.cfg.CodexLeaseHolder)
	switch {
	case err != nil || c.class != classFresh || nearLimit(c.room):
		return unclaim() // nowhere better to go: stay until the limit fallback
	case drain && (c.reserve || !comfortable(c.room)):
		return unclaim() // no normal account has room: stay on the reserve one
	case !drain && c.room.Compare(from) >= 0:
		return unclaim()
	}
	if !sharesTranscriptStore(h, fromGrant.ProfileDir, c.grant.ProfileDir) {
		return unclaim()
	}
	// Re-check right before stopping: a prompt may have arrived.
	if i2, err := s.cfg.Controller.CurrentRun(ctx, sessionID); err != nil || i2.RunID != runID || i2.State != contracts.RunWaitingInput || i2.RecentInput {
		return unclaim()
	}
	q, qerr := s.cfg.Controller.Quiesce(ctx, sessionID, runID)
	// From here the old run may be gone: finish under a context that the
	// move's deadline cannot cut between stop and restart.
	ctx = context.WithoutCancel(ctx)
	fromLabel, toLabel := s.accountLabel(ctx, fromID), acctName(c.acct)
	plan := contracts.FallbackPlan{SessionID: sessionID, FromRunID: runID, FromAccountID: fromID, UncertainWork: q.TurnInFlight}
	if qerr != nil || !q.Exited {
		// Unconfirmed stop: look again. Ended: bring it back on its own
		// account. Still up after a refused stop: left as is. Still up
		// after a stop that was sent: it may still end, so say so.
		s.markMoveFailed(sessionID)
		live := s.stillLive(ctx, sessionID, runID)
		switch {
		case live && qerr != nil:
			s.log.Warn("accounts: proactive move: could not stop the run; left as is", "session", sessionID, "run", runID, "err", qerr)
			return unclaim()
		case live:
			plan.Outcome, plan.PlannedAt = contracts.FallbackIntervention, s.now()
			plan.Reason = fmt.Sprintf("%s: stopping the run for a move from %s to %s was not confirmed (%s); it may stop later: check the session and resume it with pyre resume if it is down",
				ProactivePrefix, acctRef(fromLabel), acctRef(toLabel), q.Detail)
		default:
			s.resumeOriginal(ctx, &plan, info, fromID, fromLabel, toLabel, fmt.Errorf("stop not confirmed: %s", q.Detail))
		}
		return s.recordMove(ctx, plan)
	}
	if i3, err := s.cfg.Controller.CurrentRun(ctx, sessionID); err == nil && i3.RecentInput {
		// Something was delivered after the last check: it may be lost.
		plan.UncertainWork = true
	}
	if _, err := s.ReleaseRun(ctx, runID); err != nil && !contracts.IsCode(err, contracts.ErrNotFound) {
		s.log.Warn("accounts: release after proactive stop", "run", runID, "err", err)
	}
	prompt := ""
	if plan.UncertainWork {
		// A message raced the stop: say so, never replay it.
		prompt = continuePrompt(fromLabel, toLabel, "proactive", true)
	}
	ref := *info.NativeRef
	cont := Continuation{SessionID: sessionID, PreviousRunID: runID, Harness: h, AccountID: c.acct.AccountID,
		Kind: contracts.ContinuationNativeResume, Resume: &ref, UncertainWork: plan.UncertainWork, Reason: c.why, Prompt: prompt, NativeOnly: true}
	plan.PlannedAt = s.now()
	res, err := s.cfg.Controller.StartContinuation(ctx, cont)
	if err == nil {
		plan.Outcome, plan.ToAccountID, plan.NewRunID = contracts.FallbackNativeResume, c.acct.AccountID, res.NewRunID
		plan.Reason = fmt.Sprintf("%s: %s at %s; %s %s", ProactivePrefix, acctRef(fromLabel), from.Numbers(), acctRef(toLabel), c.why)
		if drain {
			plan.Reason = fmt.Sprintf("%s: %s is a reserve account and a normal account has room again; %s %s", ProactivePrefix, acctRef(fromLabel), acctRef(toLabel), c.why)
		}
		if err := carryPinned(ctx, s.db, runID, res.NewRunID); err != nil {
			s.log.Warn("accounts: carry pin to successor", "run", res.NewRunID, "err", err)
		}
		s.log.Info("accounts: moved an idle session to an account with headroom", "session", sessionID, "from", fromID, "to", c.acct.AccountID)
	} else {
		// The session is stopped: bring it back on its own account, and do
		// not try again soon.
		s.markMoveFailed(sessionID)
		s.log.Warn("accounts: proactive move failed; resuming on the original account", "session", sessionID, "to", c.acct.AccountID, "err", err)
		s.resumeOriginal(ctx, &plan, info, fromID, fromLabel, toLabel, err)
	}
	return s.recordMove(ctx, plan)
}

// stillLive polls briefly whether the run is still the session's live run.
func (s *Service) stillLive(ctx context.Context, sessionID, runID string) bool {
	for i := 0; i < 10; i++ {
		info, err := s.cfg.Controller.CurrentRun(ctx, sessionID)
		if err == nil && (info.RunID != runID || !info.Live) {
			return false
		}
		if i < 9 {
			time.Sleep(livePoll)
		}
	}
	return true
}

// resumeOriginal restarts a stopped session on its original account after a
// failed move (a handoff there beats leaving it down) and words the plan.
func (s *Service) resumeOriginal(ctx context.Context, plan *contracts.FallbackPlan, info RunInfo, fromID, fromLabel, toLabel string, cause error) {
	plan.PlannedAt = s.now()
	back := Continuation{SessionID: plan.SessionID, PreviousRunID: plan.FromRunID, Harness: info.Harness, AccountID: fromID,
		Kind: contracts.ContinuationNativeResume, UncertainWork: plan.UncertainWork, Reason: "proactive move failed; resumed on the original account"}
	if info.NativeRef != nil {
		ref := *info.NativeRef
		back.Resume = &ref
	}
	bres, berr := s.cfg.Controller.StartContinuation(ctx, back)
	if berr == nil {
		plan.Outcome, plan.ToAccountID, plan.NewRunID = contracts.FallbackNativeResume, fromID, bres.NewRunID
		if bres.Kind == contracts.ContinuationHandoff {
			plan.Outcome = contracts.FallbackHandoff
		}
		plan.Reason = fmt.Sprintf("%s: the move from %s to %s failed (%s); resumed on %s", ProactivePrefix, acctRef(fromLabel), acctRef(toLabel), cause, acctRef(fromLabel))
		if err := carryPinned(ctx, s.db, plan.FromRunID, bres.NewRunID); err != nil {
			s.log.Warn("accounts: carry pin to successor", "run", bres.NewRunID, "err", err)
		}
		return
	}
	s.log.Error("accounts: session left stopped after a failed proactive move", "session", plan.SessionID, "err", berr)
	plan.Outcome = contracts.FallbackIntervention
	plan.Reason = fmt.Sprintf("%s: the move from %s to %s failed (%s) and resuming on %s failed too (%s); the session is stopped, resume it with pyre resume",
		ProactivePrefix, acctRef(fromLabel), acctRef(toLabel), cause, acctRef(fromLabel), berr)
}

// recordMove stores (replacing the claim) and reports a move's plan.
func (s *Service) recordMove(ctx context.Context, plan contracts.FallbackPlan) (contracts.FallbackPlan, bool) {
	plan.Reason = redact(plan.Reason)
	if err := setFallback(ctx, s.db, plan); err != nil {
		s.log.Warn("accounts: record proactive move", "session", plan.SessionID, "err", err)
	}
	s.report(ctx, contracts.ReportFallback, plan)
	s.takeDeferred(plan.FromRunID) // the old run is settled; its limit is moot
	return plan, true
}
