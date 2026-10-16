package tui_test

import (
	"context"
	"slices"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

// reparentTreeAPI is treeAPI whose synthetic lineage yields to an explicit
// reparent (W-reparent).
type reparentTreeAPI struct {
	contracts.API
	parents map[string]string
}

func (a reparentTreeAPI) ListSessions(ctx context.Context, r contracts.ListSessionsRequest) (contracts.Page[contracts.SessionView], error) {
	p, err := a.API.ListSessions(ctx, r)
	for i := range p.Items {
		if par, ok := a.parents[p.Items[i].Session.SessionID]; ok && p.Items[i].Session.Lineage.ReparentedAt == nil {
			p.Items[i].Session.Lineage.ParentSessionID = par
		}
	}
	return p, err
}

// > opens a picker of possible parents (never the session itself or its
// descendants); enter moves the session and the tree re-lays; :parent none
// moves it to the top level.
func TestReparentPickerMovesTree(t *testing.T) {
	h, _ := start(t, 160, 30, func(api contracts.API) contracts.API {
		ps := map[string]string{}
		for k, v := range synthTree {
			ps[k] = v
		}
		return reparentTreeAPI{api, ps}
	}, nil)
	if h.M.TreeDepth("ses_synth-offline") != 2 {
		t.Fatalf("fixture: long refactor depth %d", h.M.TreeDepth("ses_synth-offline"))
	}
	// The coordinator's picker hides its own descendants.
	selectSession(t, h, "ses_synth-a")
	h.Key(">")
	h.Settle()
	if !h.Contains("Move coordinator under") {
		t.Fatalf("picker not open:\n%s", h.View())
	}
	got := h.M.SwitcherRows()
	for _, id := range []string{"ses_synth-a", "ses_synth-b", "ses_synth-fb", "ses_synth-offline"} {
		if slices.Contains(got, id) {
			t.Fatalf("%s (itself or a descendant) offered as the new parent: %v", id, got)
		}
	}
	if len(got) == 0 || slices.Contains(got, "none") {
		t.Fatalf("picker rows: %v", got)
	}
	h.Key("esc")
	h.Settle()

	// Move long refactor (under worker) under migration work.
	selectSession(t, h, "ses_synth-offline")
	h.Key(">")
	h.Settle()
	if !h.Contains("current parent") || !h.Contains("top level (no parent)") {
		t.Fatalf("picker lacks the current parent / top level rows:\n%s", h.View())
	}
	h.Type("migration")
	h.Key("enter")
	h.Settle()
	rows := h.M.SessionRows()
	i := slices.Index(rows, "ses_synth-fb")
	if i < 0 || i+1 >= len(rows) || rows[i+1] != "ses_synth-offline" || h.M.TreeDepth("ses_synth-offline") != 2 {
		t.Fatalf("not moved under migration work: %v\n%s", rows, h.View())
	}
	if !h.Contains("now reports to migration work") {
		t.Fatalf("toast after the move:\n%s", h.View())
	}
	h.Key("i")
	h.Settle()
	if !h.Contains("Reports to") || !h.Contains("migration work (reparented from") {
		t.Fatalf("detail after the move:\n%s", h.View())
	}
	h.Key("esc")
	h.Settle()

	// :parent none: to the top level.
	selectSession(t, h, "ses_synth-offline")
	h.Key(":")
	h.Type("parent none")
	h.Key("enter")
	h.Settle()
	if h.M.TreeDepth("ses_synth-offline") != 0 {
		t.Fatalf("not at the top level:\n%s", h.View())
	}
}
