package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/persistence"
	"github.com/jhh3/pyre/internal/runfail"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// Automatic reports to the parent session (W-childnotify).
//
// A session started by another session (Lineage.ParentSessionID, tagged
// contracts.TagReportParent) tells its parent when it reaches a reportable
// point, so the parent does not depend on prompt wording to hear back:
//
//   - idle: a turn ended and the run waits for input (once per turn; the
//     report carries the child's last assistant message);
//   - waiting: a turn ended, but the child said it waits on its own job (a
//     CI run, review or watcher it started; waiting_on.go), so it is not
//     idle; waiting_overdue follows when no new turn started by the time
//     the child expected the job to finish;
//   - needs_input: the run waits on an approval or question;
//   - ended: the run exited or failed.
//
// Detection happens inside the run-state transaction (trackChildTx), which
// records a durable report row; a background worker renders and sends it
// after commit as an ordinary Pyre message from the child to the parent
// (queue mode, so the parent's doorbell/[pyre-msg:…] delivery applies and
// a reply threads back to the child).
//
// Noise rules (I30; live data: 45 % of idle reports followed a message the
// child had just sent its parent, hundreds of idle→ended pairs minutes apart):
//
//   - an idle report is skipped when the child itself messaged the parent
//     within childRecentMessage before the turn ended (it sent its result
//     and then went idle). An earlier message, even one in the same turn,
//     does not count: a long turn's mid-turn question or coordination ask
//     is not its result (issue iss_01M5J3XGZGGWF1APPPE7PZ0B25: a worker
//     asked at 07:57, 08:09 and 08:25 and ended at 08:37 with its result;
//     its idle report was skipped and its ended report folded, so the lead
//     never heard). The window is used rather than the transcript's last
//     assistant entry, which usually follows the send_message call that
//     carried the result ("Sent the result to the parent.");
//   - an ended report replaces the same turn's idle report when that one is
//     still pending or undelivered (cancelled), is skipped when the idle
//     report went out within childReportCoalesce, and otherwise refers to it
//     instead of repeating the last message; one that replaced the idle
//     report after a clean end (exit code 0, or Pyre's archive-when-done
//     stop) is folded when the child messaged the parent within
//     childRecentMessage before that turn ended (childMessagedTurn);
//   - an archived child still reports (a bounded child archiving itself at
//     the end of its last turn is the normal finish: "<title> finished and
//     archived itself: …"). Not reported: a turn that ended before the
//     parent archived the child or scheduled it (audit log, archivedBy; a
//     mid-turn schedule still gets that turn's result), and the ended
//     report of a child the owner or Pyre archived after its turn was
//     already reported (an owner bulk archive of idle children);
//   - a prompted child whose run went from starting straight to
//     waiting_input ran its first turn between two observations
//     (launchTurnTx); that turn is reported after childLaunchSettle once
//     the run is idle with an assistant message (launchTurnStill), unless
//     a running or approval observation shows the run was idle before it
//     began;
//   - the body leads with one line "<title> finished: <first sentence>" and
//     carries at most childSummaryMax runes of the last message.
//
// A report for a parent without a live run waits (pending) for the
// parent's next run, up to childReportHold; it is dropped when the parent
// is archived. The owner's inbox is never used as a fallback.
//
// Batching (setup review F3/F5, Oct 2026: about 8k reports in two weeks,
// leads answering reports instead of the owner):
//
//   - a turn-end report (idle, waiting, ended, waiting_overdue) for a parent
//     that is mid-turn is held until the parent's turn ends (at most
//     childBatchMax); then every report ready for that parent goes out as
//     one digest message (sender: the system, each section names its child);
//     a lone report still comes from its child, so a reply threads to it;
//   - a clean exit whose last message already reached the parent (the
//     turn's idle report went out, or the child messaged the parent within
//     childRecentMessage before the turn ended, as for the idle rule) wakes
//     nobody: it is folded into the next report that parent gets ("also
//     exited"), or dropped after childReportHold;
//   - needs_input reports, and the ended report of a run that failed to
//     launch (urgentReport), are never batched.

var childReportMigrations = []sqlitex.Migration{{Namespace: "hub_childreport", Version: 1, Name: "child_reports", SQL: `
CREATE TABLE child_turns (
	session_id TEXT PRIMARY KEY,
	turn_seq INTEGER NOT NULL DEFAULT 0,
	turn_started_at TEXT,
	closed_seq INTEGER NOT NULL DEFAULT 0,
	closed_at TEXT,
	needs_seq INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE child_reports (
	report_id INTEGER PRIMARY KEY AUTOINCREMENT,
	child_session_id TEXT NOT NULL,
	parent_session_id TEXT NOT NULL,
	run_id TEXT NOT NULL,
	kind TEXT NOT NULL,
	dedupe_key TEXT NOT NULL,
	run_state TEXT NOT NULL,
	state_reason TEXT,
	exit_code INTEGER,
	turn_started_at TEXT,
	created_at TEXT NOT NULL,
	not_before TEXT NOT NULL,
	status TEXT NOT NULL,
	attempts INTEGER NOT NULL DEFAULT 0,
	body TEXT,
	message_id TEXT,
	detail TEXT,
	updated_at TEXT NOT NULL,
	UNIQUE (child_session_id, dedupe_key)
);
CREATE INDEX child_reports_pending ON child_reports(status, not_before);
CREATE INDEX child_reports_message ON child_reports(message_id);
`}, {Namespace: "hub_childreport", Version: 2, Name: "child_report_hold_claim", SQL: `
ALTER TABLE child_reports ADD COLUMN hold_state TEXT NOT NULL DEFAULT '';
ALTER TABLE child_reports ADD COLUMN send_op TEXT;
ALTER TABLE child_reports ADD COLUMN send_body TEXT;
ALTER TABLE child_reports ADD COLUMN send_sender TEXT;
UPDATE child_reports SET hold_state='held' WHERE status='pending' AND detail='held: the parent has no live run';
CREATE INDEX child_reports_send_op ON child_reports(send_op) WHERE send_op IS NOT NULL;
`}, {Namespace: "hub_childreport", Version: 3, Name: "child_report_turn_ended", SQL: `
ALTER TABLE child_reports ADD COLUMN turn_ended_at TEXT;
`}}

// Report kinds and statuses.
const (
	reportIdle    = "idle"
	reportWaiting = "waiting"
	reportOverdue = "waiting_overdue"
	reportNeeds   = "needs_input"
	reportEnded   = "ended"
	reportQueued  = "pending"
	reportSent    = "sent"
	reportSkip    = "skipped"
	reportFailed  = "failed"
)

const (
	// childSummaryMax bounds the relayed last assistant message (runes);
	// read_session has the rest.
	childSummaryMax = 400
	// childLeadMax bounds the first sentence on the lead line (runes).
	childLeadMax = 160
	// needsInputDelay lets the approval report that accompanies a
	// waiting_approval state land before the report is rendered.
	needsInputDelay = 2 * time.Second
	// childReportAttempts bounds send retries (capacity, transient errors).
	childReportAttempts = 5
	// childReportBurst/childReportWindow bound reports from one child, so a
	// parent and child that keep answering each other cannot loop forever.
	childReportBurst  = 12
	childReportWindow = 10 * time.Minute
	// childRecentMessage: an idle report is skipped (a clean ended report
	// folded) when the child messaged its parent at most this long before
	// the turn ended (even in an earlier turn); an older message, even in
	// the same turn, is not taken for the turn's result.
	childRecentMessage = 5 * time.Minute
	// childReportCoalesce: an ended report this soon after the same turn's
	// idle report went out is not sent.
	childReportCoalesce = 3 * time.Minute
	// childReportRecheck/childReportHold: a report for a parent without a
	// live run is re-checked this often (and at once when a parent run
	// becomes live), for at most childReportHold.
	childReportRecheck = time.Minute
	childReportHold    = 3 * 24 * time.Hour
	// childReportRetention keeps settled report rows (sent/skipped/failed).
	childReportRetention = 7 * 24 * time.Hour
	// childBatchMax: a report held for a busy parent goes out after this
	// long even if the parent's turn has not ended.
	childBatchMax = 20 * time.Minute
	// childLaunchSettle: the report of a first turn counted without a
	// running observation (launchTurnTx) waits this long, so a run that
	// was idle only before its initial prompt's turn began is not
	// reported (hub data, Oct 2026: launch idle to running took at most
	// 18 s over 660 prompted launches; p99 8 s).
	childLaunchSettle = 30 * time.Second
	// childLaunchWait: a launch report still without evidence of the
	// turn (launchTurnStill) after this long is dropped.
	childLaunchWait = 30 * time.Minute
)

