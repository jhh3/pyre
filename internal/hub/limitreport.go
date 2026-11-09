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

// Child reports of turns that failed on an account limit
// (iss_…, iss_…). A Claude
// turn that hits a limit ends with StopFailure and the run goes idle; the
// node then plans the fallback (a switch, a wait for a reset, or a paused
// switch). The idle report of such a turn waits briefly for that plan, so
// the parent reads "stopped on "mobot"'s spend limit" instead of a plain
// "finished: You've hit your individual spend limit", and a run that keeps
// failing on the same limit (its own background monitors wake it every few
// seconds) is reported once, not once per failed turn.

// limitPlanWait bounds how long the idle report of a failed turn waits for
// the node's fallback plan (the node plans within seconds of the
// observation that carried the failure).
const limitPlanWait = 45 * time.Second

// limitPlanRecheck spaces the checks meanwhile.
const limitPlanRecheck = 5 * time.Second

// limitReportMark is in the body of every limit report: a later failed
// turn on the same run is not reported again.
const limitReportMark = "is stopped on account"

// ownLoginLimitPrefix starts the reason of a plan for a run on a login
// Pyre does not manage (agy), which the node left running
// (accounts.OwnLoginLimitPrefix).
const ownLoginLimitPrefix = "own login limited"

// failedTurn reports whether a run state reason is a turn that failed
// (Claude's StopFailure hook).
func failedTurn(reason string) bool {
	return strings.HasPrefix(reason, "turn failed")
}

// latestPlanFor is the newest fallback plan the node reported for a run
// (nil when none).
func (h *Hub) latestPlanFor(ctx context.Context, sessionID, runID string) *contracts.FallbackPlan {
	var raw string
	if h.db.QueryRowContext(ctx, `SELECT plan FROM fallback_plans WHERE session_id=? AND from_run_id=? ORDER BY id DESC LIMIT 1`, sessionID, runID).Scan(&raw) != nil {
		return nil
	}
	var p contracts.FallbackPlan
	if json.Unmarshal([]byte(raw), &p) != nil {
		return nil
	}
	return &p
}

