package hub

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jhh3/pyre/internal/askdetect"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/persistence"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// "Waiting on you" (owner request 2026-10-14; contracts.AttnWaiting).
//
// Leads (top-level sessions) wrote their open decisions for the owner at
// the end of a chat turn instead of filing asks, so nothing flagged them.
// The hub tracks turns of top-level sessions the way it tracks children's
// (child_turns, trackChildTx): when a turn ends (running -> waiting_input),
// it records one check row in the same transaction. A worker (the child
// report loop) then reads the session's last assistant message through its
// node, outside any transaction and bounded like a child report, and runs
// askdetect.OwnerQuestion on it. A question, with no open ask the session
// filed or repeated during that turn or later, refreshes the session's
// active "waiting" attention item or raises a new one, its own inbox
// conversation (attention.Store.RaiseWaiting). A turn that ends
// on the request already raised (the asking sentence, askdetect.QuestionKey,
// equal to the last one and specific enough, 6+ words; table
// lead_wait_questions, with the item raised for it) is not a new
// question: the item is left as it is (attention.Store.RepeatWaiting), so a
// coordinator's idle "still waiting on your yes" turns neither count up nor
// reopen what the owner dismissed (owner report 2026-10-19: 27 reopenings).
//
// The check is dropped when the session moved on first (its next turn
// started, the run ended, it was archived or reparented); a raise that lost
// that race is withdrawn at once. Otherwise the attention deriver resolves
// the item (next turn, an ask, the run ending, archive) and the owner
// engaging with the session handles it (engageKinds/readKinds).

var waitingMigrations = []sqlitex.Migration{{Namespace: "hub_waiting", Version: 1, Name: "lead_waits", SQL: `
CREATE TABLE lead_waits (
	session_id TEXT PRIMARY KEY,
	run_id TEXT NOT NULL,
	turn_seq INTEGER NOT NULL,
	turn_started_at TEXT,
	not_before TEXT NOT NULL,
	attempts INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL
);
CREATE INDEX lead_waits_due ON lead_waits(not_before);
`}, {Namespace: "hub_waiting", Version: 2, Name: "lead_wait_questions", SQL: `
CREATE TABLE lead_wait_questions (
	session_id TEXT PRIMARY KEY,
	question_key TEXT NOT NULL, -- askdetect.QuestionKey of the last question raised
	raised_at TEXT NOT NULL
);
`}, {Namespace: "hub_waiting", Version: 3, Name: "lead_wait_questions_item", SQL: `
-- the item raised for the question ('' for rows recorded before v3)
ALTER TABLE lead_wait_questions ADD COLUMN attention_id TEXT NOT NULL DEFAULT '';
`}}

const (
	// waitingAttempts bounds transcript reads / raises for one turn end.
	waitingAttempts = 3
	// waitingRetry is the base delay between attempts.
	waitingRetry = 10 * time.Second
	// waitingRepeatWords: a question key repeats the last one only when it
	// is the same and at least this many words long; a generic "waiting on
	// your approval" may be about something new each time.
	waitingRepeatWords = 6
	// waitingExcerptMax bounds the item summary (runes), starting a little
	// before the question.
	waitingExcerptMax  = 300
	waitingExcerptLead = 100
)

// waitingRaiser is implemented by the attention store (waiting.go there).
type waitingRaiser interface {
	RaiseWaiting(ctx context.Context, sessionID string, projectID *string, question, summary string) (contracts.AttentionItem, error)
	WithdrawWaiting(ctx context.Context, sessionID, summary string) (bool, error)
}

// waitingRepeater is implemented by the attention store: a turn that ends
// on the question already raised leaves the item as it is (waiting.go
// there). Without it a repeat raises again.
type waitingRepeater interface {
	RepeatWaiting(ctx context.Context, sessionID, attentionID string) (contracts.AttentionItem, bool, error)
}

// waitingSink is where "waiting" items go (nil: no attention store).
func (h *Hub) waitingSink() waitingRaiser {
	if h.waiting != nil {
		return h.waiting
	}
	if w, ok := h.cfg.Attention.(waitingRaiser); ok {
		return w
	}
	return nil
}

