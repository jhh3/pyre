package tui_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/tui"
	"github.com/jhh3/pyre/internal/tui/tuitest"
)

// at returns the screen cell where s is drawn (the n-th occurrence, from
// the top), failing the test when it is not on screen.
func at(t *testing.T, h *tuitest.Harness, s string) (x, y int) {
	t.Helper()
	for y, l := range strings.Split(h.View(), "\n") {
		if i := strings.Index(l, s); i >= 0 {
			return ansi.StringWidth(l[:i]), y
		}
	}
	t.Fatalf("%q not on screen:\n%s", s, h.View())
	return 0, 0
}

func click(t *testing.T, h *tuitest.Harness, s string) {
	t.Helper()
	x, y := at(t, h, s)
	h.Send(tea.MouseClickMsg{X: x + 1, Y: y, Button: tea.MouseLeft})
	h.Send(tea.MouseReleaseMsg{X: x + 1, Y: y, Button: tea.MouseLeft})
	h.Settle()
}

func wheel(h *tuitest.Harness, x, y int, b tea.MouseButton) {
	h.Send(tea.MouseWheelMsg{X: x, Y: y, Button: b})
	h.Settle()
}

func TestMouseModeAndCleanFrame(t *testing.T) {
	h, _ := start(t, 160, 40, nil, nil)
	v := h.M.View()
	if v.MouseMode != tea.MouseModeCellMotion {
		t.Fatalf("mouse mode = %v, want cell motion", v.MouseMode)
	}
	if strings.Contains(v.Content, "\x1b[5;") || strings.Contains(v.Content, "\x1b[6;") {
		t.Fatal("zone markers leaked into the frame")
	}
}

func TestMouseClickTabsSwitchViews(t *testing.T) {
	h, _ := start(t, 160, 40, nil, nil)
	click(t, h, "P Projects")
	if h.M.ActiveView() != "Projects" {
		t.Fatalf("view = %s, want Projects", h.M.ActiveView())
	}
	click(t, h, "M Machines")
	if h.M.ActiveView() != "Machines" {
		t.Fatalf("view = %s, want Machines", h.M.ActiveView())
	}
	click(t, h, "A Accounts")
	if h.M.ActiveView() != "Accounts" {
		t.Fatalf("view = %s, want Accounts", h.M.ActiveView())
	}
	click(t, h, "S Sessions")
	if h.M.ActiveView() != "Sessions" {
		t.Fatalf("view = %s, want Sessions", h.M.ActiveView())
	}
}

func TestMouseClickRowSelectsThenOpens(t *testing.T) {
	h, _ := start(t, 160, 40, nil, nil)
	before := h.M.SelectedSession()
	click(t, h, "coordinator")
	after := h.M.SelectedSession()
	if after == before || after == "" {
		t.Fatalf("click did not select coordinator (still %s)", after)
	}
	if !strings.Contains(strings.Split(h.View(), "\n")[2], "coordinator") {
		t.Fatalf("detail pane does not show the clicked session:\n%s", h.View())
	}
	if n := len(h.Attaches()); n != 0 {
		t.Fatalf("first click attached (%d)", n)
	}
	// A second click on the selected row (a double-click) opens it: the
	// embedded terminal takes the keyboard. (The pane header also reads
	// "coordinator": click the row itself.)
	click(t, h, "working    coordinator")
	h.WaitFor("terminal focus", 5e9, func() bool { return h.M.TermFocused() })
	if h.M.SelectedSession() != after {
		t.Fatal("selection moved on open")
	}
	if n := len(h.Attaches()); n != 0 {
		t.Fatalf("opening the embedded terminal must not suspend for a full-screen attach (%d)", n)
	}
}

func TestMouseShiftClickIsLeftToTheTerminal(t *testing.T) {
	h, _ := start(t, 160, 40, nil, nil)
	x, y := at(t, h, "P Projects")
	h.Send(tea.MouseClickMsg{X: x + 1, Y: y, Button: tea.MouseLeft, Mod: tea.ModShift})
	h.Settle()
	if h.M.ActiveView() != "Sessions" {
		t.Fatalf("shift+click acted: view %s", h.M.ActiveView())
	}
}

