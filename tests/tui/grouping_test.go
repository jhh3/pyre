package tui_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/mock"
	"github.com/jhh3/pyre/internal/tui"
	"github.com/jhh3/pyre/internal/tui/tuitest"
)

// Project is the default grouping; gb cycles project → attention →
// machine → repo → none → project; :group sets a mode; an explicit choice is remembered in
// the state file and applied on the next start.
func TestGroupByCyclesAndIsRemembered(t *testing.T) {
	state := filepath.Join(t.TempDir(), "tui-state.json")
	mod := func(o *tui.Options) { o.StateFile = state }
	h, _, _ := startMock(t, 160, 34, nil, mod)
	if h.M.GroupMode() != "project" || !h.Contains("Synthetic Outfit 1") || !h.Contains("No project 8") || h.Contains("SESSION · by") {
		t.Fatalf("default grouping is not by project with counts:\n%s", h.View())
	}
	// Machine is no longer the header: each row names its machine, and a
	// quiet footer keeps every machine's health in view.
	if !h.Contains("MACHINE") || !strings.Contains(rowOf(h.View(), "working    outfit API client"), "synth-linux") || !h.Contains("machines  synth-linux ● ") || !h.Contains(" · synth-mac ● ") {
		t.Fatalf("project view lacks the machine column or footer:\n%s", h.View())
	}
	// Projects lead (by their latest session start), "No project" last.
	rows := h.M.SessionRows()
	if len(rows) == 0 || rows[0] != "ses_synth-project" {
		t.Fatalf("project group not first: %v", rows)
	}
	h.Key("g", "b")
	h.Settle()
	if h.M.GroupMode() != "attention" || !h.Contains("Needs you 3") || !h.Contains("Working 4") || !h.Contains("Idle 2") {
		t.Fatalf("gb did not group by attention:\n%s", h.View())
	}
	h.Key("g", "b")
	h.Settle()
	if h.M.GroupMode() != "machine" || !h.Contains("SESSION · by machine") || !h.Contains("synth-mac 5") || h.Contains("MACHINE") {
		t.Fatalf("gb did not group by machine:\n%s", h.View())
	}
	b, err := os.ReadFile(state)
	if err != nil || !strings.Contains(string(b), `"group": "machine"`) || !strings.Contains(string(b), `"group_version": 2`) {
		t.Fatalf("explicit machine choice not remembered: %s %v", b, err)
	}
	h.Key("g", "b")
	h.Settle()
	if h.M.GroupMode() != "repo" || !h.Contains("SESSION · by repo") || !h.Contains("scratch 1") {
		t.Fatalf("gb did not group by repo:\n%s", h.View())
	}
	h.Key("g", "b")
	h.Settle()
	if h.M.GroupMode() != "none" || !h.Contains("SESSION · flat") || h.Contains("synth-mac 5") {
		t.Fatalf("gb did not flatten the list:\n%s", h.View())
	}
	// The flat list leads with what needs you (worker, deploy prep, scratch).
	rows = h.M.SessionRows()
	if len(rows) < 3 || !slices.Contains(rows[:3], "ses_synth-b") || !slices.Contains(rows[:3], "ses_synth-apr") || !slices.Contains(rows[:3], "ses_synth-standalone") {
		t.Fatalf("flat list does not lead with needs-you: %v\n%s", rows, h.View())
	}
	h.Key("g", "b")
	h.Settle()
	if h.M.GroupMode() != "project" {
		t.Fatalf("gb did not wrap to project: %s", h.M.GroupMode())
	}
	h.Key(":")
	h.Type("group machine")
	h.Key("enter")
	h.Settle()
	if h.M.GroupMode() != "machine" {
		t.Fatalf(":group machine → %s", h.M.GroupMode())
	}
	h2, _, _ := startMock(t, 160, 34, nil, mod)
	if h2.M.GroupMode() != "machine" || !h2.Contains("synth-mac 5") {
		t.Fatalf("remembered machine grouping not applied:\n%s", h2.View())
	}
}

