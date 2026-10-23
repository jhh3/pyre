package hub

import (
	"context"
	"database/sql"
	"slices"
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
)

// Archive cascade (issue iss_…: a lead archived
// itself and three sessions in its tree, one child and two grandchildren
// started by its archived reviewers, stayed live and idle for up to 37
// hours; the coordinator above them could archive them only through owner
// approvals).
//
// When the owner or a session archives a session (itself, a descendant, or
// any session on the owner's say), its live descendants are archived with
// it, recursively through lineage (Session.Parent):
//
//   - the walk starts once the session is archived (at once, or when its
//     archive-when-done completes in finishArchive), so every report a
//     cascaded session makes goes to a parent that is already archived and
//     is dropped ("parent session is archived", childreport.go): no storm
//     of reports into a lead's last turn;
//   - a descendant with a live run is scheduled to archive when done (an
//     idle one is stopped at once, one mid-turn when its turn ends) and its
//     own descendants follow when its archive completes; an ended one is
//     archived now and the walk continues under it; an archived one (a
//     reviewer that archived itself) is passed through;
//   - a descendant the owner started, or attached to, messaged or typed
//     into since it was created (ownerTouchedSince) is his: it and what it
//     started are left alone and listed;
//   - each cascaded archive is audited by Pyre with the reason
//     "archived with ancestor <session>" (the session first archived);
//   - the archive answer (SessionView.ArchiveCascade) and the archived
//     session's final report to its parent say what was archived with it
//     and what was left.
//
// Pyre's own automatic archives (finished children, the nightly sweep)
// do not cascade. Walks are bounded by archiveTreeDepth levels and
// archiveTreeMax sessions, so a lineage cycle cannot loop.

const (
	// archiveTreeDepth bounds lineage walks for archives: the ancestor check
	// of checkArchive and the descendant walk of a cascade.
	archiveTreeDepth = 32
	// archiveTreeMax bounds the sessions one cascade walk visits.
	archiveTreeMax = 500
	// cascadeMarker starts the audit reason of a cascaded archive; the ID of
	// the session first archived follows.
	cascadeMarker = "archived with ancestor "
)

// isAncestor reports whether ancestor is s's parent or one of its
// ancestors (within archiveTreeDepth; a missing session or a cycle ends
// the walk).
func (h *Hub) isAncestor(ctx context.Context, ancestor string, s contracts.Session) bool {
	seen := map[string]bool{s.SessionID: true}
	for id, depth := s.Parent(), 0; id != "" && !seen[id] && depth < archiveTreeDepth; depth++ {
		if id == ancestor {
			return true
		}
		seen[id] = true
		up, err := h.getSession(ctx, h.db, id)
		if err != nil {
			return false
		}
		id = up.Parent()
	}
	return false
}

// childIndex maps each session ID to the sessions directly under it,
// archived ones included, oldest first: one scan of the sessions that can
// have a parent (a lineage parent, or a session creator).
func (h *Hub) childIndex(ctx context.Context) map[string][]contracts.Session {
	rows, err := h.db.QueryContext(ctx, `SELECT `+sesCols+` FROM sessions WHERE lineage LIKE '%"parent_session_id"%' OR created_by LIKE '%"kind":"session"%' ORDER BY created_at, session_id`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := map[string][]contracts.Session{}
	for rows.Next() {
		if s, err := scanSession(rows); err == nil && s.Parent() != "" {
			out[s.Parent()] = append(out[s.Parent()], s)
		}
	}
	return out
}

// ownerUsing: the owner started s, or attached to, messaged or typed into
// it since it was created (any of its runs).
func (h *Hub) ownerUsing(ctx context.Context, s contracts.Session) bool {
	if s.CreatedBy.Kind == contracts.PrincipalOwner {
		return true
	}
	if h.ownerTouchedSince(ctx, s, "", s.CreatedAt) {
		return true
	}
	rows, err := h.db.QueryContext(ctx, `SELECT run_id FROM runs WHERE session_id=?`, s.SessionID)
	if err != nil {
		return false
	}
	var runs []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			runs = append(runs, id)
		}
	}
	rows.Close()
	for _, id := range runs {
		if h.ownerTouchedSince(ctx, s, id, s.CreatedAt) {
			return true
		}
	}
	return false
}

// walkDescendants calls visit for each session under from, breadth first;
// visit says whether to walk on under that session.
func (h *Hub) walkDescendants(ctx context.Context, from string, visit func(contracts.Session) bool) {
	type level struct {
		id    string
		depth int
	}
	children := h.childIndex(ctx)
	queue := []level{{from, 0}}
	seen := map[string]bool{from: true}
	for len(queue) > 0 {
		at := queue[0]
		queue = queue[1:]
		if at.depth >= archiveTreeDepth {
			continue
		}
		for _, c := range children[at.id] {
			if seen[c.SessionID] || len(seen) >= archiveTreeMax {
				continue
			}
			seen[c.SessionID] = true
			if visit(c) {
				queue = append(queue, level{c.SessionID, at.depth + 1})
			}
		}
	}
}

