package accounts

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Automatic resume after an account limit (decision I70). The hub keeps
// one timer per session that stopped on a limit with no account free (a
// waiting plan) or whose limit paused automatic switching (the run waits
// on its own account), and asks the node (session.limit_resume) once an
// account may have room. The node answers from its own view of the
// accounts: a limited run that ended continues on the best eligible
// account (the limited one included, once it has room again), exactly as
// an account switch continues it; a run still up continues in place once
// its own account has room. Nothing is planned again and the interrupted
// instruction is never replayed: the successor's first prompt is the
// standard continue prompt.

// limitResumeAttempts bounds the candidates tried for one resume.
const limitResumeAttempts = 8

// resumablePlan reports whether p is a limit the hub may resume
// automatically, and in which mode.
func resumablePlan(p *contracts.FallbackPlan) (string, bool) {
	if p == nil || p.Limit == nil || (p.Limit.Kind != LimitUsage && p.Limit.Kind != LimitRate) {
		return "", false
	}
	switch {
	case p.Outcome == contracts.FallbackWaiting:
		return contracts.LimitResumeContinue, true
	case p.Outcome == contracts.FallbackIntervention && strings.HasPrefix(p.Reason, SwitchPausedPrefix):
		return contracts.LimitResumeInPlace, true
	}
	return "", false
}

// ResumeAfterLimit continues session sessionID, whose run runID stopped on
// a limit with no account free (its plan is waiting), on the best
// eligible account now. No account with room is a no_room result naming
// the earliest reset; a run that is not that stop, or no longer the
// session's current one, is refused.
func (s *Service) ResumeAfterLimit(ctx context.Context, sessionID, runID string) (contracts.LimitResumeResult, error) {
	refused := func(why string) (contracts.LimitResumeResult, error) {
		return contracts.LimitResumeResult{Outcome: contracts.LimitResumeRefused, Detail: why}, nil
	}
	s.fbmu.Lock()
	defer s.fbmu.Unlock()
	if s.cfg.Controller == nil {
		return contracts.LimitResumeResult{}, contracts.Errorf(contracts.ErrUnavailable, "no run controller configured")
	}
	plan, err := getFallback(ctx, s.db, runID)
	if err != nil {
		return contracts.LimitResumeResult{}, err
	}
	if mode, ok := resumablePlan(plan); !ok || mode != contracts.LimitResumeContinue {
		return refused("the run did not stop on a usage limit with no account free")
	}
	info, err := s.cfg.Controller.CurrentRun(ctx, sessionID)
	if err != nil {
		return contracts.LimitResumeResult{}, err
	}
	if info.RunID != runID {
		return refused("the session's current run is " + info.RunID)
	}
	if info.Live {
		return refused("the run is still up")
	}
	asg, err := s.assignmentForRun(ctx, s.db, runID)
	if err != nil {
		return refused("the run has no account assignment on this node")
	}
	if asg.CredentialKind == contracts.CredAmbient {
		return refused("the run used the ambient default login")
	}
	fromProfile := ""
	if g, err := s.grantByID(ctx, s.db, asg.GrantID); err == nil {
		fromProfile = g.ProfileDir
	}
	fromLabel := s.accountLabel(ctx, asg.AccountID)
	cross := contracts.HasCap(info.Capabilities, contracts.CapAccountCrossResume)
	var exclude []string
	for attempt := 0; attempt < limitResumeAttempts; attempt++ {
		req := ResolveRequest{Harness: info.Harness, Policy: contracts.AccountPolicy{Mode: contracts.AccountEligible}, RunID: "limit-resume", Exclude: exclude}
		cands, notes, err := s.gather(ctx, req)
		if err != nil {
			return contracts.LimitResumeResult{}, err
		}
		c, cerr := s.choose(ctx, s.db, req, cands, notes, s.cfg.CodexLeaseHolder)
		if cerr != nil {
			return noRoom(cerr), nil
		}
		kind, why := continuationKind(info, cross, fromProfile, c.grant.ProfileDir)
		if c.acct.AccountID == asg.AccountID && info.NativeRef != nil && contracts.HasCap(info.Capabilities, contracts.CapResumeNative) {
			// The account that ran out has room again: its own profile.
			kind, why = contracts.ContinuationNativeResume, "native resume on the same account"
		}
		cont := Continuation{SessionID: sessionID, PreviousRunID: runID, Harness: info.Harness, AccountID: c.acct.AccountID,
			Kind: kind, UncertainWork: plan.UncertainWork, Reason: "limit resume: " + c.why,
			Prompt: continuePrompt(fromLabel, acctName(c.acct), plan.Limit.Kind, plan.UncertainWork) + stoppedTasksNote(plan.StoppedTasks)}
		if kind == contracts.ContinuationNativeResume {
			ref := *info.NativeRef
			cont.Resume = &ref
		}
		res, err := s.cfg.Controller.StartContinuation(ctx, cont)
		if err != nil {
			if contracts.IsCode(err, contracts.ErrConflict) || contracts.IsCode(err, contracts.ErrCapacityExhausted) || contracts.IsCode(err, contracts.ErrUnavailable) {
				exclude = append(exclude, c.acct.AccountID)
				continue
			}
			return contracts.LimitResumeResult{}, err
		}
		if res.Kind != "" {
			kind = res.Kind
		}
		if err := carryPinned(ctx, s.db, runID, res.NewRunID); err != nil {
			s.log.Warn("accounts: carry pin to limit resume", "run", res.NewRunID, "err", err)
		}
		s.log.Info("accounts: resumed after a limit", "session", sessionID, "run", runID, "account", c.acct.AccountID, "new_run", res.NewRunID, "how", why)
		return contracts.LimitResumeResult{Outcome: contracts.LimitResumed, AccountID: c.acct.AccountID, NewRunID: res.NewRunID, Kind: string(kind),
			Detail: redact(c.why)}, nil
	}
	return contracts.LimitResumeResult{Outcome: contracts.LimitResumeNoRoom, Detail: "every candidate account refused the continuation"}, nil
}

