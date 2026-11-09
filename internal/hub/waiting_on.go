package hub

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/persistence"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// Waiting on own job (contracts/waiting_on.go): a session about to end its
// turn while a CI run, review or watcher it started is still pending
// records what it waits on, for the turn it is in. When that turn ends:
//
//   - a child's report is kind "waiting" instead of idle (trackChildTx),
//     and a "waiting_overdue" report is scheduled for Until: if the child
//     has not started another turn by then, its parent hears that the job
//     (or the watcher that should wake it) looks stalled;
//   - a top-level session's turn end is not a "waiting on you" check
//     (trackLeadTx);
//   - with quiet, a child's turn end is reported only when it says
//     something new (quietTurnSkip): its waiting report goes out with the
//     turn's last message and says the child may continue on its own, and
//     is recorded as skipped when the child messaged the parent itself as
//     the turn ended or the message is the one its previous report
//     carried (reportQuietDetail). A lead waiting on its own workers, CI or
//     a review ends its turn instead of polling its inbox. The
//     waiting_overdue report still goes out, and a run that exits is
//     reported as usual.
//
// The marker names its turn (child_turns.turn_seq), so it never leaks into
// a later turn. A turn with a marker ends when the run goes idle, including
// waiting_on_background (waitEndsTurnTx): the job the session waits on is
// usually that background task, and the turn its completion starts is a
// new one. A later turn that ends without a marker is reported as usual,
// and the earlier wait's overdue report is dropped.

var waitingOnMigrations = []sqlitex.Migration{{Namespace: "hub_waiting_on", Version: 1, Name: "session_waits", SQL: `
CREATE TABLE session_waits (
	session_id TEXT PRIMARY KEY,
	turn_seq INTEGER NOT NULL,
	what TEXT NOT NULL,
	until TEXT NOT NULL,
	set_at TEXT NOT NULL
);
`}, {Namespace: "hub_waiting_on", Version: 2, Name: "session_waits_quiet", SQL: `
ALTER TABLE session_waits ADD COLUMN quiet INTEGER NOT NULL DEFAULT 0;
`}, {Namespace: "hub_waiting_on", Version: 3, Name: "session_waits_woke", SQL: `
ALTER TABLE session_waits ADD COLUMN woke_at TEXT;
`}}

var _ contracts.WaitingOnAPI = (*Hub)(nil)

// sessionWait is a recorded marker.
type sessionWait struct {
	Turn  int64
	What  string
	Until time.Time
	Quiet bool // the turn's end is reported only with a new last message (quietTurnSkip)
}