// cascadeArchive archives the live descendants of from, which was just
// archived, on behalf of root (the session first archived; see above).
func (h *Hub) cascadeArchive(ctx context.Context, from, root string) {
	reason := cascadeMarker + root
	byNode := map[string][]string{}
	h.walkDescendants(ctx, from, func(c contracts.Session) bool {
		switch {
		case c.Archived:
			return true // archived already: what it started may still run
		case hasArchiveWhenDone(c):
			return false // its own pending archive cascades when it completes
		case h.ownerUsing(ctx, c):
			return false // the owner's: left alone, with what it started
		case h.runLive(ctx, c.SessionID):
			h.scheduleCascaded(ctx, c, reason+" (when its current turn ends)")
			return false // what it started follows once it is archived
		}
		run := c.CurrentRunID
		ok, err := h.archiveAsPyre(ctx, c, reason, func(cur contracts.Session) bool { return cur.CurrentRunID == run })
		if err != nil || !ok {
			return false
		}
		if r, err := h.getRun(ctx, h.db, run); err == nil {
			byNode[r.NodeID] = append(byNode[r.NodeID], c.SessionID)
		}
		return true
	})
	for node, ids := range byNode {
		h.requestCleanup(node, ids)
	}
}

// scheduleCascaded schedules s to archive when done, audited as Pyre's
// with reason, and wakes the archive-when-done worker.
func (h *Hub) scheduleCascaded(ctx context.Context, s contracts.Session, reason string) {
	done := false
	_ = h.tx(ctx, func(tx *sql.Tx) error {
		cur, err := h.getSession(ctx, tx, s.SessionID)
		if err != nil || cur.Archived || hasArchiveWhenDone(cur) {
			return err
		}
		node := ""
		if r, err := h.getRun(ctx, tx, cur.CurrentRunID); err == nil {
			node = r.NodeID
		}
		cur.Tags = append(slices.Clone(cur.Tags), contracts.TagArchiveWhenDone)
		if err := h.updateSessionTx(ctx, tx, cur); err != nil {
			return err
		}
		if err := h.emitSession(ctx, tx, contracts.EvSessionUpdated, cur, "hub", 0); err != nil {
			return err
		}
		h.auditTx(ctx, tx, auditRec{Action: contracts.AuditSessionArchiveScheduled, Automatic: true, Actor: pyreActor, NodeID: node, SessionID: cur.SessionID,
			Reason: reason})
		done = true
		return nil
	})
	if done {
		h.noteArchiveScheduled(s.SessionID)
	}
}

// cascadeRootOf is the session a completed archive-when-done of sessionID
// cascades for: the root named by a cascaded schedule, the session itself
// when the owner or a session scheduled it, else "" (Pyre's own tags, such
// as a node-started run's, do not cascade).
func (h *Hub) cascadeRootOf(ctx context.Context, sessionID string) string {
	var kind, reason string
	if err := h.db.QueryRowContext(ctx, `SELECT actor_kind, COALESCE(reason,'') FROM audit_log WHERE session_id=? AND action=? ORDER BY id DESC LIMIT 1`,
		sessionID, contracts.AuditSessionArchiveScheduled).Scan(&kind, &reason); err != nil {
		return ""
	}
	if i := strings.Index(reason, cascadeMarker); i >= 0 {
		if f := strings.Fields(reason[i+len(cascadeMarker):]); len(f) > 0 {
			return f[0]
		}
	}
	if kind == "session" || kind == "owner" {
		return sessionID
	}
	return ""
}

// archiveCascadeSummary says what archiving root does (or did) to the
// sessions under it: those archived with it, scheduled to be, or still to
// be reached once a live one's archive completes, and those left because
// the owner is using them. nil when there are none.
func (h *Hub) archiveCascadeSummary(ctx context.Context, root string) *contracts.ArchiveCascade {
	cascaded := map[string]bool{}
	if rows, err := h.db.QueryContext(ctx, `SELECT DISTINCT session_id FROM audit_log WHERE action IN (?,?) AND reason LIKE ?`,
		contracts.AuditSessionArchived, contracts.AuditSessionArchiveScheduled, "%"+cascadeMarker+root+"%"); err == nil {
		for rows.Next() {
			var id string
			if rows.Scan(&id) == nil {
				cascaded[id] = true
			}
		}
		rows.Close()
	}
	var out contracts.ArchiveCascade
	h.walkDescendants(ctx, root, func(c contracts.Session) bool {
		ref := contracts.CascadeSession{SessionID: c.SessionID, Title: c.Title}
		switch {
		case c.Archived:
			if cascaded[c.SessionID] {
				out.Archived = append(out.Archived, ref)
			}
		case !hasArchiveWhenDone(c) && h.ownerUsing(ctx, c):
			out.Left = append(out.Left, ref)
			return false
		default:
			out.Archived = append(out.Archived, ref)
		}
		return true
	})
	if len(out.Archived)+len(out.Left) == 0 {
		return nil
	}
	return &out
}

// archiveCascadeNote is the line an archived session's report to its
// parent carries about the sessions under it ("" when none).
func (h *Hub) archiveCascadeNote(ctx context.Context, sessionID string) string {
	if line := h.archiveCascadeSummary(ctx, sessionID).Line(); line != "" {
		return "Pyre " + line + ".\n"
	}
	return ""
}
