package tui_test

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/tui"
)

// A drag over the embedded terminal selects and copies (like a native
// terminal, or Claude Code without Pyre) and never reaches the harness or
// takes the keyboard; a double-click copies a word; a drag over the list
// copies the list's text; a paste while the pane shows types into it.
func TestDragSelectsAndCopies(t *testing.T) {
	var clip []string
	h, _ := startEmbed(t, 160, 40, func(o *tui.Options) { o.Clipboard = func(s string) { clip = append(clip, s) } })
	selectSession(t, h, "ses_synth-a")
	h.WaitFor("pane", 5*sec, func() bool {
		return strings.Contains(h.M.TermText("ses_synth-a"), "Welcome to Claude Code (synthetic)")
	})
	h.Settle()
	x, y := at(t, h, "Welcome to Claude Code")
	h.Send(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	h.Send(tea.MouseMotionMsg{X: x + 3, Y: y, Button: tea.MouseLeft})
	h.Send(tea.MouseMotionMsg{X: x + 6, Y: y, Button: tea.MouseLeft})
	h.Send(tea.MouseReleaseMsg{X: x + 6, Y: y, Button: tea.MouseLeft})
	h.Settle()
	if len(clip) != 1 || clip[0] != "Welcome" {
		t.Fatalf("drag copied %q", clip)
	}
	if h.M.TermFocused() {
		t.Fatal("a drag selection took the keyboard")
	}
	// Double-click: a word ("Claude" starts 11 cells in).
	for i := 0; i < 2; i++ {
		h.Send(tea.MouseClickMsg{X: x + 13, Y: y, Button: tea.MouseLeft})
		h.Send(tea.MouseReleaseMsg{X: x + 13, Y: y, Button: tea.MouseLeft})
	}
	h.Settle()
	if len(clip) != 2 || clip[1] != "Claude" {
		t.Fatalf("double-click copied %q", clip)
	}
	if !h.M.TermFocused() {
		t.Fatal("a click on the terminal should still focus it")
	}
	h.Key("ctrl+]")
	h.Settle()

	// The rest of the frame: a drag over the header copies its text.
	bx, by := at(t, h, "Sessions")
	h.Send(tea.MouseClickMsg{X: bx, Y: by, Button: tea.MouseLeft})
	h.Send(tea.MouseMotionMsg{X: bx + 7, Y: by, Button: tea.MouseLeft})
	h.Send(tea.MouseReleaseMsg{X: bx + 7, Y: by, Button: tea.MouseLeft})
	h.Settle()
	if len(clip) != 3 || clip[2] != "Sessions" {
		t.Fatalf("frame drag copied %q", clip)
	}

	// Paste with the list focused types into the shown terminal.
	h.Paste("synthetic pasted text")
	h.Settle()
	if !h.M.TermFocused() {
		t.Fatal("paste did not focus the terminal")
	}
	h.WaitFor("paste reaches the session", 5*sec, func() bool {
		for _, ft := range h.Terms() {
			if strings.Contains(ft.Input(), "synthetic pasted text") {
				return true
			}
		}
		return false
	})
	for _, ft := range h.Terms() {
		if strings.Contains(ft.Input(), "\x1b[<") {
			t.Fatalf("a drag reached the harness as mouse input: %q", ft.Input())
		}
	}
}
