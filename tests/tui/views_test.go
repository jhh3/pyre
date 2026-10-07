package tui_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/tui"
)

// Sessions has three views: sidenav (default), table and grid. v cycles
// them, G toggles the grid and returns to the view it came from, :view sets
// one, and the choice is remembered. The table opens a session's terminal
// with enter and returns when the terminal lets go (ctrl+]).
func TestSessionsViewsCycleZoomAndRemember(t *testing.T) {
	state := filepath.Join(t.TempDir(), "tui-state.json")
	h, _ := startEmbed(t, 200, 55, func(o *tui.Options) { o.StateFile = state })
	if h.M.SessionsMode() != "sidenav" || h.Contains("ACCOUNT · USAGE") {
		t.Fatalf("default view is not the sidenav:\n%s", h.View())
	}
	h.Key("v")
	h.Settle()
	if h.M.SessionsMode() != "table" || !h.Contains("ACCOUNT · USAGE") || !h.Contains("claude-personal-a 5h 22% wk 41%") || !h.Contains("v table") {
		t.Fatalf("v did not open the table with usage at a glance:\n%s", h.View())
	}
	if b, _ := os.ReadFile(state); !strings.Contains(string(b), `"sessions_view": "table"`) {
		t.Fatalf("view not remembered: %s", b)
	}
	// enter opens the selected session's terminal, typing; ctrl+] returns.
	selectSession(t, h, "ses_synth-a")
	h.Key("enter")
	h.WaitFor("terminal focused from the table", 3*sec, func() bool { return h.M.TermFocused() })
	if h.Contains("ACCOUNT · USAGE") {
		t.Fatalf("the table is still drawn over the terminal:\n%s", h.View())
	}
	h.Key("ctrl+]")
	h.Settle()
	if !h.Contains("ACCOUNT · USAGE") || h.M.SelectedSession() != "ses_synth-a" {
		t.Fatalf("ctrl+] did not return to the table (sel %s):\n%s", h.M.SelectedSession(), h.View())
	}
	// G: the grid and back to the table.
	h.Key("G")
	h.Settle()
	if h.M.ActiveView() != "Grid" || h.M.SessionsMode() != "grid" {
		t.Fatalf("G did not open the grid: %s %s", h.M.ActiveView(), h.M.SessionsMode())
	}
	h.Key("G")
	h.Settle()
	if h.M.SessionsMode() != "table" || !h.Contains("ACCOUNT · USAGE") {
		t.Fatalf("G did not return to the table: %s", h.M.SessionsMode())
	}
	// v in the grid cycles on, to the sidenav; :view sets one.
	h.Key("v", "v")
	h.Settle()
	if h.M.SessionsMode() != "sidenav" || h.M.ActiveView() != "Sessions" {
		t.Fatalf("v from the grid: %s %s", h.M.SessionsMode(), h.M.ActiveView())
	}
	h.Key(":")
	h.Type("view grid")
	h.Key("enter")
	h.Settle()
	if h.M.ActiveView() != "Grid" {
		t.Fatalf(":view grid → %s", h.M.ActiveView())
	}
	// Other tabs and back: Sessions opens in its view.
	h.Key("2", "1")
	h.Settle()
	if h.M.ActiveView() != "Grid" {
		t.Fatalf("Sessions did not reopen in the grid: %s", h.M.ActiveView())
	}
	h2, _ := startEmbed(t, 200, 55, func(o *tui.Options) { o.StateFile = state })
	if h2.M.SessionsMode() != "grid" || h2.M.ActiveView() != "Sessions" {
		// The first screen is the list until Sessions is opened in its view.
		if h2.M.SessionsMode() != "grid" {
			t.Fatalf("remembered view not applied: %s", h2.M.SessionsMode())
		}
	}
}

// The attention sort floats needs-you sessions to the top inside every
// grouping; :sort sets it and it is remembered.
func TestSortAttentionInsideGroups(t *testing.T) {
	state := filepath.Join(t.TempDir(), "tui-state.json")
	h, _, _ := startMock(t, 160, 34, nil, func(o *tui.Options) { o.StateFile = state })
	h.Key("g", "s")
	h.Settle()
	if h.M.SortMode() != "attention" {
		t.Fatalf("g s: sort %s", h.M.SortMode())
	}
	rows := h.M.SessionRows()
	// The project group first (by recency), then No project led by needs you.
	i := slices.Index(rows, "ses_synth-b")
	if i < 0 || !slices.Contains(rows[i:i+3], "ses_synth-apr") || !slices.Contains(rows[i:i+3], "ses_synth-standalone") {
		t.Fatalf("needs-you sessions do not lead their group: %v\n%s", rows, h.View())
	}
	if b, _ := os.ReadFile(state); !strings.Contains(string(b), `"sort": "attention"`) {
		t.Fatalf("sort not remembered: %s", b)
	}
	h.Key(":")
	h.Type("sort activity")
	h.Key("enter")
	h.Settle()
	if h.M.SortMode() != "activity" {
		t.Fatalf(":sort activity → %s", h.M.SortMode())
	}
	h2, _, _ := startMock(t, 160, 34, nil, func(o *tui.Options) { o.StateFile = state })
	if h2.M.SortMode() != "activity" {
		t.Fatalf("remembered sort not applied: %s", h2.M.SortMode())
	}
}
