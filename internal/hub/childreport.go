package hub

import (
	"context"
	"database/sql"
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
//   - an idle report is skipped when the child messaged the parent during
//     that turn or within the last childRecentMessage;
//   - an ended report replaces the same turn's idle report when that one is
//     still pending or undelivered (cancelled), is skipped when the idle
//     report went out within childReportCoalesce, and otherwise refers to it
//     instead of repeating the last message;
//   - the body leads with one line "<title> finished: <first sentence>" and
//     carries at most childSummaryMax runes of the last message.
//
// A report for a parent without a live run waits (pending) for the
// parent's next run, up to childReportHold; it is dropped when the parent
// is archived. The owner's inbox is never used as a fallback.

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
`}}

// Report kinds and statuses.
const (
	reportIdle   = "idle"
	reportNeeds  = "needs_input"
	reportEnded  = "ended"
	reportQueued = "pending"
	reportSent   = "sent"
	reportSkip   = "skipped"
	reportFailed = "failed"
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
	// childRecentMessage: an idle report is skipped when the child messaged
	// its parent this recently (even in an earlier turn).
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
)

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
	if r.State == contracts.RunRunning || r.State == contracts.RunWaitingInput {
		if err := h.wakeHeldReportsTx(ctx, tx, r.SessionID); err != nil {
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
	enqueue := func(kind, key string, notBefore time.Time) error {
		var exit any
		if r.ExitCode != nil {
			exit = *r.ExitCode
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO child_reports(child_session_id,parent_session_id,run_id,kind,dedupe_key,run_state,state_reason,exit_code,turn_started_at,created_at,not_before,status,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(child_session_id,dedupe_key) DO NOTHING`,
			sess.SessionID, sess.Lineage.ParentSessionID, r.RunID, kind, key, string(r.State), persistence.NullStr(r.StateReason), exit,
			startedAt, persistence.Time(now), persistence.Time(notBefore), reportQueued, persistence.Time(now))
		if err == nil {
			h.reportKick.Store(true)
		}
		return err
	}
	switch {
	case r.State == contracts.RunRunning:
		// A new turn starts unless the current one is still open (a run
		// returning from an approval, a lost observation or background
		// waiting continues the same turn). Its window for "the child
		// already messaged the parent" starts where the previous turn
		// closed (observations can lag the child's own messages).
		if turn == 0 || closed >= turn {
			turn++
			startedAt = closedAt
			if !startedAt.Valid {
				startedAt = sql.NullString{String: persistence.Time(sess.CreatedAt), Valid: true}
			}
		}
	case r.State == contracts.RunWaitingInput:
		if turn > closed {
			closed = turn
			closedAt = sql.NullString{String: persistence.Time(now), Valid: true}
			if err := enqueue(reportIdle, "idle:"+strconv.FormatInt(turn, 10), now); err != nil {
				return err
			}
		}
	case r.State == contracts.RunWaitingApproval:
		needs++
		if err := enqueue(reportNeeds, "needs:"+strconv.FormatInt(needs, 10), now.Add(needsInputDelay)); err != nil {
			return err
		}
	case r.State.Terminal() && !prev.Terminal():
		closed = turn
		closedAt = sql.NullString{String: persistence.Time(now), Valid: true}
		// One report for the turn: the ended report carries the last
		// message, so this turn's unsent idle report is dropped.
		if _, err := tx.ExecContext(ctx, `UPDATE child_reports SET status=?, detail=?, updated_at=? WHERE child_session_id=? AND kind=? AND status=? AND turn_started_at IS ?`,
			reportSkip, "replaced by the ended report", persistence.Time(now), sess.SessionID, reportIdle, reportQueued, startedAt); err != nil {
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

// reportHeldDetail marks a pending report held for the parent's next run.
const reportHeldDetail = "held: the parent has no live run"

// wakeHeldReportsTx makes reports held for parent (no live run) due now:
// its run is live again.
func (h *Hub) wakeHeldReportsTx(ctx context.Context, tx *sql.Tx, parent string) error {
	now := persistence.Time(h.now())
	res, err := tx.ExecContext(ctx, `UPDATE child_reports SET not_before=? WHERE status=? AND parent_session_id=? AND detail=? AND not_before>?`,
		now, reportQueued, parent, reportHeldDetail, now)
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
	RunState  contracts.RunState
	Reason    string
	ExitCode  *int
	TurnStart string
	Attempts  int
	Body      string
	CreatedAt string
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
	for range 64 { // bounded per pass
		now := persistence.Time(h.now())
		rows, err := h.db.QueryContext(ctx, `SELECT report_id, child_session_id, parent_session_id, run_id, kind, run_state, COALESCE(state_reason,''), exit_code,
			COALESCE(turn_started_at,''), attempts, COALESCE(body,''), created_at FROM child_reports WHERE status=? AND not_before<=? ORDER BY report_id LIMIT 16`, reportQueued, now)
		if err != nil {
			return time.Time{}, err
		}
		var due []childReport
		for rows.Next() {
			var cr childReport
			var exit sql.NullInt64
			var st string
			if err := rows.Scan(&cr.ID, &cr.Child, &cr.Parent, &cr.RunID, &cr.Kind, &st, &cr.Reason, &exit, &cr.TurnStart, &cr.Attempts, &cr.Body, &cr.CreatedAt); err != nil {
				rows.Close()
				return time.Time{}, err
			}
			cr.RunState = contracts.RunState(st)
			if exit.Valid {
				c := int(exit.Int64)
				cr.ExitCode = &c
			}
			due = append(due, cr)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
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
	_, _ = h.db.ExecContext(ctx, `UPDATE child_reports SET status=?, detail=?, message_id=?, updated_at=? WHERE report_id=?`,
		status, persistence.NullStr(detail), persistence.NullStr(messageID), persistence.Time(h.now()), id)
}

// processChildReport decides, renders and sends one report.
func (h *Hub) processChildReport(ctx context.Context, cr childReport) {
	skip := func(why string) { h.settleChildReport(ctx, cr.ID, reportSkip, why, "") }
	child, err := h.getSession(ctx, h.db, cr.Child)
	if err != nil {
		skip("child session not found")
		return
	}
	if !reportsTo(child, cr) {
		skip("reports to the parent are off")
		return
	}
	if child.Archived {
		skip("child session is archived")
		return
	}
	parent, err := h.getSession(ctx, h.db, cr.Parent)
	if err != nil {
		skip("parent session not found")
		return
	}
	if parent.Archived {
		skip("parent session is archived")
		return
	}
	if cr.Kind == reportNeeds {
		// The prompt may have been answered (by the owner, in the terminal)
		// during the settle delay or while the report was held.
		if r, err := h.getRun(ctx, h.db, cr.RunID); err != nil || r.State != contracts.RunWaitingApproval {
			skip("no longer waiting for input")
			return
		}
	}
	if cr.Body == "" {
		earlier := ""
		switch cr.Kind {
		case reportIdle:
			since := persistence.Time(h.now().Add(-childRecentMessage))
			if cr.TurnStart != "" && cr.TurnStart < since {
				since = cr.TurnStart
			}
			if n := h.childMessagesToParent(ctx, cr.Child, cr.Parent, since); n > 0 {
				skip(fmt.Sprintf("the child messaged the parent during this turn or in the last %s", childRecentMessage))
				return
			}
		case reportEnded:
			var why string
			if earlier, why = h.coalesceEnded(ctx, cr); why != "" {
				skip(why)
				return
			}
		}
		var recent int
		_ = h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM child_reports WHERE child_session_id=? AND status=? AND updated_at>=?`,
			cr.Child, reportSent, persistence.Time(h.now().Add(-childReportWindow))).Scan(&recent)
		if recent >= childReportBurst {
			skip(fmt.Sprintf("rate limited: %d reports in %s", recent, childReportWindow))
			return
		}
		cr.Body = h.renderChildReport(ctx, child, cr, earlier)
		if _, err := h.db.ExecContext(ctx, `UPDATE child_reports SET body=?, updated_at=? WHERE report_id=?`, cr.Body, persistence.Time(h.now()), cr.ID); err != nil {
			return // retried on the next pass
		}
	}
	if !h.sessionLive(ctx, parent) {
		// Rendered now (the child's transcript may be gone later), sent
		// when the parent runs again (wakeHeldReportsTx).
		if created := persistence.ParseTime(cr.CreatedAt); !created.IsZero() && h.now().Sub(created) > childReportHold {
			skip(fmt.Sprintf("the parent had no live run for %s", childReportHold))
			return
		}
		_, _ = h.db.ExecContext(ctx, `UPDATE child_reports SET detail=?, not_before=?, updated_at=? WHERE report_id=?`,
			reportHeldDetail, persistence.Time(h.now().Add(childReportRecheck)), persistence.Time(h.now()), cr.ID)
		return
	}
	sender := contracts.WithPrincipal(ctx, contracts.Principal{Kind: contracts.PrincipalSession, ID: cr.Child})
	v, err := h.msgs.SendMessage(sender, contracts.SendMessageRequest{
		Mutation:  contracts.Mutation{OperationID: "op_childreport-" + strconv.FormatInt(cr.ID, 10)},
		Recipient: contracts.Address{Kind: contracts.AddrSession, ID: cr.Parent},
		ProjectID: parent.ProjectID,
		Mode:      contracts.ModeQueue,
		Body:      contracts.MessageBody{Text: cr.Body},
	})
	if err != nil {
		cr.Attempts++
		if cr.Attempts >= childReportAttempts || contracts.IsCode(err, contracts.ErrInvalid) || contracts.IsCode(err, contracts.ErrNotFound) {
			_, _ = h.db.ExecContext(ctx, `UPDATE child_reports SET status=?, attempts=?, detail=?, updated_at=? WHERE report_id=?`,
				reportFailed, cr.Attempts, err.Error(), persistence.Time(h.now()), cr.ID)
			return
		}
		_, _ = h.db.ExecContext(ctx, `UPDATE child_reports SET attempts=?, detail=?, not_before=?, updated_at=? WHERE report_id=?`,
			cr.Attempts, err.Error(), persistence.Time(h.now().Add(time.Duration(cr.Attempts)*5*time.Second)), persistence.Time(h.now()), cr.ID)
		return
	}
	h.settleChildReport(ctx, cr.ID, reportSent, "", v.Envelope.MessageID)
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
// replaces it), one sent within childReportCoalesce makes the ended report
// redundant (skip reason), an older one is referred to by message ID.
func (h *Hub) coalesceEnded(ctx context.Context, cr childReport) (earlier, skipWhy string) {
	var msgID, sentAt string
	err := h.db.QueryRowContext(ctx, `SELECT COALESCE(message_id,''), updated_at FROM child_reports WHERE child_session_id=? AND parent_session_id=? AND kind=? AND status=?
		AND report_id<? AND turn_started_at IS ? ORDER BY report_id DESC LIMIT 1`,
		cr.Child, cr.Parent, reportIdle, reportSent, cr.ID, persistence.NullStr(cr.TurnStart)).Scan(&msgID, &sentAt)
	if err != nil || msgID == "" {
		return "", ""
	}
	if mv, err := h.msgs.GetMessage(h.systemCtx(ctx), contracts.GetRequest{ID: msgID}); err == nil &&
		mv.Status.Runtime == contracts.DeliveryQueued && mv.Status.BoundRunID == "" && mv.Status.Ack == contracts.AckNone {
		sender := contracts.WithPrincipal(ctx, contracts.Principal{Kind: contracts.PrincipalSession, ID: cr.Child})
		if _, err := h.msgs.CancelMessage(sender, contracts.CancelMessageRequest{
			Mutation: contracts.Mutation{OperationID: "op_childreport-cancel-" + strconv.FormatInt(cr.ID, 10)}, MessageID: msgID}); err == nil {
			return "", ""
		}
	}
	if ago := h.now().Sub(persistence.ParseTime(sentAt)); ago < childReportCoalesce {
		return "", fmt.Sprintf("the parent got this turn's idle report %s %s ago", msgID, ago.Round(time.Second))
	}
	return msgID, ""
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
	switch cr.Kind {
	case reportIdle:
		text, err := h.childLastMessage(ctx, cr)
		first := firstSentence(text)
		lead(title+" finished", first)
		fmt.Fprintf(&b, "[pyre auto-report] Session %s, %s, finished its turn and is waiting for input%s", name, rel, facts)
		writeSummary(&b, text, first, err)
		b.WriteString("Reply (send_message reply_to) to give it more work; read_session shows the whole transcript; otherwise ack_message.\n")
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
		default:
			lead(title+" "+verb, first)
		}
		fmt.Fprintf(&b, "[pyre auto-report] Session %s, %s, has %s", name, rel, verb)
		if cr.Reason != "" {
			fmt.Fprintf(&b, ": %s", oneLineText(cr.Reason, 300))
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
	}
	return strings.TrimRight(b.String(), "\n")
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