// trackLeadTx observes a run state transition of a top-level session: it
// keeps the session's turn count (child_turns, same rules as a child's)
// and records a check when a turn ends. It runs inside the transaction
// that stores the transition.
func (h *Hub) trackLeadTx(ctx context.Context, tx *sql.Tx, prev contracts.RunState, r contracts.Run, sess contracts.Session) error {
	var turn, closed, needs int64
	var startedAt, closedAt sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT turn_seq, turn_started_at, closed_seq, closed_at, needs_seq FROM child_turns WHERE session_id=?`, sess.SessionID).
		Scan(&turn, &startedAt, &closed, &closedAt, &needs)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	now := h.now()
	drop := func() error {
		_, err := tx.ExecContext(ctx, `DELETE FROM lead_waits WHERE session_id=?`, sess.SessionID)
		return err
	}
	switch {
	case r.State == contracts.RunRunning:
		if turn == 0 || closed >= turn {
			turn++
			// asks_chat.go: the prompt that started this turn may answer the
			// session's open asks (closedAt: when the previous turn ended).
			if err := h.queueAskChatTx(ctx, tx, sess.SessionID, r.RunID, turn, closedAt, now); err != nil {
				return err
			}
			startedAt = closedAt
			if !startedAt.Valid {
				startedAt = sql.NullString{String: persistence.Time(sess.CreatedAt), Valid: true}
			}
		}
		if err := drop(); err != nil { // the previous turn's check is moot
			return err
		}
	case r.State == contracts.RunWaitingInput:
		if turn == 0 && prev == contracts.RunRunning {
			// First transition seen for a session already mid-turn.
			turn = 1
			startedAt = sql.NullString{String: persistence.Time(sess.CreatedAt), Valid: true}
		}
		if turn <= closed {
			return nil
		}
		closed = turn
		closedAt = sql.NullString{String: persistence.Time(now), Valid: true}
		if _, waiting := h.waitForTurnTx(ctx, tx, sess.SessionID, turn); waiting {
			break // it waits on its own job, not on the owner (waiting_on.go)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO lead_waits(session_id,run_id,turn_seq,turn_started_at,not_before,attempts,created_at) VALUES(?,?,?,?,?,0,?)
			ON CONFLICT(session_id) DO UPDATE SET run_id=excluded.run_id, turn_seq=excluded.turn_seq, turn_started_at=excluded.turn_started_at,
			not_before=excluded.not_before, attempts=0, created_at=excluded.created_at`,
			sess.SessionID, r.RunID, turn, startedAt, persistence.Time(now), persistence.Time(now)); err != nil {
			return err
		}
		h.reportKick.Store(true)
	case r.State.Terminal() && !prev.Terminal():
		closed = turn
		closedAt = sql.NullString{String: persistence.Time(now), Valid: true}
		if err := drop(); err != nil {
			return err
		}
	default:
		return nil
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO child_turns(session_id,turn_seq,turn_started_at,closed_seq,closed_at,needs_seq) VALUES(?,?,?,?,?,?)
		ON CONFLICT(session_id) DO UPDATE SET turn_seq=excluded.turn_seq, turn_started_at=excluded.turn_started_at, closed_seq=excluded.closed_seq,
		closed_at=excluded.closed_at, needs_seq=excluded.needs_seq`,
		sess.SessionID, turn, startedAt, closed, closedAt, needs)
	return err
}

// leadWait is one pending turn-end check.
type leadWait struct {
	Session   string
	RunID     string
	Turn      int64
	TurnStart string
	Attempts  int
}

// processLeadWaits runs every due check and returns when the next pending
// one becomes due (zero if none). Called from ProcessChildReports (under
// reportMu).
func (h *Hub) processLeadWaits(ctx context.Context) (time.Time, error) {
	for range 8 { // bounded per pass; a processed row is deleted or deferred
		rows, err := h.db.QueryContext(ctx, `SELECT session_id, run_id, turn_seq, COALESCE(turn_started_at,''), attempts FROM lead_waits
			WHERE not_before<=? ORDER BY not_before LIMIT 16`, persistence.Time(h.now()))
		if err != nil {
			return time.Time{}, err
		}
		var due []leadWait
		for rows.Next() {
			var w leadWait
			if err := rows.Scan(&w.Session, &w.RunID, &w.Turn, &w.TurnStart, &w.Attempts); err != nil {
				rows.Close()
				return time.Time{}, err
			}
			due = append(due, w)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return time.Time{}, err
		}
		if len(due) == 0 {
			break
		}
		for _, w := range due {
			if err := ctx.Err(); err != nil {
				return time.Time{}, err
			}
			h.processLeadWait(ctx, w)
		}
	}
	var next sql.NullString
	if err := h.db.QueryRowContext(ctx, `SELECT MIN(not_before) FROM lead_waits`).Scan(&next); err != nil {
		return time.Time{}, err
	}
	if !next.Valid {
		return time.Time{}, nil
	}
	return persistence.ParseTime(next.String), nil
}

// processLeadWait decides one turn end: raise the session's "waiting" item
// or nothing. The row is deleted unless a transient failure defers it.
func (h *Hub) processLeadWait(ctx context.Context, w leadWait) {
	done := func() {
		_, _ = h.db.ExecContext(ctx, `DELETE FROM lead_waits WHERE session_id=? AND turn_seq=?`, w.Session, w.Turn)
	}
	retry := func() {
		if w.Attempts+1 >= waitingAttempts {
			done()
			return
		}
		_, _ = h.db.ExecContext(ctx, `UPDATE lead_waits SET attempts=attempts+1, not_before=? WHERE session_id=? AND turn_seq=?`,
			persistence.Time(h.now().Add(time.Duration(w.Attempts+1)*waitingRetry)), w.Session, w.Turn)
	}
	sink := h.waitingSink()
	if sink == nil || !h.leadTurnCurrent(ctx, w) {
		done()
		return
	}
	fetch := h.childSummary
	if fetch == nil {
		fetch = h.lastAssistantText
	}
	text, err := fetch(ctx, w.Session, w.RunID)
	if err != nil {
		retry()
		return
	}
	question, ok := askdetect.OwnerQuestion(text)
	if !ok || h.askedInTurn(ctx, w) || !h.leadTurnCurrent(ctx, w) {
		done()
		return
	}
	sess, err := h.getSession(ctx, h.db, w.Session)
	if err != nil {
		done()
		return
	}
	sctx := h.systemCtx(ctx)
	key := askdetect.QuestionKey(text)
	repeated := false
	if rep, ok := sink.(waitingRepeater); ok {
		if item, same := h.sameWaitQuestion(ctx, w.Session, key); same {
			// Still waiting on the same request (an idle report, a child's
			// report answered): not a new question for the owner.
			if _, repeated, err = rep.RepeatWaiting(sctx, w.Session, item); err != nil {
				retry()
				return
			}
		}
	}
	if !repeated {
		it, err := sink.RaiseWaiting(sctx, w.Session, sess.ProjectID, question, waitingExcerpt(text, question))
		if err != nil {
			retry()
			return
		}
		h.recordWaitQuestion(ctx, w.Session, key, it.AttentionID)
	}
	if !h.leadTurnCurrent(ctx, w) {
		// It moved on while the item was raised; the deriver may already
		// have passed that event.
		_, _ = sink.WithdrawWaiting(sctx, w.Session, "The session started its next turn.")
	}
	done()
}

// sameWaitQuestion reports whether key (askdetect.QuestionKey) repeats the
// question last raised for the session: the same words (numbers included,
// so "merge PR 41" and "merge PR 42" differ), long enough to be specific;
// item is the attention item raised for it ("" before hub_waiting v3).
func (h *Hub) sameWaitQuestion(ctx context.Context, session, key string) (item string, same bool) {
	var last string
	if key == "" || h.db.QueryRowContext(ctx, `SELECT question_key, attention_id FROM lead_wait_questions WHERE session_id=?`, session).Scan(&last, &item) != nil {
		return "", false
	}
	return item, last == key && len(strings.Fields(key)) >= waitingRepeatWords
}

// recordWaitQuestion remembers the question just raised for the session
// and the item raised for it.
func (h *Hub) recordWaitQuestion(ctx context.Context, session, key, item string) {
	_, _ = h.db.ExecContext(ctx, `INSERT INTO lead_wait_questions(session_id,question_key,raised_at,attention_id) VALUES(?,?,?,?)
		ON CONFLICT(session_id) DO UPDATE SET question_key=excluded.question_key, raised_at=excluded.raised_at, attention_id=excluded.attention_id`,
		session, key, persistence.Time(h.now()), item)
}

// leadTurnCurrent reports whether w is still the session's latest turn,
// ended and not followed by another: the session is top level and not
// archived, w's run is its current run and waits for input.
func (h *Hub) leadTurnCurrent(ctx context.Context, w leadWait) bool {
	sess, err := h.getSession(ctx, h.db, w.Session)
	if err != nil || sess.Archived || sess.Parent() != "" || sess.CurrentRunID != w.RunID {
		return false
	}
	r, err := h.getRun(ctx, h.db, w.RunID)
	if err != nil || r.State != contracts.RunWaitingInput {
		return false
	}
	var turn, closed int64
	if err := h.db.QueryRowContext(ctx, `SELECT turn_seq, closed_seq FROM child_turns WHERE session_id=?`, w.Session).Scan(&turn, &closed); err != nil {
		return false
	}
	return turn == w.Turn && closed == w.Turn
}

// askedInTurn reports whether the session has an open ask it filed or
// repeated (merged) during w's turn or later: the question in chat is that
// ask, already in the owner's inbox, so the session never shows both. An
// older open ask does not hide a new question (the trade-off: a chat
// question that repeats an old ask raises an item too; the Stop-hook ask
// nudge asks the session to file it instead).
func (h *Hub) askedInTurn(ctx context.Context, w leadWait) bool {
	var n int
	err := h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM asks WHERE session_id=? AND state=? AND filed_turn>=?`,
		w.Session, string(contracts.AskOpen), w.Turn).Scan(&n)
	return err != nil || n > 0 // unknown: do not raise
}

// waitingExcerpt is the item summary: at most waitingExcerptMax runes of
// text, starting a little before the question (else its tail).
func waitingExcerpt(text, question string) string {
	r := []rune(strings.TrimSpace(text))
	start := max(len(r)-waitingExcerptMax, 0)
	probe := question
	if utf8.RuneCountInString(probe) > 40 {
		probe = string([]rune(probe)[:40])
	}
	if i := strings.Index(string(r), probe); probe != "" && i >= 0 {
		start = max(utf8.RuneCountInString(string(r)[:i])-waitingExcerptLead, 0)
	}
	end := min(start+waitingExcerptMax, len(r))
	out := string(r[start:end])
	if start > 0 {
		out = "…" + out
	}
	if end < len(r) {
		out += "…"
	}
	return out
}
