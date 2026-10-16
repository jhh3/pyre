package hub

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/persistence"
)

// Reparenting (W-reparent). UpdateSessionRequest.Parent moves a session
// under another session (or to the top level): Lineage.ParentSessionID
// changes, CreatedBy stays the historical creator, and Lineage records
// where it came from. Automatic reports follow the new parent from the next
// reportable point on (pending ones retarget in the same transaction); the
// new parent and the child each get a Pyre notice message afterwards.

// reportNotice is the kind and status of a recorded reparent notice row.
const reportNotice = "reparent_notice"

// reparentTarget normalizes a requested parent ("none" means top level).
func reparentTarget(p *string) string {
	t := strings.TrimSpace(*p)
	if strings.EqualFold(t, "none") {
		return ""
	}
	return t
}

// checkReparent validates a reparent request and the caller's permission
// (outside the write transaction, like checkArchive).
func (h *Hub) checkReparent(ctx context.Context, p contracts.Principal, req contracts.UpdateSessionRequest) error {
	if req.Parent == nil {
		return nil
	}
	child, err := h.getSession(ctx, h.db, req.SessionID)
	if err != nil {
		return err
	}
	to := reparentTarget(req.Parent)
	if err := h.reparentAllowed(ctx, p, child); err != nil {
		return err
	}
	if to == "" {
		return nil
	}
	if to == child.SessionID {
		return contracts.Errorf(contracts.ErrInvalid, "a session cannot be its own parent").WithRef(contracts.KindSession, to)
	}
	parent, err := h.getSession(ctx, h.db, to)
	if err != nil {
		if contracts.IsCode(err, contracts.ErrNotFound) {
			return contracts.Errorf(contracts.ErrInvalid, "parent session %s not found", to).WithRef(contracts.KindSession, to)
		}
		return err
	}
	if parent.Archived {
		return contracts.Errorf(contracts.ErrInvalid, "parent session %s (%q) is archived; unarchive it or choose another parent", to, parent.Title).
			WithRef(contracts.KindSession, to)
	}
	// No cycles: the new parent must not sit under the child.
	seen := map[string]bool{}
	for a := parent; ; {
		up := a.Parent()
		if up == "" || seen[up] || len(seen) > 256 {
			break
		}
		if up == child.SessionID {
			return contracts.Errorf(contracts.ErrInvalid, "cannot move %q under %q: %q is under %q (that would make a cycle)",
				child.Title, parent.Title, parent.Title, child.Title).WithRef(contracts.KindSession, to)
		}
		seen[up] = true
		if a, err = h.getSession(ctx, h.db, up); err != nil {
			break
		}
	}
	return nil
}

// reparentAllowed: the owner (and the hub) may reparent any session; a
// session principal itself, the sessions it currently parents, and the
// sessions in its own project.
func (h *Hub) reparentAllowed(ctx context.Context, p contracts.Principal, child contracts.Session) error {
	switch {
	case isOwner(p) || p.Kind == contracts.PrincipalSystem:
		return nil
	case p.Kind == contracts.PrincipalSession:
		if p.ID == child.SessionID || child.Parent() == p.ID {
			return nil
		}
		if child.ProjectID != nil && *child.ProjectID != "" {
			if self, err := h.getSession(ctx, h.db, p.ID); err == nil && self.ProjectID != nil && *self.ProjectID == *child.ProjectID {
				return nil
			}
		}
		return contracts.Errorf(contracts.ErrForbidden, "an agent may reparent only itself, the sessions it currently parents, and sessions in its own project; %q (%s) is none of these; ask the owner",
			child.Title, child.SessionID).WithRef(contracts.KindSession, child.SessionID)
	}
	return contracts.Errorf(contracts.ErrForbidden, "only the owner or a session may reparent sessions").WithRef(contracts.KindSession, child.SessionID)
}

// applyReparent sets s's parent to `to` ("" = top level) and returns the
// changed fields. Attaching to a parent turns reports to it on unless the
// session opted out (report:none).
func (h *Hub) applyReparent(s *contracts.Session, to string) map[string]any {
	from := s.Parent()
	if from == to {
		return nil
	}
	now := h.now()
	s.Lineage.ParentSessionID = to
	s.Lineage.ReparentedFrom = from
	s.Lineage.ReparentedAt = &now
	ch := map[string]any{"lineage": s.Lineage}
	if to != "" && !slices.Contains(s.Tags, contracts.TagReportNone) && !slices.Contains(s.Tags, contracts.TagReportParent) {
		s.Tags = append(slices.Clone(s.Tags), contracts.TagReportParent)
		ch["tags"] = s.Tags
	}
	return ch
}

