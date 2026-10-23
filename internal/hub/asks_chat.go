package hub

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/persistence"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// Answer in chat (owner request 2026-10-19: "asks don't seem to go ahead
// when I respond in chat"). The owner often answers a session's question
// by writing to the session itself instead of answering the ask. The rule
// is deliberately simple:
//
//   - Owner input to the session itself answers every open ask the session
//     filed before it: anything the owner types into the session (TUI or
//     web terminal, a local tmux attach, the harness's own window), and a
//     Pyre message from the owner to the session that is not a reply in a
//     thread (`pyre send`, the TUI composer, the web inbox's reply to a
//     "waiting on you" item). Each ask becomes "answered" with Answer.Source
//     "chat", Answer.Text the owner's words and Answer.With the other asks
//     the same input answered; its deadline default can no longer fire, and
//     its inbox item is resolved by the attention deriver (ask.changed).
//   - A reply in a thread (an ask's thread in the web inbox, a reply to a
//     session's message) answers nothing: it is delivered to the session
//     as a message ("what do you think is better?"), and the session
//     answers, withdraws or re-asks. To make a reply the answer, the owner
//     uses "Answer with this" in the web inbox (asks.answer with text) or
//     `pyre ask answer <id> --text`.
//   - Not answered in chat: proposals (approve/reject writes a grant or a
//     fact), owner-action requests, and asks with a held answer (the
//     owner's queued choice is sent instead).
//
// The asking session already has the owner's words (it was typed into it,
// or the message is delivered), so the decision it is told is an info
// message (stored in its inbox, never injected): no extra turn.
//
// Typed input is seen through the session's transcript, ordered by the
// hub's own turn count, never by comparing clocks (the transcript's times
// are the node's; Claude also writes a prompt typed during a busy turn
// only when that turn ends, with a new time). Each ask records the lead
// turn that filed it (asks.filed_turn; a merge moves it to the turn that
// asked again). When a top-level session starts a new turn (trackLeadTx,
// running after waiting_input) it is checked only when all of these hold:
//   - it has an open ask filed in an earlier turn;
//   - its previous turn ended at least askChatPause before (the session
//     sat idle with the question on screen: a prompt queued during the
//     asking turn starts the next one at once, and is not an answer);
//   - the turn is not its run's first (a launch, resume or handoff prompt).
// The check row (ask_chat_checks) is read by its own worker, outside every
// lock and in parallel with a short timeout per read: it reads the run's
// transcript tail through the node and takes the newest prompt (the one
// that started the turn). Only a prompt a person typed answers: kind
// "user", no Pyre message marker, not harness meta, a plugin, a task
// notification or one of Pyre's own prompts (ownerChatText).

var askChatMigrations = []sqlitex.Migration{{Namespace: "hub_ask_chat", Version: 1, Name: "ask_chat_checks", SQL: `
ALTER TABLE asks ADD COLUMN filed_turn INTEGER NOT NULL DEFAULT 0;
CREATE TABLE ask_chat_checks (
	session_id TEXT PRIMARY KEY,
	run_id TEXT NOT NULL,
	turn_seq INTEGER NOT NULL,
	not_before TEXT NOT NULL,
	attempts INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL
);
CREATE INDEX ask_chat_checks_due ON ask_chat_checks(not_before);
CREATE TABLE ask_chat_runs (
	session_id TEXT PRIMARY KEY,
	run_id TEXT NOT NULL -- the run whose turn the hub last saw start
);
`}}

const (
	// askChatDelay lets the harness write the prompt to its transcript
	// before the check reads it.
	askChatDelay = 3 * time.Second
	// askChatAttempts bounds transcript reads for one check.
	askChatAttempts = 3
	// askChatRetry is the base delay between attempts.
	askChatRetry = 10 * time.Second
	// askChatPause: a turn that starts sooner than this after the previous
	// one ended was not started by someone who read the question.
	askChatPause = 5 * time.Second
	// askChatParallel bounds concurrent transcript reads.
	askChatParallel = 8
	// askChatTail is how many transcript entries a check reads.
	askChatTail = 40
)

// askChatReadTimeout bounds one transcript read (a sleeping node must not
// hold the others; a variable for tests).
var askChatReadTimeout = 5 * time.Second

// askChatOwner answers asks for input typed into a session (the owner, as
// seen through its transcript).
var askChatOwner = contracts.Principal{Kind: contracts.PrincipalOwner, ID: "owner", Name: "the owner (in the session)"}