// SetWaitingOn implements sessions.waiting_on (the calling session only).
func (h *Hub) SetWaitingOn(ctx context.Context, req contracts.SetWaitingOnRequest) (contracts.SetWaitingOnResponse, error) {
	var out contracts.SetWaitingOnResponse
	p, err := principal(ctx)
	if err != nil {
		return out, err
	}
	if p.Kind != contracts.PrincipalSession {
		return out, contracts.Errorf(contracts.ErrForbidden, "only a session says what it is waiting on (for itself)")
	}
	if err := requireOp(req.Mutation); err != nil {
		return out, err
	}
	what := strings.Join(strings.Fields(req.What), " ")
	if !req.Clear && what == "" {
		return out, contracts.Errorf(contracts.ErrInvalid, "what is required: the job you wait on, e.g. \"CI on PR #12 https://github.com/...\"")
	}
	if n := utf8.RuneCountInString(what); n > contracts.WaitingOnMaxWhat {
		return out, contracts.Errorf(contracts.ErrInvalid, "what is %d characters; limit %d", n, contracts.WaitingOnMaxWhat)
	}
	expect := contracts.WaitingOnDefault
	if req.ExpectMinutes < 0 || time.Duration(req.ExpectMinutes)*time.Minute > contracts.WaitingOnMax {
		return out, contracts.Errorf(contracts.ErrInvalid, "expect_minutes must be between 0 (default %d) and %d", int(contracts.WaitingOnDefault/time.Minute), int(contracts.WaitingOnMax/time.Minute))
	}
	if req.ExpectMinutes > 0 {
		expect = time.Duration(req.ExpectMinutes) * time.Minute
	}
	err = h.mutate(ctx, req.OperationID, contracts.OpSessionsWaitingOn, p, req, &out, func(tx *sql.Tx) (any, error) {
		sess, err := h.getSession(ctx, tx, p.ID)
		if err != nil {
			return nil, err
		}
		if req.Clear {
			if _, err := tx.ExecContext(ctx, `DELETE FROM session_waits WHERE session_id=?`, sess.SessionID); err != nil {
				return nil, err
			}
			return contracts.SetWaitingOnResponse{SessionID: sess.SessionID, Detail: "cleared: this turn's end is reported as usual"}, nil
		}
		var turn, closed int64
		err = tx.QueryRowContext(ctx, `SELECT turn_seq, closed_seq FROM child_turns WHERE session_id=?`, sess.SessionID).Scan(&turn, &closed)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if turn <= closed { // the turn's start was not observed yet
			turn = closed + 1
		}
		now := h.now()
		until := now.Add(expect)
		if _, err := tx.ExecContext(ctx, `INSERT INTO session_waits(session_id,turn_seq,what,until,set_at,quiet) VALUES(?,?,?,?,?,?)
			ON CONFLICT(session_id) DO UPDATE SET turn_seq=excluded.turn_seq, what=excluded.what, until=excluded.until, set_at=excluded.set_at, quiet=excluded.quiet, woke_at=NULL`,
			sess.SessionID, turn, what, persistence.Time(until), persistence.Time(now), req.Quiet); err != nil {
			return nil, err
		}
		out := contracts.SetWaitingOnResponse{SessionID: sess.SessionID, What: what, Until: &until, Quiet: req.Quiet}
		switch {
		case req.Quiet && sess.Parent() != "":
			out.Detail = fmt.Sprintf("recorded for this turn (quiet): when it ends, your parent session gets your last message only if it is new, marked as still waiting (no reply needed). End your turn; a watcher, a child's report or a message wakes you. If no new turn starts by %s, Pyre wakes you to check on %s, and your parent is told it looks stalled",
				askClock(until), what)
		default:
			who := "your parent session hears"
			if sess.Parent() == "" {
				who = "the owner is not told you are waiting on them"
			}
			out.Detail = fmt.Sprintf("recorded for this turn: when it ends, %s that you are waiting on %s (expected by %s), not idle. Make sure a watcher or background task will wake you; if no new turn starts by then, Pyre wakes you to check on it (and a parent session is told it looks stalled)",
				who, what, askClock(until))
		}
		return out, nil
	})
	return out, err
}

// waitForTurnTx returns the session's marker for turn, if any.
func (h *Hub) waitForTurnTx(ctx context.Context, q queryer, sessionID string, turn int64) (sessionWait, bool) {
	var w sessionWait
	var until string
	err := q.QueryRowContext(ctx, `SELECT turn_seq, what, until, quiet FROM session_waits WHERE session_id=? AND turn_seq=?`, sessionID, turn).Scan(&w.Turn, &w.What, &until, &w.Quiet)
	if err != nil {
		return w, false
	}
	w.Until = persistence.ParseTime(until)
	return w, true
}

// waitCarryTurn bounds the turn a wait carries over (carryWaitTx): Pyre
// started it to say a background command finished and the session ended
// it this soon (the rc146 lead answered its wake in 5 s).
const waitCarryTurn = 2 * time.Minute

