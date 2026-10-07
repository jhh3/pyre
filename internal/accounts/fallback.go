package accounts

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Limit event kinds (contracts.LimitEvent.Kind).
const (
	LimitUsage       = "usage_limit"
	LimitRate        = "rate_limited"
	LimitAuthInvalid = "auth_invalid"
)

// ExhaustedPayload is the payload of the account.exhausted event reported
// when no same-provider account can continue a session.
type ExhaustedPayload struct {
	Provider      contracts.Provider        `json:"provider"`
	SessionID     string                    `json:"session_id"`
	FromRunID     string                    `json:"from_run_id"`
	FromAccountID string                    `json:"from_account_id,omitempty"`
	Outcome       contracts.FallbackOutcome `json:"outcome"`
	EarliestReset *time.Time                `json:"earliest_reset,omitempty"`
	Reasons       []string                  `json:"reasons,omitempty"`
}

// HandleLimitEvent plans recovery after an adapter observed a usage limit or
// unusable login on run R:
//
//  1. record the limit (cooldown/observation) or classify the login failure
//     (dead/expired grant, no retries);
//  2. quiesce R through the daemon (stop + wait) before anything replaces it;
//  3. pick another eligible account of the SAME provider and ask the daemon
//     to continue: native_resume only when the harness advertises
//     account.cross_account_resume and both profiles share the transcript
//     store, otherwise a handoff successor;
//  4. if none is eligible: waiting (earliest reset) or intervention, plus an
//     account.exhausted event so attention/notifications fire.
//
// The interrupted instruction is never replayed (UncertainWork marks a turn
// that was in flight). R's records stay immutable. Repeated events for the
// same run return the first plan.
func (s *Service) HandleLimitEvent(ctx context.Context, sessionID, runID string, ev contracts.LimitEvent) (contracts.FallbackPlan, error) {
	s.fbmu.Lock()
	defer s.fbmu.Unlock()
	if p, err := getFallback(ctx, s.db, runID); err != nil {
		return contracts.FallbackPlan{}, err
	} else if p != nil {
		return *p, nil
	}
	if ev.At.IsZero() {
		ev.At = s.now()
	}
	plan := contracts.FallbackPlan{SessionID: sessionID, FromRunID: runID, PlannedAt: s.now()}
	finish := func(outcome contracts.FallbackOutcome, reason string) (contracts.FallbackPlan, error) {
		plan.Outcome, plan.Reason = outcome, redact(reason)
		if err := putFallback(ctx, s.db, plan); err != nil {
			return plan, err
		}
		s.report(ctx, contracts.ReportFallback, plan)
		s.log.Info("accounts: fallback planned", "session", sessionID, "run", runID, "outcome", string(outcome))
		return plan, nil
	}
	if s.cfg.Controller == nil {
		return plan, contracts.Errorf(contracts.ErrUnavailable, "no run controller configured")
	}

	asg, err := s.assignmentForRun(ctx, s.db, runID)
	if err != nil {
		return finish(contracts.FallbackIntervention, "run has no account assignment on this node; cannot plan same-provider recovery")
	}
	plan.FromAccountID = asg.AccountID
	info, err := s.cfg.Controller.CurrentRun(ctx, sessionID)
	if err != nil {
		return plan, err
	}
	h := info.Harness
	provider := contracts.ProviderFor(h)
	if provider == "" {
		return finish(contracts.FallbackIntervention, "harness has no subscription provider")
	}

	// 1. Record what happened to the old account.
	var fromGrant grantRow
	var fromReset *time.Time
	if asg.CredentialKind != contracts.CredAmbient {
		fromGrant, err = s.grantByID(ctx, s.db, asg.GrantID)
		if err != nil {
			return plan, err
		}
		fromReset = s.recordLimit(ctx, asg.AccountID, provider, fromGrant, ev)
	}

	// 2. Quiesce the old run before anything takes over.
	q, qerr := s.cfg.Controller.Quiesce(ctx, sessionID, runID)
	plan.UncertainWork = info.TurnInFlight || q.TurnInFlight
	if qerr != nil || !q.Exited {
		detail := q.Detail
		if qerr != nil {
			detail = qerr.Error()
		}
		return finish(contracts.FallbackIntervention, "could not quiesce the limited run; no replacement started (would risk two live runs): "+detail)
	}
	if _, err := s.ReleaseRun(ctx, runID); err != nil && !contracts.IsCode(err, contracts.ErrNotFound) {
		s.log.Warn("accounts: release after quiesce", "run", runID, "err", err)
	}
	if asg.CredentialKind == contracts.CredAmbient {
		return finish(contracts.FallbackIntervention, "run used the ambient default login; automatic account fallback is disabled for ambient runs")
	}

	// 3. Pick another same-provider account and continue.
	exclude := []string{asg.AccountID}
	cross := contracts.HasCap(info.Capabilities, contracts.CapAccountCrossResume)
	for attempt := 0; attempt < 8; attempt++ {
		req := ResolveRequest{Harness: h, Policy: contracts.AccountPolicy{Mode: contracts.AccountEligible}, RunID: "fallback-plan", Exclude: exclude}
		cands, notes, err := s.gather(ctx, req)
		if err != nil {
			return plan, err
		}
		c, cerr := s.choose(ctx, s.db, req, cands, notes, s.cfg.CodexLeaseHolder)
		if cerr != nil {
			return s.finishNoneEligible(ctx, plan, provider, cerr, fromReset, finish)
		}
		kind := contracts.ContinuationHandoff
		why := ""
		switch {
		case !cross:
			why = fmt.Sprintf("%s does not advertise %s; linked successor with handoff", h, contracts.CapAccountCrossResume)
		case info.NativeRef == nil:
			why = "no native session reference; linked successor with handoff"
		case !sharesTranscriptStore(h, fromGrant.ProfileDir, c.grant.ProfileDir):
			why = "profiles do not share the transcript store; linked successor with handoff"
		default:
			kind = contracts.ContinuationNativeResume
			why = "native resume over the shared transcript store"
		}
		cont := Continuation{SessionID: sessionID, PreviousRunID: runID, Harness: h, AccountID: c.acct.AccountID,
			Kind: kind, UncertainWork: plan.UncertainWork, Reason: c.why}
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
			return finish(contracts.FallbackIntervention, "successor launch failed: "+err.Error())
		}
		plan.ToAccountID, plan.NewRunID = c.acct.AccountID, res.NewRunID
		reason := fmt.Sprintf("%s on %s; %s; %s", ev.Kind, asg.AccountID, c.acct.AccountID+" "+c.why, why)
		if plan.UncertainWork {
			reason += "; a turn was in flight: its instruction is not replayed"
		}
		if kind == contracts.ContinuationNativeResume {
			return finish(contracts.FallbackNativeResume, reason)
		}
		return finish(contracts.FallbackHandoff, reason)
	}
	return finish(contracts.FallbackIntervention, "every candidate account refused the successor launch")
}