// A state file from before project became the default — machine was the
// unwritten default, so it holds no group — opens grouped by project; an
// old explicit non-default choice is kept.
func TestGroupDefaultMigratesOldStateFiles(t *testing.T) {
	for _, c := range []struct{ file, want string }{
		{`{"tips_dismissed": true, "show_ended": true}`, "project"},
		{`{"tips_dismissed": true, "group": "machine"}`, "project"},
		{`{"tips_dismissed": true, "group": "repo", "collapsed": ["repo:outfit"]}`, "repo"},
		{`{"tips_dismissed": true, "group": "machine", "group_version": 2}`, "machine"},
	} {
		state := filepath.Join(t.TempDir(), "tui-state.json")
		if err := os.WriteFile(state, []byte(c.file), 0o600); err != nil {
			t.Fatal(err)
		}
		h, _, _ := startMock(t, 160, 34, nil, func(o *tui.Options) { o.StateFile = state })
		if h.M.GroupMode() != c.want {
			t.Errorf("%s: grouping %s, want %s", c.file, h.M.GroupMode(), c.want)
		}
	}
}

// za folds the selected row's project into one row; enter opens it again;
// a click on a header folds it and a click on the folded row opens it.
// Folds are remembered.
func TestFoldGroupsWithZaEnterAndClick(t *testing.T) {
	state := filepath.Join(t.TempDir(), "tui-state.json")
	h, _, _ := startMock(t, 100, 30, nil, func(o *tui.Options) { o.StateFile = state })
	selectSession(t, h, "ses_synth-project")
	h.Key("z", "a")
	h.Settle()
	if h.M.SelectedSession() != "@group:prj_synth-outfit" || !h.Contains("▸ Synthetic Outfit 1") || slices.Contains(h.M.SessionRows(), "ses_synth-project") {
		t.Fatalf("za did not fold the project (sel %s):\n%s", h.M.SelectedSession(), h.View())
	}
	golden(t, "sessions-folded-100", h.View())
	if b, _ := os.ReadFile(state); !strings.Contains(string(b), "project:prj_synth-outfit") {
		t.Fatalf("fold not remembered: %s", b)
	}
	if !h.Contains("enter expand") {
		t.Fatalf("folded row has no expand hint:\n%s", h.View())
	}
	h.Key("enter")
	h.Settle()
	if slices.Contains(h.M.SessionRows(), "@group:prj_synth-outfit") || h.M.SelectedSession() != "ses_synth-project" {
		t.Fatalf("enter did not open the project: %v sel %s", h.M.SessionRows(), h.M.SelectedSession())
	}
	// A click on the No project header folds it; a click on the folded row opens it.
	x, y := textAt(t, h, "No project 8")
	clickAt(h, x, y)
	h.Settle()
	if !slices.Contains(h.M.SessionRows(), "@group:\x7f") {
		t.Fatalf("click on the header did not fold: %v\n%s", h.M.SessionRows(), h.View())
	}
	x, y = textAt(t, h, "No project 8")
	clickAt(h, x, y)
	h.Settle()
	if slices.Contains(h.M.SessionRows(), "@group:\x7f") {
		t.Fatalf("click on the folded row did not open it: %v\n%s", h.M.SessionRows(), h.View())
	}
	// A filter shows every match, folded or not.
	selectSession(t, h, "ses_synth-apr")
	h.Key("z", "a")
	h.Key("/")
	h.Type("deploy")
	h.Key("enter")
	h.Settle()
	if !h.Contains("deploy prep") {
		t.Fatalf("filter hid a match in a folded group:\n%s", h.View())
	}
}

// The Grid's tiles follow the list's project grouping: pinned first, then
// projects, then machine and title.
func TestGridFollowsProjectGrouping(t *testing.T) {
	h, _, _ := startMock(t, 160, 40, nil, nil)
	h.Key("G")
	h.Settle()
	g := h.M.GridOrder()
	if len(g) == 0 || g[0] != "ses_synth-project" {
		t.Fatalf("grid does not lead with the project's session: %v\n%s", g, h.View())
	}
}