// carryWaitTx gives a turn that ended without a marker of its own the
// previous turn's wait, when Pyre started this turn only to say a
// background command finished (a node.automation background_wake since
// the previous turn ended) and the session ended it within waitCarryTurn
// of that wake, before the wait was due. The session did not stop waiting: it answered
// the notice and ended its turn. Without this, that short turn dropped the
// wait, its overdue report and its wake (iss_…:
// a lead set a quiet 15-minute wait on its worker, was woken a second
// later for a finished script, and nothing woke it again). A longer turn,
// or one a message, a report or the owner started, ends the wait as
// before. The marker moves to turn; prevEnd is when the previous turn
// ended.
func (h *Hub) carryWaitTx(ctx context.Context, tx *sql.Tx, sessionID string, turn int64, prevEnd sql.NullString, now time.Time) (sessionWait, bool) {
	if !prevEnd.Valid {
		return sessionWait{}, false
	}
	w, ok := h.waitForTurnTx(ctx, tx, sessionID, turn-1)
	if !ok || !w.Until.After(now) {
		return sessionWait{}, false
	}
	var woke sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT MIN(ingested_at) FROM events WHERE session_id=? AND kind=? AND ingested_at>=? AND json_extract(payload, '$.action')=?`,
		sessionID, string(contracts.EvNodeAutomation), prevEnd.String, contracts.AutomationBackgroundWake).Scan(&woke); err != nil || !woke.Valid ||
		now.Sub(persistence.ParseTime(woke.String)) > waitCarryTurn {
		return sessionWait{}, false
	}
	if _, err := tx.ExecContext(ctx, `UPDATE session_waits SET turn_seq=? WHERE session_id=? AND turn_seq=?`, turn, sessionID, turn-1); err != nil {
		return sessionWait{}, false
	}
	w.Turn = turn
	return w, true
}

// waitForTurnOrCarryTx is the wait a turn ending now ends on: its own
// marker, or the previous turn's carried over (carryWaitTx). prevEnd is
// when the previous turn ended (the turn's start in child_turns).
func (h *Hub) waitForTurnOrCarryTx(ctx context.Context, tx *sql.Tx, sessionID string, turn int64, prevEnd sql.NullString, now time.Time) (sessionWait, bool) {
	if w, ok := h.waitForTurnTx(ctx, tx, sessionID, turn); ok {
		if !h.promptSinceWaitTx(ctx, tx, sessionID) {
			return w, true
		}
		// Its turn ended unseen: no wake or overdue report for it.
		_, _ = tx.ExecContext(ctx, `DELETE FROM session_waits WHERE session_id=? AND turn_seq=?`, sessionID, turn)
		return sessionWait{}, false
	}
	return h.carryWaitTx(ctx, tx, sessionID, turn, prevEnd, now)
}

// promptSinceWaitTx reports whether the session took a new prompt after it
// set its marker: the turn the marker names ended and another began, even
// though the hub never saw the run idle in between (both fell between two
// observations). The marker's turn is over, so the turn ending now is
// reported as usual and the marker is dropped (iss_…: a child set a quiet
// wait and ended its turn; a background task's notification started the
// next turn 2 s later, the hub saw running throughout, and the child's
// final result six minutes on was skipped as that quiet wait's turn end).
func (h *Hub) promptSinceWaitTx(ctx context.Context, q queryer, sessionID string) bool {
	var n int
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE session_id=? AND kind=? AND ingested_at>(SELECT set_at FROM session_waits WHERE session_id=?)
		AND json_extract(payload, '$.state')=? AND json_extract(payload, '$.state_reason')=?`,
		sessionID, string(contracts.EvRunObserved), sessionID, string(contracts.RunRunning), contracts.RunReasonPromptSubmitted).Scan(&n)
	return err == nil && n > 0
}

// waitWakeLate: a wait found due longer ago than this is not woken (one
// the hub was down for, or every expired wait when this feature arrived);
// the worker normally sees a wait fall due within seconds.
const waitWakeLate = time.Hour

// waitWakeSender sends the wake at a wait's expected time.
var waitWakeSender = contracts.Principal{Kind: contracts.PrincipalSystem, ID: "waiting-on"}

