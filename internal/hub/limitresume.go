package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/persistence"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// Automatic resume after an account limit (decision I70; follows I69).
//
// A Claude session that stops on a usage or spend limit while no other
// account has room used to sit idle until someone resumed it ("resumable
// after <reset>"). The hub now keeps one durable timer per such session,
// in limit_resumes, armed when the node's fallback plan arrives:
//
//   - a waiting plan (the fallback stopped the run and found no account
//     with room): mode continue, due at the plan's earliest reset (the
//     limited account's own reset, or an earlier one the node named), or
//     earlier, the moment another account of the provider is observed to
//     regain room on that node;
//   - a paused switch (the limit came right after an automatic switch; the
//     run stays up on its account and its messages are held, I69): mode
//     in_place, due at that account's reset.
//
// When the timer fires the hub checks what only it knows: the session is
// not archived or being archived, its current run is still the limited
// one, nobody gave it input since the stop (the owner typed, attached or
// messaged; any other session or the owner sent it a message; Pyre's own
// reports and notes do not count), and its node is connected and serves
// session.limit_resume. Then it asks the node, which answers from its own
// view of the accounts: a continuation on the best eligible account (the
// existing StartContinuation path, with the standard continue prompt), the
// continue prompt sent to the live run, no room yet (rescheduled at the
// reset it names) or refused (dropped). The interrupted instruction is
// never replayed; a request whose result is lost is asked again, which is
// safe: the node refuses a run that is no longer the session's current
// one, and the in-place prompt has one stable message ID per run and
// limit.
//
// Resumes are spaced LimitResumeGap apart across the hub (an account
// reset frees 18 sessions at once: they resume over three minutes, not
// in one second). A resume whose turn hits the limit again within
// limitResumeWatch is a repeat: it is retried after a growing backoff, at
// most limitResumeMaxRepeats times, then left to the owner. The parent of
// a child is told once, when a resume holds ("resumed on account X"), or
// when Pyre gives up. Every resume is audited (session.limit_resumed).
// Options.LimitAutoResumeOff (pyre-hub serve --limit-auto-resume=false)
// turns it all off.

var limitResumeMigrations = []sqlitex.Migration{{Namespace: "hub_limit_resume", Version: 1, Name: "limit_resumes", SQL: `
CREATE TABLE limit_resumes (
	session_id TEXT PRIMARY KEY,
	node_id TEXT NOT NULL,
	run_id TEXT NOT NULL,
	mode TEXT NOT NULL,
	account_id TEXT NOT NULL DEFAULT '',
	provider TEXT NOT NULL DEFAULT '',
	limit_kind TEXT NOT NULL DEFAULT '',
	limit_detail TEXT NOT NULL DEFAULT '',
	stopped_at TEXT NOT NULL,
	reset_at TEXT NOT NULL,
	due_at TEXT NOT NULL,
	state TEXT NOT NULL,
	command_id TEXT,
	sent_at TEXT,
	repeats INTEGER NOT NULL DEFAULT 0,
	tries INTEGER NOT NULL DEFAULT 0,
	early_after TEXT NOT NULL DEFAULT '',
	resumed_run_id TEXT NOT NULL DEFAULT '',
	resumed_account TEXT NOT NULL DEFAULT '',
	resumed_kind TEXT NOT NULL DEFAULT '',
	resumed_at TEXT,
	detail TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE INDEX limit_resumes_state ON limit_resumes(state, due_at);
CREATE INDEX limit_resumes_node ON limit_resumes(node_id, state);
`}}

const (
	// limitResumeSlack follows a reset before the resume (Claude names
	// resets to the minute).
	limitResumeSlack = time.Minute
	// defaultLimitResumeGap spaces resumes across the hub.
	defaultLimitResumeGap = 10 * time.Second
	// limitResumePoll bounds how long the worker waits for a wake-up.
	limitResumePoll = 5 * time.Second
	// limitResumeWatch: a resumed turn that hits the limit again within
	// this is a repeat.
	limitResumeWatch = 10 * time.Minute
	// limitResumeConfirm: the parent's "resumed" line waits this long for
	// a repeat to show.
	limitResumeConfirm = 2 * time.Minute
	// limitResumeBackoff is the wait after the first repeat; it doubles.
	limitResumeBackoff = 10 * time.Minute
	// limitResumeMaxRepeats: after this many repeats Pyre gives up.
	limitResumeMaxRepeats = 4
	// limitResumeRetry spaces requests the node could not serve yet (no
	// room, an error, a lost request).
	limitResumeRetry = 10 * time.Minute
	// limitResumeMinRetry is the least wait after a no-room answer.
	limitResumeMinRetry = 2 * time.Minute
	// limitResumeMaxTries bounds those per stop.
	limitResumeMaxTries = 12
	// limitResumeSentMax: a request without a final result by then is
	// looked at again (pyred restarted mid-request).
	limitResumeSentMax = 20 * time.Minute
	// limitResumeEarlyGap: an account regaining room triggers a session at
	// most this often.
	limitResumeEarlyGap = 15 * time.Minute
	// limitResumeHorizon: resets further out are not scheduled.
	limitResumeHorizon = 8 * 24 * time.Hour
	// limitOwnerMargin: owner engagement this long before the stop counts
	// as engagement since it (his typing is marked once a minute).
	limitOwnerMargin = 2 * ownerActiveEvery
	// limitResumeRetention keeps settled timers.
	limitResumeRetention = 7 * 24 * time.Hour
	// limitResumeEndWait: a continue resume whose run has not ended this
	// long after it was due is dropped.
	limitResumeEndWait = time.Hour
)