// launchKeySuffix marks the dedupe keys of a first turn's reports counted
// at launch (launchTurnTx): "idle:1" + launchKeySuffix.
const launchKeySuffix = ":launch"

// reportsToParent reports whether s reports to its parent session.
func reportsToParent(s contracts.Session) bool {
	return s.Lineage.ParentSessionID != "" && slices.Contains(s.Tags, contracts.TagReportParent)
}

// reportTags applies the start request's report choice to the session tags:
// the reserved report:* tags a caller supplied are replaced by the hub's
// decision. creatorIsSession selects the default.
func reportTags(tags []string, parent string, want *bool, creatorIsSession bool) []string {
	optOut := slices.Contains(tags, contracts.TagReportNone)
	out := make([]string, 0, len(tags)+1)
	for _, t := range tags {
		if t != contracts.TagReportParent && t != contracts.TagReportNone {
			out = append(out, t)
		}
	}
	if parent == "" {
		return out
	}
	on := creatorIsSession
	if want != nil {
		on = *want
	}
	if optOut {
		on = false
	}
	switch {
	case on:
		return append(out, contracts.TagReportParent)
	case optOut || want != nil:
		return append(out, contracts.TagReportNone) // an explicit opt-out stays visible
	}
	return out // an owner start naming a parent: unchanged, no tag
}

// trackChildTx observes a run state transition of a session that reports to
// its parent and records any report it calls for. It runs inside the
// transaction that stores the transition.
func (h *Hub) trackChildTx(ctx context.Context, tx *sql.Tx, prev contracts.RunState, r contracts.Run) error {
	if prev == r.State {
		return nil
	}
	if r.State == contracts.RunRunning || r.State == contracts.RunWaitingInput || r.State == contracts.RunWaitingBackground {
		if err := h.wakeHeldReportsTx(ctx, tx, r.SessionID, r.State != contracts.RunRunning); err != nil {
			return err
		}
	}
	sess, err := h.getSession(ctx, tx, r.SessionID)
	if err != nil {
		if contracts.IsCode(err, contracts.ErrNotFound) {
			return nil
		}
		return err
	}
	if cur := sess.CurrentRunID; cur != "" && cur != r.RunID && r.PreviousRunID != cur {
		return nil // a non-current (conflicting) run speaks for nobody
	}
	if !reportsToParent(sess) {
		if sess.Parent() == "" {
			return h.trackLeadTx(ctx, tx, prev, r, sess) // waiting on you (waiting.go)
		}
		return h.trackChildFailureTx(ctx, tx, prev, r, sess)
	}
	var turn, closed, needs int64
	var startedAt, closedAt sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT turn_seq, turn_started_at, closed_seq, closed_at, needs_seq FROM child_turns WHERE session_id=?`, sess.SessionID).
		Scan(&turn, &startedAt, &closed, &closedAt, &needs)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	now := h.now()
	var turnEnded sql.NullString // ended reports: when the turn it covers ended
	enqueue := func(kind, key string, notBefore time.Time) error {
		var exit any
		if r.ExitCode != nil {
			exit = *r.ExitCode
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO child_reports(child_session_id,parent_session_id,run_id,kind,dedupe_key,run_state,state_reason,exit_code,turn_started_at,turn_ended_at,created_at,not_before,status,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(child_session_id,dedupe_key) DO NOTHING`,
			sess.SessionID, sess.Lineage.ParentSessionID, r.RunID, kind, key, string(r.State), persistence.NullStr(r.StateReason), exit,
			startedAt, turnEnded, persistence.Time(now), persistence.Time(notBefore), reportQueued, persistence.Time(now))
		if err == nil {
			h.reportKick.Store(true)
		}
		return err
	}
	// reopenLaunch: a first turn counted at launch (launchTurnTx) whose
	// report is still unsent was not a turn when the run is seen working
	// (running, or an approval the prompt's turn asks for): the run was
	// idle before its initial prompt's turn began, and this is that turn.
	reopenLaunch := func() error {
		if turn != 1 || closed != 1 {
			return nil
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM child_reports WHERE child_session_id=? AND dedupe_key IN (?,?,?) AND status=? AND send_op IS NULL`,
			sess.SessionID, reportIdle+":1"+launchKeySuffix, reportWaiting+":1"+launchKeySuffix, "overdue:1"+launchKeySuffix, reportQueued)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			closed, closedAt = 0, sql.NullString{}
		}
		return nil
	}
	switch {
	case r.State == contracts.RunRunning:
		// A new turn starts unless the current one is still open (a run
		// returning from an approval, a lost observation or background
		// waiting continues the same turn). Its window for "the child
		// already messaged the parent" starts where the previous turn
		// closed (observations can lag the child's own messages).
		if err := reopenLaunch(); err != nil {
			return err
		}
		if turn == 0 || closed >= turn {
			turn++
			startedAt = closedAt
			if !startedAt.Valid {
				startedAt = sql.NullString{String: persistence.Time(sess.CreatedAt), Valid: true}
			}
		}
	case r.State == contracts.RunWaitingInput:
		due, suffix := now, ""
		if turn == 0 && h.launchTurnTx(ctx, tx, r) {
			// The initial prompt's turn ran between two observations.
			turn = 1
			startedAt = sql.NullString{String: persistence.Time(sess.CreatedAt), Valid: true}
			due, suffix = now.Add(childLaunchSettle), launchKeySuffix
		}
		if turn > closed {
			closed = turn
			closedAt = sql.NullString{String: persistence.Time(now), Valid: true}
			seq := strconv.FormatInt(turn, 10) + suffix
			if w, ok := h.waitForTurnTx(ctx, tx, sess.SessionID, turn); ok { // waiting_on.go
				if err := enqueue(reportWaiting, reportWaiting+":"+seq, due); err != nil {
					return err
				}
				if w.Quiet { // recorded, not sent: the turn counts as not reported
					if _, err := tx.ExecContext(ctx, `UPDATE child_reports SET status=?, detail=?, updated_at=? WHERE child_session_id=? AND dedupe_key=? AND status=? AND send_op IS NULL`,
						reportSkip, reportQuietDetail, persistence.Time(now), sess.SessionID, reportWaiting+":"+seq, reportQueued); err != nil {
						return err
					}
				}
				if err := enqueue(reportOverdue, "overdue:"+seq, w.Until); err != nil {
					return err
				}
			} else if err := enqueue(reportIdle, reportIdle+":"+seq, due); err != nil {
				return err
			}
		}
	case r.State == contracts.RunWaitingApproval:
		if err := reopenLaunch(); err != nil {
			return err
		}
		needs++
		if err := enqueue(reportNeeds, "needs:"+strconv.FormatInt(needs, 10), now.Add(needsInputDelay)); err != nil {
			return err
		}
	case r.State.Terminal() && !prev.Terminal():
		// The ended report's turn ended now, or (the run was idle) when it
		// went idle; unknown when no turn was ever counted.
		if turn > closed {
			turnEnded = sql.NullString{String: persistence.Time(now), Valid: true}
		} else if turn > 0 {
			turnEnded = closedAt
		}
		closed = turn
		closedAt = sql.NullString{String: persistence.Time(now), Valid: true}
		// One report for the turn: the ended report carries the last
		// message, so this turn's unsent idle report is dropped. The
		// ended report applies the idle report's rules in its place
		// (prepareChildReport: the child messaged the parent; only a
		// parent that archived the child is not told).
		if _, err := tx.ExecContext(ctx, `UPDATE child_reports SET status=?, detail=?, updated_at=? WHERE child_session_id=? AND kind IN (?,?) AND status=? AND turn_started_at IS ?`,
			reportSkip, reportReplacedDetail, persistence.Time(now), sess.SessionID, reportIdle, reportWaiting, reportQueued, startedAt); err != nil {
			return err
		}
		if err := enqueue(reportEnded, "ended:"+r.RunID, now); err != nil {
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

// launchTurnTx reports whether a run that reached waiting_input with no
// turn counted yet ran a turn the hub never saw running: it was started
// with an initial prompt, so it went idle after that prompt's turn (one
// shorter than the node's observation interval; a Codex run that answered
// "ok" in 1.6 s went starting -> waiting_input). A run without a prompt
// (or a resumed one) is idle at launch and has no turn. When the idle
// state came before the prompt's turn instead, the running observation
// that follows withdraws the turn (trackChildTx), within
// childLaunchSettle.
func (h *Hub) launchTurnTx(ctx context.Context, tx *sql.Tx, r contracts.Run) bool {
	var payload string
	err := tx.QueryRowContext(ctx, `SELECT c.payload FROM runs r JOIN commands c ON c.command_id=r.start_command_id WHERE r.run_id=? AND c.kind=?`,
		r.RunID, string(contracts.CmdStartRun)).Scan(&payload)
	if err != nil {
		return false
	}
	var sc contracts.StartRunCommand
	return json.Unmarshal([]byte(payload), &sc) == nil && strings.TrimSpace(sc.InitialPrompt) != "" && sc.PreviousRun == nil
}

// trackChildFailureTx: a session under a parent that does not take
// reports (opted out, or reparented) still tells that parent when a run
// fails, since the owner's inbox no longer carries failures of sessions
// that sit under another session (attention deriver). One ended report per
// run, same row and worker as the tagged path, so nothing is sent twice.
func (h *Hub) trackChildFailureTx(ctx context.Context, tx *sql.Tx, prev contracts.RunState, r contracts.Run, sess contracts.Session) error {
	parent := sess.Parent()
	if parent == "" || r.State != contracts.RunFailed || prev.Terminal() {
		return nil
	}
	now := h.now()
	var exit any
	if r.ExitCode != nil {
		exit = *r.ExitCode
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO child_reports(child_session_id,parent_session_id,run_id,kind,dedupe_key,run_state,state_reason,exit_code,turn_started_at,created_at,not_before,status,updated_at)
		VALUES(?,?,?,?,?,?,?,?,NULL,?,?,?,?) ON CONFLICT(child_session_id,dedupe_key) DO NOTHING`,
		sess.SessionID, parent, r.RunID, reportEnded, "ended:"+r.RunID, string(r.State), persistence.NullStr(r.StateReason), exit,
		persistence.Time(now), persistence.Time(now), reportQueued, persistence.Time(now))
	if err == nil {
		h.reportKick.Store(true)
	}
	return err
}

