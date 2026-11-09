package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/jhh3/pyre/internal/autotitle"
	"github.com/jhh3/pyre/internal/contracts"
)

// Harness fallback at runtime (iss_…).
//
// A Codex session started with fallback_harness claude carries
// TagFallbackClaude. When its run stops on a usage or rate limit and the
// node finds no other OpenAI account to move it to (a waiting plan, or the
// ambient own login, which is never switched), waiting for the reset would
// leave a review undone for days. Instead the plan arms a limit resume in
// harness fallback mode, due at once; once the run has ended (and nobody
// gave it input meanwhile) the hub starts a Claude session in its place,
// as the launch-time fallback does (cli harnessPreflight): same parent,
// node, folder and project, the stand-in title and prompt, a handoff note
// pointing at the Codex session's transcript, then the original task. The
// Codex model is dropped (Claude's default runs); an effort Claude offers
// is kept. The parent reads in the Codex session's report that Pyre
// restarts it on Claude, and one line with the new session once it starts.

// limitResumeHarness is the hub-only limit resume mode of a harness
// fallback (never sent to a node).
const limitResumeHarness = "harness_fallback"

// Per-launch model and effort args (daemon.ModelArg, daemon.EffortArg).
const (
	fallbackModelArg  = "--pyre-model"
	fallbackEffortArg = "--pyre-effort"
)

// harnessFallbackFor reports whether plan f of sess restarts it on Claude:
// a Codex session with TagFallbackClaude whose run the node stopped on a
// usage or rate limit with no other account. A paused switch (the run
// stays up, I69) and a run the node could not stop are not.
func harnessFallbackFor(sess contracts.Session, f contracts.FallbackPlan) bool {
	if sess.Archived || sess.Harness != contracts.HarnessCodex || !slices.Contains(sess.Tags, contracts.TagFallbackClaude) {
		return false
	}
	if f.Limit == nil || (f.Limit.Kind != "usage_limit" && f.Limit.Kind != "rate_limited") {
		return false
	}
	switch f.Outcome {
	case contracts.FallbackWaiting:
		return true
	case contracts.FallbackIntervention:
		return !strings.HasPrefix(f.Reason, "account switching paused") && !strings.HasPrefix(f.Reason, ownLoginLimitPrefix) &&
			!strings.HasPrefix(f.Reason, "could not quiesce")
	}
	return false
}

// armHarnessFallbackTx schedules the restart on Claude (due now).
func (h *Hub) armHarnessFallbackTx(ctx context.Context, tx *sql.Tx, nodeID string, f contracts.FallbackPlan) error {
	now := h.now()
	stopped := f.PlannedAt
	if !f.Limit.At.IsZero() && f.Limit.At.Before(stopped) {
		stopped = f.Limit.At
	}
	reset := now
	if t := planReset(&f); t != nil {
		reset = t.UTC()
	} else if f.Limit.ResetAt != nil {
		reset = f.Limit.ResetAt.UTC()
	}
	r := limitResume{SessionID: f.SessionID, NodeID: nodeID, RunID: f.FromRunID, Mode: limitResumeHarness, AccountID: f.FromAccountID,
		Provider: string(contracts.ProviderOpenAI), LimitKind: f.Limit.Kind, LimitDetail: oneLineText(f.Limit.Detail, 200), StoppedAt: stopped,
		ResetAt: reset, DueAt: now, State: lrScheduled, Detail: "restarts on Claude (fallback_harness claude)"}
	if err := h.putLimitResumeTx(ctx, tx, r); err != nil {
		return err
	}
	h.lres.kick.Store(true)
	return nil
}