// Timer states.
const (
	lrScheduled = "scheduled" // waiting for its due time
	lrSent      = "sent"      // the node was asked
	lrWatching  = "watching"  // resumed; a repeat limit within limitResumeWatch is retried
	lrDone      = "done"
	lrCancelled = "cancelled"
	lrGaveUp    = "gave_up"
)

// reportLimitResumed is the child report kind of a resume or of Pyre
// giving up (dedupe keys limit_resumed:… and limit_gave_up:…).
const reportLimitResumed = "limit_resumed"

// limitResumeState is the worker's wake-up.
type limitResumeState struct {
	kick atomic.Bool
	wake chan struct{}
}

type limitResume struct {
	SessionID, NodeID, RunID, Mode string
	AccountID, Provider            string
	LimitKind, LimitDetail         string
	StoppedAt, ResetAt, DueAt      time.Time
	State, CommandID               string
	SentAt                         *time.Time
	Repeats, Tries                 int
	EarlyAfter                     time.Time
	ResumedRun, ResumedAccount     string
	ResumedKind                    string
	ResumedAt                      *time.Time
	Detail                         string
}

const limitResumeCols = `session_id,node_id,run_id,mode,account_id,provider,limit_kind,limit_detail,stopped_at,reset_at,due_at,state,command_id,sent_at,repeats,tries,early_after,resumed_run_id,resumed_account,resumed_kind,resumed_at,detail`

func scanLimitResume(sc interface{ Scan(...any) error }) (limitResume, error) {
	var r limitResume
	var stopped, reset, due, early string
	var cmd, sent, resumedAt sql.NullString
	err := sc.Scan(&r.SessionID, &r.NodeID, &r.RunID, &r.Mode, &r.AccountID, &r.Provider, &r.LimitKind, &r.LimitDetail, &stopped, &reset, &due, &r.State,
		&cmd, &sent, &r.Repeats, &r.Tries, &early, &r.ResumedRun, &r.ResumedAccount, &r.ResumedKind, &resumedAt, &r.Detail)
	if err != nil {
		return r, err
	}
	r.StoppedAt, r.ResetAt, r.DueAt, r.EarlyAfter = persistence.ParseTime(stopped), persistence.ParseTime(reset), persistence.ParseTime(due), persistence.ParseTime(early)
	r.CommandID = cmd.String
	r.SentAt, r.ResumedAt = persistence.ParseTimePtr(sent), persistence.ParseTimePtr(resumedAt)
	return r, nil
}

func (h *Hub) getLimitResume(ctx context.Context, q queryer, sessionID string) (limitResume, bool, error) {
	r, err := scanLimitResume(q.QueryRowContext(ctx, `SELECT `+limitResumeCols+` FROM limit_resumes WHERE session_id=?`, sessionID))
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	return r, err == nil, err
}

func (h *Hub) putLimitResumeTx(ctx context.Context, tx *sql.Tx, r limitResume) error {
	now := persistence.Time(h.now())
	_, err := tx.ExecContext(ctx, `INSERT INTO limit_resumes(`+limitResumeCols+`,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(session_id) DO UPDATE SET node_id=excluded.node_id, run_id=excluded.run_id, mode=excluded.mode, account_id=excluded.account_id,
		provider=excluded.provider, limit_kind=excluded.limit_kind, limit_detail=excluded.limit_detail, stopped_at=excluded.stopped_at,
		reset_at=excluded.reset_at, due_at=excluded.due_at, state=excluded.state, command_id=excluded.command_id, sent_at=excluded.sent_at,
		repeats=excluded.repeats, tries=excluded.tries, early_after=excluded.early_after, resumed_run_id=excluded.resumed_run_id,
		resumed_account=excluded.resumed_account, resumed_kind=excluded.resumed_kind, resumed_at=excluded.resumed_at, detail=excluded.detail,
		updated_at=excluded.updated_at`,
		r.SessionID, r.NodeID, r.RunID, r.Mode, r.AccountID, r.Provider, r.LimitKind, r.LimitDetail, persistence.Time(r.StoppedAt), persistence.Time(r.ResetAt),
		persistence.Time(r.DueAt), r.State, persistence.NullStr(r.CommandID), persistence.TimePtr(r.SentAt), r.Repeats, r.Tries, persistence.Time(r.EarlyAfter),
		r.ResumedRun, r.ResumedAccount, r.ResumedKind, persistence.TimePtr(r.ResumedAt), clipReason(r.Detail), now, now)
	if err == nil {
		h.lres.kick.Store(true)
	}
	return err
}