// reportsTo reports whether child still reports cr to cr.Parent: its
// reports are on and go there, or cr is a failure and cr.Parent is the
// session it sits under (trackChildFailureTx).
func reportsTo(child contracts.Session, cr childReport) bool {
	if reportsToParent(child) && child.Lineage.ParentSessionID == cr.Parent {
		return true
	}
	return cr.Kind == reportEnded && cr.RunState == contracts.RunFailed && child.Parent() == cr.Parent
}

// Pending report hold states (hold_state; detail says the same for
// people): held for the parent's next run, batched until the parent's
// turn ends, folded into the next report.
const (
	holdHeld    = "held"
	holdBatched = "batched"
	holdFolded  = "folded"

	reportHeldDetail    = "held: the parent has no live run"
	reportBatchedDetail = "batched: the parent is mid-turn"
	reportFoldDetail    = "folded: clean exit already reported; rides along with the parent's next report"
)

// wakeHeldReportsTx makes reports held for parent (no live run) due now:
// its run is live again. idle (its turn ended) also releases reports
// batched while it was busy.
func (h *Hub) wakeHeldReportsTx(ctx context.Context, tx *sql.Tx, parent string, idle bool) error {
	now := persistence.Time(h.now())
	batched := holdHeld
	if idle {
		batched = holdBatched
	}
	res, err := tx.ExecContext(ctx, `UPDATE child_reports SET not_before=? WHERE status=? AND parent_session_id=? AND hold_state IN (?,?) AND not_before>?`,
		now, reportQueued, parent, holdHeld, batched, now)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		h.reportKick.Store(true)
	}
	return nil
}

// childReport is one pending report row.
type childReport struct {
	ID        int64
	Child     string
	Parent    string
	RunID     string
	Kind      string
	Key       string // dedupe key
	RunState  contracts.RunState
	Reason    string
	ExitCode  *int
	TurnStart string
	Attempts  int
	Body      string
	Hold      string // hold_state
	CreatedAt string
	TurnEnded string // ended reports: when the turn they cover ended ("" unknown)
	// Archiver (not stored) is who archived the child or scheduled it,
	// when it is (archivedBy): set by prepareChildReport, read by
	// renderChildReport, so a lone report and a digest part agree.
	Archiver string
}

// childReportPoll bounds how long a report waits when its wake-up was
// consumed by a concurrent commit (the pass is one indexed query).
const childReportPoll = 2 * time.Second

// kickChildReports wakes the report worker after a commit that recorded a
// report.
func (h *Hub) kickChildReports() {
	if h.reportKick.Swap(false) {
		select {
		case h.reportWake <- struct{}{}:
		default:
		}
	}
}

// childReportLoop sends reports until Close.
func (h *Hub) childReportLoop() {
	defer h.wg.Done()
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		next, _ := h.ProcessChildReports(ctx)
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
		case <-h.reportWake:
		case <-timer.C:
		}
	}
}

// ProcessChildReports sends every due report, runs every due turn-end
// check of a top-level session (waiting.go), and returns when the next
// pending one becomes due (zero if none). Safe to call concurrently.
func (h *Hub) ProcessChildReports(ctx context.Context) (time.Time, error) {
	h.reportMu.Lock()
	defer h.reportMu.Unlock()
	waitNext, err := h.processLeadWaits(ctx)
	if err != nil {
		return time.Time{}, err
	}
	h.resumeClaimedReports(ctx)
	for range 64 { // bounded per pass
		now := persistence.Time(h.now())
		rows, err := h.db.QueryContext(ctx, `SELECT `+childReportCols+` FROM child_reports WHERE status=? AND send_op IS NULL AND not_before<=? ORDER BY report_id LIMIT 16`, reportQueued, now)
		if err != nil {
			return time.Time{}, err
		}
		due, err := scanChildReports(rows)
		if err != nil {
			return time.Time{}, err
		}
		if len(due) == 0 {
			break
		}
		for _, cr := range due {
			if err := ctx.Err(); err != nil {
				return time.Time{}, err
			}
			h.processChildReport(ctx, cr)
		}
	}
	var next sql.NullString
	if err := h.db.QueryRowContext(ctx, `SELECT MIN(not_before) FROM child_reports WHERE status=?`, reportQueued).Scan(&next); err != nil {
		return time.Time{}, err
	}
	if !next.Valid {
		return waitNext, nil
	}
	if t := persistence.ParseTime(next.String); waitNext.IsZero() || t.Before(waitNext) {
		return t, nil
	}
	return waitNext, nil
}

// settleChildReport records a report's final status.
func (h *Hub) settleChildReport(ctx context.Context, id int64, status, detail, messageID string) {
	_, _ = h.db.ExecContext(ctx, `UPDATE child_reports SET status=?, hold_state='', detail=?, message_id=?, updated_at=? WHERE report_id=?`,
		status, persistence.NullStr(detail), persistence.NullStr(messageID), persistence.Time(h.now()), id)
}

// childReportCols is the column list scanChildReports reads.
const childReportCols = `report_id, child_session_id, parent_session_id, run_id, kind, dedupe_key, run_state, COALESCE(state_reason,''), exit_code,
	COALESCE(turn_started_at,''), attempts, COALESCE(body,''), hold_state, created_at, COALESCE(turn_ended_at,'')`

func scanChildReports(rows *sql.Rows) ([]childReport, error) {
	defer rows.Close()
	var out []childReport
	for rows.Next() {
		var cr childReport
		var exit sql.NullInt64
		var st string
		if err := rows.Scan(&cr.ID, &cr.Child, &cr.Parent, &cr.RunID, &cr.Kind, &cr.Key, &st, &cr.Reason, &exit, &cr.TurnStart, &cr.Attempts, &cr.Body, &cr.Hold, &cr.CreatedAt, &cr.TurnEnded); err != nil {
			return nil, err
		}
		cr.RunState = contracts.RunState(st)
		if exit.Valid {
			c := int(exit.Int64)
			cr.ExitCode = &c
		}
		out = append(out, cr)
	}
	return out, rows.Err()
}

// prepResult is what prepareChildReport decided.
type prepResult int

const (
	prepDone  prepResult = iota // settled or deferred: nothing to send now
	prepReady                   // rendered (cr.Body): send
	prepFold                    // a folded clean exit: only rides along
)