// finishNoneEligible turns a selection failure into waiting/intervention and
// emits account.exhausted.
func (s *Service) finishNoneEligible(ctx context.Context, plan contracts.FallbackPlan, provider contracts.Provider, cerr error,
	fromReset *time.Time, finish func(contracts.FallbackOutcome, string) (contracts.FallbackPlan, error)) (contracts.FallbackPlan, error) {
	e := contracts.AsError(cerr)
	outcome := contracts.FallbackIntervention
	if o, _ := e.Details["outcome"].(string); o == string(contracts.FallbackWaiting) {
		outcome = contracts.FallbackWaiting
	}
	var earliest *time.Time
	if v, _ := e.Details["earliest_reset"].(string); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			earliest = &t
		}
	}
	if fromReset != nil {
		// The limited account itself comes back at its reset: that is a wait,
		// not a dead end (unusable alternatives are still listed in reasons).
		outcome = contracts.FallbackWaiting
		if earliest == nil || fromReset.Before(*earliest) {
			t := *fromReset
			earliest = &t
		}
	}
	reasons, _ := e.Details["reasons"].([]string)
	reason := fmt.Sprintf("no eligible %s subscription; work preserved and resumable", provider)
	if earliest != nil {
		reason += "; earliest reset " + earliest.Format(time.RFC3339)
	}
	if len(reasons) > 0 {
		reason += " (" + strings.Join(reasons, "; ") + ")"
	}
	payload := ExhaustedPayload{Provider: provider, SessionID: plan.SessionID, FromRunID: plan.FromRunID,
		FromAccountID: plan.FromAccountID, Outcome: outcome, EarliestReset: earliest, Reasons: redactAll(reasons)}
	s.reportEvent(ctx, contracts.EvAccountExhausted, contracts.ObjectRef{Kind: contracts.KindSession, ID: plan.SessionID}, plan.SessionID, payload)
	return finish(outcome, reason)
}

func redactAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = redact(s)
	}
	return out
}

func (s *Service) reportEvent(ctx context.Context, kind contracts.EventKind, obj contracts.ObjectRef, sessionID string, payload any) {
	b, _ := json.Marshal(payload)
	s.report(ctx, contracts.ReportEvent, contracts.Event{
		EventID: contracts.NewID(contracts.PrefixEvent), Origin: "node:" + s.cfg.NodeID, Kind: kind, Object: obj,
		SessionID: sessionID, PayloadVersion: 1, Payload: b, OccurredAt: s.now(),
	})
}