// limitResumeMode is the mode a fallback plan may be resumed in (false: it
// is not a limit Pyre resumes by itself). Keep in sync with
// accounts.resumablePlan.
func limitResumeMode(p contracts.FallbackPlan) (string, bool) {
	if p.Limit == nil || (p.Limit.Kind != "usage_limit" && p.Limit.Kind != "rate_limited") {
		return "", false
	}
	switch {
	case p.Outcome == contracts.FallbackWaiting:
		return contracts.LimitResumeContinue, true
	case p.Outcome == contracts.FallbackIntervention && strings.HasPrefix(p.Reason, "account switching paused"):
		return contracts.LimitResumeInPlace, true
	}
	return "", false
}

// limitResumeReset is when a plan's limit leaves room again: the earliest
// reset the node named for a waiting plan, the limited account's own for
// a paused one (the run stays on that account).
func limitResumeReset(p contracts.FallbackPlan, mode string) *time.Time {
	if mode == contracts.LimitResumeInPlace {
		if p.Limit != nil && p.Limit.ResetAt != nil {
			t := p.Limit.ResetAt.UTC()
			return &t
		}
		return nil
	}
	return planReset(&p) // limitreport.go
}

// nodeHasCap reads a node capability from its latest hello.
func (h *Hub) nodeHasCap(ctx context.Context, q queryer, nodeID, capName string) bool {
	var hello sql.NullString
	if err := q.QueryRowContext(ctx, `SELECT hello FROM node_state WHERE node_id=?`, nodeID).Scan(&hello); err != nil || !hello.Valid {
		return false
	}
	var nh contracts.NodeHello
	return json.Unmarshal([]byte(hello.String), &nh) == nil && contracts.HasCap(nh.Capabilities, capName)
}

// limitResumeBackoffFor is the wait after the n-th repeat.
func limitResumeBackoffFor(n int) time.Duration {
	d := limitResumeBackoff
	for i := 1; i < n && d < 8*time.Hour; i++ {
		d *= 2
	}
	return d
}