// processChildReport decides, renders and sends one report, with every
// other report ready for the same parent (one digest).
func (h *Hub) processChildReport(ctx context.Context, cr childReport) {
	// A digest may have taken this row since the pass listed it.
	rows, err := h.db.QueryContext(ctx, `SELECT `+childReportCols+` FROM child_reports WHERE report_id=? AND status=? AND send_op IS NULL`, cr.ID, reportQueued)
	if err != nil {
		return
	}
	cur, err := scanChildReports(rows)
	if err != nil || len(cur) == 0 {
		return
	}
	cr = cur[0]
	parent, res := h.prepareChildReport(ctx, &cr)
	switch res {
	case prepDone:
		return
	case prepFold:
		h.settleChildReport(ctx, cr.ID, reportSkip, "clean exit already reported; no other report went to the parent meanwhile", "")
		return
	}
	if !h.sessionLive(ctx, parent) {
		// Rendered now (the child's transcript may be gone later), sent
		// when the parent runs again (wakeHeldReportsTx).
		if created := persistence.ParseTime(cr.CreatedAt); !created.IsZero() && h.now().Sub(created) > childReportHold {
			h.settleChildReport(ctx, cr.ID, reportSkip, fmt.Sprintf("the parent had no live run for %s", childReportHold), "")
			return
		}
		_, _ = h.db.ExecContext(ctx, `UPDATE child_reports SET hold_state=?, detail=?, not_before=?, updated_at=? WHERE report_id=?`,
			holdHeld, reportHeldDetail, persistence.Time(h.now().Add(childReportRecheck)), persistence.Time(h.now()), cr.ID)
		return
	}
	if urgentReport(cr) {
		h.sendChildReports(ctx, parent, []childReport{cr}, nil)
		return
	}
	if created := persistence.ParseTime(cr.CreatedAt); h.parentBusy(ctx, parent) && h.now().Sub(created) < childBatchMax {
		// Sent with whatever else is ready when the parent's turn ends
		// (wakeHeldReportsTx), or after childBatchMax.
		_, _ = h.db.ExecContext(ctx, `UPDATE child_reports SET hold_state=?, detail=?, not_before=?, updated_at=? WHERE report_id=?`,
			holdBatched, reportBatchedDetail, persistence.Time(earliest(h.now().Add(childReportRecheck), created.Add(childBatchMax))), persistence.Time(h.now()), cr.ID)
		return
	}
	ready, folds := h.batchFor(ctx, cr)
	h.sendChildReports(ctx, parent, append([]childReport{cr}, ready...), folds)
}

// prepareChildReport applies the noise rules to cr and renders its body
// (stored on the row). It settles or defers cr itself when nothing is to
// be sent now.
func (h *Hub) prepareChildReport(ctx context.Context, cr *childReport) (contracts.Session, prepResult) {
	skip := func(why string) (contracts.Session, prepResult) {
		h.settleChildReport(ctx, cr.ID, reportSkip, why, "")
		return contracts.Session{}, prepDone
	}
	child, err := h.getSession(ctx, h.db, cr.Child)
	if err != nil {
		return skip("child session not found")
	}
	if !reportsTo(child, *cr) {
		return skip("reports to the parent are off")
	}
	// An archived child still reports: archiving itself at the end of its
	// last turn is how a bounded child finishes (agent guide), and the
	// owner or Pyre archiving it does not tell the parent anything. Only a
	// parent that archived the child (or scheduled it) after the reported
	// turn ended already knows; one that scheduled it mid-turn still gets
	// that turn's result.
	cr.Archiver = archiverUnknown
	if child.Archived || hasArchiveWhenDone(child) {
		var at time.Time
		cr.Archiver, at = h.archivedBy(ctx, child, cr.Parent)
		if cr.Archiver == archiverParent && !at.Before(h.reportTurnEnd(ctx, *cr)) {
			return skip(reportParentArchivedDetail)
		}
	}
	parent, err := h.getSession(ctx, h.db, cr.Parent)
	if err != nil {
		return skip("parent session not found")
	}
	if parent.Archived {
		return skip("parent session is archived")
	}
	if cr.Hold == holdFolded {
		return parent, prepFold
	}
	switch cr.Kind {
	case reportNeeds:
		// The prompt may have been answered (by the owner, in the terminal)
		// during the settle delay or while the report was held.
		if r, err := h.getRun(ctx, h.db, cr.RunID); err != nil || r.State != contracts.RunWaitingApproval {
			return skip("no longer waiting for input")
		}
	case reportOverdue:
		why, later := h.overdueStill(ctx, *cr)
		if why != "" {
			return skip(why)
		}
		if !later.IsZero() { // the child moved its expected time
			_, _ = h.db.ExecContext(ctx, `UPDATE child_reports SET not_before=?, updated_at=? WHERE report_id=?`, persistence.Time(later), persistence.Time(h.now()), cr.ID)
			return parent, prepDone
		}
	}
	if cr.Body == "" && strings.HasSuffix(cr.Key, launchKeySuffix) && cr.Kind != reportOverdue {
		if why := h.launchTurnStill(ctx, *cr); why != "" {
			if created := persistence.ParseTime(cr.CreatedAt); h.now().Sub(created) >= childLaunchWait {
				return skip(why)
			}
			_, _ = h.db.ExecContext(ctx, `UPDATE child_reports SET detail=?, not_before=?, updated_at=? WHERE report_id=?`,
				"deferred: "+why, persistence.Time(h.now().Add(childLaunchSettle)), persistence.Time(h.now()), cr.ID)
			return parent, prepDone
		}
	}
	if cr.Body == "" {
		earlier := ""
		switch cr.Kind {
		case reportIdle:
			if h.childMessagedTurn(ctx, *cr) {
				return skip(childMessagedSkip)
			}
		case reportEnded:
			if (cr.Archiver == archiverOwner || cr.Archiver == archiverPyre) && h.turnReported(ctx, *cr) {
				// An owner bulk archive (or Pyre's sweep) stops idle children
				// whose turn already reached the parent: nothing new to say.
				// Decided before coalesceEnded, which would cancel that report.
				return skip("archived by the " + cr.Archiver + " after this turn was reported")
			}
			var why string
			var fold, replaced bool
			earlier, why, fold, replaced = h.coalesceEnded(ctx, *cr)
			if !fold && why == "" && earlier == "" && replaced && cr.TurnStart != "" && h.endedCleanly(ctx, *cr) {
				// This report replaced the turn's idle report after a clean
				// end: the idle rule applies (a child that sent its result
				// itself right before the turn ended is not reported twice).
				fold = h.childMessagedTurn(ctx, *cr)
			}
			if fold {
				// Wakes nobody: rides along with the parent's next report,
				// dropped after childReportHold.
				created := persistence.ParseTime(cr.CreatedAt)
				_, _ = h.db.ExecContext(ctx, `UPDATE child_reports SET hold_state=?, detail=?, not_before=?, updated_at=? WHERE report_id=?`,
					holdFolded, reportFoldDetail, persistence.Time(created.Add(childReportHold)), persistence.Time(h.now()), cr.ID)
				return parent, prepDone
			} else if why != "" {
				return skip(why)
			}
		}
		var recent int
		_ = h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM child_reports WHERE child_session_id=? AND status=? AND updated_at>=?`,
			cr.Child, reportSent, persistence.Time(h.now().Add(-childReportWindow))).Scan(&recent)
		if recent >= childReportBurst {
			return skip(fmt.Sprintf("rate limited: %d reports in %s", recent, childReportWindow))
		}
		cr.Body = h.renderChildReport(ctx, child, *cr, earlier)
		if _, err := h.db.ExecContext(ctx, `UPDATE child_reports SET body=?, updated_at=? WHERE report_id=?`, cr.Body, persistence.Time(h.now()), cr.ID); err != nil {
			return parent, prepDone // retried on the next pass
		}
	}
	return parent, prepReady
}

// launchTurnStill checks a report of a first turn counted at launch
// (launchTurnTx) before it is sent: time alone does not prove the turn
// ran. It is sent only while the run is idle and its transcript has an
// assistant message (the run is the first of a fresh session, so any is
// the prompt's answer); otherwise the reason it waits. A running or
// approval observation meanwhile withdraws it (trackChildTx).
func (h *Hub) launchTurnStill(ctx context.Context, cr childReport) string {
	r, err := h.getRun(ctx, h.db, cr.RunID)
	if err != nil {
		return "the run is gone"
	}
	if r.State != contracts.RunWaitingInput {
		return "the run is " + string(r.State) + ", not idle after its first turn"
	}
	if text, err := h.childLastMessage(ctx, cr); err != nil || strings.TrimSpace(text) == "" {
		return "no assistant message since the launch"
	}
	return ""
}

// childMessagedSkip is the skip reason of an idle report whose turn the
// child already told its parent about (coalesceEnded and turnReported look
// for it). Rows skipped under the earlier rule ("during this turn or in the
// last 5m0s") do not match, so their ended reports are sent, not folded.
var childMessagedSkip = fmt.Sprintf("the child messaged the parent within %s before this turn ended", childRecentMessage)

// parentBusy reports whether the parent is mid-turn (its turn-end reports
// wait for the turn to end).
func (h *Hub) parentBusy(ctx context.Context, s contracts.Session) bool {
	r, err := h.getRun(ctx, h.db, s.CurrentRunID)
	if err != nil {
		return false
	}
	switch r.State {
	case contracts.RunStarting, contracts.RunRunning, contracts.RunWaitingApproval, contracts.RunExiting:
		return true
	}
	return false
}

// childReportBatchMax bounds one digest.
const childReportBatchMax = 24

// batchFor prepares the other reports pending for cr's parent that may go
// out now (due, batched, held or folded; needs_input excluded).
func (h *Hub) batchFor(ctx context.Context, cr childReport) (ready, folds []childReport) {
	rows, err := h.db.QueryContext(ctx, `SELECT `+childReportCols+` FROM child_reports WHERE status=? AND parent_session_id=? AND report_id!=? AND kind!=?
		AND send_op IS NULL AND (not_before<=? OR hold_state IN (?,?,?)) ORDER BY report_id LIMIT ?`,
		reportQueued, cr.Parent, cr.ID, reportNeeds, persistence.Time(h.now()), holdBatched, holdHeld, holdFolded, childReportBatchMax)
	if err != nil {
		return nil, nil
	}
	others, err := scanChildReports(rows)
	if err != nil {
		return nil, nil
	}
	for _, o := range others {
		switch _, res := h.prepareChildReport(ctx, &o); res {
		case prepReady:
			ready = append(ready, o)
		case prepFold:
			folds = append(folds, o)
		}
	}
	return ready, folds
}

// childDigestSender sends digests that carry reports of several children.
var childDigestSender = contracts.Principal{Kind: contracts.PrincipalSystem, ID: "child-reports"}

// childDigestMaxBytes keeps a digest well under the message size limit;
// reports that do not fit go in the next one.
var childDigestMaxBytes = 48 << 10

// sendChildReports sends ready (at least one) as one message: a lone
// report from its child (a reply threads to it), several as a digest; the
// folded clean exits ride along as a closing line. Reports that would
// push a digest past childDigestMaxBytes stay pending for the next one.
//
// Exactly once: the rows are claimed first in one transaction with the
// rendered text, sender and operation ID (send_op); the stored text is
// then sent under that operation ID and the rows settle together by
// send_op. A crash after the send leaves claimed rows that the next pass
// resends (resumeClaimedReports): the message store returns the original
// message for the same operation, so nothing is lost or doubled.
func (h *Hub) sendChildReports(ctx context.Context, parent contracts.Session, ready, folds []childReport) {
	first := ready[0]
	sender := "session:" + first.Child
	op := "op_childreport-" + strconv.FormatInt(first.ID, 10)
	text := first.Body
	if len(ready) > 1 {
		sender = "system:" + childDigestSender.ID
		op = "op_childdigest-" + strconv.FormatInt(first.ID, 10)
		for len(ready) > 1 {
			text = h.renderDigest(ctx, ready)
			if len(text) <= childDigestMaxBytes {
				break
			}
			ready = ready[:len(ready)-1] // the rest go next pass
		}
		if len(ready) == 1 {
			sender, op, text = "session:"+first.Child, "op_childreport-"+strconv.FormatInt(first.ID, 10), first.Body
		}
	}
	text += h.renderFolds(ctx, folds)
	ids := make([]int64, 0, len(ready)+len(folds))
	for _, cr := range append(append([]childReport{}, ready...), folds...) {
		ids = append(ids, cr.ID)
	}
	if !h.claimChildReports(ctx, ids, op, sender, text) {
		return // taken meanwhile; the next pass sees it
	}
	if err := h.deliverClaim(ctx, op, parent.SessionID, sender, text, len(ready) > 1); err != nil {
		h.claimFailed(ctx, op, first.ID, err)
	}
}

// claimFailed handles a claimed send that did not settle. Only a definite
// refusal (the store rejected the message, so nothing was stored) releases
// the claim: its first row fails and the others come round in a new batch.
// Anything else (transient, or a failure after the message committed)
// keeps the claim with its stored text and operation ID, and
// resumeClaimedReports retries it with backoff; the store returns the
// original message if it was already stored. After childReportAttempts
// the claimed rows fail.
func (h *Hub) claimFailed(ctx context.Context, op string, firstID int64, err error) {
	now := persistence.Time(h.now())
	if !retryableSend(err) {
		_, _ = h.db.ExecContext(ctx, `UPDATE child_reports SET send_op=NULL, send_body=NULL, send_sender=NULL WHERE send_op=? AND status=? AND report_id!=?`, op, reportQueued, firstID)
		_, _ = h.db.ExecContext(ctx, `UPDATE child_reports SET status=?, hold_state='', send_op=NULL, attempts=attempts+1, detail=?, updated_at=? WHERE report_id=? AND status=?`,
			reportFailed, err.Error(), now, firstID, reportQueued)
		return
	}
	var attempts int
	_ = h.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(attempts),0)+1 FROM child_reports WHERE send_op=? AND status=?`, op, reportQueued).Scan(&attempts)
	if attempts >= childReportAttempts {
		_, _ = h.db.ExecContext(ctx, `UPDATE child_reports SET status=?, hold_state='', attempts=?, detail=?, updated_at=? WHERE send_op=? AND status=?`,
			reportFailed, attempts, err.Error(), now, op, reportQueued)
		return
	}
	_, _ = h.db.ExecContext(ctx, `UPDATE child_reports SET attempts=?, detail=?, not_before=?, updated_at=? WHERE send_op=? AND status=?`,
		attempts, err.Error(), persistence.Time(h.now().Add(time.Duration(attempts)*5*time.Second)), now, op, reportQueued)
}

