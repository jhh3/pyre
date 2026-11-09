package hub

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
)

// Pending archives: seeing, canceling and not tripping over them
// (iss_…: a lead called archive_session with no
// arguments mid-build, which scheduled its own archive and, through the
// cascade, its 13 working children; nothing showed whether unarchiving
// had canceled it).
//
//   - Views (sessions.get, get_many, list) carry ArchivePending: the
//     session has TagArchiveWhenDone, or ArchivePendingWith names the
//     ancestor whose pending archive will take it along when it completes
//     (the cascade walk of cascadeArchive, predicted).
//   - sessions.update cancel_archive (and archived=false) removes the tag
//     from the session and from the descendants its archive cascaded to
//     (their schedule is audited "archived with ancestor <session>");
//     descendants scheduled by anyone else keep theirs.
//   - A session archiving itself while sessions under it are still working
//     is refused unless include_children: an accidental no-argument
//     archive_session must not stop a tree of busy workers. Idle and ended
//     descendants cascade as before.

// runWorking: s has a live run that is doing something (not idle at its
// prompt), and that run's state.
func (h *Hub) runWorking(ctx context.Context, s contracts.Session) (contracts.RunState, bool) {
	if s.CurrentRunID == "" || !h.runLive(ctx, s.SessionID) {
		return "", false
	}
	r, err := h.getRun(ctx, h.db, s.CurrentRunID)
	if err != nil {
		return "", false
	}
	switch r.State {
	case contracts.RunStarting, contracts.RunRunning, contracts.RunWaitingApproval, contracts.RunWaitingBackground:
		return r.State, true
	}
	return r.State, false
}

// checkSelfArchiveChildren refuses a session principal's archive of itself
// while sessions its archive would cascade to are still working, unless
// the request includes them.
func (h *Hub) checkSelfArchiveChildren(ctx context.Context, p contracts.Principal, req contracts.UpdateSessionRequest) error {
	if p.Kind != contracts.PrincipalSession || p.ID != req.SessionID || req.Archived == nil || !*req.Archived || req.IncludeChildren {
		return nil
	}
	var working []string
	h.walkDescendants(ctx, req.SessionID, func(c contracts.Session) bool {
		switch {
		case c.Archived:
			return true
		case hasArchiveWhenDone(c):
			return false // already going, with what it started
		case h.ownerUsing(ctx, c):
			return false // left alone by the cascade
		}
		if state, ok := h.runWorking(ctx, c); ok {
			working = append(working, fmt.Sprintf("%s %q (%s)", c.SessionID, c.Title, state))
		}
		return true
	})
	if len(working) == 0 {
		return nil
	}
	shown := working
	if len(shown) > 5 {
		shown = append(shown[:5:5], fmt.Sprintf("… %d more", len(working)-5))
	}
	return contracts.Errorf(contracts.ErrConflict, "not archived: %d session(s) you started are still working (%s); archiving yourself would stop them too. "+
		"Wait for their reports, or pass include_children: true (pyre sessions archive --include-children) to archive them with you",
		len(working), strings.Join(shown, ", ")).WithRef(contracts.KindSession, req.SessionID)
}

// cancelCascaded removes the pending archive of every descendant of root
// whose schedule was a cascade of root's archive, audited for p, and
// returns them.
func (h *Hub) cancelCascaded(ctx context.Context, p contracts.Principal, root string) []contracts.CascadeSession {
	var targets []contracts.Session
	h.walkDescendants(ctx, root, func(c contracts.Session) bool {
		if !c.Archived && hasArchiveWhenDone(c) && h.cascadeRootOf(ctx, c.SessionID) == root {
			targets = append(targets, c)
		}
		return true
	})
	var out []contracts.CascadeSession
	for _, t := range targets {
		done := false
		_ = h.tx(ctx, func(tx *sql.Tx) error {
			cur, err := h.getSession(ctx, tx, t.SessionID)
			if err != nil || cur.Archived || !hasArchiveWhenDone(cur) {
				return err
			}
			node := ""
			if r, err := h.getRun(ctx, tx, cur.CurrentRunID); err == nil {
				node = r.NodeID
			}
			cur.Tags = withoutTag(cur.Tags, contracts.TagArchiveWhenDone)
			if err := h.updateSessionTx(ctx, tx, cur); err != nil {
				return err
			}
			if err := h.emitSession(ctx, tx, contracts.EvSessionUpdated, cur, "hub", 0); err != nil {
				return err
			}
			h.auditTx(ctx, tx, auditRec{Action: contracts.AuditSessionArchiveCanceled, Actor: actorOf(p), NodeID: node, SessionID: cur.SessionID,
				Reason: "canceled with ancestor " + root})
			done = true
			return nil
		})
		if done {
			h.awd.mu.Lock()
			delete(h.awd.scheduled, t.SessionID)
			h.awd.mu.Unlock()
			out = append(out, contracts.CascadeSession{SessionID: t.SessionID, Title: t.Title})
		}
	}
	return out
}

// attachArchivePending fills ArchivePending and ArchivePendingWith on
// views. Cheap when nothing is pending (one indexed query); a failed
// lookup leaves the views unmarked rather than failing the read.
func (h *Hub) attachArchivePending(ctx context.Context, views []contracts.SessionView) {
	if len(views) == 0 {
		return
	}
	for i := range views {
		if !views[i].Session.Archived && hasArchiveWhenDone(views[i].Session) {
			views[i].ArchivePending = true
		}
	}
	rows, err := h.db.QueryContext(ctx, `SELECT session_id FROM sessions WHERE archived=0 AND tags LIKE ?`, "%"+contracts.TagArchiveWhenDone+"%")
	if err != nil {
		return
	}
	var pending []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			pending = append(pending, id)
		}
	}
	rows.Close()
	with := map[string]string{} // descendant -> the pending ancestor that takes it along
	for _, id := range pending {
		if h.cascadeRootOf(ctx, id) == "" {
			continue // Pyre's own tag (a node-started run): it does not cascade
		}
		h.walkDescendants(ctx, id, func(c contracts.Session) bool {
			switch {
			case c.Archived:
				return true
			case hasArchiveWhenDone(c), h.ownerUsing(ctx, c):
				return false
			}
			if _, ok := with[c.SessionID]; !ok {
				with[c.SessionID] = id
			}
			return true
		})
	}
	for i := range views {
		if a, ok := with[views[i].Session.SessionID]; ok && !views[i].ArchivePending {
			views[i].ArchivePending, views[i].ArchivePendingWith = true, a
		}
	}
}