// limitPlanOf decides an idle report of a failed turn: the limit plan to
// word it from (nil: a plain report), whether it waits for the plan, or
// why it is not sent.
func (h *Hub) limitPlanOf(ctx context.Context, cr childReport) (plan *contracts.FallbackPlan, deferred bool, skipWhy string) {
	if cr.Kind != reportIdle || !failedTurn(cr.Reason) {
		return nil, false, ""
	}
	p := h.latestPlanFor(ctx, cr.Child, cr.RunID)
	if p == nil {
		if r, err := h.getRun(ctx, h.db, cr.RunID); err == nil && r.AdapterData[contracts.AdapterDataAccountSwitch] != "" {
			// Being switched: the run's ended report says where it went.
			return nil, false, "the run is being switched to another account; its restart report covers this turn"
		}
		created := persistence.ParseTime(cr.CreatedAt)
		if !created.IsZero() && h.now().Sub(created) < limitPlanWait {
			_, _ = h.db.ExecContext(ctx, `UPDATE child_reports SET detail=?, not_before=?, updated_at=? WHERE report_id=?`,
				"deferred: waiting for the node's limit plan", persistence.Time(h.now().Add(limitPlanRecheck)), persistence.Time(h.now()), cr.ID)
			return nil, true, ""
		}
		return nil, false, "" // no limit: a plain report
	}
	if strings.HasPrefix(p.Reason, "proactive move") {
		return nil, false, ""
	}
	switch p.Outcome {
	case contracts.FallbackNativeResume, contracts.FallbackHandoff:
		return nil, false, "the account switch restarted the session; its restart report covers this turn"
	}
	var n int
	_ = h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM child_reports WHERE child_session_id=? AND run_id=? AND report_id!=? AND status=? AND body LIKE ?`,
		cr.Child, cr.RunID, cr.ID, reportSent, "%"+limitReportMark+"%").Scan(&n)
	if n > 0 {
		return nil, false, "the run's account limit was already reported; this turn failed on it again"
	}
	return p, false, ""
}

// limitNoun names the limit a plan answers: "spend limit", "usage limit",
// "rate limit", ...
func limitNoun(p *contracts.FallbackPlan) string {
	if p == nil || p.Limit == nil {
		return "usage limit"
	}
	d := strings.ToLower(p.Limit.Detail)
	switch {
	case strings.Contains(d, "spend limit"):
		return "spend limit"
	case strings.Contains(d, "shared budget"):
		return "shared budget"
	case strings.Contains(d, "usage credits"), strings.Contains(d, "extra usage"):
		return "usage credits"
	case strings.Contains(d, "weekly limit"):
		return "weekly limit"
	case strings.Contains(d, "quota"): // agy: "Individual quota reached."
		return "quota"
	case strings.Contains(d, "session limit"):
		return "session limit"
	case p.Limit.Kind == "rate_limited":
		return "rate limit"
	}
	return "usage limit"
}

// planReset is when an account has room again for a plan: the earliest
// reset the node named, else the limited account's own reset.
func planReset(p *contracts.FallbackPlan) *time.Time {
	if p == nil {
		return nil
	}
	if _, rest, ok := strings.Cut(p.Reason, "earliest reset "); ok {
		f := strings.Fields(rest)
		if len(f) > 0 {
			if t, err := time.Parse(time.RFC3339, strings.TrimRight(f[0], ";,)")); err == nil {
				return &t
			}
		}
	}
	if p.Limit != nil && p.Limit.ResetAt != nil {
		t := *p.Limit.ResetAt
		return &t
	}
	return nil
}

// resetPhrase renders a reset for a report ("Sat Oct 24 6:00 PM PT").
func resetPhrase(t *time.Time) string {
	if t == nil {
		return "an unknown time"
	}
	return askClock(*t)
}

// renderLimitIdle writes the idle report of a turn that failed on an
// account limit the node did not switch away from.
func (h *Hub) renderLimitIdle(ctx context.Context, b *strings.Builder, p *contracts.FallbackPlan, title, name, rel, facts string) {
	acct := h.accountLabelOf(ctx, p.FromAccountID)
	if acct == "" {
		acct = "this machine's own login" // ambient: no Pyre account
	}
	noun := limitNoun(p)
	ownLogin := strings.HasPrefix(p.Reason, ownLoginLimitPrefix)
	var reset *time.Time
	if p.Limit != nil {
		reset = p.Limit.ResetAt
	}
	paused := strings.HasPrefix(p.Reason, "account switching paused")
	auto, armed := h.limitResumeArmed(ctx, p.SessionID, p.FromRunID) // limitresume.go (decision I70)
	switch {
	case paused && armed:
		fmt.Fprintf(b, "%s stopped on %q's %s; automatic account switching is paused; resumes automatically after %s\n", title, acct, noun, resetPhrase(&auto))
	case paused:
		fmt.Fprintf(b, "%s stopped on %q's %s; automatic account switching is paused\n", title, acct, noun)
	case p.Outcome == contracts.FallbackWaiting && armed:
		fmt.Fprintf(b, "%s stopped on %q's %s; no other account has room; resumes automatically after %s\n", title, acct, noun, resetPhrase(&auto))
	case p.Outcome == contracts.FallbackWaiting:
		fmt.Fprintf(b, "%s stopped on %q's %s; no other account has room; resumable after %s\n", title, acct, noun, resetPhrase(planReset(p)))
	default:
		fmt.Fprintf(b, "%s stopped on %q's %s\n", title, acct, noun)
	}
	fmt.Fprintf(b, "[pyre auto-report] Session %s, %s, %s %q: its turn failed on the account's %s (resets %s)%s", name, rel, limitReportMark, acct, noun, resetPhrase(reset), facts)
	switch {
	case paused && armed:
		b.WriteString("Pyre paused automatic account switching for it: " + oneLineText(strings.TrimPrefix(p.Reason, "account switching paused: "), 200) + ". It stays on that account and every turn there fails until the reset. It is not finished. ")
		fmt.Fprintf(b, "Pyre tells it to continue on that account after %s unless it gets input first; you get one line when it resumes. To move it sooner, resume it on another account.\n", resetPhrase(&auto))
	case paused:
		b.WriteString("Pyre paused automatic account switching for it: " + oneLineText(strings.TrimPrefix(p.Reason, "account switching paused: "), 200) + ". It stays on that account and every turn there fails until the reset. It is not finished. Resume it on another account, or tell the owner.\n")
	case p.Outcome == contracts.FallbackWaiting && armed:
		fmt.Fprintf(b, "No other account has room. It is not finished; Pyre resumes it by itself after %s, or sooner when another account has room, unless it is archived or gets input first; you get one line when it resumes.\n", resetPhrase(&auto))
	case p.Outcome == contracts.FallbackWaiting:
		fmt.Fprintf(b, "No other account has room. It is not finished; Pyre does not restart it by itself: resume it after %s.\n", resetPhrase(planReset(p)))
	case ownLogin:
		b.WriteString("It runs on this machine's own login, which Pyre does not manage, so Pyre neither stopped nor moved it: the run stays up, and every turn on that login fails until the reset. It is not finished; message it to continue after the reset, or tell the owner if it cannot wait.\n")
	default:
		b.WriteString("Pyre could not move it to another account: " + oneLineText(p.Reason, 300) + "\nIt is not finished. Tell the owner if you cannot help.\n")
	}
	if reset != nil {
		b.WriteString("Pyre holds its messages until the reset. ")
	}
	b.WriteString("This is reported once: further failed turns of this run are not reported.\n")
}