func laterOf(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// armLimitResumeTx schedules the automatic resume a fallback plan calls
// for (inside the plan's ingest transaction), or counts the plan as a
// repeat of a resume that is being watched.
func (h *Hub) armLimitResumeTx(ctx context.Context, tx *sql.Tx, nodeID string, f contracts.FallbackPlan) error {
	if h.opt.LimitAutoResumeOff {
		return nil
	}
	mode, ok := limitResumeMode(f)
	cur, found, err := h.getLimitResume(ctx, tx, f.SessionID)
	if err != nil {
		return err
	}
	now := h.now()
	if found && cur.State == lrSent {
		// The continuation's own plan can arrive before the node's answer
		// to the request that started it: record that resume first.
		if nr, err := h.getRun(ctx, tx, f.FromRunID); err == nil && nr.PreviousRunID == cur.RunID && nr.RunID != cur.RunID {
			if err := h.limitResumedTx(ctx, tx, cur, contracts.LimitResumeResult{Outcome: contracts.LimitResumed, AccountID: nr.AccountID,
				NewRunID: nr.RunID, Kind: string(nr.ContinuationKind)}); err != nil {
				return err
			}
			if cur, found, err = h.getLimitResume(ctx, tx, f.SessionID); err != nil {
				return err
			}
		}
	}
	recent := found && cur.ResumedRun == f.FromRunID && cur.ResumedAt != nil && now.Sub(*cur.ResumedAt) < limitResumeWatch
	if recent && (cur.State == lrWatching || cur.State == lrDone) {
		if !ok {
			// The resumed run hit a limit the node handled itself (it
			// switched accounts, or left it to the owner): not ours.
			cur.State, cur.Detail = lrDone, "the continuation hit a limit the node handled: "+string(f.Outcome)
			return h.putLimitResumeTx(ctx, tx, cur)
		}
		return h.limitRepeatTx(ctx, tx, cur, f.FromRunID, mode, limitResumeReset(f, mode), f.FromAccountID, "the continuation hit a limit again")
	}
	if !ok || mode == contracts.LimitResumeContinue {
		// A Codex session started with fallback_harness claude restarts on
		// Claude instead of waiting for the reset (harnessfallback.go).
		if sess, err := h.getSession(ctx, tx, f.SessionID); err == nil && harnessFallbackFor(sess, f) {
			return h.armHarnessFallbackTx(ctx, tx, nodeID, f)
		}
	}
	if !ok {
		return nil
	}
	reset := limitResumeReset(f, mode)
	if reset == nil || reset.After(now.Add(limitResumeHorizon)) {
		return nil
	}
	sess, err := h.getSession(ctx, tx, f.SessionID)
	if err != nil || sess.Archived || hasArchiveWhenDone(sess) {
		return nil
	}
	if !h.nodeHasCap(ctx, tx, nodeID, contracts.NodeCapLimitResume) {
		return nil // an older pyred: "resumable after" as before
	}
	stopped := f.PlannedAt
	if !f.Limit.At.IsZero() && f.Limit.At.Before(stopped) {
		stopped = f.Limit.At
	}
	prov := string(f.Provider)
	if prov == "" {
		prov = string(contracts.ProviderFor(sess.Harness))
	}
	r := limitResume{SessionID: f.SessionID, NodeID: nodeID, RunID: f.FromRunID, Mode: mode, AccountID: f.FromAccountID, Provider: prov,
		LimitKind: f.Limit.Kind, LimitDetail: oneLineText(f.Limit.Detail, 200), StoppedAt: stopped, ResetAt: reset.UTC(),
		DueAt: reset.UTC().Add(limitResumeSlack), State: lrScheduled, Detail: "scheduled for the reset"}
	return h.putLimitResumeTx(ctx, tx, r)
}

// limitRepeatTx reschedules a resume whose turn hit the limit again, after
// a backoff, or gives up.
func (h *Hub) limitRepeatTx(ctx context.Context, tx *sql.Tx, r limitResume, runID, mode string, reset *time.Time, accountID, why string) error {
	now := h.now()
	r.Repeats++
	if r.Repeats > limitResumeMaxRepeats {
		return h.giveUpTx(ctx, tx, r, fmt.Sprintf("%s %d times right after Pyre resumed it", why, r.Repeats))
	}
	r.RunID, r.Mode, r.State, r.CommandID, r.SentAt = runID, mode, lrScheduled, "", nil
	if accountID != "" {
		r.AccountID = accountID
	}
	due := now.Add(limitResumeBackoffFor(r.Repeats))
	if reset != nil {
		r.ResetAt = reset.UTC()
		due = laterOf(due, reset.UTC().Add(limitResumeSlack))
	}
	r.DueAt, r.EarlyAfter = due, due
	r.Detail = fmt.Sprintf("%s (repeat %d); retried after a backoff", why, r.Repeats)
	return h.putLimitResumeTx(ctx, tx, r)
}

// giveUpTx stops resuming a session and tells its parent once.
func (h *Hub) giveUpTx(ctx context.Context, tx *sql.Tx, r limitResume, why string) error {
	r.State, r.Detail, r.CommandID, r.SentAt = lrGaveUp, "gave up: "+why, "", nil
	if err := h.putLimitResumeTx(ctx, tx, r); err != nil {
		return err
	}
	return h.queueLimitReportTx(ctx, tx, r.SessionID, r.RunID, "limit_gave_up:"+r.RunID, h.now())
}

// queueLimitReportTx records a limit_resumed child report for a child
// that reports to its parent.
func (h *Hub) queueLimitReportTx(ctx context.Context, tx *sql.Tx, sessionID, runID, key string, notBefore time.Time) error {
	sess, err := h.getSession(ctx, tx, sessionID)
	if err != nil || !reportsToParent(sess) {
		return nil
	}
	state := ""
	if r, err := h.getRun(ctx, tx, runID); err == nil {
		state = string(r.State)
	}
	now := h.now()
	_, err = tx.ExecContext(ctx, `INSERT INTO child_reports(child_session_id,parent_session_id,run_id,kind,dedupe_key,run_state,created_at,not_before,status,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(child_session_id,dedupe_key) DO NOTHING`,
		sess.SessionID, sess.Lineage.ParentSessionID, runID, reportLimitResumed, key, state, persistence.Time(now), persistence.Time(notBefore), reportQueued, persistence.Time(now))
	if err == nil {
		h.reportKick.Store(true)
	}
	return err
}

// limitResumeArmed is the reset a report of run runID may promise an
// automatic resume after (false: none is scheduled).
func (h *Hub) limitResumeArmed(ctx context.Context, sessionID, runID string) (time.Time, bool) {
	if h.opt.LimitAutoResumeOff {
		return time.Time{}, false
	}
	r, ok, err := h.getLimitResume(ctx, h.db, sessionID)
	if err != nil || !ok || r.RunID != runID || (r.State != lrScheduled && r.State != lrSent) || r.Mode == limitResumeHarness {
		return time.Time{}, false
	}
	return r.ResetAt, true
}

// limitResumePending reports whether a session waits for an automatic
// resume (the nightly sweep leaves it alone).
func (h *Hub) limitResumePending(ctx context.Context, sessionID string) bool {
	if h.opt.LimitAutoResumeOff {
		return false
	}
	r, ok, err := h.getLimitResume(ctx, h.db, sessionID)
	return err == nil && ok && (r.State == lrScheduled || r.State == lrSent || r.State == lrWatching)
}

// noteAccountRoomTx brings forward the continue resumes on nodeID that an
// account of provider regaining room may serve (inside the observation's
// ingest transaction). The node decides whether it really has room.
func (h *Hub) noteAccountRoomTx(ctx context.Context, tx *sql.Tx, nodeID string, provider contracts.Provider, observedAt time.Time) error {
	if h.opt.LimitAutoResumeOff {
		return nil
	}
	now := persistence.Time(h.now())
	res, err := tx.ExecContext(ctx, `UPDATE limit_resumes SET due_at=?, detail=?, updated_at=? WHERE node_id=? AND provider=? AND mode=? AND state=? AND due_at>? AND early_after<=? AND stopped_at<?`,
		now, "an account regained room", now, nodeID, string(provider), contracts.LimitResumeContinue, lrScheduled, now, now, persistence.Time(observedAt))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		h.lres.kick.Store(true)
	}
	return nil
}

func (h *Hub) kickLimitResumes() {
	if h.lres.kick.Swap(false) {
		select {
		case h.lres.wake <- struct{}{}:
		default:
		}
	}
}