// claimChildReports marks every row in ids as part of send op, storing the
// text on each (one transaction; false if any row is no longer free).
func (h *Hub) claimChildReports(ctx context.Context, ids []int64, op, sender, text string) bool {
	ok := false
	_ = h.tx(ctx, func(tx *sql.Tx) error {
		for _, id := range ids {
			res, err := tx.ExecContext(ctx, `UPDATE child_reports SET send_op=?, send_body=?, send_sender=?, not_before=?, updated_at=? WHERE report_id=? AND status=? AND send_op IS NULL`,
				op, text, sender, persistence.Time(h.now()), persistence.Time(h.now()), id, reportQueued)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return errClaimLost
			}
		}
		ok = true
		return nil
	})
	return ok
}

var errClaimLost = errors.New("child report claimed elsewhere")

// afterChildSend (tests) runs between the message store and the settle:
// stop=true acts as a crash, a non-nil error as a failure after the send
// committed.
var afterChildSend func() (stop bool, err error)

// deliverClaim sends a claimed text under op and settles its rows.
func (h *Hub) deliverClaim(ctx context.Context, op, parentID, sender, text string, digest bool) error {
	parent, err := h.getSession(ctx, h.db, parentID)
	if err != nil {
		return err
	}
	from := childDigestSender
	if id, ok := strings.CutPrefix(sender, "session:"); ok {
		from = contracts.Principal{Kind: contracts.PrincipalSession, ID: id}
	}
	v, err := h.msgs.SendMessage(contracts.WithPrincipal(ctx, from), contracts.SendMessageRequest{
		Mutation:  contracts.Mutation{OperationID: op},
		Recipient: contracts.Address{Kind: contracts.AddrSession, ID: parent.SessionID},
		ProjectID: parent.ProjectID,
		Mode:      contracts.ModeQueue,
		Body:      contracts.MessageBody{Text: text},
	})
	if err != nil {
		return err
	}
	if afterChildSend != nil {
		if stop, err := afterChildSend(); stop || err != nil {
			return err
		}
	}
	detail := ""
	if digest {
		detail = "in " + v.Envelope.MessageID
	}
	_, err = h.db.ExecContext(ctx, `UPDATE child_reports SET status=?, hold_state='', detail=CASE WHEN hold_state=? THEN ? ELSE ? END, message_id=?, updated_at=? WHERE send_op=? AND status=?`,
		reportSent, holdFolded, "in "+v.Envelope.MessageID, persistence.NullStr(detail), v.Envelope.MessageID, persistence.Time(h.now()), op, reportQueued)
	return err
}

// resumeClaimedReports finishes sends a crash interrupted (claimed rows
// still pending): the same text under the same operation ID.
func (h *Hub) resumeClaimedReports(ctx context.Context) {
	rows, err := h.db.QueryContext(ctx, `SELECT send_op, parent_session_id, COALESCE(send_sender,''), COALESCE(send_body,''), MIN(report_id) FROM child_reports
		WHERE status=? AND send_op IS NOT NULL GROUP BY send_op HAVING MIN(not_before)<=? LIMIT 16`, reportQueued, persistence.Time(h.now()))
	if err != nil {
		return
	}
	type claim struct {
		op, parent, sender, body string
		first                    int64
	}
	var claims []claim
	for rows.Next() {
		var c claim
		if rows.Scan(&c.op, &c.parent, &c.sender, &c.body, &c.first) == nil {
			claims = append(claims, c)
		}
	}
	rows.Close()
	for _, c := range claims {
		if err := h.deliverClaim(ctx, c.op, c.parent, c.sender, c.body, strings.HasPrefix(c.op, "op_childdigest-")); err != nil {
			h.claimFailed(ctx, c.op, c.first, err)
		}
	}
}