// fireHarnessFallback starts the Claude session in place of r's Codex
// session (the run has ended and the hub's checks passed). It reports
// whether it started one.
func (h *Hub) fireHarnessFallback(ctx context.Context, s contracts.Session, run contracts.Run, r limitResume) (bool, error) {
	sc, err := h.firstStartCommand(ctx, s.SessionID)
	if err != nil {
		return false, h.cancelLimitResume(ctx, r, "its launch command is gone, so the task cannot be restarted on Claude")
	}
	ni, err := h.nodeInfo(ctx, r.NodeID)
	if err != nil {
		return false, err
	}
	hi, ok := ni.harness(contracts.HarnessClaude)
	if !ok || !hi.Available {
		return false, h.cancelLimitResume(ctx, r, "Claude is not available on its node")
	}
	acct := h.accountLabelOf(ctx, r.AccountID)
	if acct == "" {
		acct = "this machine's own OpenAI login"
	}
	noun := limitResumeNoun(r)
	why := fmt.Sprintf("Codex session %s (%q) stopped on %q's %s and no other Codex (OpenAI) account had room", s.SessionID, s.Title, acct, noun)
	note := fmt.Sprintf("It may have done part of the work before it stopped: read its transcript (pyre read %s) and pick up from there instead of starting over; "+
		"check the effects of its last step before repeating it. Its folder is %s.\n\n", s.SessionID, run.Cwd)
	args, dropped := harnessFallbackArgs(sc.Args, hi)
	if dropped != "" {
		note += fmt.Sprintf("[pyre] The %s requested for codex does not apply to claude, so this session uses claude's default.\n\n", dropped)
	}
	tags := []string{}
	for _, t := range s.Tags {
		switch t {
		case contracts.TagFallbackClaude, contracts.TagReportParent, contracts.TagReportNone, autotitle.TagAuto:
			continue
		}
		tags = append(tags, t)
	}
	tags = append(tags, contracts.TagCodexUnavailable)
	project := ""
	if s.ProjectID != nil {
		project = *s.ProjectID
	}
	req := contracts.StartSessionRequest{Mutation: contracts.Mutation{OperationID: "op_harness-fallback-" + r.RunID},
		NodeID: r.NodeID, Harness: contracts.HarnessClaude, Title: contracts.HarnessFallbackTitle(s.Title), Cwd: run.Cwd,
		InitialPrompt: contracts.HarnessFallbackPrompt(why, note+sc.InitialPrompt), Tags: tags, ProjectID: &project,
		AccountPolicy: contracts.AccountPolicy{Mode: contracts.AccountEligible}, Args: args,
		ParentSessionID: s.Lineage.ParentSessionID}
	if slices.Contains(s.Tags, contracts.TagReportNone) {
		off := false
		req.ReportToParent = &off
	}
	by := s.CreatedBy
	if s.Lineage.ParentSessionID != "" {
		by = contracts.Principal{Kind: contracts.PrincipalSession, ID: s.Lineage.ParentSessionID}
	}
	res, err := h.StartSession(contracts.WithPrincipal(ctx, by), req)
	if err != nil {
		e := contracts.AsError(err)
		if e != nil && (e.Code == contracts.ErrUnavailable || e.Code == contracts.ErrCapacityExhausted) {
			return false, h.tx(ctx, func(tx *sql.Tx) error {
				cur, ok, err := h.getLimitResume(ctx, tx, r.SessionID)
				if err != nil || !ok || cur.State != lrScheduled || cur.RunID != r.RunID {
					return err
				}
				return h.retryLimitResumeTx(ctx, tx, cur, h.now().Add(limitResumeMinRetry), "the Claude session could not start yet: "+oneLineText(err.Error(), 200))
			})
		}
		return false, h.cancelLimitResume(ctx, r, "the Claude session could not start: "+oneLineText(err.Error(), 200))
	}
	newSession, newRun := res.Session.Session.SessionID, res.Session.Session.CurrentRunID
	return true, h.tx(ctx, func(tx *sql.Tx) error {
		cur, ok, err := h.getLimitResume(ctx, tx, r.SessionID)
		if err != nil || !ok || cur.RunID != r.RunID {
			return err
		}
		now := h.now()
		cur.State, cur.CommandID, cur.SentAt, cur.ResumedRun, cur.ResumedKind, cur.ResumedAt = lrDone, "", &now, newRun, limitResumeHarness, &now
		cur.Detail = "restarted on Claude as session " + newSession
		if err := h.putLimitResumeTx(ctx, tx, cur); err != nil {
			return err
		}
		h.auditTx(ctx, tx, auditRec{Action: contracts.AuditSessionLimitResumed, Automatic: true, Actor: pyreActor, NodeID: r.NodeID, SessionID: r.SessionID,
			RunID: newRun, To: fmt.Sprintf("on Claude as session %s (fallback_harness claude)", newSession), Reason: fmt.Sprintf("after %q's %s with no other OpenAI account", acct, noun)})
		return h.queueLimitReportTx(ctx, tx, r.SessionID, r.RunID, "limit_resumed:harness:"+r.RunID, now)
	})
}