// askChatNotOwner are prompt prefixes that are harness meta or Pyre's own
// text, not the owner writing.
var askChatNotOwner = []string{
	"<command-", "<local-command", "<bash-", "<user-prompt-submit-hook", "<system-reminder", "<task-notification", "Caveat:", "[Request interrupted",
	"Stop hook feedback", "This session is being continued from a previous conversation",
	"[pyre",         // [pyre-msg:…], [pyre] Continue where you left off (account switch), [pyre ask …]
	"Pyre handoff:", // a handoff run's first prompt (daemon)
	"Your turn ended with a question for the owner", // claude.AskNudgeText
	"You lead this project and did work this turn",  // claude.PageNudgeText
}

// ownerChatText reports whether a transcript prompt is the owner writing,
// and returns it trimmed (at most contracts.AskMaxContext runes).
func ownerChatText(text string) (string, bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", false
	}
	for _, p := range askChatNotOwner {
		if strings.HasPrefix(text, p) {
			return "", false
		}
	}
	if strings.HasPrefix(text, "/") && !strings.ContainsAny(text, " \n") {
		return "", false // a slash command
	}
	if first, _, _ := strings.Cut(text, "\n"); strings.HasPrefix(first, "The ") && strings.HasSuffix(first, " plugin sent a message:") {
		return "", false // a Claude plugin's message (older nodes report it as a user prompt)
	}
	return clipAnswer(text), true
}

func clipAnswer(text string) string {
	if utf8.RuneCountInString(text) > contracts.AskMaxContext {
		return string([]rune(text)[:contracts.AskMaxContext-1]) + "…"
	}
	return text
}

// chatAnswerable lists sessionID's open asks that chat input may answer
// (oldest first): filed before the time before (hub clock; zero: any) and,
// when beforeTurn is set, in an earlier lead turn than it.
func (h *Hub) chatAnswerable(ctx context.Context, q queryer, sessionID string, before time.Time, beforeTurn int64) ([]contracts.Ask, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+askCols+` FROM asks WHERE session_id=? AND state=? AND (?=0 OR filed_turn<?)
		AND ask_id NOT IN (SELECT ask_id FROM ask_held) AND ask_id NOT IN (SELECT ask_id FROM owner_actions) AND ask_id NOT IN (SELECT ask_id FROM ask_proposals)
		ORDER BY created_at, ask_id`, sessionID, string(contracts.AskOpen), beforeTurn, beforeTurn)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []contracts.Ask
	for rows.Next() {
		a, err := scanAsk(rows)
		if err != nil {
			return nil, err
		}
		if before.IsZero() || a.CreatedAt.Before(before) {
			out = append(out, a)
		}
	}
	return out, rows.Err()
}

// filedTurnTx is the lead turn an ask filed now belongs to: the session's
// current turn, or the next one when the hub has seen its last turn end
// (it is filing mid-turn before the hub saw the turn start).
func (h *Hub) filedTurnTx(ctx context.Context, tx *sql.Tx, sessionID string) int64 {
	var turn, closed int64
	if sessionID == "" || tx.QueryRowContext(ctx, `SELECT turn_seq, closed_seq FROM child_turns WHERE session_id=?`, sessionID).Scan(&turn, &closed) != nil {
		return 0
	}
	if closed >= turn {
		return turn + 1
	}
	return turn
}

// answerInChat answers asks with the owner's chat text (each commits on its
// own; one already decided is skipped) and tells the asking session in an
// info message. It returns the asks it answered.
func (h *Hub) answerInChat(ctx context.Context, p contracts.Principal, asks []contracts.Ask, text string) []contracts.Ask {
	if len(asks) == 0 || text == "" {
		return nil
	}
	ids := make([]string, len(asks))
	for i, a := range asks {
		ids[i] = a.AskID
	}
	var decided []contracts.Ask
	for _, a := range asks {
		with := slices.DeleteFunc(slices.Clone(ids), func(id string) bool { return id == a.AskID })
		if len(with) == 0 {
			with = nil
		}
		var d contracts.Ask
		err := h.tx(ctx, func(tx *sql.Tx) error {
			var err error
			d, err = h.decideAskTx(ctx, tx, a, contracts.AskAnswered,
				contracts.AskAnswer{Text: text, By: p, At: h.now(), Source: contracts.AskSourceChat, With: with}, "")
			return err
		})
		if err != nil {
			continue // decided meanwhile (that decision stands) or a transient failure (the default still applies)
		}
		decided = append(decided, d)
	}
	h.sendDecisions(ctx, decided, p)
	return decided
}