// noRoom turns a selection failure into a no_room result with the
// earliest reset the selection named.
func noRoom(cerr error) contracts.LimitResumeResult {
	e := contracts.AsError(cerr)
	out := contracts.LimitResumeResult{Outcome: contracts.LimitResumeNoRoom, Detail: truncate(redact(e.Message), 300)}
	if v, _ := e.Details["earliest_reset"].(string); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			out.ResetAt = &t
		}
	}
	return out
}

// LimitRoom says whether run runID, still up on the account whose limit
// paused its switching, may be told to continue now: its plan is that
// paused limit and its account is not known to be at a limit any more.
// Prompt is the continue prompt to send. OK false with ResetAt: not yet;
// Refused: never (Why says why).
type LimitRoom struct {
	OK        bool
	Refused   bool
	AccountID string
	ResetAt   *time.Time
	Why       string
	Prompt    string
}

// LimitRoomFor answers LimitRoom for runID.
func (s *Service) LimitRoomFor(ctx context.Context, runID string) (LimitRoom, error) {
	plan, err := getFallback(ctx, s.db, runID)
	if err != nil {
		return LimitRoom{}, err
	}
	if mode, ok := resumablePlan(plan); !ok || mode != contracts.LimitResumeInPlace {
		return LimitRoom{Refused: true, Why: "the run's limit did not pause automatic switching"}, nil
	}
	asg, err := s.assignmentForRun(ctx, s.db, runID)
	if err != nil {
		return LimitRoom{Refused: true, Why: "the run has no account assignment on this node"}, nil
	}
	out := LimitRoom{AccountID: asg.AccountID}
	obs, err := getObservation(ctx, s.db, asg.AccountID)
	if err != nil {
		return LimitRoom{}, err
	}
	cd, err := getCooldown(ctx, s.db, asg.AccountID)
	if err != nil {
		return LimitRoom{}, err
	}
	switch class, _, reset, _, why := s.classify(obs, cd, s.now()); class {
	case classUnusable:
		out.Refused, out.Why = true, why
		return out, nil
	case classCooldown:
		out.ResetAt, out.Why = reset, why
		return out, nil
	}
	label := s.accountLabel(ctx, asg.AccountID)
	out.OK = true
	out.Prompt = continuePrompt(label, label, plan.Limit.Kind, false)
	out.Why = fmt.Sprintf("%s has room again", acctRef(label))
	return out, nil
}