// recordLimit stores the limit as a cooldown + observation, or classifies a
// login failure. Returns the account's expected reset (nil if none).
func (s *Service) recordLimit(ctx context.Context, accountID string, provider contracts.Provider, g grantRow, ev contracts.LimitEvent) *time.Time {
	now := s.now()
	switch ev.Kind {
	case LimitAuthInvalid:
		state, detail := s.classifyAuthFailure(ctx, g, ev.Detail)
		s.setGrantState(ctx, g, state, detail)
		o := contracts.AccountObservation{AccountID: accountID, Provider: provider, ReportingNode: s.cfg.NodeID, ObservedAt: now,
			Source: "limit-event", Confidence: "high", Eligibility: contracts.EligibleUnusable, Detail: redact(detail)}
		if putObservation(ctx, s.db, o) == nil {
			s.report(ctx, contracts.ReportAccountObs, o)
		}
		return nil
	default:
		until := now.Add(s.cfg.DefaultCooldown)
		if ev.ResetAt != nil && ev.ResetAt.After(now) {
			until = ev.ResetAt.UTC()
		}
		el := contracts.EligibleExhausted
		if ev.Kind == LimitRate {
			el = contracts.EligibleCooldown
		}
		_ = putCooldown(ctx, s.db, accountID, until, ev.Kind)
		prev, _ := getObservation(ctx, s.db, accountID)
		o := contracts.AccountObservation{AccountID: accountID, Provider: provider, ReportingNode: s.cfg.NodeID, ObservedAt: now,
			Source: "limit-event", Confidence: "high", Eligibility: el, ResetAt: &until, Detail: truncate(redact(ev.Kind+": "+ev.Detail), 300)}
		if prev != nil {
			o.Windows = prev.Windows
		}
		if putObservation(ctx, s.db, o) == nil {
			s.report(ctx, contracts.ReportAccountObs, o)
		}
		return &until
	}
}

var deadLoginHints = []string{"sign in again", "log out and sign in", "/login", "please log in", "please login", "invalid_grant",
	"refresh token", "token revoked", "oauth token has been revoked", "login required"}

// classifyAuthFailure decides dead vs expired from local signals: a blanked
// Claude store, a setup-token rejection, or the harness's own "sign in
// again" text are final (dead, no retries); anything else needs re-login
// review (expired). Neither is ever retried automatically.
func (s *Service) classifyAuthFailure(ctx context.Context, g grantRow, detail string) (contracts.GrantState, string) {
	low := strings.ToLower(detail)
	hint := false
	for _, h := range deadLoginHints {
		if strings.Contains(low, h) {
			hint = true
			break
		}
	}
	switch {
	case g.Kind == contracts.CredClaudeTokenFD:
		return contracts.GrantDead, "setup token rejected; re-login required"
	case g.Kind == contracts.CredOAuthToken:
		s.tok.drop(g.AccountID)
		return contracts.GrantDead, tokenSpecs[g.Harness].RejectHint
	case g.Harness == contracts.HarnessClaude:
		si := s.readClaudeStore(ctx, g)
		if si.Exists && !si.Present {
			return contracts.GrantDead, "credential store blanked by Claude (login rejected); re-login required"
		}
		if hint {
			return contracts.GrantDead, "Claude reported the login unusable; re-login required"
		}
	case g.Harness == contracts.HarnessCodex:
		if hint {
			return contracts.GrantDead, "Codex asked to sign in again; re-login required"
		}
	}
	return contracts.GrantExpired, "authentication failed; re-login may be required (not retried automatically)"
}

func (s *Service) setGrantState(ctx context.Context, g grantRow, state contracts.GrantState, detail string) {
	g.State, g.Detail, g.ObservedAt = state, detail, s.now()
	if err := updateGrantState(ctx, s.db, g); err != nil {
		s.log.Warn("accounts: grant state", "grant", g.GrantID, "err", err)
		return
	}
	s.log.Warn("accounts: grant unusable", "grant", g.GrantID, "account", g.AccountID, "state", string(state))
	s.report(ctx, contracts.ReportGrant, g.DeviceGrant)
}

func (s *Service) markDead(ctx context.Context, g grantRow, detail string) {
	s.setGrantState(ctx, g, contracts.GrantDead, detail)
}

// InjectLimit synthesizes a limit on the session's current run (test mode
// only) to exercise fallback without burning real quota.
func (s *Service) InjectLimit(ctx context.Context, p contracts.InjectLimitParams) (contracts.FallbackPlan, error) {
	if !s.cfg.TestMode {
		return contracts.FallbackPlan{}, contracts.Errorf(contracts.ErrForbidden, "account.inject_limit is available only in test mode")
	}
	if s.cfg.Controller == nil {
		return contracts.FallbackPlan{}, contracts.Errorf(contracts.ErrUnavailable, "no run controller configured")
	}
	info, err := s.cfg.Controller.CurrentRun(ctx, p.SessionID)
	if err != nil {
		return contracts.FallbackPlan{}, err
	}
	kind := p.Kind
	if kind == "" {
		kind = LimitUsage
	}
	if kind != LimitUsage && kind != LimitAuthInvalid && kind != LimitRate {
		return contracts.FallbackPlan{}, contracts.Errorf(contracts.ErrInvalid, "unknown limit kind %q", kind)
	}
	ev := contracts.LimitEvent{Kind: kind, Detail: "injected (test mode)", At: s.now()}
	if kind != LimitAuthInvalid {
		r := s.now().Add(s.cfg.InjectReset)
		ev.ResetAt = &r
	}
	return s.HandleLimitEvent(ctx, p.SessionID, info.RunID, ev)
}
