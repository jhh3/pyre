package tui_test

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/tui/tuitest"
)

// R opens the title editor prefilled; typing and enter saves through
// sessions.update with a fresh operation id; esc cancels; an empty title
// is refused with a hint.
func TestRenameSessionWithR(t *testing.T) {
	h, rec, _ := startMock(t, 100, 30, nil, nil)
	selectSession(t, h, "ses_synth-b")
	h.Key("R")
	if !h.Contains("Rename session") {
		t.Fatalf("R did not open the editor:\n%s", h.View())
	}
	golden(t, "rename-editor-100", h.View())
	h.Key("esc")
	h.Settle()
	if h.Contains("Rename session") || len(rec.Calls("UpdateSession")) != 0 {
		t.Fatalf("esc did not cancel cleanly:\n%s", h.View())
	}

	h.Key("R", "ctrl+u")
	h.Key("enter")
	if !h.Contains("A session needs a name") || len(rec.Calls("UpdateSession")) != 0 {
		t.Fatalf("empty title not refused with a hint:\n%s", h.View())
	}
	h.Type("billing webhooks")
	h.Key("enter")
	h.WaitFor("rename sent", 3*sec, func() bool { return len(rec.Calls("UpdateSession")) == 1 })
	req := rec.Calls("UpdateSession")[0].Request.(contracts.UpdateSessionRequest)
	if req.SessionID != "ses_synth-b" || req.Title == nil || *req.Title != "billing webhooks" || req.OperationID == "" || req.Archived != nil || req.Tags != nil {
		t.Fatalf("rename request %+v", req)
	}
	h.WaitFor("new title in the list and header", 3*sec, func() bool { return strings.Count(h.View(), "billing webhooks") >= 3 })
}

func TestRenameSessionCommandAndHeaderClick(t *testing.T) {
	h, rec, _ := startMock(t, 100, 30, nil, nil)
	selectSession(t, h, "ses_synth-b")
	h.Key(":")
	h.Type("rename stripe retry fix")
	h.Key("enter")
	h.WaitFor(":rename sent", 3*sec, func() bool { return len(rec.Calls("UpdateSession")) == 1 })
	req := rec.Calls("UpdateSession")[0].Request.(contracts.UpdateSessionRequest)
	if req.SessionID != "ses_synth-b" || req.Title == nil || *req.Title != "stripe retry fix" {
		t.Fatalf(":rename request %+v", req)
	}
	h.WaitFor("renamed in the list and header", 3*sec, func() bool { return strings.Count(h.View(), "stripe retry fix") >= 3 })
	// The title in the pane header is clickable: it opens the same editor.
	x, y := headerTitleAt(t, h, "stripe retry fix")
	clickAt(h, x, y)
	if !h.Contains("Rename session") {
		t.Fatalf("clicking the header title did not open the editor:\n%s", h.View())
	}
	h.Key("esc")
}

// headerTitleAt finds s in the right-hand pane (the header), not the list.
func headerTitleAt(t *testing.T, h *tuitest.Harness, s string) (x, y int) {
	t.Helper()
	for y, l := range strings.Split(h.View(), "\n") {
		if i := strings.LastIndex(l, s); i >= 0 && ansi.StringWidth(l[:i]) > 30 {
			return ansi.StringWidth(l[:i]), y
		}
	}
	t.Fatalf("%q not in the pane header:\n%s", s, h.View())
	return 0, 0
}

func clickAt(h *tuitest.Harness, x, y int) {
	h.Send(tea.MouseClickMsg{X: x + 1, Y: y, Button: tea.MouseLeft})
	h.Send(tea.MouseReleaseMsg{X: x + 1, Y: y, Button: tea.MouseLeft})
	h.Settle()
}
