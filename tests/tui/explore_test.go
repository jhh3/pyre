package tui_test

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestExplore prints a scripted tour for manual review:
//
//	PYRE_TUI_EXPLORE=1 PYRE_TUI_WIDTH=100 go test ./tests/tui -run TestExplore -v
//
// PYRE_TUI_KEYS optionally overrides the tour with comma-separated steps;
// each step is space-separated key names, and the screen is printed after
// each step.
func TestExplore(t *testing.T) {
	if os.Getenv("PYRE_TUI_EXPLORE") == "" {
		t.Skip("set PYRE_TUI_EXPLORE=1 to print screens")
	}
	w, _ := strconv.Atoi(os.Getenv("PYRE_TUI_WIDTH"))
	if w == 0 {
		w = 100
	}
	steps := []string{"", "I", "down", "P", "enter", "right", "right", "enter", "esc esc S", ": m a c h", "enter", ": a c c", "enter", ": e n v", "enter", "S n", "enter", "enter", "enter", "enter", "S m", "?"}
	if s := os.Getenv("PYRE_TUI_KEYS"); s != "" {
		steps = strings.Split(s, ",")
	}
	h, _ := start(t, w, 32, nil, nil)
	for _, st := range steps {
		if st != "" {
			h.Key(strings.Fields(st)...)
		}
		h.Settle()
		t.Logf("after %q:\n%s", st, h.View())
	}
}
