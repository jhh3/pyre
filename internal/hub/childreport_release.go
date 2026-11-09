package hub

import (
	"context"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/persistence"
)

// Held reports for a parent that reads its inbox
// (iss_…: a Codex lead in a long turn checked its
// inbox at 07:06 and did not see its reviewer's final report from 07:03,
// held as "batched: the parent is mid-turn").
//
// Batching keeps a busy parent's turn free of reports and folds them into
// one digest when the turn ends. A parent that reads its own inbox is
// asking for its mail now, so its batched reports go out at once, as one
// digest or a lone report like at turn end, and that read lists them (the
// read also settles them: never pasted afterwards). A parent that does not
// look keeps the batching. Only the session itself releases its reports,
// never the owner or another session reading on its behalf.

var _ contracts.ChildReportReleaseAPI = (*Hub)(nil)

// releaseMaxPasses bounds the sends of one release (a digest carries up to
// childReportBatchMax reports).
const releaseMaxPasses = 4

// ReleaseChildReports implements contracts.OpReleaseChildReports.
func (h *Hub) ReleaseChildReports(ctx context.Context, req contracts.ReleaseChildReportsRequest) (contracts.ReleaseChildReportsResponse, error) {
	var out contracts.ReleaseChildReportsResponse
	p, err := principal(ctx)
	if err != nil {
		return out, err
	}
	if p.Kind != contracts.PrincipalSession || p.OnBehalfOf != nil {
		return out, nil
	}
	parent := p.ID
	held := func() int {
		var n int
		_ = h.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM child_reports WHERE status=? AND parent_session_id=? AND hold_state=? AND send_op IS NULL`,
			reportQueued, parent, holdBatched).Scan(&n)
		return n
	}
	start := held()
	if start == 0 {
		return out, nil // the common case: one indexed count, no lock
	}
	h.reportMu.Lock()
	defer h.reportMu.Unlock()
	h.reportPull = parent
	defer func() { h.reportPull = "" }()
	before := start
	for range releaseMaxPasses {
		rows, err := h.db.QueryContext(ctx, `SELECT `+childReportCols+` FROM child_reports WHERE status=? AND parent_session_id=? AND hold_state=? AND send_op IS NULL ORDER BY report_id LIMIT 1`,
			reportQueued, parent, holdBatched)
		if err != nil {
			return out, err
		}
		next, err := scanChildReports(rows)
		if err != nil {
			return out, err
		}
		if len(next) == 0 {
			break
		}
		// Due now (processChildReport re-reads the row); the busy parent
		// does not hold it back while reportPull names it.
		if _, err := h.db.ExecContext(ctx, `UPDATE child_reports SET not_before=? WHERE report_id=?`, persistence.Time(h.now()), next[0].ID); err != nil {
			return out, err
		}
		h.processChildReport(ctx, next[0])
		n := held()
		if n == before {
			break // not sent (deferred or failed): it stays batched, retried by the worker
		}
		before = n
	}
	out.Released = max(0, start-held())
	return out, nil
}
