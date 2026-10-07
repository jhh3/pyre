package tui_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/tui/tuitest"
)

// treeAPI adds synthetic lineage to the contract fixture sessions:
// coordinator (synth-mac) started worker (synth-linux) and migration work
// (synth-mac); worker started long refactor (synth-linux); coordinator also
// started outfit API client, which sits in another project.
type treeAPI struct {
	contracts.API
	parents map[string]string
}

func (a treeAPI) ListSessions(ctx context.Context, r contracts.ListSessionsRequest) (contracts.Page[contracts.SessionView], error) {
	p, err := a.API.ListSessions(ctx, r)
	for i := range p.Items {
		if par, ok := a.parents[p.Items[i].Session.SessionID]; ok {
			p.Items[i].Session.Lineage.ParentSessionID = par
		}
	}
	return p, err
}

var synthTree = map[string]string{
	"ses_synth-b":       "ses_synth-a", // worker ← coordinator
	"ses_synth-fb":      "ses_synth-a", // migration work ← coordinator
	"ses_synth-offline": "ses_synth-b", // long refactor ← worker
	"ses_synth-project": "ses_synth-a", // outfit API client (other project) ← coordinator
}

func startTree(t *testing.T, w int) *tuitest.Harness {
	h, _ := start(t, w, 30, func(api contracts.API) contracts.API { return treeAPI{api, synthTree} }, nil)
	return h
}

// Children sit under their parent in its group, indented with tree glyphs;
// a parent in another group leaves a top-level child with a "↳" hint; za
// on a parent folds its children ("+N"), a click on a child's glyph folds
// too, and ⌥↓ walks the displayed tree order.
func TestSessionTree(t *testing.T) {
	h := startTree(t, 160)
	rows := h.M.SessionRows()
	i := slices.Index(rows, "ses_synth-a")
	if i < 0 || i+3 >= len(rows) || !slices.Equal(rows[i:i+4], []string{"ses_synth-a", "ses_synth-b", "ses_synth-offline", "ses_synth-fb"}) {
		t.Fatalf("children not under their parent: %v\n%s", rows, h.View())
	}
	v := h.View()
	if !strings.Contains(lineOf(v, "worker"), "├ worker") || !strings.Contains(lineOf(v, "long refactor"), "│ └ long refactor") || !strings.Contains(lineOf(v, "migration work"), "└ migration work") {
		t.Fatalf("tree glyphs missing:\n%s", v)
	}
	if !strings.Contains(v, "outfit API client ↳ coordinator") || h.M.TreeDepth("ses_synth-project") != 0 {
		t.Fatalf("cross-group child lacks the parent hint:\n%s", v)
	}
	golden(t, "sessions-tree-160", v)

	// ⌥↓ follows the tree order.
	selectSession(t, h, "ses_synth-a")
	h.Key("alt+down")
	h.Settle()
	if h.M.SelectedSession() != "ses_synth-b" {
		t.Fatalf("alt+down from the parent went to %s", h.M.SelectedSession())
	}
	h.Key("alt+down")
	h.Settle()
	if h.M.SelectedSession() != "ses_synth-offline" {
		t.Fatalf("alt+down did not reach the grandchild: %s", h.M.SelectedSession())
	}
	h.Key("esc")
	h.Settle()

	// za on the parent folds its three descendants.
	selectSession(t, h, "ses_synth-a")
	h.Key("z", "a")
	h.Settle()
	rows = h.M.SessionRows()
	if slices.Contains(rows, "ses_synth-b") || slices.Contains(rows, "ses_synth-offline") || !h.Contains("coordinator +3") {
		t.Fatalf("za did not fold the tree: %v\n%s", rows, h.View())
	}
	// A click on "+3" unfolds.
	x, y := textAt(t, h, "+3")
	clickAt(h, x, y)
	if !slices.Contains(h.M.SessionRows(), "ses_synth-offline") {
		t.Fatalf("click on +N did not unfold:\n%s", h.View())
	}
	// A click on a child's glyph folds its parent and selects it.
	x, y = textAt(t, h, "└ long refactor")
	clickAt(h, x-1, y)
	if slices.Contains(h.M.SessionRows(), "ses_synth-offline") || h.M.SelectedSession() != "ses_synth-b" || !h.Contains("worker +1") {
		t.Fatalf("glyph click did not fold the parent (sel %s):\n%s", h.M.SelectedSession(), h.View())
	}
	h.Key("z", "a")
	h.Settle()

	// Switching to a session under a folded parent opens the parent.
	selectSession(t, h, "ses_synth-a")
	h.Key("z", "a")
	h.Settle()
	h.Key(":")
	h.Type("group machine")
	h.Key("enter")
	h.Settle()
	h.Key(":")
	h.Type("group project")
	h.Key("enter")
	h.Settle()
	if slices.Contains(h.M.SessionRows(), "ses_synth-fb") {
		t.Fatalf("fold lost across regrouping: %v", h.M.SessionRows())
	}
}

// By machine, the cross-machine children leave their parent's group.
func TestSessionTreeByMachineAndTable(t *testing.T) {
	h := startTree(t, 120)
	h.Key("v")
	h.Settle()
	golden(t, "sessions-table-tree-120", h.View())
	h.Key(":")
	h.Type("group machine")
	h.Key("enter")
	h.Settle()
	v := h.View()
	if !strings.Contains(v, "└ migration work") || !strings.Contains(v, "worker ↳ coordinator") || !strings.Contains(v, "└ long refactor") {
		t.Fatalf("machine grouping tree wrong:\n%s", v)
	}
}

// Pinning the parent takes its children into the Pinned group.
func TestSessionTreePinnedFollows(t *testing.T) {
	h := startTree(t, 100)
	selectSession(t, h, "ses_synth-a")
	h.Key("*")
	h.WaitFor("pinned", 3*sec, func() bool { return h.Contains("Pinned 5") })
	rows := h.M.SessionRows()
	// outfit API client (another project) follows too: in Pinned it is
	// in its parent's group.
	if !slices.Equal(rows[:5], []string{"ses_synth-a", "ses_synth-project", "ses_synth-b", "ses_synth-offline", "ses_synth-fb"}) {
		t.Fatalf("children did not follow the pinned parent: %v\n%s", rows, h.View())
	}
}

// lineOf is the first view line containing title (tree guides contain "│",
// so rowOf's pane split does not apply).
func lineOf(view, title string) string {
	for _, l := range strings.Split(view, "\n") {
		if strings.Contains(l, title) {
			return l
		}
	}
	return ""
}