func TestMouseWheelScrollsListAndDetail(t *testing.T) {
	h, _ := start(t, 160, 40, nil, nil)
	before := h.M.SelectedSession()
	x, y := at(t, h, "coordinator")
	wheel(h, x, y, tea.MouseWheelDown)
	if h.M.SelectedSession() == before {
		t.Fatal("wheel over the list did not move the selection")
	}
	wheel(h, x, y, tea.MouseWheelUp)
	if h.M.SelectedSession() != before {
		t.Fatal("wheel up did not move back")
	}
	// The terminal pane: watching, the wheel never takes the keyboard.
	dx, dy := at(t, h, "read-only snapshot")
	wheel(h, dx, dy, tea.MouseWheelDown)
	if h.M.TermFocused() || h.M.SelectedSession() != before {
		t.Fatalf("wheel over the unfocused terminal acted:\n%s", h.View())
	}
	// A click on it focuses it; a click on the list returns focus.
	click(t, h, "read-only snapshot")
	h.WaitFor("terminal focus", 3*sec, func() bool { return h.M.TermFocused() })
	click(t, h, "STATUS")
	if h.M.TermFocused() {
		t.Fatal("click on the list did not return focus")
	}
}

func TestMouseClickFooterHints(t *testing.T) {
	h, _ := start(t, 160, 40, nil, nil)
	click(t, h, "? help")
	if !h.M.OverlayOpen() {
		t.Fatal("clicking ? help did not open help")
	}
	// The wheel scrolls the help overlay; a click on its esc hint closes it.
	x, y := at(t, h, "Move")
	wheel(h, x, y, tea.MouseWheelDown)
	click(t, h, "esc close")
	if h.M.OverlayOpen() {
		t.Fatal("clicking esc close did not close help")
	}
	click(t, h, "n new session")
	if !h.M.QuickLaunchOpen() {
		t.Fatalf("clicking n new session: view %s", h.M.ActiveView())
	}
}

func TestMouseClickProjectRowAndManageTab(t *testing.T) {
	h, _ := start(t, 160, 40, nil, nil)
	h.Key("3")
	h.Settle()
	if h.M.ActiveView() != "Machines" {
		t.Fatalf("3: view %s, want Machines", h.M.ActiveView())
	}
	h.Key("2")
	h.Settle()
	if h.M.ActiveView() != "Projects" {
		t.Fatalf("2: view %s, want Projects", h.M.ActiveView())
	}
	first := h.M.SelectedProject()
	h.Key("down")
	second := h.M.SelectedProject()
	h.Key("up")
	if first == second {
		t.Skip("fixture has one project")
	}
	// Click the second project's row: find its name in the list.
	var name string
	for _, p := range mustProjects(t, h) {
		if p.ProjectID == second {
			name = p.Name
		}
	}
	click(t, h, "› "+mustName(t, h, first))
	if h.M.SelectedProject() != first {
		t.Fatal("click on the selected project moved the selection")
	}
	h.Key("esc") // back from the project home if the click opened it
	h.Key("2")
	h.Settle()
	click(t, h, name)
	if h.M.SelectedProject() != second {
		t.Fatalf("click on %q did not select it", name)
	}
}

