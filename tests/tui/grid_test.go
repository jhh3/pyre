package tui_test

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/tui/tuitest"
)

// openGrid opens the Grid and waits for its tiles' terminals.
func openGrid(t *testing.T, h *tuitest.Harness) {
	t.Helper()
	h.Key("G")
	h.WaitFor("grid tiles live", 3*sec, func() bool {
		return h.M.ActiveView() == "Grid" && h.M.GridLiveTiles() > 0
	})
	h.Settle()
}

// gridTerms are the fake terminals still open (tile clients and others).
func openTerms(h *tuitest.Harness) []*tuitest.FakeTerm {
	var out []*tuitest.FakeTerm
	for _, f := range h.Terms() {
		if !f.Closed() {
			out = append(out, f)
		}
	}
	return out
}

// Every live session is a live tile with a caption; tiles are read-only
// clients and never take the input lease; leaving the view closes them.
func TestGridTilesEveryLiveSession(t *testing.T) {
	h, rec := startEmbed(t, 240, 65, nil)
	openGrid(t, h)
	v := h.View()
	for _, want := range []string{
		"[S Sessions 3] v grid", "9 live sessions", "! 3 need you",
		"coordinator · ", " working · claude@synth-mac",
		"deploy prep · ! approval · claude@synth-mac",
		"worker · ! needs you · claude@synth-linux",
		"Welcome to Claude Code (synthetic)",
		"enter zoom", ". show ended",
	} {
		if !strings.Contains(v, want) {
			t.Errorf("grid lacks %q:\n%s", want, v)
		}
	}
	if n := len(h.M.GridTiles()); n != 9 {
		t.Fatalf("want 9 tiles (every live session), got %d: %v", n, h.M.GridTiles())
	}
	if live := h.M.GridLiveTiles(); live < 1 || live > 12 {
		t.Fatalf("live tile clients out of bounds: %d", live)
	}
	for _, f := range openTerms(h) {
		if !f.D.ReadOnly || f.D.Command[len(f.D.Command)-1] != "-r" {
			t.Fatalf("a tile must be a read-only attach: %+v", f.D)
		}
	}
	if n := len(rec.Calls("AcquireLease")); n != 0 {
		t.Fatalf("tiles took the input lease (%d)", n)
	}
	h.Key("G") // back to the list view
	h.WaitFor("tile clients closed", 3*sec, func() bool { return h.M.GridLiveTiles() == 0 })
	if open := len(openTerms(h)); open > 2 {
		t.Fatalf("leaving the grid left %d terminals open", open)
	}
}

// Arrows and hjkl move between tiles; down keeps the column.
func TestGridNavigation(t *testing.T) {
	h, _ := startEmbed(t, 240, 65, nil)
	openGrid(t, h)
	tiles := h.M.GridTiles() // 3x3 at this size
	if h.M.GridSelected() != tiles[0] {
		t.Fatalf("first tile should be selected: %s", h.M.GridSelected())
	}
	h.Key("l")
	if h.M.GridSelected() != tiles[1] {
		t.Fatalf("l: want %s, got %s", tiles[1], h.M.GridSelected())
	}
	h.Key("j")
	if h.M.GridSelected() != tiles[4] {
		t.Fatalf("j: want %s (below), got %s", tiles[4], h.M.GridSelected())
	}
	h.Key("right", "down", "h", "up", "k")
	if h.M.GridSelected() != tiles[1] {
		t.Fatalf("after moves: want %s, got %s", tiles[1], h.M.GridSelected())
	}
	h.Key("end")
	if h.M.GridSelected() != tiles[len(tiles)-1] {
		t.Fatalf("end: want the last tile")
	}
	if !strings.Contains(h.View(), "┏━ ") {
		t.Fatalf("the selected tile has a heavy border:\n%s", h.View())
	}
}

// Enter zooms into the Sessions view's live pane with the keyboard;
// ctrl+] returns to the grid (the input lease is released).
func TestGridZoomAndReturn(t *testing.T) {
	h, rec := startEmbed(t, 240, 65, nil)
	openGrid(t, h)
	h.Key("l")
	sid := h.M.GridSelected()
	h.Key("enter")
	h.WaitFor("zoomed and typing", 3*sec, func() bool {
		return h.M.ActiveView() == "Sessions" && h.M.SelectedSession() == sid && h.M.TermFocused() && h.M.GridZoomed() == sid
	})
	if h.M.GridLiveTiles() != 0 {
		t.Fatalf("tiles must close while zoomed")
	}
	h.WaitFor("writable attach", 3*sec, func() bool {
		f := termOf(h, sid)
		return f != nil && !f.D.ReadOnly && len(rec.Calls("AcquireLease")) == 1
	})
	h.Type("hi")
	h.WaitFor("keys reach the session", 3*sec, func() bool { return strings.Contains(termOf(h, sid).Input(), "hi") })
	h.Key("ctrl+]")
	h.WaitFor("back on the grid", 3*sec, func() bool {
		return h.M.ActiveView() == "Grid" && h.M.GridZoomed() == "" && h.M.GridLiveTiles() > 0
	})
	h.WaitFor("lease released", 3*sec, func() bool { return len(rec.Calls("ReleaseLease")) == 1 })
	if h.M.GridSelected() != sid {
		t.Fatalf("selection should survive the zoom: %s", h.M.GridSelected())
	}
}

