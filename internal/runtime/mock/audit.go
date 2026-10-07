package mock

import (
	"context"
	"sort"

	"github.com/jhh3/pyre/internal/audit"
	c "github.com/jhh3/pyre/internal/contracts"
)

var _ c.AuditAPI = (*Backend)(nil)

// ListAudit mirrors the hub's audit.list over the mock state: node
// enrollments, session starts, active account assignments and fallback
// plans are derived on the fly (synthetic; no separate audit store).
func (b *Backend) ListAudit(ctx context.Context, req c.ListAuditRequest) (c.Page[c.AuditEntry], error) {
	return query(b, ctx, func(p c.Principal) (c.Page[c.AuditEntry], error) {
		var out []c.AuditEntry
		pyre := c.AuditActor{Kind: "pyre"}
		title := func(id string) string {
			if s := b.sessions[id]; s != nil {
				return s.Title
			}
			return ""
		}
		label := func(id string) string {
			if a := b.accounts[id]; a != nil {
				return a.Account.Label
			}
			return id
		}
		nodeName := func(id string) string {
			if n := b.nodes[id]; n != nil {
				return n.Name
			}
			return ""
		}
		for _, id := range sortedKeys(b.nodes) {
			n := b.nodes[id]
			if n.EnrolledAt.IsZero() {
				continue
			}
			out = append(out, c.AuditEntry{At: n.EnrolledAt, Action: c.AuditNodeEnrolled, Actor: c.AuditActor{Kind: "owner"}, NodeID: id, NodeName: n.Name})
		}
		for _, id := range sortedKeys(b.sessions) {
			s := b.sessions[id]
			e := c.AuditEntry{At: s.CreatedAt, Action: c.AuditSessionStarted, SessionID: id, SessionTitle: s.Title, To: string(s.Harness),
				Actor: c.AuditActor{Kind: string(s.CreatedBy.Kind), ID: s.CreatedBy.ID, Name: s.CreatedBy.Name}}
			if s.CreatedBy.Kind == c.PrincipalSession {
				e.Actor.Name = title(s.CreatedBy.ID)
			}
			if r := b.runs[s.CurrentRunID]; r != nil {
				e.NodeID, e.NodeName, e.RunID = r.NodeID, nodeName(r.NodeID), r.RunID
			}
			out = append(out, e)
		}
		for _, id := range sortedKeys(b.accounts) {
			a := b.accounts[id]
			for _, as := range a.Assignments {
				out = append(out, c.AuditEntry{At: as.AssignedAt, Action: c.AuditAccountChosen, Automatic: true, Actor: pyre,
					NodeID: as.NodeID, NodeName: nodeName(as.NodeID), SessionID: as.SessionID, SessionTitle: title(as.SessionID), RunID: as.RunID,
					AccountID: id, AccountLabel: a.Account.Label, ToNote: audit.UsageNote(a.Latest), Reason: as.Reason})
			}
		}
		for _, f := range b.fallbacks {
			e := c.AuditEntry{At: f.PlannedAt, Automatic: true, Actor: pyre, SessionID: f.SessionID, SessionTitle: title(f.SessionID), RunID: f.FromRunID,
				AccountID: f.FromAccountID, AccountLabel: label(f.FromAccountID), FromAccountID: f.FromAccountID, From: label(f.FromAccountID), Reason: f.Reason}
			if r := b.runs[f.FromRunID]; r != nil {
				e.NodeID, e.NodeName = r.NodeID, nodeName(r.NodeID)
			}
			if a := b.accounts[f.FromAccountID]; a != nil {
				e.FromNote = audit.UsageNote(a.Latest)
			}
			switch f.Outcome {
			case c.FallbackNativeResume, c.FallbackHandoff:
				e.Action, e.ToAccountID, e.To = c.AuditAccountSwitched, f.ToAccountID, label(f.ToAccountID)
				if a := b.accounts[f.ToAccountID]; a != nil {
					e.ToNote = audit.UsageNote(a.Latest)
				}
			default:
				e.Action, e.To = c.AuditAccountWaiting, "waiting for a reset"
				if f.Outcome == c.FallbackIntervention {
					e.To = "needs you"
				}
			}
			out = append(out, e)
		}
		kinds := map[c.AuditKind]bool{}
		for _, k := range req.Kinds {
			kinds[k] = true
		}
		var keep []c.AuditEntry
		for _, e := range out {
			e.Kind = audit.KindOf(e.Action)
			switch {
			case len(kinds) > 0 && !kinds[e.Kind],
				req.SessionID != "" && e.SessionID != req.SessionID,
				req.NodeID != "" && e.NodeID != req.NodeID,
				req.AccountID != "" && e.AccountID != req.AccountID && e.ToAccountID != req.AccountID,
				req.Since != nil && e.At.Before(*req.Since),
				req.Until != nil && e.At.After(*req.Until):
				continue
			}
			e.Summary = audit.Summarize(e)
			keep = append(keep, e)
		}
		sort.SliceStable(keep, func(i, j int) bool { return keep[i].At.After(keep[j].At) })
		for i := range keep {
			keep[i].AuditID = int64(len(keep) - i)
		}
		return paginate(keep, req.PageRequest, b.now())
	})
}
