package daemon

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/resumenote"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// session.limit_resume (decision I70): the hub asks this node to continue a
// session that stopped on an account limit, once an account may have room.
// The hub decided the session may be touched (not archived, nobody gave it
// input since the stop); this node checks what only it knows and acts:
//
//   - the run ended (the limit fallback stopped it and found no account
//     with room): the account service starts a continuation on the best
//     eligible account (StartContinuation, as an account switch does),
//     or answers no_room with the earliest reset;
//   - the run is still up (its limit paused automatic switching): once
//     the limit hold has lapsed and its account is not known to be at a
//     limit, the session gets the standard continue prompt as a queued
//     message from itself, with a stable ID per run and limit, so a
//     repeated request never sends it twice; a run that is busy, attached
//     to a terminal or no longer the session's current run is refused.

func (d *Daemon) registerLimitResume() {
	d.RegisterAction(contracts.ActionSessionLimitResume, d.actionLimitResume)
}

// limitResumer is the account service's limit resume hook, if any.
func (d *Daemon) limitResumer() (LimitResumer, bool) {
	lr, ok := d.cfg.Accounts.(LimitResumer)
	return lr, ok
}

func (d *Daemon) actionLimitResume(ctx context.Context, raw json.RawMessage) (any, error) {
	p, err := decodeAction[contracts.LimitResumeParams](contracts.ActionSessionLimitResume, raw)
	if err != nil {
		return nil, err
	}
	if !contracts.ValidID(p.SessionID, contracts.PrefixSession) || !contracts.ValidID(p.RunID, contracts.PrefixRun) {
		return nil, contracts.Errorf(contracts.ErrInvalid, "session_id and run_id are required")
	}
	if p.Mode != contracts.LimitResumeContinue && p.Mode != contracts.LimitResumeInPlace {
		return nil, contracts.Errorf(contracts.ErrInvalid, "unknown mode %q", p.Mode)
	}
	lres, ok := d.limitResumer()
	if !ok {
		return nil, contracts.Errorf(contracts.ErrUnsupported, "this node has no account service to resume limited sessions")
	}
	d.limitResumeMu.Lock() // one at a time (quick but for a launch; the hub spaces them anyway)
	defer d.limitResumeMu.Unlock()
	refused := func(why string) (any, error) {
		d.cfg.Logger.Info("limit resume refused", "session", p.SessionID, "run", p.RunID, "why", why)
		return contracts.LimitResumeResult{Outcome: contracts.LimitResumeRefused, Detail: why}, nil
	}
	rec, ok, err := d.st.latestRunForSession(ctx, p.SessionID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return refused("the session has no run on this node")
	}
	if rec.Run.RunID != p.RunID {
		return refused("the session's current run is " + rec.Run.RunID)
	}
	if rec.Session.Archived {
		return refused("the session is archived")
	}
	live, isLive := d.getLive(p.RunID)
	if isLive {
		rec, _ = live.snapshot()
	}
	switch {
	case rec.Phase == phaseEnded && p.Mode == contracts.LimitResumeContinue:
		if rec.Run.AdapterData[contracts.AdapterDataAccountSwitch] != contracts.AccountSwitchLimit {
			return refused("the run was not stopped for an account limit")
		}
		res, err := lres.ResumeAfterLimit(ctx, p.SessionID, p.RunID)
		if err != nil {
			return nil, err
		}
		return res, nil
	case rec.Phase == phaseEnded:
		return refused("the run ended")
	case p.Mode == contracts.LimitResumeContinue:
		return refused("the run is still up")
	case !isLive || rec.Phase != phaseLive:
		return refused("the run is not under observation (" + rec.Phase + ")")
	}
	return d.limitResumeInPlace(ctx, live, lres, p)
}

// limitResumeInPlace sends a live limited run the continue prompt once its
// account has room.
func (d *Daemon) limitResumeInPlace(ctx context.Context, lr *liveRun, lres LimitResumer, p contracts.LimitResumeParams) (any, error) {
	rec, attached := lr.snapshot()
	lr.mu.Lock()
	until := lr.limitedUntil
	lr.mu.Unlock()
	r := rec.Run
	switch {
	case r.State != contracts.RunWaitingInput:
		return contracts.LimitResumeResult{Outcome: contracts.LimitResumeRefused, Detail: "the run is " + string(r.State)}, nil
	case attached:
		return contracts.LimitResumeResult{Outcome: contracts.LimitResumeRefused, Detail: "a terminal is attached to the run"}, nil
	case until.After(d.now()):
		u := until.UTC()
		return contracts.LimitResumeResult{Outcome: contracts.LimitResumeNoRoom, ResetAt: &u, Detail: "the run's account is at its limit until " + u.Format(time.RFC3339)}, nil
	}
	room, err := lres.LimitRoom(ctx, r.RunID)
	if err != nil {
		return nil, err
	}
	switch {
	case room.Refused:
		return contracts.LimitResumeResult{Outcome: contracts.LimitResumeRefused, Detail: room.Why}, nil
	case !room.OK:
		return contracts.LimitResumeResult{Outcome: contracts.LimitResumeNoRoom, AccountID: room.AccountID, ResetAt: room.ResetAt, Detail: room.Why}, nil
	}
	tok, ok, err := d.st.tokenForSession(ctx, r.SessionID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return contracts.LimitResumeResult{Outcome: contracts.LimitResumeRefused, Detail: "no session credential on this node"}, nil
	}
	id := limitResumeMessageID(r)
	self := contracts.Principal{Kind: contracts.PrincipalSession, ID: r.SessionID}
	sctx := WithBearer(contracts.WithPrincipal(ctx, self), tok)
	req := contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: "op_" + id[len("msg_"):]}, MessageID: id,
		Recipient: contracts.Address{Kind: contracts.AddrSession, ID: r.SessionID}, Mode: contracts.ModeQueue,
		Body: contracts.MessageBody{Text: room.Prompt}}
	if _, err := d.LocalAPI().SendMessage(sctx, req); err != nil {
		return nil, err
	}
	d.cfg.Logger.Info("limit resume: continue prompt sent", "session", r.SessionID, "run", r.RunID, "message", id)
	return contracts.LimitResumeResult{Outcome: contracts.LimitResumed, AccountID: room.AccountID, NewRunID: r.RunID,
		Kind: contracts.LimitResumeInPlace, Detail: room.Why}, nil
}

// limitResumeMessageID is the stable ID of the continue prompt for the
// run's latest limit: one per run and limit.
func limitResumeMessageID(r contracts.Run) string {
	at := "0"
	if t, err := sqlitex.ParseTime(r.AdapterData[contracts.AdapterDataLimitAt]); err == nil && !t.IsZero() {
		at = strconv.FormatInt(t.Unix(), 36)
	}
	return "msg_limitresume-" + resumenote.IDPart(r.RunID) + "-" + at
}
