package hub

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/persistence"
	"github.com/jhh3/pyre/internal/resumenote"
)

// Interrupted child runs (issue iss_…: devbox
// rebooted for an OS upgrade; pyred resumed its Claude children by itself,
// and their lead heard nothing it could use: the children's "has exited"
// reports waited behind the lead's long turn, said nothing of the resume,
// and the lead's list showed the resumed runs simply "running").
//
// pyred marks a run whose process did not survive its restart
// (contracts.AdapterDataInterrupted*: the cause, "machine restarted" when
// the machine booted after the run started, else "process gone"; the boot
// and death times; and what Pyre did). For a child that reports to its
// parent the hub sends one "interrupted" report in place of the ended
// report: the machine, the time, the cause, what Pyre did (resumed it as a
// new run, restarted it in place, or left it exited and why), and that
// in-flight tool calls and background jobs were lost. It is never batched
// behind a busy parent. It waits for pyred's decision (recorded on the dead
// run right after its crash resume launches or is skipped) up to
// interruptSettle; after that it is sent with the hub's own view (a run
// that continues the dead one is the resume).
//
// A run its adapter brought back after a reboot (Codex reloads its thread)
// is marked too, once per boot, and reported the same way.

// interruptSettle bounds how long an interrupted report waits for pyred
// to say whether it resumed the session.
const interruptSettle = 2 * time.Minute

// interruptKey is the report's dedupe key: one per run and boot (a run
// restarted in place can be interrupted by a later reboot again).
func interruptKey(r contracts.Run) string {
	return "interrupted:" + r.RunID + ":" + r.AdapterData[contracts.AdapterDataInterruptedBootAt]
}

// interruptedEnd: r ended because pyred found its process gone. A run
// restarted in place keeps its marks (resumed as itself) while it runs on;
// its later end is an ordinary one.
func interruptedEnd(r contracts.Run) bool {
	return r.AdapterData[contracts.AdapterDataInterrupted] != "" && r.AdapterData[contracts.AdapterDataInterruptedResumedAs] != r.RunID
}

// interruptDecided: pyred has said what it did about the interruption.
func interruptDecided(r contracts.Run) bool {
	switch r.AdapterData[contracts.AdapterDataInterruptedOutcome] {
	case contracts.InterruptResumed, contracts.InterruptLeftExited:
		return true
	}
	return false
}

// interruptDue is when an interrupted report of r may go out.
func interruptDue(r contracts.Run, now time.Time) time.Time {
	if interruptDecided(r) {
		return now
	}
	return now.Add(interruptSettle)
}