// childReplyHint closes a lone idle report; a digest names the child to
// message instead (its replies would reach the system sender).
const childReplyHint = "Reply (send_message reply_to) to give it more work; read_session shows the whole transcript; otherwise ack_message.\n"

// childArchivedHint closes a report of a child that is archived (or
// archives when its turn ends): there is no live run to reply to.
const childArchivedHint = "It is archived: read_session shows the whole transcript; to give it more work, unarchive and resume it (or start a new session); otherwise ack_message.\n"

// renderDigest joins several reports: a lead line listing them, then each
// report under a numbered rule.
func (h *Hub) renderDigest(ctx context.Context, ready []childReport) string {
	leads := make([]string, 0, len(ready))
	for _, cr := range ready {
		lead, _, _ := strings.Cut(cr.Body, "\n")
		leads = append(leads, oneLineText(lead, 80))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d reports from sessions you started: %s\n", len(ready), strings.Join(leads, " · "))
	b.WriteString("[pyre digest] Collected while you were busy, oldest first. Each part names its session; to answer one, send_message to that session (a reply to this digest reaches nobody).\n")
	for i, cr := range ready {
		fmt.Fprintf(&b, "\n— %d/%d —\n", i+1, len(ready))
		b.WriteString(strings.Replace(cr.Body, strings.TrimSuffix(childReplyHint, "\n"),
			"To give it more work: send_message to "+cr.Child+"; read_session shows the whole transcript.", 1))
		b.WriteString("\n")
	}
	b.WriteString("\nAck this digest (ack_message) when none of it needs anything.")
	return b.String()
}

// renderFolds is the closing line naming clean exits already reported: a
// report is folded only when the turn's idle report went out or the child
// messaged the parent right before the turn ended (childMessagedTurn), so
// the claim holds.
func (h *Hub) renderFolds(ctx context.Context, folds []childReport) string {
	if len(folds) == 0 {
		return ""
	}
	names := make([]string, 0, len(folds))
	for _, cr := range folds {
		title := cr.Child
		if s, err := h.getSession(ctx, h.db, cr.Child); err == nil && s.Title != "" {
			title = fmt.Sprintf("%q (%s)", oneLineText(s.Title, 80), cr.Child)
		}
		names = append(names, title)
	}
	return "\n\nAlso exited cleanly since (their last messages already reached you): " + strings.Join(names, ", ") + "."
}

// sessionLive reports whether s has a current, non-terminal run.
func (h *Hub) sessionLive(ctx context.Context, s contracts.Session) bool {
	if s.CurrentRunID == "" {
		return false
	}
	r, err := h.getRun(ctx, h.db, s.CurrentRunID)
	return err == nil && !r.State.Terminal()
}

// coalesceEnded relates an ended report to the same turn's idle report that
// was already sent: a still undelivered one is cancelled (the ended report
// replaces it). Otherwise a clean exit is folded (fold: the parent already
// has the last message, from the idle report or from the child itself); a
// failure within childReportCoalesce of the idle report is redundant
// (skip reason), a later one refers to it by message ID.
//
// replaced: this report stands in for the turn's idle (or waiting) report,
// replaced when the run ended (trackChildTx) or cancelled here.
func (h *Hub) coalesceEnded(ctx context.Context, cr childReport) (earlier, skipWhy string, fold, replaced bool) {
	clean := cr.RunState != contracts.RunFailed
	var msgID, sentAt string
	err := h.db.QueryRowContext(ctx, `SELECT COALESCE(message_id,''), updated_at FROM child_reports WHERE child_session_id=? AND parent_session_id=? AND kind=? AND status=?
		AND report_id<? AND turn_started_at IS ? ORDER BY report_id DESC LIMIT 1`,
		cr.Child, cr.Parent, reportIdle, reportSent, cr.ID, persistence.NullStr(cr.TurnStart)).Scan(&msgID, &sentAt)
	if err != nil || msgID == "" {
		var n int
		_ = h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM child_reports WHERE child_session_id=? AND parent_session_id=? AND kind IN (?,?) AND status=? AND detail=?
			AND report_id<? AND turn_started_at IS ?`, cr.Child, cr.Parent, reportIdle, reportWaiting, reportSkip, reportReplacedDetail, cr.ID, persistence.NullStr(cr.TurnStart)).Scan(&n)
		replaced = n > 0
		if clean {
			_ = h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM child_reports WHERE child_session_id=? AND parent_session_id=? AND kind=? AND status=? AND detail=?
				AND report_id<? AND turn_started_at IS ?`, cr.Child, cr.Parent, reportIdle, reportSkip, childMessagedSkip, cr.ID, persistence.NullStr(cr.TurnStart)).Scan(&n)
			return "", "", n > 0, replaced
		}
		return "", "", false, replaced
	}
	if mv, err := h.msgs.GetMessage(h.systemCtx(ctx), contracts.GetRequest{ID: msgID}); err == nil &&
		mv.Status.Runtime == contracts.DeliveryQueued && mv.Status.BoundRunID == "" && mv.Status.Ack == contracts.AckNone {
		sender := contracts.WithPrincipal(ctx, contracts.Principal{Kind: contracts.PrincipalSession, ID: cr.Child})
		if _, err := h.msgs.CancelMessage(sender, contracts.CancelMessageRequest{
			Mutation: contracts.Mutation{OperationID: "op_childreport-cancel-" + strconv.FormatInt(cr.ID, 10)}, MessageID: msgID}); err == nil {
			return "", "", false, true
		}
	}
	if clean {
		return "", "", true, false
	}
	if ago := h.now().Sub(persistence.ParseTime(sentAt)); ago < childReportCoalesce {
		return "", fmt.Sprintf("the parent got this turn's idle report %s %s ago", msgID, ago.Round(time.Second)), false, false
	}
	return msgID, "", false, false
}

// reportReplacedDetail is the skip reason of a turn-end report the ended
// report replaced (trackChildTx).
const reportReplacedDetail = "replaced by the ended report"

// reportQuietDetail is the skip reason of a waiting report whose turn the
// child marked quiet (waiting_on.go). The parent never got that turn's
// last message: the overdue and ended reports carry it (turnReported and
// coalesceEnded do not count it as reported).
const reportQuietDetail = "quiet wait (waiting_on quiet)"

// endedCleanly: cr's run exited with a recorded exit code of 0, or Pyre
// stopped it after its turn ended to archive it when done (archivewhen.go;
// a harness stopped that way may report no exit code). A missing or
// non-zero code otherwise is not clean (reboot, OOM kill, crash).
func (h *Hub) endedCleanly(ctx context.Context, cr childReport) bool {
	if cr.RunState != contracts.RunExited {
		return false
	}
	if cr.ExitCode != nil {
		return *cr.ExitCode == 0
	}
	var n int
	_ = h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_log WHERE session_id=? AND run_id=? AND action=? AND to_val=?`,
		cr.Child, cr.RunID, contracts.AuditSessionStopped, auditWhenDone).Scan(&n)
	return n > 0
}

// reportTurnEnd is when the turn cr reports on ended: for an ended report,
// when the run went idle after that turn (recorded with the report; the
// run may have ended later), else when that turn's idle/waiting report
// was recorded (rows from before turn_ended_at), else when cr was
// recorded. A parent that archived the child after the turn ended is not
// told about it again (prepareChildReport).
func (h *Hub) reportTurnEnd(ctx context.Context, cr childReport) time.Time {
	at := cr.CreatedAt
	if cr.Kind == reportEnded && cr.TurnEnded != "" {
		return persistence.ParseTime(cr.TurnEnded)
	}
	if cr.Kind == reportEnded && cr.TurnStart != "" {
		_ = h.db.QueryRowContext(ctx, `SELECT created_at FROM child_reports WHERE child_session_id=? AND kind IN (?,?) AND turn_started_at=? AND report_id<?
			ORDER BY report_id DESC LIMIT 1`, cr.Child, reportIdle, reportWaiting, cr.TurnStart, cr.ID).Scan(&at)
	}
	return persistence.ParseTime(at)
}

// turnReported: the parent already has the turn cr's run ended on, from
// its idle/waiting report or from the child's own message.
func (h *Hub) turnReported(ctx context.Context, cr childReport) bool {
	if cr.TurnStart == "" {
		return false
	}
	var n int
	_ = h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM child_reports WHERE child_session_id=? AND parent_session_id=? AND kind IN (?,?) AND turn_started_at=? AND report_id<?
		AND (status=? OR (status=? AND detail=?))`, cr.Child, cr.Parent, reportIdle, reportWaiting, cr.TurnStart, cr.ID, reportSent, reportSkip, childMessagedSkip).Scan(&n)
	return n > 0
}

