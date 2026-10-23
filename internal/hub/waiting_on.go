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
//   - with quiet, a child's turn end is not reported at all (its waiting
//     report is recorded as skipped, reportQuietDetail): a lead waiting on
//     its own workers, CI or a review ends its turn instead of polling its
//     inbox. The waiting_overdue report still goes out, with the last
//     message the parent never got, and a run that exits is reported as
//     usual.
//
// The marker names its turn (child_turns.turn_seq), so it never leaks into
// a later turn.

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
`}}

var _ contracts.WaitingOnAPI = (*Hub)(nil)

// sessionWait is a recorded marker.
type sessionWait struct {
	Turn  int64
	What  string
	Until time.Time
	Quiet bool // the turn's end is not reported (overdue and ended still are)
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
			ON CONFLICT(session_id) DO UPDATE SET turn_seq=excluded.turn_seq, what=excluded.what, until=excluded.until, set_at=excluded.set_at, quiet=excluded.quiet`,
			sess.SessionID, turn, what, persistence.Time(until), persistence.Time(now), req.Quiet); err != nil {
			return nil, err
		}
		out := contracts.SetWaitingOnResponse{SessionID: sess.SessionID, What: what, Until: &until, Quiet: req.Quiet}
		switch {
		case req.Quiet && sess.Parent() != "":
			out.Detail = fmt.Sprintf("recorded for this turn (quiet): when it ends, your parent session is not told. End your turn; a watcher, a child's report or a message wakes you. If no new turn starts by %s, your parent is told your wait on %s looks stalled, with your last message",
				askClock(until), what)
		default:
			who := "your parent session hears"
			if sess.Parent() == "" {
				who = "the owner is not told you are waiting on them"
			}
			out.Detail = fmt.Sprintf("recorded for this turn: when it ends, %s that you are waiting on %s (expected by %s), not idle. Make sure a watcher or background task will wake you; if no new turn starts by then, your parent is told it looks stalled",
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
