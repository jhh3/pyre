package hub

import (
	"context"
	"database/sql"

	"github.com/jhh3/pyre/internal/contracts"
)

// Monitors in session views (contracts/monitors.go): sessions.list leaves
// monitor sessions out unless asked, and every view lists the live
// monitors that wake its session, so clients show a badge on the watched
// session instead of a row per monitor.

// liveMonitors returns the live monitors (not archived, run starting,
// running or waiting: contracts.MonitorLive) by the session they wake, oldest first;
// target "" for every session, else only those waking target. One query;
// the tag filter keeps SQLite from decoding any session that is not a
// monitor (tags are a JSON array of strings).
func (h *Hub) liveMonitors(ctx context.Context, q queryer, target string) (map[string][]contracts.MonitorRef, error) {
	tagLike := `%"` + contracts.TagMonitorToPrefix + `%`
	if target != "" {
		tagLike = `%"` + contracts.TagMonitorToPrefix + target + `"%`
	}
	rows, err := q.QueryContext(ctx, `SELECT s.session_id,s.harness,s.title,s.project_id,s.created_by,s.created_at,s.current_run_id,s.lineage,s.tags,s.archived,s.icon,s.owner_active_at,r.state
		FROM sessions s JOIN runs r ON r.run_id = s.current_run_id
		WHERE s.archived = 0 AND s.harness = ? AND s.tags LIKE ? AND r.state IN (?,?,?,?,?)
		ORDER BY s.created_at, s.session_id`,
		string(contracts.HarnessShell), tagLike,
		string(contracts.RunStarting), string(contracts.RunRunning), string(contracts.RunWaitingInput), string(contracts.RunWaitingApproval), string(contracts.RunWaitingBackground))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]contracts.MonitorRef{}
	for rows.Next() {
		var state sql.NullString
		s, err := scanSession(extraScan{sc: rows, extra: []any{&state}})
		if err != nil {
			return nil, err
		}
		if t := s.MonitorTarget(); t != "" {
			out[t] = append(out[t], contracts.MonitorRef{SessionID: s.SessionID, Title: s.Title, State: contracts.RunState(state.String)})
		}
	}
	return out, rows.Err()
}

// attachMonitors fills Monitors on views. A failed lookup leaves the views
// without badges rather than failing the read.
func (h *Hub) attachMonitors(ctx context.Context, q queryer, views []contracts.SessionView) {
	if len(views) == 0 {
		return
	}
	target := ""
	if len(views) == 1 {
		if views[0].Session.Harness == contracts.HarnessShell && views[0].Session.IsMonitor() {
			return // a monitor (polled by itself every check) wakes others, nobody wakes it
		}
		target = views[0].Session.SessionID
	}
	mons, err := h.liveMonitors(ctx, q, target)
	if err != nil || len(mons) == 0 {
		return
	}
	for i := range views {
		if m := mons[views[i].Session.SessionID]; len(m) > 0 {
			views[i].Monitors = m
		}
	}
}
