package hub

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/persistence"
)

// Processes an archived session left running (rc122).
//
// Archiving a session revokes its Pyre credential (by design), so a job it
// started detached (setsid -f, systemd-run --user --unit) loses Pyre
// access: its pyre lookups fail. The node's session.cleanup lists such
// processes (SessionCleanup.LeftRunning: Linux, by PYRE_SESSION_ID in
// their environment or the session's run scopes; nothing is stopped), and
// the parent of a session that reports to it gets one line about them:
//
//   - in the session's final report, when that is still waiting to go
//     (held for a busy or stopped parent);
//   - otherwise as a short report of its own (kind reportLeftRunning,
//     rendered here, one per run), since the final report already went.

// reportLeftRunning is the report kind of that short report.
const reportLeftRunning = "left_running"

// leftRunningLine is the line about c's processes ("" when none).
func leftRunningLine(c contracts.SessionCleanup) string {
	n := max(c.LeftRunningCount, len(c.LeftRunning))
	if n == 0 {
		return ""
	}
	parts := make([]string, 0, min(len(c.LeftRunning), 3)+1)
	for i, p := range c.LeftRunning {
		if i == 3 {
			break
		}
		part := "pid " + strconv.Itoa(p.PID)
		if cmd := oneLineText(p.Command, 80); cmd != "" {
			part += ", cmd " + cmd
		}
		parts = append(parts, part)
	}
	if more := n - len(parts); more > 0 && len(parts) > 0 {
		parts = append(parts, fmt.Sprintf("%d more", more))
	}
	list := ""
	if len(parts) > 0 {
		list = " (" + strings.Join(parts, "; ") + ")"
	}
	if n == 1 {
		return "1 process it started is still running" + list + "; it loses this session's Pyre access now that it is archived."
	}
	return fmt.Sprintf("%d processes it started are still running%s; they lose this session's Pyre access now that it is archived.", n, list)
}

// noteLeftRunning tells the parent of an archived session about the
// processes its node's cleanup found still running (see above). Best
// effort; nothing when the session does not report to a live parent.
func (h *Hub) noteLeftRunning(ctx context.Context, nodeID string, c contracts.SessionCleanup) {
	line := leftRunningLine(c)
	if line == "" || c.Error != "" {
		return
	}
	child, err := h.getSession(ctx, h.db, c.SessionID)
	if err != nil || !child.Archived || !reportsToParent(child) {
		return
	}
	parentID := child.Lineage.ParentSessionID
	if parent, err := h.getSession(ctx, h.db, parentID); err != nil || parent.Archived {
		return
	}
	// Under the report lock: a pending report's body is not read for a
	// send while the line is added to it.
	h.reportMu.Lock()
	defer h.reportMu.Unlock()
	now := persistence.Time(h.now())
	var id int64
	var body string
	err = h.db.QueryRowContext(ctx, `SELECT report_id, body FROM child_reports WHERE child_session_id=? AND parent_session_id=? AND status=? AND send_op IS NULL
		AND body IS NOT NULL AND body!='' AND hold_state!=? AND kind!=? ORDER BY report_id DESC LIMIT 1`,
		c.SessionID, parentID, reportQueued, holdFolded, reportNeeds).Scan(&id, &body)
	if err == nil {
		if strings.Contains(body, line) {
			return
		}
		res, err := h.db.ExecContext(ctx, `UPDATE child_reports SET body=?, updated_at=? WHERE report_id=? AND status=? AND send_op IS NULL`,
			withLeftRunning(body, line), now, id, reportQueued)
		if err == nil {
			if n, _ := res.RowsAffected(); n == 1 {
				return
			}
		}
	}
	// The final report already went (or none is coming): a short one.
	title := oneLineText(child.Title, 80)
	if title == "" {
		title = child.SessionID
	}
	machine := h.nodeName(ctx, h.db, nodeID, true)
	if machine == "" {
		machine = nodeID
	}
	text := fmt.Sprintf("%s was archived with processes still running\n[pyre auto-report] Session %q (%s), %s, is archived · machine: %s\n%s",
		title, child.Title, child.SessionID, startedOrReports(child, parentID), orUnknown(machine), line)
	runID, state := child.CurrentRunID, ""
	if r, err := h.getRun(ctx, h.db, runID); err == nil {
		state = string(r.State)
	}
	if _, err := h.db.ExecContext(ctx, `INSERT INTO child_reports(child_session_id,parent_session_id,run_id,kind,dedupe_key,run_state,created_at,not_before,status,body,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(child_session_id,dedupe_key) DO NOTHING`,
		c.SessionID, parentID, runID, reportLeftRunning, reportLeftRunning+":"+runID, state, now, now, reportQueued, text, now); err == nil {
		h.reportKick.Store(true)
		h.kickChildReports()
	}
}

// withLeftRunning adds line to a rendered report body: before its closing
// archived hint when it has one, else at the end.
func withLeftRunning(body, line string) string {
	hint := strings.TrimSuffix(childArchivedHint, "\n")
	if i := strings.LastIndex(body, hint); i >= 0 {
		return body[:i] + line + "\n" + body[i:]
	}
	return strings.TrimRight(body, "\n") + "\n" + line
}
