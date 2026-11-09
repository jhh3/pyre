package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/persistence"
)

// Ended reports of runs stopped for an account switch (decision I67).
// pyred marks such a run (Run.AdapterData AdapterDataAccountSwitch) before
// it stops it and reports the fallback plan once the successor runs, so
// the hub words the end as a restart on the new account, with what the
// switch ended, never as a finished child that is about to be archived
// (ArchiveFinishedChildren skips it: fallback_plans has a row for it).

// switchPlanWait bounds how long an ended report of a switch stop waits
// for the node's fallback plan (it follows the successor's start, which
// normally takes seconds).
const switchPlanWait = 2 * time.Minute

// switchPlanRecheck spaces the checks meanwhile.
const switchPlanRecheck = 10 * time.Second

// switchStop is a run stopped for an account switch and the plan the node
// reported for it (nil until it arrives).
type switchStop struct {
	Kind string // contracts.AccountSwitch*
	Plan *contracts.FallbackPlan
}

// switchStopOf reports whether cr's run was stopped for an account switch.
func (h *Hub) switchStopOf(ctx context.Context, cr childReport) (switchStop, bool) {
	if cr.Kind != reportEnded {
		return switchStop{}, false
	}
	r, err := h.getRun(ctx, h.db, cr.RunID)
	if err != nil || r.AdapterData[contracts.AdapterDataAccountSwitch] == "" {
		return switchStop{}, false
	}
	sw := switchStop{Kind: r.AdapterData[contracts.AdapterDataAccountSwitch]}
	var raw string
	// A read error reads as "no plan yet": the report waits, then says so.
	if h.db.QueryRowContext(ctx, `SELECT plan FROM fallback_plans WHERE session_id=? AND from_run_id=? ORDER BY id DESC LIMIT 1`, cr.Child, cr.RunID).Scan(&raw) == nil {
		var p contracts.FallbackPlan
		if json.Unmarshal([]byte(raw), &p) == nil {
			sw.Plan = &p
		}
	}
	return sw, true
}

// switchStranded reports whether cr ends a run Pyre stopped for an account
// switch that did not continue the session (the plan says waiting or
// intervention). The parent cannot know why from the run's state alone
// ("exited / stopped by pyre"), so archiving the child does not cancel
// this report (iss_…: the parent saw two Codex
// children exit 20 s after launch, archived them, and never read why).
func (h *Hub) switchStranded(ctx context.Context, cr childReport) bool {
	sw, ok := h.switchStopOf(ctx, cr)
	if !ok || sw.Plan == nil {
		return false
	}
	return sw.Plan.Outcome != contracts.FallbackNativeResume && sw.Plan.Outcome != contracts.FallbackHandoff
}

// switchStopFailed: cr ends a run stopped for an account switch that
// needs someone to act (intervention). It is sent at once, not batched
// until the parent's turn ends; a wait for a reset is not urgent.
func (h *Hub) switchStopFailed(ctx context.Context, cr childReport) bool {
	sw, ok := h.switchStopOf(ctx, cr)
	return ok && sw.Plan != nil && sw.Plan.Outcome == contracts.FallbackIntervention
}

// deferForSwitchPlan holds an ended report of a switch stop until the
// node's plan arrives (at most switchPlanWait), so it can say where the
// session went. It reports whether it deferred cr.
func (h *Hub) deferForSwitchPlan(ctx context.Context, cr childReport) bool {
	sw, ok := h.switchStopOf(ctx, cr)
	if !ok || sw.Plan != nil {
		return false
	}
	if created := persistence.ParseTime(cr.CreatedAt); created.IsZero() || h.now().Sub(created) >= switchPlanWait {
		return false
	}
	_, _ = h.db.ExecContext(ctx, `UPDATE child_reports SET detail=?, not_before=?, updated_at=? WHERE report_id=?`,
		"deferred: waiting for the account switch's plan", persistence.Time(h.now().Add(switchPlanRecheck)), persistence.Time(h.now()), cr.ID)
	return true
}

// accountLabelOf is an account's display name (its ID when unknown).
func (h *Hub) accountLabelOf(ctx context.Context, id string) string {
	if a, err := h.getAccount(ctx, h.db, id); err == nil && a.Label != "" {
		return a.Label
	}
	return id
}

