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
	"unicode/utf8"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/persistence"
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
// a reply threads back to the child). An idle report is skipped when the
// child already messaged the parent during that turn, and every report is
// dropped when the parent is archived or has no live run: the owner's inbox
// is never used as a fallback.

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
	// childSummaryMax bounds the relayed last assistant message (runes).
	childSummaryMax = 1500
	// needsInputDelay lets the approval report that accompanies a
	// waiting_approval state land before the report is rendered.
	needsInputDelay = 2 * time.Second
	// childReportAttempts bounds send retries (capacity, transient errors).
	childReportAttempts = 5
	// childReportBurst/childReportWindow bound reports from one child, so a
	// parent and child that keep answering each other cannot loop forever.
	childReportBurst  = 12
	childReportWindow = 10 * time.Minute
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
	sess, err := h.getSession(ctx, tx, r.SessionID)
	if err != nil {
		if contracts.IsCode(err, contracts.ErrNotFound) {
			return nil
		}
		return err
	}
	if !reportsToParent(sess) {
		return nil
	}
	if cur := sess.CurrentRunID; cur != "" && cur != r.RunID && r.PreviousRunID != cur {
		return nil // a non-current (conflicting) run speaks for nobody
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

// ProcessChildReports sends every due report and returns when the next
// pending one becomes due (zero if none). Safe to call concurrently.
func (h *Hub) ProcessChildReports(ctx context.Context) (time.Time, error) {
	h.reportMu.Lock()
	defer h.reportMu.Unlock()
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
		return time.Time{}, nil
	}
	return persistence.ParseTime(next.String), nil
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
	if !reportsToParent(child) || child.Lineage.ParentSessionID != cr.Parent {
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
	if parent.CurrentRunID == "" {
		skip("parent session has no run")
		return
	}
	if pr, err := h.getRun(ctx, h.db, parent.CurrentRunID); err != nil || pr.State.Terminal() {
		skip("parent session has ended")
		return
	}
	if cr.Body == "" {
		switch cr.Kind {
		case reportIdle:
			if n := h.childMessagesToParent(ctx, cr.Child, cr.Parent, cr.TurnStart); n > 0 {
				skip("the child already messaged the parent during this turn")
				return
			}
		case reportNeeds:
			// The prompt may have been answered (by the owner, in the
			// terminal) during the settle delay.
			if r, err := h.getRun(ctx, h.db, cr.RunID); err != nil || r.State != contracts.RunWaitingApproval {
				skip("no longer waiting for input")
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
		cr.Body = h.renderChildReport(ctx, child, cr)
		if _, err := h.db.ExecContext(ctx, `UPDATE child_reports SET body=?, updated_at=? WHERE report_id=?`, cr.Body, persistence.Time(h.now()), cr.ID); err != nil {
			return // retried on the next pass
		}
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

// childMessagesToParent counts messages the child itself sent the parent
// after the previous turn closed (automatic reports excluded).
func (h *Hub) childMessagesToParent(ctx context.Context, child, parent, since string) int {
	var n int
	_ = h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE sender_kind=? AND sender_id=? AND recipient_kind=? AND recipient_id=? AND stored_at>?
		AND message_id NOT IN (SELECT message_id FROM child_reports WHERE child_session_id=? AND message_id IS NOT NULL)`,
		string(contracts.PrincipalSession), child, string(contracts.AddrSession), parent, since, child).Scan(&n)
	return n
}

// renderChildReport writes the message text.
func (h *Hub) renderChildReport(ctx context.Context, child contracts.Session, cr childReport) string {
	var b strings.Builder
	name := fmt.Sprintf("%q (%s)", child.Title, child.SessionID)
	machine := ""
	harness := string(child.Harness)
	if r, err := h.getRun(ctx, h.db, cr.RunID); err == nil {
		machine = h.nodeName(ctx, h.db, r.NodeID, true) // outside any write transaction
		if machine == "" {
			machine = r.NodeID
		}
	}
	facts := func(state string) {
		b.WriteString("Machine: ")
		b.WriteString(orUnknown(machine))
		b.WriteString(" · state: ")
		b.WriteString(state)
		b.WriteString(" · harness: ")
		b.WriteString(orUnknown(harness))
		b.WriteString("\n")
	}
	switch cr.Kind {
	case reportIdle:
		fmt.Fprintf(&b, "[pyre auto-report] Session %s, which you started, finished its turn and is waiting for input.\n", name)
		facts(string(cr.RunState))
		h.writeSummary(ctx, &b, cr, "\nIts last message:\n")
		b.WriteString("\nReply to this message (send_message reply_to) to give it more work, or read_session for the full transcript. No reply is needed otherwise; ack_message it.\n")
	case reportNeeds:
		fmt.Fprintf(&b, "[pyre auto-report] Session %s, which you started, needs input: it is waiting on an approval or question.\n", name)
		facts(string(cr.RunState))
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
		fmt.Fprintf(&b, "[pyre auto-report] Session %s, which you started, has %s", name, verb)
		if cr.Reason != "" {
			fmt.Fprintf(&b, ": %s", oneLineText(cr.Reason, 300))
		}
		if cr.ExitCode != nil {
			fmt.Fprintf(&b, " (exit code %d)", *cr.ExitCode)
		}
		b.WriteString(".\n")
		facts(string(cr.RunState))
		h.writeSummary(ctx, &b, cr, "\nIts last message:\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// writeSummary appends the child's last assistant message, or why it is
// unavailable.
func (h *Hub) writeSummary(ctx context.Context, b *strings.Builder, cr childReport, head string) {
	fetch := h.childSummary
	if fetch == nil {
		fetch = h.lastAssistantText
	}
	text, err := fetch(ctx, cr.Child, cr.RunID)
	switch {
	case err != nil:
		fmt.Fprintf(b, "\n(Its last message is unavailable: %s. Use read_session.)\n", oneLineText(err.Error(), 200))
	case strings.TrimSpace(text) == "":
		b.WriteString("\n(No assistant message was found in its transcript.)\n")
	default:
		b.WriteString(head)
		b.WriteString(clipRunes(strings.TrimSpace(text), childSummaryMax))
		b.WriteString("\n")
	}
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