// reparentTx records a parent change inside the update transaction: the
// audit entry, and pending automatic reports retarget to the new parent
// (or are dropped when the session moves to the top level).
func (h *Hub) reparentTx(ctx context.Context, tx *sql.Tx, act contracts.AuditActor, node string, before, after contracts.Session) {
	from, to := before.Parent(), after.Parent()
	if from == to {
		return
	}
	title := func(id string) string {
		if id == "" {
			return "the top level"
		}
		if s, err := h.getSession(ctx, tx, id); err == nil && s.Title != "" {
			return fmt.Sprintf("%q", s.Title)
		}
		return id
	}
	h.auditTx(ctx, tx, auditRec{Action: contracts.AuditSessionReparented, Actor: act, NodeID: node, SessionID: after.SessionID,
		From: from, To: to, Reason: "from " + title(from) + " to " + title(to)})
	if to != "" {
		_, _ = tx.ExecContext(ctx, `UPDATE child_reports SET parent_session_id=?, body=NULL WHERE child_session_id=? AND status=?`,
			to, after.SessionID, reportQueued)
		return
	}
	_, _ = tx.ExecContext(ctx, `UPDATE child_reports SET status=?, detail=?, updated_at=? WHERE child_session_id=? AND status=?`,
		reportSkip, "moved to the top level", persistence.Time(h.now()), after.SessionID, reportQueued)
}

// reparentNotices tells the new parent and the child about a reparent with
// ordinary Pyre messages (queue mode, so doorbell delivery applies). The
// notice to the parent comes from the child (a reply reaches the child);
// the one to the child comes from its new parent (a reply reaches the
// parent). A recipient that made the change itself, or is archived, is
// not told. Best effort: the reparent has already committed.
func (h *Hub) reparentNotices(ctx context.Context, p contracts.Principal, opID string, child contracts.Session, from string) {
	to := child.Parent()
	name := func(id string) (string, contracts.Session, bool) {
		s, err := h.getSession(ctx, h.db, id)
		if err != nil {
			return id, s, false
		}
		return fmt.Sprintf("%q (%s)", s.Title, id), s, true
	}
	self := p.Kind == contracts.PrincipalSession
	childName := fmt.Sprintf("%q (%s)", child.Title, child.SessionID)
	fromNote := ""
	if from != "" {
		n, _, _ := name(from)
		fromNote = " It used to report to " + n + "."
	}
	send := func(sender contracts.Principal, to contracts.Session, suffix, text string) {
		if to.Archived || (self && p.ID == to.SessionID) {
			return
		}
		mv, err := h.msgs.SendMessage(contracts.WithPrincipal(ctx, sender), contracts.SendMessageRequest{
			Mutation:  contracts.Mutation{OperationID: opID + ":reparent-" + suffix},
			Recipient: contracts.Address{Kind: contracts.AddrSession, ID: to.SessionID},
			ProjectID: to.ProjectID,
			Mode:      contracts.ModeQueue,
			Body:      contracts.MessageBody{Text: text},
		})
		if err == nil && suffix == "parent" {
			// Recorded like an automatic report, so it does not count as the
			// child messaging its parent this turn (the idle report still goes).
			now := persistence.Time(h.now())
			_, _ = h.db.ExecContext(ctx, `INSERT INTO child_reports(child_session_id,parent_session_id,run_id,kind,dedupe_key,run_state,created_at,not_before,status,message_id,updated_at)
				VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(child_session_id,dedupe_key) DO NOTHING`,
				child.SessionID, to.SessionID, child.CurrentRunID, reportNotice, "reparent:"+opID, "", now, now, reportNotice, mv.Envelope.MessageID, now)
		}
	}
	childP := contracts.Principal{Kind: contracts.PrincipalSession, ID: child.SessionID}
	if to == "" {
		sender := p
		if p.Kind == contracts.PrincipalSystem {
			return
		}
		send(sender, child, "child", "[pyre notice] You no longer report to a parent session; you are a top-level session now."+
			fromNote+" Automatic reports are off. Message the owner only if you are told to.")
		return
	}
	parentName, parent, ok := name(to)
	if !ok {
		return
	}
	reports := slices.Contains(child.Tags, contracts.TagReportParent)
	pt := "[pyre notice] Session " + childName + " now reports to you." + fromNote
	if reports {
		pt += " Its automatic end-of-turn reports come to you from now on."
	}
	pt += " Reply to this message to reach it; no reply is needed otherwise (ack_message it)."
	send(childP, parent, "parent", pt)
	ct := "[pyre notice] You now report to " + parentName + "; send status there (send_message to " + to + ")." + fromNote
	if reports {
		ct += " Pyre relays your end-of-turn summaries to it automatically."
	}
	ct += " $PYRE_PARENT_SESSION_ID still names the session that started you; this message supersedes it."
	send(contracts.Principal{Kind: contracts.PrincipalSession, ID: to}, child, "child", ct)
}

// startedOrReports words the relation in an automatic report: "which you
// started" for the launcher, "which reports to you" after a reparent.
func startedOrReports(child contracts.Session, parentID string) string {
	if (child.CreatedBy.Kind == contracts.PrincipalSession && child.CreatedBy.ID == parentID) ||
		(child.Lineage.ReparentedAt == nil && child.Lineage.ParentSessionID == parentID) {
		return "which you started"
	}
	return "which reports to you"
}