// reportTurn is the child turn a waiting/overdue report belongs to (its
// dedupe key "<kind>:<turn>", launchKeySuffix after a turn counted at
// launch).
func reportTurn(cr childReport) int64 {
	_, seq, _ := strings.Cut(cr.Key, ":")
	n, _ := strconv.ParseInt(strings.TrimSuffix(seq, launchKeySuffix), 10, 64)
	return n
}

// overdueStill decides a due waiting_overdue report: a skip reason when
// the child moved on, or a later time when it moved its expectation.
func (h *Hub) overdueStill(ctx context.Context, cr childReport) (string, time.Time) {
	turn := reportTurn(cr)
	var cur int64
	_ = h.db.QueryRowContext(ctx, `SELECT turn_seq FROM child_turns WHERE session_id=?`, cr.Child).Scan(&cur)
	if cur > turn {
		return "the child started a new turn", time.Time{}
	}
	w, ok := h.waitForTurnTx(ctx, h.db, cr.Child, turn)
	if !ok {
		return "the child cleared its wait", time.Time{}
	}
	if r, err := h.getRun(ctx, h.db, cr.RunID); err != nil || r.State.Terminal() {
		return "the child's run ended (its ended report covers it)", time.Time{}
	}
	if w.Until.After(h.now()) {
		return "", w.Until
	}
	return "", time.Time{}
}