// trackInterruptTx handles the interrupted marks trackChildTx does not: a
// dead run's later decision (pyred re-reports the ended run with its
// outcome: the waiting report goes out now), and a live run its adapter
// restarted after a reboot (a new report, once per boot). The ended
// transition itself is trackChildTx's. Runs inside the observation's
// transaction.
func (h *Hub) trackInterruptTx(ctx context.Context, tx *sql.Tx, old contracts.Run, isNew bool, r contracts.Run) error {
	cause := r.AdapterData[contracts.AdapterDataInterrupted]
	if cause == "" {
		return nil
	}
	now := h.now()
	key := interruptKey(r)
	if r.State.Terminal() {
		if isNew || !old.State.Terminal() || !interruptDecided(r) || !interruptedEnd(r) {
			return nil
		}
		res, err := tx.ExecContext(ctx, `UPDATE child_reports SET not_before=?, updated_at=? WHERE child_session_id=? AND dedupe_key=? AND status=? AND send_op IS NULL AND not_before>?`,
			persistence.Time(now), persistence.Time(now), r.SessionID, key, reportQueued, persistence.Time(now))
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			h.reportKick.Store(true)
		}
		return nil
	}
	if r.AdapterData[contracts.AdapterDataInterruptedResumedAs] != r.RunID {
		return nil // only a run restarted in place runs on with the marks
	}
	if !isNew && interruptKey(old) == key && old.AdapterData[contracts.AdapterDataInterrupted] == cause {
		return nil // this boot's restart is already counted
	}
	sess, err := h.getSession(ctx, tx, r.SessionID)
	if err != nil {
		if contracts.IsCode(err, contracts.ErrNotFound) {
			return nil
		}
		return err
	}
	if !reportsToParent(sess) || (sess.CurrentRunID != "" && sess.CurrentRunID != r.RunID) {
		return nil
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO child_reports(child_session_id,parent_session_id,run_id,kind,dedupe_key,run_state,state_reason,created_at,not_before,status,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(child_session_id,dedupe_key) DO NOTHING`,
		sess.SessionID, sess.Lineage.ParentSessionID, r.RunID, reportInterrupted, key, string(r.State), persistence.NullStr(r.StateReason),
		persistence.Time(now), persistence.Time(interruptDue(r, now)), reportQueued, persistence.Time(now))
	if err == nil {
		h.reportKick.Store(true)
	}
	return err
}

// interruptOutcome is what Pyre did about r's interruption: pyred's
// record, else the hub's own view (a run continuing r is its resume);
// outcome "" when nothing is known.
func (h *Hub) interruptOutcome(ctx context.Context, child contracts.Session, r contracts.Run) (outcome, detail, resumedAs string) {
	ad := r.AdapterData
	outcome, detail, resumedAs = ad[contracts.AdapterDataInterruptedOutcome], ad[contracts.AdapterDataInterruptedDetail], ad[contracts.AdapterDataInterruptedResumedAs]
	if interruptDecided(r) {
		return outcome, detail, resumedAs
	}
	if cur := child.CurrentRunID; cur != "" && cur != r.RunID {
		if nr, err := h.getRun(ctx, h.db, cur); err == nil && nr.PreviousRunID == r.RunID {
			return contracts.InterruptResumed, "", cur
		}
	}
	return "", "", ""
}

// renderInterrupted writes an interrupted report (renderChildReport).
func (h *Hub) renderInterrupted(ctx context.Context, b *strings.Builder, child contracts.Session, cr childReport, title, name, rel, machine, facts string) {
	r, err := h.getRun(ctx, h.db, cr.RunID)
	if err != nil {
		r = contracts.Run{RunID: cr.RunID}
	}
	ad := r.AdapterData
	parse := func(k string) time.Time { return persistence.ParseTime(ad[k]) }
	boot, died := parse(contracts.AdapterDataInterruptedBootAt), parse(contracts.AdapterDataInterruptedDiedAt)
	if died.IsZero() && r.EndedAt != nil {
		died = *r.EndedAt
	}
	where := orUnknown(machine)
	restarted := ad[contracts.AdapterDataInterrupted] == contracts.InterruptMachineRestarted
	outcome, detail, resumedAs := h.interruptOutcome(ctx, child, r)
	inPlace := outcome == contracts.InterruptResumed && resumedAs == r.RunID

	cause, short := "process gone", "its process on "+where+" died"
	if restarted {
		cause, short = "machine restarted", where+" restarted"
	}
	did := "Pyre has not resumed it"
	switch {
	case inPlace:
		did = "Pyre restarted it in place"
	case outcome == contracts.InterruptResumed:
		did = "Pyre resumed it"
	case outcome == contracts.InterruptLeftExited:
		did = "left exited"
	}
	fmt.Fprintf(b, "%s was interrupted: %s; %s\n", title, short, did)

	fmt.Fprintf(b, "[pyre auto-report] Session %s, %s, was interrupted: its process on %s did not survive", name, rel, where)
	if !died.IsZero() {
		fmt.Fprintf(b, " (last seen alive at about %s)", resumenote.PT(died))
	}
	fmt.Fprintf(b, ". Cause: %s", cause)
	switch {
	case restarted && !boot.IsZero():
		fmt.Fprintf(b, " (it booted at %s, after the run started at %s)", resumenote.PT(boot), resumenote.PT(r.StartedAt))
	case !restarted:
		b.WriteString(" (the machine did not restart: a crash, an OOM kill or a killed terminal server)")
	}
	b.WriteString(facts)

	switch {
	case inPlace:
		fmt.Fprintf(b, "What Pyre did: restarted it in place (same run %s, same conversation); Pyre tells it what happened, and nothing was replayed.\n", r.RunID)
	case outcome == contracts.InterruptResumed:
		fmt.Fprintf(b, "What Pyre did: resumed it as a new run %s in the same conversation; Pyre tells it what happened once it is up, and the interrupted instruction was not replayed.\n", orUnknown(resumedAs))
	case outcome == contracts.InterruptLeftExited:
		b.WriteString("What Pyre did: left it exited")
		if detail != "" {
			fmt.Fprintf(b, " (%s)", oneLineText(detail, 200))
		}
		b.WriteString(". If its work still matters, resume it (it continues the same conversation) or start a new session.\n")
	default:
		fmt.Fprintf(b, "What Pyre did: nothing yet; pyred had not resumed it %s after finding it gone. Check list_sessions before relying on it.\n", interruptSettle)
	}
	b.WriteString("Lost: tool calls that were in flight and its background jobs (watchers, Monitors, dev servers, tunnels, shells). Work it was in the middle of may be half done: check its state (read_session, git, PRs) before relying on it.\n")
	text, terr := h.childLastMessage(ctx, cr)
	writeSummary(b, text, "", terr)
}
