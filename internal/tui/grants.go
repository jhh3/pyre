package tui

import (
	"context"
	"fmt"

	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/contracts"
)

// Standing grants (W-grants): the project's lasting go-aheads from the
// owner, listed on the project's Grants tab with a revoke action (x). The
// owner creates them by approving a session's proposal in his inbox (or
// `pyre grant add`); revoking sends asks for that action back to him.

func grantsKey(pid string) string { return "proj:" + pid + ":grants" }

func (m *Model) grantsAPI() (contracts.StandingGrantsAPI, bool) {
	g, ok := m.api.(contracts.StandingGrantsAPI)
	return g, ok
}

// loadProjectGrants loads the project's active grants.
func (m *Model) loadProjectGrants(force bool) tea.Cmd {
	api, ok := m.grantsAPI()
	if !ok {
		return nil
	}
	pid := m.curProject
	fn := func(ctx context.Context) (any, error) {
		r, err := api.ListGrants(ctx, contracts.ListGrantsRequest{ProjectID: &pid})
		return r.Grants, err
	}
	if force {
		return m.reload(grantsKey(pid), fn)
	}
	return m.ensure(grantsKey(pid), 0, fn)
}

// grantWords is a grant's source: the owner's own words only.
func grantWords(g contracts.StandingGrant) string {
	switch {
	case g.AskID != "" && g.Quote != "":
		return fmt.Sprintf("you: %q (approved %s)", g.Quote, g.AskID)
	case g.AskID != "":
		return "you approved proposal " + g.AskID + " (no words)"
	case g.Quote != "":
		return fmt.Sprintf("set by you: %q", g.Quote)
	}
	return "set by you"
}

func (m *Model) projectGrantsView(p contracts.Project, w, h int) []string {
	out := []string{m.colHeader("  STANDING GO-AHEADS", w)}
	if _, ok := m.grantsAPI(); !ok {
		return append(out, m.th.dim.Render("  this hub has no standing grants"))
	}
	gs, st := get[[]contracts.StandingGrant](m, grantsKey(p.ProjectID))
	l := m.listFor(grantsKey(p.ProjectID))
	ids := make([]string, len(gs))
	for i, g := range gs {
		ids[i] = g.GrantID
	}
	l.setIDs(ids)
	if len(gs) == 0 {
		return append(out, m.loadingOr(st, "none — sessions propose them (propose_grant); you approve in the inbox"))
	}
	now := m.now()
	start, end := l.window((h - 2) / 3)
	for i := start; i < end; i++ {
		g := gs[i]
		sel := i == l.idx
		scope := "project"
		if g.Scope == contracts.GrantScopeSession {
			scope = "tree " + g.SessionID
		}
		used := ""
		if g.UsedCount > 0 {
			used = fmt.Sprintf(" · used %d×", g.UsedCount)
		}
		row := m.cursor(sel) + pad(g.Action+" → "+g.Option, w-36) + " " + m.rowTone(sel, toneMuted, padLeft("expires in "+countdown(now, g.ExpiresAt)+used, 33))
		out = append(out, m.markRow(l, i, sel, trunc(row, w), w))
		out = append(out, "    "+m.th.dim.Render(trunc(grantWords(g), w-4)))
		note := scope
		if g.Condition != "" {
			note += " · note (not checked): " + g.Condition
		}
		out = append(out, "    "+m.th.faint.Render(trunc(note, w-4)))
	}
	return append(out, "", m.th.dim.Render(trunc("x revoke · asks for a revoked action reach you again", w)))
}

// keyProjectGrants handles the Grants tab.
func (m *Model) keyProjectGrants(key string) tea.Cmd {
	pid := m.curProject
	l := m.listFor(grantsKey(pid))
	if navKey(l, key, m.halfPage()) {
		return nil
	}
	if key != "x" || l.sel == "" {
		return nil
	}
	api, ok := m.grantsAPI()
	if !ok {
		return nil
	}
	gs, _ := get[[]contracts.StandingGrant](m, grantsKey(pid))
	var g *contracts.StandingGrant
	for i := range gs {
		if gs[i].GrantID == l.sel {
			g = &gs[i]
		}
	}
	if g == nil {
		return nil
	}
	id, what := g.GrantID, g.Action+" → "+g.Option
	m.overlay = &confirmOverlay{
		title: "Revoke standing go-ahead",
		body:  []string{fmt.Sprintf("Revoke %q (%s)?", what, id), "", "Asks for it reach you again."},
		yes: func(m *Model) tea.Cmd {
			req := contracts.RevokeGrantRequest{Mutation: contracts.Mutation{OperationID: m.opID("revoke-grant:" + id)}, GrantID: id, Reason: "revoked in the TUI"}
			return m.do(func(ctx context.Context) (any, error) { return api.RevokeGrant(ctx, req) }, func(m *Model, v any, err error) tea.Cmd {
				if err != nil {
					return tea.Batch(m.errToast("revoke failed", err), m.loadProjectGrants(true))
				}
				delete(m.opIDs, "revoke-grant:"+id)
				return tea.Batch(m.setToast("revoked "+trunc(what, 40), toneMuted), m.loadProjectGrants(true))
			})
		},
	}
	return nil
}