// childMessagesToParent counts messages the child itself sent the parent
// after since (automatic reports excluded).
func (h *Hub) childMessagesToParent(ctx context.Context, child, parent, since string) int {
	var n int
	_ = h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE sender_kind=? AND sender_id=? AND recipient_kind=? AND recipient_id=? AND stored_at>?
		AND message_id NOT IN (SELECT message_id FROM child_reports WHERE child_session_id=? AND message_id IS NOT NULL)`,
		string(contracts.PrincipalSession), child, string(contracts.AddrSession), parent, since, child).Scan(&n)
	return n
}

// childMessagedTurn reports whether the child itself messaged the parent
// within childRecentMessage before cr's turn ended (reportTurnEnd), so the
// parent already has that turn's result. A message sent earlier in a long
// turn (a question, a coordination ask) does not count.
func (h *Hub) childMessagedTurn(ctx context.Context, cr childReport) bool {
	end := h.reportTurnEnd(ctx, cr)
	if end.IsZero() {
		end = h.now()
	}
	return h.childMessagesToParent(ctx, cr.Child, cr.Parent, persistence.Time(end.Add(-childRecentMessage))) > 0
}

// Who archived a child (or scheduled its archive when done), from the
// audit log (archivedBy).
const (
	archiverUnknown = ""
	archiverSelf    = "self"
	archiverParent  = "parent"
	archiverOwner   = "owner"
	archiverPyre    = "pyre"
	archiverOther   = "other" // another session (an owner-approved request is the owner's)
)

// reportParentArchivedDetail is the skip reason of a report whose child
// the parent archived.
const reportParentArchivedDetail = "the parent archived the child session"

// archivedBy names who archived child or scheduled its archive when done,
// and when: the newest archive entry in its audit log; a when-done archive
// (done by Pyre, marked auditWhenDone) is credited to whoever scheduled
// it. Unknown when the log does not say (pruned, or not archived).
func (h *Hub) archivedBy(ctx context.Context, child contracts.Session, parent string) (string, time.Time) {
	rows, err := h.db.QueryContext(ctx, `SELECT action, at, actor_kind, COALESCE(actor_id,''), COALESCE(to_val,'') FROM audit_log
		WHERE session_id=? AND action IN (?,?,?,?) ORDER BY id DESC LIMIT 8`, child.SessionID,
		contracts.AuditSessionArchived, contracts.AuditSessionArchiveScheduled, contracts.AuditSessionArchiveCanceled, contracts.AuditSessionUnarchived)
	if err != nil {
		return archiverUnknown, time.Time{}
	}
	defer rows.Close()
	for rows.Next() {
		var action, at, kind, id, to string
		if rows.Scan(&action, &at, &kind, &id, &to) != nil {
			return archiverUnknown, time.Time{}
		}
		switch action {
		case contracts.AuditSessionArchiveCanceled, contracts.AuditSessionUnarchived:
			return archiverUnknown, time.Time{} // anything older is not the current archive
		case contracts.AuditSessionArchived:
			if to == auditWhenDone {
				continue // finishArchive: the scheduling entry names who asked
			}
		}
		when := persistence.ParseTime(at)
		switch {
		case kind == "session" && id == child.SessionID:
			return archiverSelf, when
		case kind == "session" && id == parent:
			return archiverParent, when
		case kind == "session":
			return archiverOther, when
		case kind == "owner":
			return archiverOwner, when
		}
		return archiverPyre, when
	}
	return archiverUnknown, time.Time{}
}

// renderChildReport writes the message text: one lead line a reader can
// stop at ("<title> finished: <first sentence>"), one line of facts, then
// at most childSummaryMax runes of the last message. earlier (ended
// reports) names this turn's idle report, which already carried it.
func (h *Hub) renderChildReport(ctx context.Context, child contracts.Session, cr childReport, earlier string) string {
	var b strings.Builder
	title := oneLineText(child.Title, 80)
	if title == "" {
		title = child.SessionID
	}
	name := fmt.Sprintf("%q (%s)", child.Title, child.SessionID)
	rel := startedOrReports(child, cr.Parent) // W-reparent
	machine := ""
	harness := string(child.Harness)
	if r, err := h.getRun(ctx, h.db, cr.RunID); err == nil {
		machine = h.nodeName(ctx, h.db, r.NodeID, true) // outside any write transaction
		if machine == "" {
			machine = r.NodeID
		}
	}
	facts := " · machine: " + orUnknown(machine) + " · state: " + string(cr.RunState) + " · harness: " + orUnknown(harness) + "\n"
	lead := func(head, sentence string) {
		b.WriteString(head)
		if sentence != "" {
			b.WriteString(": " + sentence)
		}
		b.WriteString("\n")
	}
	archiver := cr.Archiver // prepareChildReport
	archiving := child.Archived || hasArchiveWhenDone(child)
	switch cr.Kind {
	case reportIdle:
		text, err := h.childLastMessage(ctx, cr)
		first := firstSentence(text)
		if archiving && archiver == archiverSelf {
			lead(title+" finished and archived itself", first)
			fmt.Fprintf(&b, "[pyre auto-report] Session %s, %s, finished its last turn and archived itself (Pyre stops it now)%s", name, rel, facts)
			writeSummary(&b, text, first, err)
			b.WriteString(h.archiveCascadeNote(ctx, child.SessionID)) // archivecascade.go
			b.WriteString(childArchivedHint)
			break
		}
		lead(title+" finished", first)
		fmt.Fprintf(&b, "[pyre auto-report] Session %s, %s, finished its turn and is waiting for input%s", name, rel, facts)
		writeSummary(&b, text, first, err)
		b.WriteString(childReplyHint)
	case reportWaiting:
		what, until := h.reportWait(ctx, cr)
		text, err := h.childLastMessage(ctx, cr)
		lead(title+" is waiting on its own job", oneLineText(what, childLeadMax))
		fmt.Fprintf(&b, "[pyre auto-report] Session %s, %s, ended its turn but is not idle: it waits on %s (expected by %s) and reports again when that finishes%s",
			name, rel, what, until, facts)
		b.WriteString("Don't give it new work or treat it as done. If it has not started a new turn by then, Pyre tells you it looks stalled.\n")
		writeSummary(&b, text, firstSentence(text), err)
	case reportOverdue:
		what, until := h.reportWait(ctx, cr)
		lead(title+" may be stuck", oneLineText("it waited on "+what+", expected by "+until+", and has not resumed", childLeadMax))
		w, _ := h.waitForTurnTx(ctx, h.db, cr.Child, reportTurn(cr))
		quiet := ""
		if w.Quiet {
			quiet = " (a quiet wait: that turn's end was not reported to you)"
		}
		fmt.Fprintf(&b, "[pyre auto-report] Session %s, %s, ended a turn waiting on %s, expected by %s, and has not started a new turn since%s%s",
			name, rel, what, until, quiet, facts)
		b.WriteString("The watcher or background task that should wake it may have died. Check with read_session, or message it to look at the job.\n")
		if w.Quiet { // the parent never got this turn's last message
			text, err := h.childLastMessage(ctx, cr)
			writeSummary(&b, text, "", err)
		}
	case reportNeeds:
		lead(title+" needs input", "")
		fmt.Fprintf(&b, "[pyre auto-report] Session %s, %s, is waiting on an approval or question%s", name, rel, facts)
		ids := h.pendingApprovalIDs(ctx, h.db, cr.RunID)
		for _, id := range ids {
			a, err := h.getApproval(ctx, h.db, id)
			if err != nil {
				continue
			}
			fmt.Fprintf(&b, "Pending %s: %s", a.ApprovalID, oneLineText(a.Title, 200))
			if len(a.Options) > 0 {
				opts := make([]string, 0, len(a.Options))
				for _, o := range a.Options {
					opts = append(opts, o.ID)
				}
				fmt.Fprintf(&b, " (options: %s)", strings.Join(opts, ", "))
			}
			b.WriteString("\n")
		}
		if len(ids) > 0 {
			b.WriteString("If you can decide, answer with answer_approval (approval_id, option_id); otherwise tell the owner.\n")
		} else {
			b.WriteString("No structured approval was reported; it may be waiting on a prompt in its terminal. Tell the owner if you cannot help.\n")
		}
	case reportEnded:
		verb := "exited"
		if cr.RunState == contracts.RunFailed {
			verb = "failed"
		}
		launch := verb == "failed" && isLaunchFailure(cr.Reason)
		var text string
		var err error
		if earlier == "" && !launch {
			text, err = h.childLastMessage(ctx, cr)
		}
		first := firstSentence(text)
		switch {
		case launch:
			// It never ran: no transcript to read; say why in plain words.
			lead(title+" could not start", oneLineText(strings.TrimPrefix(runfail.Plain(cr.Reason, false, runfail.Names{}), "Couldn't start: "), childLeadMax))
		case verb == "failed" && cr.Reason != "":
			lead(title+" failed", oneLineText(cr.Reason, childLeadMax))
		case archiving && archiver == archiverSelf:
			lead(title+" finished and archived itself", first)
		case archiving && archiver == archiverOwner:
			lead(title+" exited and was archived by the owner", first)
		case archiving:
			lead(title+" exited and was archived", first)
		default:
			lead(title+" "+verb, first)
		}
		if archiving && archiver == archiverSelf && verb == "exited" {
			// The reason is Pyre's own stop ("stopped by pyre"): said plainly.
			fmt.Fprintf(&b, "[pyre auto-report] Session %s, %s, finished and archived itself; Pyre stopped it after its last turn", name, rel)
		} else {
			fmt.Fprintf(&b, "[pyre auto-report] Session %s, %s, has %s", name, rel, verb)
			if cr.Reason != "" {
				fmt.Fprintf(&b, ": %s", oneLineText(cr.Reason, 300))
			}
		}
		if cr.ExitCode != nil {
			fmt.Fprintf(&b, " (exit code %d)", *cr.ExitCode)
		}
		b.WriteString(facts)
		if launch {
			if _, capacity := runfail.Capacity(cr.Reason); capacity {
				b.WriteString("It never ran: no account had capacity. Resume it (or start it again) after the reset, or archive it if it is no longer needed. The owner was not notified.\n")
			} else {
				b.WriteString("It never ran, so it has no transcript. Fix the cause and resume it, or archive it if it is no longer needed. The owner was not notified.\n")
			}
		} else if earlier != "" {
			fmt.Fprintf(&b, "Its last message is in the earlier report %s.\n", earlier)
		} else {
			if verb == "failed" {
				first = "" // the lead line carried the reason, not the message
			}
			writeSummary(&b, text, first, err)
		}
		if archiving {
			b.WriteString(h.archiveCascadeNote(ctx, child.SessionID)) // archivecascade.go
			b.WriteString(childArchivedHint)
		} else if !launch && cr.RunState == contracts.RunExited && cr.ExitCode != nil && *cr.ExitCode == 0 && child.CreatedBy.Kind == contracts.PrincipalSession {
			// W-lifecycle: ArchiveFinishedChildren follows this report.
			b.WriteString("Pyre archives it in about five minutes and then removes the worktrees Pyre created for it that are clean and merged or pushed; to give it more work, resume it before then.\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// reportWait is what a waiting/overdue report's turn waited on and when
// it was expected done (Pacific).
func (h *Hub) reportWait(ctx context.Context, cr childReport) (what, until string) {
	w, ok := h.waitForTurnTx(ctx, h.db, cr.Child, reportTurn(cr))
	if !ok {
		return "a job it started", "unknown"
	}
	return w.What, askClock(w.Until)
}

// childLastMessage fetches the child's last assistant message.
func (h *Hub) childLastMessage(ctx context.Context, cr childReport) (string, error) {
	fetch := h.childSummary
	if fetch == nil {
		fetch = h.lastAssistantText
	}
	return fetch(ctx, cr.Child, cr.RunID)
}

// writeSummary appends the last message (bounded), or why it is
// unavailable. A message the lead line already showed whole is not
// repeated.
func writeSummary(b *strings.Builder, text, lead string, err error) {
	text = strings.TrimSpace(text)
	switch {
	case err != nil:
		fmt.Fprintf(b, "(Its last message is unavailable: %s. Use read_session.)\n", oneLineText(err.Error(), 200))
	case text == "":
		b.WriteString("(No assistant message was found in its transcript.)\n")
	case lead != "" && oneLineText(text, childLeadMax+1) == lead:
	default:
		b.WriteString("Its last message: ")
		b.WriteString(clipRunes(text, childSummaryMax))
		if utf8.RuneCountInString(text) > childSummaryMax {
			b.WriteString(" (read_session for the rest)")
		}
		b.WriteString("\n")
	}
}

// firstSentence is the first sentence of the first non-empty line of text
// that is not a markdown heading (else of the first heading), list/quote
// markers dropped, at most childLeadMax runes.
func firstSentence(text string) string {
	var heading string
	for _, line := range strings.Split(text, "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "#") {
			if heading == "" {
				heading = strings.TrimSpace(strings.TrimLeft(l, "# "))
			}
			continue
		}
		l = strings.TrimSpace(strings.TrimLeft(l, ">-*• "))
		if l == "" {
			continue
		}
		r := []rune(l)
		for i := 0; i+1 < len(r); i++ {
			if (r[i] == '.' || r[i] == '!' || r[i] == '?') && r[i+1] == ' ' && (i+2 >= len(r) || !unicode.IsLower(r[i+2])) {
				l = string(r[:i+1])
				break
			}
		}
		return oneLineText(l, childLeadMax)
	}
	return oneLineText(heading, childLeadMax)
}

// lastAssistantText reads the child's transcript tail through its node and
// returns the last assistant message.
func (h *Hub) lastAssistantText(ctx context.Context, sessionID, runID string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, h.opt.CommandWait+time.Second)
	defer cancel()
	resp, err := h.ReadSession(h.systemCtx(ctx), contracts.ReadSessionRequest{SessionID: sessionID, RunID: runID, Tail: true, Limit: 40})
	if err != nil {
		return "", err
	}
	for i := len(resp.Entries) - 1; i >= 0; i-- {
		if e := resp.Entries[i]; e.Kind == "assistant" && strings.TrimSpace(e.Text) != "" {
			return e.Text, nil
		}
	}
	return "", nil
}

// pruneChildReports drops settled report rows past retention.
func (h *Hub) pruneChildReports(ctx context.Context) error {
	_, err := h.db.ExecContext(ctx, `DELETE FROM child_reports WHERE status!=? AND updated_at<?`, reportQueued, persistence.Time(h.now().Add(-childReportRetention)))
	return err
}

// isLaunchFailure reports whether a failed run's reason says it never
// started (the hub's "launch failed: ..." from a failed start command).
func isLaunchFailure(reason string) bool {
	return strings.HasPrefix(reason, "launch ") || strings.HasPrefix(reason, "Launch ")
}

// urgentReport: needs_input, and the ended report of a run that failed to
// launch, go out at once on their own, never batched for a busy parent: the
// parent is waiting on work that will not happen (a launch that tmux
// refused reached its parent 17 minutes later, issue
// iss_01M5KW3QAPJSDZZ4TAMKETXBF7).
func urgentReport(cr childReport) bool {
	return cr.Kind == reportNeeds || (cr.Kind == reportEnded && cr.RunState == contracts.RunFailed && isLaunchFailure(cr.Reason))
}

func earliest(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

func oneLineText(s string, n int) string {
	return clipRunes(strings.Join(strings.Fields(s), " "), n)
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