// textAt finds s in the left half of the screen (the list).
func textAt(t *testing.T, h *tuitest.Harness, s string) (x, y int) {
	t.Helper()
	for y, l := range strings.Split(h.View(), "\n") {
		if i := strings.Index(l, s); i >= 0 && ansi.StringWidth(l[:i]) < 60 {
			return ansi.StringWidth(l[:i]), y
		}
	}
	t.Fatalf("%q not in the list:\n%s", s, h.View())
	return 0, 0
}

// P pins through sessions.update with the reserved tags (other tags
// kept); the session moves to a Pinned group on top in every grouping and
// leads the Grid; P again unpins. A pin made elsewhere (another TUI, the
// phone) arrives through the session event.
func TestPinMovesToTopAndSyncsThroughTags(t *testing.T) {
	var mb *mock.Backend
	h, rec, b := startMock(t, 100, 30, func(b *mock.Backend) { mb = b }, func(o *tui.Options) { o.Watch = tui.PollWatcher(mb) })
	selectSession(t, h, "ses_synth-b")
	h.Key("*")
	h.WaitFor("pin sent", 3*sec, func() bool { return len(rec.Calls("UpdateSession")) == 1 })
	req := rec.Calls("UpdateSession")[0].Request.(contracts.UpdateSessionRequest)
	if req.SessionID != "ses_synth-b" || req.Tags == nil || req.Title != nil || req.Archived != nil || req.OperationID == "" {
		t.Fatalf("pin request %+v", req)
	}
	if !slices.Contains(*req.Tags, "pinned") || !slices.ContainsFunc(*req.Tags, func(s string) bool { return strings.HasPrefix(s, "pinned_at:") }) {
		t.Fatalf("pin tags %v", *req.Tags)
	}
	h.Settle()
	if rows := h.M.SessionRows(); rows[0] != "ses_synth-b" || !h.Contains("Pinned 1") {
		t.Fatalf("pinned session not on top: %v\n%s", rows, h.View())
	}
	golden(t, "sessions-pinned-100", h.View())
	for _, mode := range []string{"machine", "repo", "none", "project"} {
		h.Key(":")
		h.Type("group " + mode)
		h.Key("enter")
		h.Settle()
		if rows := h.M.SessionRows(); rows[0] != "ses_synth-b" || !h.Contains("Pinned 1") {
			t.Fatalf("%s: pinned session not on top: %v\n%s", mode, rows, h.View())
		}
	}
	h.Key("G")
	h.Settle()
	if g := h.M.GridOrder(); len(g) == 0 || g[0] != "ses_synth-b" {
		t.Fatalf("grid does not lead with the pinned session: %v\n%s", g, h.View())
	}
	h.Key("G")
	h.Settle()
	selectSession(t, h, "ses_synth-b")
	h.Key("*")
	h.WaitFor("unpin sent", 3*sec, func() bool { return len(rec.Calls("UpdateSession")) == 2 })
	req = rec.Calls("UpdateSession")[1].Request.(contracts.UpdateSessionRequest)
	if req.Tags == nil || slices.ContainsFunc(*req.Tags, func(s string) bool { return strings.HasPrefix(s, "pinned") }) {
		t.Fatalf("unpin tags %v", req.Tags)
	}
	h.Settle()
	if h.Contains("Pinned 1") {
		t.Fatalf("unpinned session still pinned:\n%s", h.View())
	}

	// Another client pins a session: the event brings it here.
	tags := []string{"pinned", "pinned_at:1700000000"}
	if _, err := b.UpdateSession(context.Background(), contracts.UpdateSessionRequest{Mutation: contracts.Mutation{OperationID: contracts.NewID(contracts.PrefixOperation)}, SessionID: "ses_synth-a", Tags: &tags}); err != nil {
		t.Fatal(err)
	}
	h.WaitFor("pin from elsewhere", 3*sec, func() bool { r := h.M.SessionRows(); return len(r) > 0 && r[0] == "ses_synth-a" })
}