// renderSwitchEnded writes the ended report of a switch stop: the lead
// line, the facts and what the switch ended.
func (h *Hub) renderSwitchEnded(ctx context.Context, b *strings.Builder, sw switchStop, title, name, rel, facts string) {
	p := sw.Plan
	if p == nil {
		fmt.Fprintf(b, "%s was stopped for an account switch\n", title)
		fmt.Fprintf(b, "[pyre auto-report] Session %s, %s, was stopped by Pyre for an account switch; its node has not said where it went yet%s", name, rel, facts)
		b.WriteString("It is not finished and Pyre does not archive it. Check it with get_session; if it stays down, resume it.\n")
		return
	}
	if r, ok := h.harnessFallbackOf(ctx, p.SessionID, p.FromRunID); ok {
		h.renderHarnessFallbackEnded(ctx, b, p, r, title, name, rel, facts) // harnessfallback.go
		return
	}
	from, to := h.accountLabelOf(ctx, p.FromAccountID), h.accountLabelOf(ctx, p.ToAccountID)
	why := ""
	if p.Reason != "" {
		why = "Why: " + oneLineText(p.Reason, 300) + "\n"
	}
	switch {
	case (p.Outcome == contracts.FallbackNativeResume || p.Outcome == contracts.FallbackHandoff) && p.ToAccountID == p.FromAccountID:
		fmt.Fprintf(b, "%s restarted on its own account %q (the account switch did not complete)\n", title, to)
		fmt.Fprintf(b, "[pyre auto-report] Session %s, %s, was stopped for an account switch that did not complete and restarted on its own account %q%s", name, rel, to, facts)
	case p.Outcome == contracts.FallbackNativeResume || p.Outcome == contracts.FallbackHandoff:
		how := "same session, conversation kept"
		if p.Outcome == contracts.FallbackHandoff {
			how = "continued from a handoff note"
		}
		fmt.Fprintf(b, "%s restarted on account %q (account switch)\n", title, to)
		fmt.Fprintf(b, "[pyre auto-report] Session %s, %s, was restarted on account %q (account switch from %q; %s)%s", name, rel, to, from, how, facts)
	case p.Outcome == contracts.FallbackWaiting && sw.Kind == contracts.AccountSwitchLimit && planReset(p) != nil:
		// No account had room (iss_…): say which
		// limit stopped it and when an account has room again.
		at := resetPhrase(planReset(p)) // limitreport.go
		// Scheduled to resume by itself (limitresume.go, decision I70).
		if reset, ok := h.limitResumeArmed(ctx, p.SessionID, p.FromRunID); ok {
			at = resetPhrase(&reset)
			fmt.Fprintf(b, "%s stopped on %q's %s; no other account has room; resumes automatically after %s\n", title, from, limitNoun(p), at)
			fmt.Fprintf(b, "[pyre auto-report] Session %s, %s, was stopped on %q's %s and no other account has room%s", name, rel, from, limitNoun(p), facts)
			b.WriteString(why)
			fmt.Fprintf(b, "It is not finished and Pyre does not archive it. Pyre resumes it by itself after %s, or sooner when another account has room, unless it is archived or gets input first; you get one line when it resumes.\n", at)
			return
		}
		fmt.Fprintf(b, "%s stopped on %q's %s; no other account has room; resumable after %s\n", title, from, limitNoun(p), at)
		fmt.Fprintf(b, "[pyre auto-report] Session %s, %s, was stopped on %q's %s and no other account has room%s", name, rel, from, limitNoun(p), facts)
		b.WriteString(why)
		fmt.Fprintf(b, "It is not finished and Pyre does not archive it. Pyre does not restart it by itself: resume it after %s (an account has room again then), or tell the owner.\n", at)
		return
	default:
		fmt.Fprintf(b, "%s was stopped for an account switch that did not complete\n", title)
		fmt.Fprintf(b, "[pyre auto-report] Session %s, %s, was stopped for an account switch from %q that did not complete%s", name, rel, from, facts)
		b.WriteString(why)
		b.WriteString("It is not finished and Pyre does not archive it. Resume it when an account has room, or tell the owner.\n")
		return
	}
	b.WriteString("It is not finished and Pyre does not archive it.\n")
	if len(p.StoppedTasks) > 0 {
		fmt.Fprintf(b, "Lost: background tasks were stopped by the account switch (%s). The session was told to re-check its jobs; if it was waiting on one, make sure it restarted its watcher.\n",
			oneLineText(strings.Join(p.StoppedTasks, "; "), 300))
	}
	if p.UncertainWork {
		b.WriteString("Lost: its last step was cut off; it was told to check that step's effects before repeating anything.\n")
	}
	switch {
	case len(p.StoppedTasks) > 0 || p.UncertainWork:
	case sw.Kind == contracts.AccountSwitchProactive:
		b.WriteString("Nothing was lost: it was idle with no background tasks, and it waits for its next message on the new account.\n")
	default:
		b.WriteString("It was told to continue where it left off.\n")
	}
	b.WriteString(why)
}
