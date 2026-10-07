package tui_test

import (
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/tui"
)

// TestViewFrameCost logs the cost of one frame (Model.View) with the
// embedded terminal focused at 160x45 in colour: what every keystroke echo
// pays before Bubble Tea can flush it.
func TestViewFrameCost(t *testing.T) {
	h, _ := startEmbed(t, 160, 45, func(o *tui.Options) { o.NoColor = false })
	selectSession(t, h, "ses_synth-a")
	h.Key("enter")
	h.WaitFor("focused", 3*sec, func() bool { return h.M.TermFocused() && strings.Contains(h.M.TermText("ses_synth-a"), "Welcome") })
	const n = 200
	start := time.Now()
	for i := 0; i < n; i++ {
		_ = h.M.View()
	}
	t.Logf("View: %v per frame (160x45, colour, focused terminal)", time.Since(start)/n)
}