// processWaitDeadlines wakes each session whose wait passed its expected
// time while it sat idle in the turn it named (no turn since): the job or
// the watcher that should have woken it may be stuck, and only the
// session knows how to check (a child that can also be stalled is
// reported to its parent too, by the overdue report). Once per wait; a
// session mid-turn is looked at again later. Returns when the next wait
// falls due (zero if none). Called from ProcessChildReports (under
// reportMu).
func (h *Hub) processWaitDeadlines(ctx context.Context) (time.Time, error) {
	now := h.now()
	rows, err := h.db.QueryContext(ctx, `SELECT session_id, turn_seq, what, until, set_at FROM session_waits WHERE woke_at IS NULL AND until<=? ORDER BY until LIMIT 32`, persistence.Time(now))
	if err != nil {
		return time.Time{}, err
	}
	type due struct {
		session, what, until, setAt string
		turn                        int64
	}
	var ds []due
	for rows.Next() {
		var d due
		if rows.Scan(&d.session, &d.turn, &d.what, &d.until, &d.setAt) == nil {
			ds = append(ds, d)
		}
	}
	rows.Close()
	var next time.Time
	for _, d := range ds {
		settle := func() {
			_, _ = h.db.ExecContext(ctx, `UPDATE session_waits SET woke_at=? WHERE session_id=? AND turn_seq=? AND until=?`, persistence.Time(h.now()), d.session, d.turn, d.until)
		}
		s, err := h.getSession(ctx, h.db, d.session)
		if err != nil || s.Archived || s.CurrentRunID == "" || now.Sub(persistence.ParseTime(d.until)) > waitWakeLate {
			settle()
			continue
		}
		var turn, closed int64
		if err := h.db.QueryRowContext(ctx, `SELECT turn_seq, closed_seq FROM child_turns WHERE session_id=?`, d.session).Scan(&turn, &closed); err != nil || turn > d.turn {
			settle() // a later turn: the session moved on
			continue
		}
		r, err := h.getRun(ctx, h.db, s.CurrentRunID)
		if err != nil || r.State.Terminal() {
			settle()
			continue
		}
		if closed < d.turn || (r.State != contracts.RunWaitingInput && r.State != contracts.RunWaitingBackground) {
			// Still in the turn that set the wait, or busy: look again.
			if t := now.Add(childReportRecheck); next.IsZero() || t.Before(next) {
				next = t
			}
			continue
		}
		text := fmt.Sprintf("Pyre: your wait on %s was expected to finish by %s (set %s), and nothing has woken you since. Check on it now (read_session on the worker, the CI run, the review or the watcher that should have woken you). If it is done or you no longer wait on it, carry on; if it needs longer, call waiting_on again with a new expect_minutes.",
			d.what, askClock(persistence.ParseTime(d.until)), askClock(persistence.ParseTime(d.setAt)))
		_, err = h.msgs.SendMessage(contracts.WithPrincipal(h.systemCtx(ctx), waitWakeSender), contracts.SendMessageRequest{
			Mutation:  contracts.Mutation{OperationID: fmt.Sprintf("op_waitwake-%s-%d-%d", d.session, d.turn, persistence.ParseTime(d.until).Unix())},
			Recipient: contracts.Address{Kind: contracts.AddrSession, ID: d.session},
			ProjectID: s.ProjectID,
			Mode:      contracts.ModeQueue,
			Body:      contracts.MessageBody{Text: text},
		})
		if err != nil {
			if t := now.Add(childReportRecheck); next.IsZero() || t.Before(next) {
				next = t
			}
			continue
		}
		settle()
	}
	var pending sql.NullString
	if err := h.db.QueryRowContext(ctx, `SELECT MIN(until) FROM session_waits WHERE woke_at IS NULL AND until>?`, persistence.Time(now)).Scan(&pending); err == nil && pending.Valid {
		if t := persistence.ParseTime(pending.String); next.IsZero() || t.Before(next) {
			next = t
		}
	}
	return next, nil
}

// waitEndsTurnTx reports whether a run going waiting_on_background ends its
// open turn: only when that turn set a wait marker. Without one, background
// waiting continues the same turn (trackChildTx). Claude Code ends a turn
// with Stop while the build or watcher it waits on runs in the background
// (waiting_on_background), then starts the next turn itself from the
// task's notification; counting both as one turn put the marker on the
// later turn's end, so a session that resumed and finished got a quiet
// skipped report and, at its expected time, a "may be stuck" alert
// (rc135/rc136 release sessions, 2026-10-27).
func (h *Hub) waitEndsTurnTx(ctx context.Context, q queryer, r contracts.Run, sessionID string, turn, closed int64) bool {
	if r.State != contracts.RunWaitingBackground || turn <= closed {
		return false
	}
	_, ok := h.waitForTurnTx(ctx, q, sessionID, turn)
	return ok && !h.promptSinceWaitTx(ctx, q, sessionID)
}