// answerChatMessage: the owner's message env to a session answers its open
// asks when it is chat: not a reply in a thread and not an info note (it is
// queued for the session whether or not the session is running now). Asks
// filed after the message was sent are not answered (both times are the
// hub's).
func (h *Hub) answerChatMessage(ctx context.Context, p contracts.Principal, env contracts.MessageEnvelope) {
	if !isOwner(p) || env.Recipient.Kind != contracts.AddrSession || env.ThreadID != "" || env.InReplyTo != "" || env.Mode == contracts.ModeInfo {
		return
	}
	at := env.CreatedAt
	if at.IsZero() {
		at = h.now()
	}
	asks, err := h.chatAnswerable(ctx, h.db, env.Recipient.ID, at, 0)
	if err != nil {
		return
	}
	h.answerInChat(ctx, p, asks, clipAnswer(strings.TrimSpace(env.Body.Text)))
}

// queueAskChatTx runs when a top-level session's turn turn starts (inside
// the transaction that stores the transition, trackLeadTx); prevEnd is when
// its previous turn ended. It records a check of the prompt that started
// the turn when that prompt may answer an open ask (rules above).
func (h *Hub) queueAskChatTx(ctx context.Context, tx *sql.Tx, sessionID, runID string, turn int64, prevEnd sql.NullString, now time.Time) error {
	var lastRun sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT run_id FROM ask_chat_runs WHERE session_id=?`, sessionID).Scan(&lastRun); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if !lastRun.Valid || lastRun.String != runID {
		if _, err := tx.ExecContext(ctx, `INSERT INTO ask_chat_runs(session_id,run_id) VALUES(?,?) ON CONFLICT(session_id) DO UPDATE SET run_id=excluded.run_id`,
			sessionID, runID); err != nil {
			return err
		}
		return nil // the run's first turn: its launch, resume or handoff prompt
	}
	if !prevEnd.Valid || now.Sub(persistence.ParseTime(prevEnd.String)) < askChatPause {
		return nil // started straight after the last turn: not someone who read the question
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM asks WHERE session_id=? AND state=? AND filed_turn<?`,
		sessionID, string(contracts.AskOpen), turn).Scan(&n); err != nil || n == 0 {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO ask_chat_checks(session_id,run_id,turn_seq,not_before,attempts,created_at) VALUES(?,?,?,?,0,?)
		ON CONFLICT(session_id) DO UPDATE SET run_id=excluded.run_id, turn_seq=excluded.turn_seq, not_before=excluded.not_before, attempts=0, created_at=excluded.created_at`,
		sessionID, runID, turn, persistence.Time(now.Add(askChatDelay)), persistence.Time(now)); err != nil {
		return err
	}
	h.askChatKick.Store(true)
	return nil
}

// askChat is one due check.
type askChat struct {
	session, run, created string
	turn                  int64
	attempts              int
}

// ProcessAskChats runs every due check, reads in parallel (askChatParallel,
// each bounded by askChatReadTimeout), and returns when the next pending
// one becomes due (zero if none). It holds no lock the child reports use.
func (h *Hub) ProcessAskChats(ctx context.Context) (time.Time, error) {
	h.askChatMu.Lock() // one pass at a time
	defer h.askChatMu.Unlock()
	for range 8 { // bounded per pass; a processed row is deleted or deferred
		rows, err := h.db.QueryContext(ctx, `SELECT session_id, run_id, turn_seq, attempts, created_at FROM ask_chat_checks WHERE not_before<=? ORDER BY not_before LIMIT 32`,
			persistence.Time(h.now()))
		if err != nil {
			return time.Time{}, err
		}
		var due []askChat
		for rows.Next() {
			var c askChat
			if err := rows.Scan(&c.session, &c.run, &c.turn, &c.attempts, &c.created); err != nil {
				rows.Close()
				return time.Time{}, err
			}
			due = append(due, c)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return time.Time{}, err
		}
		if len(due) == 0 {
			break
		}
		sem := make(chan struct{}, askChatParallel)
		var wg sync.WaitGroup
		for _, c := range due {
			if err := ctx.Err(); err != nil {
				break
			}
			sem <- struct{}{}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				h.processAskChat(ctx, c)
			}()
		}
		wg.Wait()
		if err := ctx.Err(); err != nil {
			return time.Time{}, err
		}
	}
	var next sql.NullString
	if err := h.db.QueryRowContext(ctx, `SELECT MIN(not_before) FROM ask_chat_checks`).Scan(&next); err != nil {
		return time.Time{}, err
	}
	if !next.Valid {
		return time.Time{}, nil
	}
	return persistence.ParseTime(next.String), nil
}

// askChatLoop runs the checks until Close (its own worker: a slow node
// never delays child reports).
func (h *Hub) askChatLoop() {
	defer h.wg.Done()
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		next, _ := h.ProcessAskChats(ctx)
		cancel()
		wait := childReportPoll
		if !next.IsZero() {
			wait = min(max(next.Sub(h.now()), 50*time.Millisecond), childReportPoll)
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(wait)
		select {
		case <-h.stop:
			return
		case <-h.askChatWake:
		case <-timer.C:
		}
	}
}

// kickAskChats wakes the worker after a commit that recorded a check.
func (h *Hub) kickAskChats() {
	if h.askChatKick.Swap(false) {
		select {
		case h.askChatWake <- struct{}{}:
		default:
		}
	}
}

// processAskChat reads the transcript tail of c's run and, when the
// newest prompt (the one that started c's turn) is the owner's typing,
// answers the open asks filed in earlier turns with it. The row is deleted
// unless a transient failure defers it (a newer check replaces it).
func (h *Hub) processAskChat(ctx context.Context, c askChat) {
	done := func() {
		_, _ = h.db.ExecContext(ctx, `DELETE FROM ask_chat_checks WHERE session_id=? AND created_at=?`, c.session, c.created)
	}
	retry := func() {
		if c.attempts+1 >= askChatAttempts {
			done()
			return
		}
		_, _ = h.db.ExecContext(ctx, `UPDATE ask_chat_checks SET attempts=attempts+1, not_before=? WHERE session_id=? AND created_at=?`,
			persistence.Time(h.now().Add(time.Duration(c.attempts+1)*askChatRetry)), c.session, c.created)
	}
	open, err := h.chatAnswerable(ctx, h.db, c.session, time.Time{}, c.turn)
	if err != nil {
		retry()
		return
	}
	if len(open) == 0 {
		done()
		return
	}
	fetch := h.chatTranscript
	if fetch == nil {
		fetch = h.transcriptTail
	}
	rctx, cancel := context.WithTimeout(ctx, askChatReadTimeout)
	entries, err := fetch(rctx, c.session, c.run)
	cancel()
	if err != nil {
		retry()
		return
	}
	e, ok := turnPrompt(entries, c.run)
	if !ok || e.Kind != "user" || e.MessageID != "" {
		done()
		return
	}
	if text, ok := ownerChatText(e.Text); ok {
		h.answerInChat(ctx, askChatOwner, open, text)
	}
	done()
}

// turnPrompt is the newest prompt of run in entries (by position): a typed
// prompt, a Pyre message, or a prompt nobody typed (system: a task
// notification, a plugin's message). Stop-hook feedback and turn ends are
// not prompts that start a turn. Entries without a time (a harness that
// did not say when) are skipped.
func turnPrompt(entries []contracts.TranscriptEntry, run string) (contracts.TranscriptEntry, bool) {
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if (e.RunID != "" && e.RunID != run) || e.At.IsZero() {
			continue
		}
		switch e.Kind {
		case "user", "pyre_message":
			return e, true
		case "system":
			t := strings.TrimSpace(e.Text)
			if strings.HasPrefix(t, "turn ended") || strings.HasPrefix(t, "Stop hook feedback") {
				continue
			}
			return e, true
		}
	}
	return contracts.TranscriptEntry{}, false
}

// transcriptTail reads the last askChatTail entries of a run's transcript
// through its node.
func (h *Hub) transcriptTail(ctx context.Context, sessionID, runID string) ([]contracts.TranscriptEntry, error) {
	resp, err := h.ReadSession(h.systemCtx(ctx), contracts.ReadSessionRequest{SessionID: sessionID, RunID: runID, Tail: true, Limit: askChatTail})
	if err != nil {
		return nil, err
	}
	return resp.Entries, nil
}
