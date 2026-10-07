package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"

	"github.com/jhh3/pyre/internal/autotitle"
	"github.com/jhh3/pyre/internal/contracts"
)

// Automatic session names (W-names). The hub is authoritative for session
// metadata; the precedence is enforced here:
//   - a session started without a title (non-shell) is tagged title:auto;
//   - a node's title suggestion is applied only while that tag is present;
//   - a person's (owner's) rename removes the reserved title: tags, so it
//     always wins and stops auto-naming;
//   - an agent may rename any session (owner decision, W-childnotify), but only while the title is
//     automatic or agent-chosen (never over a person's title); it replaces
//     title:auto with title:agent.

// checkRename authorizes a title change before the edit transaction.
func (h *Hub) checkRename(ctx context.Context, p contracts.Principal, req contracts.UpdateSessionRequest) error {
	if req.Title == nil {
		return nil
	}
	cur := contracts.Session{SessionID: req.SessionID}
	if p.Kind == contracts.PrincipalSession { // any session may rename any session (not over the owner's title)
		s, err := h.getSession(ctx, h.db, req.SessionID)
		if err != nil {
			return err
		}
		cur = s
	}
	return autotitle.AllowRename(p, cur, *req.Title)
}

// applyTitleSuggestionTx applies a node's derived title (ReportEvent kind
// session.title_suggested) when the session is still automatic. Suggestions
// for other nodes' runs, unknown sessions or person-named sessions are
// ignored (not errors: the node's outbox must not retry them).
func (h *Hub) applyTitleSuggestionTx(ctx context.Context, tx *sql.Tx, nodeID string, ev contracts.Event, origin string, seq uint64) error {
	var sg autotitle.Suggestion
	if json.Unmarshal(ev.Payload, &sg) != nil || sg.SessionID == "" {
		return contracts.Errorf(contracts.ErrInvalid, "title suggestion needs session_id")
	}
	title := autotitle.Clean(sg.Title)
	if title == "" {
		return nil
	}
	if sg.RunID != "" {
		run, err := h.getRun(ctx, tx, sg.RunID)
		if err != nil || run.NodeID != nodeID || run.SessionID != sg.SessionID {
			return nil
		}
	}
	s, err := h.getSession(ctx, tx, sg.SessionID)
	if err != nil {
		if contracts.IsCode(err, contracts.ErrNotFound) {
			return nil
		}
		return err
	}
	if !slices.Contains(s.Tags, autotitle.TagAuto) || s.Title == title {
		return nil
	}
	prev := s.Title
	s.Title = title
	if err := h.updateSessionTx(ctx, tx, s); err != nil {
		return err
	}
	h.auditTx(ctx, tx, auditRec{Action: contracts.AuditSessionAutoTitled, Automatic: true, NodeID: nodeID, SessionID: s.SessionID, RunID: sg.RunID, From: prev, To: title})
	return h.emitSession(ctx, tx, contracts.EvSessionUpdated, s, origin, seq)
}