func mustProjects(t *testing.T, h *tuitest.Harness) []contracts.Project {
	t.Helper()
	b, _ := tuitest.NewMock(t)
	p, err := b.ListProjects(context.Background(), contracts.ListProjectsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	return p.Items
}

func mustName(t *testing.T, h *tuitest.Harness, id string) string {
	t.Helper()
	for _, p := range mustProjects(t, h) {
		if p.ProjectID == id {
			return p.Name
		}
	}
	t.Fatalf("project %s not found", id)
	return ""
}

func TestNumberKeysSwitchViews(t *testing.T) {
	h, _ := start(t, 160, 40, nil, nil)
	// 1–4 follow the tabs: Sessions, Projects, Machines, Accounts.
	for _, c := range []struct{ key, view string }{{"2", "Projects"}, {"3", "Machines"}, {"4", "Accounts"}, {"1", "Sessions"}} {
		h.Key(c.key)
		h.Settle()
		if h.M.ActiveView() != c.view {
			t.Fatalf("%s: view %s, want %s", c.key, h.M.ActiveView(), c.view)
		}
	}
	// I no longer opens an Inbox: there is none.
	h.Key("I")
	h.Settle()
	if h.M.ActiveView() != "Sessions" {
		t.Fatalf("I: view %s", h.M.ActiveView())
	}
}

func TestHeaderTabsShowKeys(t *testing.T) {
	h, _ := start(t, 160, 40, nil, nil)
	head := strings.Split(h.View(), "\n")[0]
	for _, want := range []string{"S Sessions", "P Projects", "M Machines", "A Accounts"} {
		if !strings.Contains(head, want) {
			t.Errorf("header lacks %q: %s", want, head)
		}
	}
	for _, gone := range []string{"Inbox", "Grid"} { // no longer tabs
		if strings.Contains(head, gone) {
			t.Errorf("header still has %q: %s", gone, head)
		}
	}
	if !strings.Contains(h.View(), "n new session") {
		t.Error("Sessions footer lacks n new session")
	}
}

func TestTipsBarFirstLaunchDismissedAndRemembered(t *testing.T) {
	state := filepath.Join(t.TempDir(), "tui-state.json")
	mod := func(o *tui.Options) { o.StateFile = state }
	h, _ := start(t, 160, 40, nil, mod)
	if !h.Contains("Tips") || !h.Contains("1-4 switch tabs") {
		t.Fatalf("no tips bar on first launch:\n%s", h.View())
	}
	golden(t, "tips-160", h.View())
	h.Key("esc")
	h.Settle()
	if h.Contains("hide ✕") {
		t.Fatal("esc did not hide the tips bar")
	}
	b, err := os.ReadFile(state)
	if err != nil || !strings.Contains(string(b), `"tips_dismissed": true`) {
		t.Fatalf("state not saved: %q %v", b, err)
	}
	h2, _ := start(t, 160, 40, nil, mod)
	if h2.Contains("hide ✕") {
		t.Fatal("tips bar shown again after dismissal")
	}
}

func TestTipsBarHidesOnClick(t *testing.T) {
	state := filepath.Join(t.TempDir(), "tui-state.json")
	h, _ := start(t, 160, 40, nil, func(o *tui.Options) { o.StateFile = state })
	click(t, h, "hide ✕")
	if h.Contains("hide ✕") {
		t.Fatal("click did not hide the tips bar")
	}
	if _, err := os.Stat(state); err != nil {
		t.Fatalf("state not saved: %v", err)
	}
}

// noSessions hides every session (an empty fleet).
type noSessions struct{ contracts.API }

func (noSessions) ListSessions(context.Context, contracts.ListSessionsRequest) (contracts.Page[contracts.SessionView], error) {
	return contracts.Page[contracts.SessionView]{}, nil
}

func TestEmptySessionsSaysHowToStart(t *testing.T) {
	h, _ := start(t, 160, 40, func(a contracts.API) contracts.API { return noSessions{a} }, nil)
	if !h.Contains("No sessions yet — press n to start one (or ␣n anywhere)") {
		t.Fatalf("empty Sessions view lacks the start hint:\n%s", h.View())
	}
	click(t, h, "␣n anywhere")
	if !h.M.QuickLaunchOpen() {
		t.Fatalf("clicking the hint: view %s", h.M.ActiveView())
	}
}

func TestMouseWheelScrollsReader(t *testing.T) {
	h, _ := start(t, 160, 40, nil, nil)
	h.Key("r")
	h.Settle()
	if h.M.ActiveView() != "Transcript" {
		t.Fatalf("r: view %s", h.M.ActiveView())
	}
	wheel(h, 10, 10, tea.MouseWheelDown)
	wheel(h, 10, 10, tea.MouseWheelUp)
	if h.M.ActiveView() != "Transcript" {
		t.Fatal("wheel left the reader")
	}
}