// harnessFallbackArgs are a Codex launch's args for Claude: the model is
// dropped, an effort Claude does not offer too (named in dropped).
func harnessFallbackArgs(args []string, claude contracts.HarnessInfo) (out []string, dropped string) {
	var gone []string
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, fallbackModelArg+"="):
			gone = append(gone, "model "+strings.TrimPrefix(a, fallbackModelArg+"="))
			continue
		case strings.HasPrefix(a, fallbackEffortArg+"="):
			if e := strings.TrimPrefix(a, fallbackEffortArg+"="); !slices.Contains(claude.Efforts, e) {
				gone = append(gone, "effort "+e)
				continue
			}
		}
		out = append(out, a)
	}
	return out, strings.Join(gone, " and ")
}

// firstStartCommand is the launch command of a session's first run (its
// task as given).
func (h *Hub) firstStartCommand(ctx context.Context, sessionID string) (contracts.StartRunCommand, error) {
	var payload string
	var sc contracts.StartRunCommand
	err := h.db.QueryRowContext(ctx, `SELECT c.payload FROM runs r JOIN commands c ON c.command_id=r.start_command_id
		WHERE r.session_id=? AND c.kind=? ORDER BY r.started_at, r.run_id LIMIT 1`, sessionID, string(contracts.CmdStartRun)).Scan(&payload)
	if err != nil {
		return sc, err
	}
	return sc, json.Unmarshal([]byte(payload), &sc)
}

// harnessFallbackOf is the harness fallback armed for a session's run, if
// any (not dropped).
func (h *Hub) harnessFallbackOf(ctx context.Context, sessionID, runID string) (limitResume, bool) {
	r, ok, err := h.getLimitResume(ctx, h.db, sessionID)
	if err != nil || !ok || r.Mode != limitResumeHarness || r.RunID != runID || r.State == lrCancelled || r.State == lrGaveUp {
		return limitResume{}, false
	}
	return r, true
}

// renderHarnessFallbackEnded writes the ended report of a Codex run Pyre
// restarts (or restarted) on Claude.
func (h *Hub) renderHarnessFallbackEnded(ctx context.Context, b *strings.Builder, p *contracts.FallbackPlan, r limitResume, title, name, rel, facts string) {
	from := h.accountLabelOf(ctx, p.FromAccountID)
	if from == "" {
		from = "this machine's own login"
	}
	noun := limitNoun(p)
	if r.State == lrDone && r.ResumedRun != "" {
		sid := r.ResumedRun
		if nr, err := h.getRun(ctx, h.db, r.ResumedRun); err == nil {
			sid = nr.SessionID
		}
		fmt.Fprintf(b, "%s stopped on %q's %s; restarted on Claude as session %s\n", title, from, noun, sid)
		fmt.Fprintf(b, "[pyre auto-report] Session %s, %s, was stopped on %q's %s and no other OpenAI account has room%s", name, rel, from, noun, facts)
		fmt.Fprintf(b, "It was started with fallback_harness claude, so Pyre started Claude session %s in its place with the same task; it reports to you like any child. This Codex session is not resumed.\n", sid)
		return
	}
	fmt.Fprintf(b, "%s stopped on %q's %s; no other OpenAI account has room, so Pyre restarts it on Claude\n", title, from, noun)
	fmt.Fprintf(b, "[pyre auto-report] Session %s, %s, was stopped on %q's %s and no other OpenAI account has room%s", name, rel, from, noun, facts)
	b.WriteString("It was started with fallback_harness claude, so Pyre starts a Claude session in its place with the same task; you get one line with its ID, and it reports to you like any child. This Codex session is not resumed.\n")
}

// renderHarnessFallbackResumed writes the one line once the Claude session
// started.
func (h *Hub) renderHarnessFallbackResumed(ctx context.Context, b *strings.Builder, r limitResume, title, name, rel, facts string) {
	sid, stitle := r.ResumedRun, ""
	if nr, err := h.getRun(ctx, h.db, r.ResumedRun); err == nil {
		sid = nr.SessionID
		if s, err := h.getSession(ctx, h.db, sid); err == nil {
			stitle = fmt.Sprintf(" (%q)", s.Title)
		}
	}
	fmt.Fprintf(b, "%s restarted on Claude as session %s\n", title, sid)
	fmt.Fprintf(b, "[pyre auto-report] Session %s, %s, stopped on %q's %s with no other OpenAI account free; Pyre started Claude session %s%s in its place with the same task (fallback_harness claude)%s",
		name, rel, h.accountLabelOf(ctx, r.AccountID), limitResumeNoun(r), sid, stitle, facts)
	b.WriteString("The new session reports to you when it finishes; this Codex session is not resumed.\n")
}