func (h *Hub) limitResumeLoop() {
	defer h.wg.Done()
	t := time.NewTicker(limitResumePoll)
	defer t.Stop()
	for {
		select {
		case <-h.stop:
			return
		case <-h.lres.wake:
		case <-t.C:
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		_ = h.ProcessLimitResumes(ctx)
		cancel()
	}
}

// ProcessLimitResumes advances every automatic resume: results of
// requests sent, repeats of resumes being watched, then at most one due
// resume (LimitResumeGap after the previous one).
func (h *Hub) ProcessLimitResumes(ctx context.Context) error {
	if h.opt.LimitAutoResumeOff {
		return nil
	}
	// Settled timers are kept a week for inspection.
	_, _ = h.db.ExecContext(ctx, `DELETE FROM limit_resumes WHERE state IN (?,?,?) AND updated_at<?`,
		lrDone, lrCancelled, lrGaveUp, persistence.Time(h.now().Add(-limitResumeRetention)))
	rows, err := h.db.QueryContext(ctx, `SELECT `+limitResumeCols+` FROM limit_resumes WHERE state IN (?,?,?) ORDER BY due_at, session_id`, lrScheduled, lrSent, lrWatching)
	if err != nil {
		return err
	}
	var all []limitResume
	for rows.Next() {
		r, err := scanLimitResume(rows)
		if err != nil {
			rows.Close()
			return err
		}
		all = append(all, r)
	}
	rows.Close()
	var first error
	keep := func(err error) {
		if err != nil && first == nil {
			first = err
		}
	}
	now := h.now()
	var due []limitResume
	for _, r := range all {
		switch r.State {
		case lrSent:
			keep(h.checkLimitResumeSent(ctx, r))
		case lrWatching:
			keep(h.checkLimitResumeWatch(ctx, r))
		case lrScheduled:
			if !r.DueAt.After(now) {
				due = append(due, r)
			}
		}
	}
	if len(due) == 0 {
		return first
	}
	var last sql.NullString
	_ = h.db.QueryRowContext(ctx, `SELECT MAX(sent_at) FROM limit_resumes WHERE sent_at IS NOT NULL`).Scan(&last)
	if t := persistence.ParseTime(last.String); !t.IsZero() && now.Sub(t) < h.opt.LimitResumeGap {
		return first
	}
	// Leads before the sessions they started.
	parentless := func(r limitResume) bool {
		s, err := h.getSession(ctx, h.db, r.SessionID)
		return err == nil && s.Parent() == ""
	}
	ordered := make([]limitResume, 0, len(due))
	for _, r := range due {
		if parentless(r) {
			ordered = append(ordered, r)
		}
	}
	for _, r := range due {
		if !parentless(r) {
			ordered = append(ordered, r)
		}
	}
	for _, r := range ordered {
		sent, err := h.fireLimitResume(ctx, r)
		keep(err)
		if sent {
			break
		}
	}
	return first
}

// cancelLimitResume drops a resume the session must not get.
func (h *Hub) cancelLimitResume(ctx context.Context, r limitResume, why string) error {
	return h.tx(ctx, func(tx *sql.Tx) error {
		cur, ok, err := h.getLimitResume(ctx, tx, r.SessionID)
		if err != nil || !ok || cur.State != r.State || cur.RunID != r.RunID {
			return err
		}
		cur.State, cur.Detail = lrCancelled, why
		return h.putLimitResumeTx(ctx, tx, cur)
	})
}

// limitInputSince reports whether anyone gave the session input after
// since: a message to it from the owner or another session. Pyre's own
// messages (system senders, child reports, notes a session's node sends
// as the session itself) do not count; the owner typing or attaching is
// ownerTouchedSince.
func (h *Hub) limitInputSince(ctx context.Context, sessionID string, since time.Time) bool {
	var n int
	err := h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE recipient_kind='session' AND recipient_id=? AND stored_at>?
		AND sender_kind!=? AND NOT (sender_kind=? AND sender_id=?)
		AND message_id NOT IN (SELECT message_id FROM child_reports WHERE message_id IS NOT NULL)`,
		sessionID, persistence.Time(since), string(contracts.PrincipalSystem), string(contracts.PrincipalSession), sessionID).Scan(&n)
	return err != nil || n > 0 // unknown counts as input
}

// fireLimitResume asks the node to resume r once the hub's checks pass. It
// reports whether a request was sent (the gap applies to the next one).
func (h *Hub) fireLimitResume(ctx context.Context, r limitResume) (bool, error) {
	s, err := h.getSession(ctx, h.db, r.SessionID)
	if contracts.IsCode(err, contracts.ErrNotFound) {
		return false, h.cancelLimitResume(ctx, r, "the session no longer exists")
	} else if err != nil {
		return false, err
	}
	switch {
	case s.Archived:
		return false, h.cancelLimitResume(ctx, r, "the session was archived")
	case hasArchiveWhenDone(s):
		return false, h.cancelLimitResume(ctx, r, "the session is being archived")
	case s.CurrentRunID != r.RunID:
		return false, h.cancelLimitResume(ctx, r, "the session has another run ("+s.CurrentRunID+")")
	}
	run, err := h.getRun(ctx, h.db, r.RunID)
	if err != nil {
		return false, err
	}
	now := h.now()
	switch r.Mode {
	case contracts.LimitResumeContinue, limitResumeHarness:
		if !run.State.Terminal() {
			if now.Sub(r.DueAt) > limitResumeEndWait {
				return false, h.cancelLimitResume(ctx, r, "its run did not end ("+string(run.State)+")")
			}
			return false, nil // the end report may still be on its way
		}
	case contracts.LimitResumeInPlace:
		if run.State.Terminal() {
			return false, h.cancelLimitResume(ctx, r, "its run ended")
		}
		if run.State != contracts.RunWaitingInput {
			return false, h.cancelLimitResume(ctx, r, "its run is "+string(run.State)+" (new input)")
		}
	}
	// The owner's typing is marked at most once a minute (owner_activity.go)
	// and an attach by when it was opened: engagement in the minute before
	// the stop may be all there is to see of him working in it then.
	if h.ownerTouchedSince(ctx, s, r.RunID, r.StoppedAt.Add(-limitOwnerMargin)) {
		return false, h.cancelLimitResume(ctx, r, "the owner engaged with it since the stop")
	}
	if h.limitInputSince(ctx, s.SessionID, r.StoppedAt) {
		return false, h.cancelLimitResume(ctx, r, "it got new input since the stop")
	}
	if ok, _ := h.nodeConnected(ctx, h.db, r.NodeID); !ok {
		return false, nil // asked once the node is back
	}
	if r.Mode == limitResumeHarness {
		return h.fireHarnessFallback(ctx, s, run, r) // harnessfallback.go
	}
	if !h.nodeHasCap(ctx, h.db, r.NodeID, contracts.NodeCapLimitResume) {
		return false, h.cancelLimitResume(ctx, r, "its node's pyred no longer serves automatic limit resumes")
	}
	params, _ := json.Marshal(contracts.LimitResumeParams{SessionID: r.SessionID, RunID: r.RunID, Mode: r.Mode})
	sent := false
	err = h.tx(ctx, func(tx *sql.Tx) error {
		cur, ok, err := h.getLimitResume(ctx, tx, r.SessionID)
		if err != nil || !ok || cur.State != lrScheduled || cur.RunID != r.RunID {
			return err
		}
		sys := contracts.Principal{Kind: contracts.PrincipalSystem, ID: "hub"}
		req := contracts.NodeActionRequest{NodeID: r.NodeID, Action: contracts.ActionSessionLimitResume, Params: contracts.RawJSON(params)}
		cmd, err := h.enqueueTx(ctx, tx, r.NodeID, contracts.CmdAction, sys, req, h.opt.ActionTTL, contracts.ObjectRef{Kind: contracts.KindSession, ID: r.SessionID})
		if err != nil {
			return err
		}
		at := h.now()
		cur.State, cur.CommandID, cur.SentAt, cur.Detail = lrSent, cmd.CommandID, &at, "asked the node"
		sent = true
		return h.putLimitResumeTx(ctx, tx, cur)
	})
	return sent, err
}

// retryLimitResumeTx puts a request the node could not serve back on the
// timer, or gives up after limitResumeMaxTries.
func (h *Hub) retryLimitResumeTx(ctx context.Context, tx *sql.Tx, r limitResume, due time.Time, why string) error {
	r.Tries++
	if r.Tries > limitResumeMaxTries {
		return h.giveUpTx(ctx, tx, r, why)
	}
	r.State, r.CommandID, r.SentAt, r.DueAt, r.Detail = lrScheduled, "", nil, due, why
	if r.EarlyAfter.Before(h.now().Add(limitResumeEarlyGap)) {
		r.EarlyAfter = h.now().Add(limitResumeEarlyGap)
	}
	return h.putLimitResumeTx(ctx, tx, r)
}

// checkLimitResumeSent applies the node's answer to a request.
func (h *Hub) checkLimitResumeSent(ctx context.Context, r limitResume) error {
	rec, err := h.getCommand(ctx, h.db, r.CommandID)
	now := h.now()
	if err != nil && !contracts.IsCode(err, contracts.ErrNotFound) {
		return err
	}
	lost := err != nil || (!rec.finished() && r.SentAt != nil && now.Sub(*r.SentAt) >= limitResumeSentMax)
	if err == nil && !rec.finished() && !lost {
		return nil
	}
	var res contracts.LimitResumeResult
	if !lost && rec.State == cmdDone && rec.Error == nil && rec.Outcome == contracts.OutcomeSucceeded {
		if json.Unmarshal(rec.Result, &res) != nil {
			res = contracts.LimitResumeResult{}
		}
	}
	return h.tx(ctx, func(tx *sql.Tx) error {
		cur, ok, err := h.getLimitResume(ctx, tx, r.SessionID)
		if err != nil || !ok || cur.State != lrSent || cur.CommandID != r.CommandID {
			return err
		}
		switch {
		case lost:
			// No answer (pyred restarted mid-request): a continuation that
			// started shows as the session's new run; otherwise asking
			// again is safe (the node refuses a run that is no longer
			// current, and the in-place prompt has a stable ID).
			if s, err := h.getSession(ctx, tx, cur.SessionID); err == nil && s.CurrentRunID != cur.RunID {
				if nr, err := h.getRun(ctx, tx, s.CurrentRunID); err == nil && nr.PreviousRunID == cur.RunID {
					return h.limitResumedTx(ctx, tx, cur, contracts.LimitResumeResult{Outcome: contracts.LimitResumed, AccountID: nr.AccountID, NewRunID: nr.RunID,
						Kind: string(nr.ContinuationKind)})
				}
			}
			return h.retryLimitResumeTx(ctx, tx, cur, now.Add(h.opt.LimitResumeGap), "the node did not answer; asking again")
		case rec.State == cmdExpired || rec.State == cmdCancelled:
			return h.retryLimitResumeTx(ctx, tx, cur, now.Add(h.opt.LimitResumeGap), "the request expired before the node received it")
		case rec.Outcome == contracts.OutcomeUnsupported || (rec.Error != nil && rec.Error.Code == contracts.ErrUnsupported):
			cur.State, cur.Detail = lrCancelled, "the node does not serve automatic limit resumes"
			return h.putLimitResumeTx(ctx, tx, cur)
		case rec.Error != nil || rec.Outcome != contracts.OutcomeSucceeded:
			why := "the node could not resume it"
			if rec.Error != nil {
				why += ": " + oneLineText(rec.Error.Message, 200)
			}
			return h.retryLimitResumeTx(ctx, tx, cur, now.Add(limitResumeRetry), why)
		}
		switch res.Outcome {
		case contracts.LimitResumed:
			return h.limitResumedTx(ctx, tx, cur, res)
		case contracts.LimitResumeNoRoom:
			due := now.Add(limitResumeRetry)
			if res.ResetAt != nil {
				// A reset already past (the node still sees the limit):
				// not before limitResumeMinRetry.
				due = laterOf(res.ResetAt.UTC().Add(limitResumeSlack), now.Add(limitResumeMinRetry))
				cur.ResetAt = res.ResetAt.UTC()
			}
			return h.retryLimitResumeTx(ctx, tx, cur, due, "no account has room yet: "+oneLineText(res.Detail, 200))
		default:
			cur.State, cur.Detail = lrCancelled, "the node refused: "+oneLineText(res.Detail, 200)
			return h.putLimitResumeTx(ctx, tx, cur)
		}
	})
}

// limitResumedTx records a resume: audited, watched for a repeat, and
// reported to the parent once it holds.
func (h *Hub) limitResumedTx(ctx context.Context, tx *sql.Tx, r limitResume, res contracts.LimitResumeResult) error {
	now := h.now()
	newRun := res.NewRunID
	if newRun == "" {
		newRun = r.RunID
	}
	r.State, r.CommandID, r.ResumedRun, r.ResumedAccount, r.ResumedKind, r.ResumedAt = lrWatching, "", newRun, res.AccountID, res.Kind, &now
	r.Detail = "resumed; watching for a repeat limit"
	if err := h.putLimitResumeTx(ctx, tx, r); err != nil {
		return err
	}
	to := fmt.Sprintf("on account %q", h.accountLabelTx(ctx, tx, res.AccountID))
	if how := limitResumeHow(res.Kind); how != "" {
		to += " (" + how + ")"
	}
	h.auditTx(ctx, tx, auditRec{Action: contracts.AuditSessionLimitResumed, Automatic: true, Actor: pyreActor, NodeID: r.NodeID, SessionID: r.SessionID,
		RunID: newRun, AccountID: res.AccountID, To: to, Reason: fmt.Sprintf("after %q's %s", h.accountLabelTx(ctx, tx, r.AccountID), limitResumeNoun(r))})
	return h.queueLimitReportTx(ctx, tx, r.SessionID, newRun, fmt.Sprintf("limit_resumed:%s:%d", newRun, r.Repeats), now.Add(limitResumeConfirm))
}

// accountLabelTx is accountLabelOf inside a transaction.
func (h *Hub) accountLabelTx(ctx context.Context, q queryer, id string) string {
	if a, err := h.getAccount(ctx, q, id); err == nil && a.Label != "" {
		return a.Label
	}
	if id == "" {
		return "an account"
	}
	return id
}

func limitResumeHow(kind string) string {
	switch kind {
	case string(contracts.ContinuationNativeResume):
		return "native resume, conversation kept"
	case string(contracts.ContinuationHandoff):
		return "continued from a handoff note"
	case contracts.LimitResumeInPlace:
		return "the same run, told to continue"
	}
	return ""
}

// limitResumeNoun names the limit a timer is for ("spend limit", ...).
func limitResumeNoun(r limitResume) string {
	return limitNoun(&contracts.FallbackPlan{Limit: &contracts.LimitEvent{Kind: r.LimitKind, Detail: r.LimitDetail}})
}

// checkLimitResumeWatch ends the watch of a resume, or counts a turn of an
// in-place resume that hit the limit again (a continue resume's repeat
// arrives as the new run's fallback plan: armLimitResumeTx).
func (h *Hub) checkLimitResumeWatch(ctx context.Context, r limitResume) error {
	now := h.now()
	since := now
	if r.ResumedAt != nil {
		since = *r.ResumedAt
	}
	run, err := h.getRun(ctx, h.db, r.ResumedRun)
	repeat := false
	if err == nil && r.Mode == contracts.LimitResumeInPlace {
		if at := persistence.ParseTime(run.AdapterData[contracts.AdapterDataLimitAt]); at.After(since) {
			repeat = true
		}
	}
	if !repeat && now.Sub(since) < limitResumeWatch {
		return nil // a continuation that ended meanwhile is judged by its fallback plan (armLimitResumeTx)
	}
	return h.tx(ctx, func(tx *sql.Tx) error {
		cur, ok, err := h.getLimitResume(ctx, tx, r.SessionID)
		if err != nil || !ok || cur.State != lrWatching || cur.ResumedRun != r.ResumedRun {
			return err
		}
		if repeat {
			return h.limitRepeatTx(ctx, tx, cur, cur.RunID, contracts.LimitResumeInPlace, nil, "", "the resumed turn hit the limit again")
		}
		cur.State, cur.Detail = lrDone, "resumed"
		return h.putLimitResumeTx(ctx, tx, cur)
	})
}

// limitReportStill decides a limit_resumed child report when it is due:
// "" to send it, else why it is skipped.
func (h *Hub) limitReportStill(ctx context.Context, cr childReport) string {
	r, ok, err := h.getLimitResume(ctx, h.db, cr.Child)
	if err != nil || !ok {
		return "the automatic resume is gone"
	}
	if strings.HasPrefix(cr.Key, "limit_gave_up:") {
		if r.State != lrGaveUp {
			return "Pyre resumes it automatically again"
		}
		return ""
	}
	if r.Mode == limitResumeHarness {
		if r.RunID != cr.RunID || r.State != lrDone {
			return "the restart on Claude did not happen"
		}
		return ""
	}
	if r.ResumedRun != cr.RunID || (r.State != lrWatching && r.State != lrDone) {
		return "the resumed turn hit the limit again; Pyre retries later"
	}
	return ""
}

// limitRepeatSkip: cr is the idle or ended report of a continuation Pyre
// started that hit the limit again (a repeat being retried, or the last
// one before Pyre gave up). The parent already read that the session
// resumes automatically and hears again when it resumes or when Pyre
// gives up, so this one is not sent ("" sends it).
func (h *Hub) limitRepeatSkip(ctx context.Context, cr childReport) string {
	if h.opt.LimitAutoResumeOff {
		return "" // nothing resumes it: the parent hears as before
	}
	switch {
	case cr.Kind == reportIdle && failedTurn(cr.Reason):
	case cr.Kind == reportEnded:
		if run, err := h.getRun(ctx, h.db, cr.RunID); err != nil || run.AdapterData[contracts.AdapterDataAccountSwitch] != contracts.AccountSwitchLimit {
			return ""
		}
	default:
		return "" // a turn that ended normally is reported as usual
	}
	r, ok, err := h.getLimitResume(ctx, h.db, cr.Child)
	// In place, the run keeps its ID and I69 already reports its limit once
	// per run: only continuations (new runs) are suppressed here.
	if err != nil || !ok || r.Repeats == 0 || r.RunID != cr.RunID || r.Mode != contracts.LimitResumeContinue {
		return ""
	}
	switch r.State {
	case lrScheduled, lrSent, lrWatching:
		return fmt.Sprintf("the continuation Pyre resumed hit the limit again; Pyre retries it automatically (repeat %d)", r.Repeats)
	case lrGaveUp:
		return "the continuation Pyre resumed hit the limit again; Pyre's give-up report covers it"
	}
	return ""
}

// renderLimitResumed writes a limit_resumed child report.
func (h *Hub) renderLimitResumed(ctx context.Context, b *strings.Builder, cr childReport, title, name, rel, facts string) {
	r, _, _ := h.getLimitResume(ctx, h.db, cr.Child)
	from := h.accountLabelOf(ctx, r.AccountID)
	noun := limitResumeNoun(r)
	if strings.HasPrefix(cr.Key, "limit_gave_up:") {
		fmt.Fprintf(b, "%s is still stopped on %q's %s; Pyre stopped resuming it automatically\n", title, from, noun)
		fmt.Fprintf(b, "[pyre auto-report] Session %s, %s, %s%s", name, rel, oneLineText(r.Detail, 300), facts)
		b.WriteString("It is not finished. Resume it when an account has room, or tell the owner.\n")
		return
	}
	if r.Mode == limitResumeHarness {
		h.renderHarnessFallbackResumed(ctx, b, r, title, name, rel, facts) // harnessfallback.go
		return
	}
	to := h.accountLabelOf(ctx, r.ResumedAccount)
	fmt.Fprintf(b, "%s resumed on account %q\n", title, to)
	how := limitResumeHow(r.ResumedKind)
	if how != "" {
		how = "; " + how
	}
	fmt.Fprintf(b, "[pyre auto-report] Session %s, %s, was resumed by Pyre on account %q after %q's %s%s%s", name, rel, to, from, noun, how, facts)
	b.WriteString("It was told to continue where it left off.\n")
}