// A click selects a tile; a second click (double-click) zooms.
func TestGridMouse(t *testing.T) {
	h, _ := startEmbed(t, 240, 65, nil)
	openGrid(t, h)
	x, y := at(t, h, "deploy prep · ")
	h.Send(tea.MouseClickMsg{X: x, Y: y + 3, Button: tea.MouseLeft})
	h.Settle()
	sel := h.M.GridSelected()
	if !strings.Contains(h.View(), "┏━ › deploy prep") {
		t.Fatalf("click should select deploy prep (%s):\n%s", sel, h.View())
	}
	h.Send(tea.MouseClickMsg{X: x, Y: y + 3, Button: tea.MouseLeft})
	h.WaitFor("double-click zooms", 3*sec, func() bool { return h.M.ActiveView() == "Sessions" && h.M.GridZoomed() == sel })
	h.Key("ctrl+]")
	h.WaitFor("back", 3*sec, func() bool { return h.M.ActiveView() == "Grid" })
	// The header's view switch cycles sidenav → table → grid.
	h.Key("G")
	h.WaitFor("back to the list", 3*sec, func() bool { return h.M.ActiveView() == "Sessions" })
	click(t, h, "v sidenav")
	h.WaitFor("table", 3*sec, func() bool { return h.M.SessionsMode() == "table" })
	click(t, h, "v table")
	h.WaitFor("the switch opens the grid", 3*sec, func() bool { return h.M.ActiveView() == "Grid" })
}

// / filters tiles by text or machine; . shows ended sessions.
func TestGridFilterAndEnded(t *testing.T) {
	h, _ := startEmbed(t, 240, 65, nil)
	openGrid(t, h)
	all := len(h.M.GridTiles())
	h.Key("/")
	h.Type("synth-linux")
	h.Key("enter")
	h.Settle()
	v := h.View()
	if n := len(h.M.GridTiles()); n == 0 || n >= all || strings.Contains(v, "@synth-mac") {
		t.Fatalf("filter by machine: %d of %d tiles:\n%s", n, all, v)
	}
	h.Key("esc")
	h.Settle()
	if n := len(h.M.GridTiles()); n != all {
		t.Fatalf("esc should clear the filter: %d", n)
	}
	h.Key(".")
	h.Settle()
	if n := len(h.M.GridTiles()); n <= all {
		t.Fatalf(". should add ended sessions: %d", n)
	}
	if !strings.Contains(h.View(), "ended") {
		t.Fatalf("summary should count ended sessions:\n%s", h.View())
	}
	h.Key(".")
}

// More sessions than fit: the most active page first, tab pages, and the
// number of live tile clients never exceeds the page.
func TestGridPaging(t *testing.T) {
	h, _ := startEmbed(t, 100, 24, nil)
	openGrid(t, h)
	page, pages := h.M.GridPage()
	if pages < 2 || page != 0 {
		t.Fatalf("100x24 should page: %d/%d tiles %v", page, pages, h.M.GridTiles())
	}
	first := h.M.GridTiles()
	if !strings.Contains(h.View(), "more") || !strings.Contains(h.View(), "tab next page") {
		t.Fatalf("summary should say +N more:\n%s", h.View())
	}
	// Needs-you sessions rank first.
	for _, want := range []string{"needs you", "approval"} {
		if !strings.Contains(h.View(), want) {
			t.Errorf("page 1 should show %q tiles:\n%s", want, h.View())
		}
	}
	h.Key("tab")
	h.Settle()
	if p, _ := h.M.GridPage(); p != 1 {
		t.Fatalf("tab should page: %d", p)
	}
	for _, sid := range h.M.GridTiles() {
		for _, f := range first {
			if sid == f {
				t.Fatalf("%s on two pages", sid)
			}
		}
	}
	h.WaitFor("page 2 tiles", 3*sec, func() bool { return h.M.GridLiveTiles() > 0 })
	if live := h.M.GridLiveTiles(); live > len(h.M.GridTiles()) {
		t.Fatalf("%d live tile clients for %d tiles", live, len(h.M.GridTiles()))
	}
	h.Key("shift+tab")
	if p, _ := h.M.GridPage(); p != 0 {
		t.Fatalf("shift+tab should page back: %d", p)
	}
}
