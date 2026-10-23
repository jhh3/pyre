package tui_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/mock"
	"github.com/jhh3/pyre/internal/tui"
	"github.com/jhh3/pyre/internal/tui/tuitest"
)

// startMock is start() over a backend the test may change first.
func startMock(t *testing.T, w, ht int, prep func(b *mock.Backend), mod func(*tui.Options)) (*tuitest.Harness, *tuitest.Recorder, *mock.Backend) {
	t.Helper()
	b, clk := tuitest.NewMock(t)
	if prep != nil {
		prep(b)
	}
	rec := tuitest.NewRecorder(b)
	opts := tuitest.Options()
	opts.Clock = clk
	if mod != nil {
		mod(&opts)
	}
	h := tuitest.New(t, rec, opts, w, ht)
	h.WaitFor("initial sessions", 5*sec, func() bool { return h.M.Loaded("sessions") && h.M.Loaded("nodes") && h.M.Loaded("attention") })
	// A short RefreshInterval never lets the UI idle, so cap the settle at
	// 1 s instead of Settle's 5 s.
	h.SettleFor(80*time.Millisecond, time.Second)
	return h, rec, b
}

// endMachine ends every live run on one machine.
func endMachine(t *testing.T, b *mock.Backend, node string) {
	t.Helper()
	page, err := b.ListSessions(ctx(), contracts.ListSessionsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range page.Items {
		if s.NodeName == node && s.Run != nil && !s.Run.State.Terminal() {
			if err := b.SetRunState(s.Run.RunID, contracts.RunExited, "synthetic exit"); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestEndedSessionsHiddenToggleRemembered(t *testing.T) {
	state := filepath.Join(t.TempDir(), "tui-state.json")
	mod := func(o *tui.Options) { o.StateFile = state }
	h, _, _ := startMock(t, 100, 30, nil, mod)
	if h.Contains("api work (limited)") || !h.Contains("1 ended — . show") {
		t.Fatalf("ended session shown by default, or no toggle hint:\n%s", h.View())
	}
	h.Key(".")
	h.Settle()
	if !h.Contains("api work (limited)") || !h.Contains("ended shown — . hide") {
		t.Fatalf(". did not show ended sessions:\n%s", h.View())
	}
	stateHas(t, h, state, `"show_ended": true`)
	// A new TUI starts with ended sessions shown.
	h2, _, _ := startMock(t, 100, 30, nil, mod)
	if !h2.Contains("api work (limited)") {
		t.Fatalf("remembered toggle not applied:\n%s", h2.View())
	}
	h2.Key(".")
	h2.Settle()
	if h2.Contains("api work (limited)") {
		t.Fatalf(". did not hide ended sessions:\n%s", h2.View())
	}
}

func TestEmptyMachineShowsNoLiveSessions(t *testing.T) {
	h, _, _ := startMock(t, 100, 30, func(b *mock.Backend) { endMachine(t, b, "synth-mac") }, nil)
	// Project grouping (the default): the machines footer keeps it in view.
	if !h.Contains("machines  synth-linux ●") || !h.Contains("· synth-mac ●") || strings.Contains(rowOf(h.View(), "machines  synth-linux"), "no live sessions") {
		t.Fatalf("machines footer lacks synth-mac:\n%s", h.View())
	}
	if h.Contains("coordinator") {
		t.Fatalf("ended synth-mac sessions listed:\n%s", h.View())
	}
	golden(t, "sessions-ended-hidden-100", h.View())
	groupBy(h, "machine")
	if !h.Contains("synth-mac ● · no live sessions") {
		t.Fatalf("no one-line placeholder for synth-mac:\n%s", h.View())
	}
}

// groupBy sets the Sessions list grouping with :group.
func groupBy(h *tuitest.Harness, mode string) {
	h.Key(":")
	h.Type("group " + mode)
	h.Key("enter")
	h.Settle()
}

func TestArchiveEndedSessionAndUnarchive(t *testing.T) {
	h, rec, _ := startMock(t, 100, 30, nil, nil)
	h.Key(".")
	selectSession(t, h, "ses_synth-fbx")
	h.Key("X")
	h.WaitFor("archive sent", 3*sec, func() bool { return len(rec.Calls("UpdateSession")) == 1 })
	req := rec.Calls("UpdateSession")[0].Request.(contracts.UpdateSessionRequest)
	if req.SessionID != "ses_synth-fbx" || req.Archived == nil || !*req.Archived || req.OperationID == "" {
		t.Fatalf("archive request %+v", req)
	}
	h.WaitFor("archived session leaves the list", 3*sec, func() bool { return !h.Contains("– exited") })
	h.Key(":")
	h.Type("archived")
	h.Key("enter")
	h.WaitFor("archived list", 3*sec, func() bool { return h.Contains("1 archived session") })
	golden(t, "archived-100", h.View())
	h.Key("u")
	h.WaitFor("unarchive sent", 3*sec, func() bool { return len(rec.Calls("UpdateSession")) == 2 })
	req = rec.Calls("UpdateSession")[1].Request.(contracts.UpdateSessionRequest)
	if req.SessionID != "ses_synth-fbx" || req.Archived == nil || *req.Archived {
		t.Fatalf("unarchive request %+v", req)
	}
	h.Key("esc")
	h.WaitFor("restored to the list", 3*sec, func() bool { return h.Contains("– exited") })
}

func TestArchiveLiveSessionStopsFirst(t *testing.T) {
	h, rec, b := startMock(t, 100, 30, nil, nil)
	selectSession(t, h, "ses_synth-b")
	h.Key("X")
	if !h.Contains("Stop and archive") {
		t.Fatalf("no stop+archive confirmation for a live session:\n%s", h.View())
	}
	h.Key("n")
	if len(rec.Calls("StopSession")) != 0 || len(rec.Calls("UpdateSession")) != 0 {
		t.Fatal("cancel still acted")
	}
	h.Key("X", "y")
	h.WaitFor("stop sent", 3*sec, func() bool { return len(rec.Calls("StopSession")) == 1 })
	h.Settle()
	if len(rec.Calls("UpdateSession")) != 0 {
		t.Fatal("archived before the stop was observed")
	}
	// The node reports the exit; the next sessions load archives it.
	if err := b.SetRunState(runOf(t, b, "ses_synth-b"), contracts.RunExited, "synthetic exit"); err != nil && !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatal(err)
	}
	h.Key("ctrl+r")
	h.WaitFor("archived after exit", 3*sec, func() bool { return len(rec.Calls("UpdateSession")) == 1 })
	if r := rec.Calls("UpdateSession")[0].Request.(contracts.UpdateSessionRequest); r.SessionID != "ses_synth-b" || !*r.Archived {
		t.Fatalf("archive request %+v", r)
	}
}

func TestArchiveAllEndedOnMachineRow(t *testing.T) {
	h, rec, _ := startMock(t, 100, 30, func(b *mock.Backend) { endMachine(t, b, "synth-mac") }, nil)
	groupBy(h, "machine") // machine rows belong to the machine grouping
	h.Key(".")
	h.Settle()
	selectSession(t, h, "@machine:synth-mac")
	if !h.Contains("5 ended") || !h.Contains("X archive all") {
		t.Fatalf("machine row lacks its ended count / hint:\n%s", h.View())
	}
	h.Key("X")
	if !h.Contains("Archive 5 ended sessions on synth-mac?") {
		t.Fatalf("no confirmation:\n%s", h.View())
	}
	h.Key("y")
	h.WaitFor("five archives", 3*sec, func() bool { return len(rec.Calls("UpdateSession")) == 5 })
	for _, c := range rec.Calls("UpdateSession") {
		r := c.Request.(contracts.UpdateSessionRequest)
		if !*r.Archived || r.SessionID == "ses_synth-fbx" {
			t.Fatalf("unexpected archive %+v", r)
		}
	}
	h.WaitFor("machine now empty", 3*sec, func() bool { return h.Contains("synth-mac ● · no live sessions") })
}

func TestDetailNarrowLabelsAndFailureReason(t *testing.T) {
	var run string
	h, _, _ := startMock(t, 100, 30, func(b *mock.Backend) {
		run = runOf(t, b, "ses_synth-b")
		if err := b.SetRunState(run, contracts.RunFailed, "launch failed: claude: synthetic executable not found"); err != nil {
			t.Fatal(err)
		}
	}, nil)
	h.Key(".")
	selectSession(t, h, "ses_synth-b")
	h.Settle()
	v := h.View()
	// The reason leads the terminal pane, right under its header line.
	lines := strings.Split(v, "\n")
	found := false
	for i, l := range lines {
		if strings.Contains(l, "│ worker") && i+1 < len(lines) && strings.Contains(lines[i+1], "Could not start: claude:") {
			found = true
		}
	}
	if !found {
		t.Fatalf("failure reason does not lead the detail:\n%s", v)
	}
	golden(t, "session-failed-100", v)
	h.Key("i") // the labelled facts live in the info overlay
	if v := h.View(); strings.Contains(v, "Unavailableatta") || !strings.Contains(v, "Unavailable") {
		t.Fatalf("label runs into its value:\n%s", v)
	}
}

// The stop+archive confirm is bound to the session it was opened for:
// refreshes never close or re-target it, X while it is open does nothing,
// a held key never confirms, and after y no second dialog appears for the
// same action (the archive continuation never asks again, and the
// worktree offer never pops up by itself).
func TestArchiveConfirmBoundToItsSession(t *testing.T) {
	h, rec, b := startMock(t, 100, 30, nil, func(o *tui.Options) { o.RefreshInterval = 20 * time.Millisecond })
	selectSession(t, h, "ses_synth-b")
	h.Key("X")
	if !h.Contains("Stop and archive") || !h.Contains("synth-b") {
		t.Fatalf("confirm does not name its session:\n%s", h.View())
	}
	before := h.View()
	// Refreshes run while it is open; another session changes meanwhile.
	if err := b.SetRunState(runOf(t, b, "ses_synth-a"), contracts.RunExited, "synthetic exit"); err != nil && !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatal(err)
	}
	h.SettleFor(150*time.Millisecond, time.Second)
	if !h.Contains("Stop and archive") || !h.Contains("synth-b") {
		t.Fatalf("a refresh closed or re-targeted the confirm:\nbefore:\n%s\nafter:\n%s", before, h.View())
	}
	h.Key("X") // ignored while open
	h.Send(tea.KeyPressMsg{Code: 'y', Text: "y", IsRepeat: true})
	if len(rec.Calls("StopSession")) != 0 || !h.M.OverlayOpen() {
		t.Fatal("a repeated key confirmed")
	}
	h.Key("y")
	h.WaitFor("stop sent", 3*sec, func() bool { return len(rec.Calls("StopSession")) == 1 })
	if r := rec.Calls("StopSession")[0].Request.(contracts.SessionCommandRequest); r.SessionID != "ses_synth-b" {
		t.Fatalf("stopped %s", r.SessionID)
	}
	h.SettleFor(150*time.Millisecond, time.Second)
	if h.M.OverlayOpen() {
		t.Fatalf("a dialog reappeared after confirming:\n%s", h.View())
	}
	// X again on the same session while its stop is pending: no new confirm.
	if h.M.SelectedSession() == "ses_synth-b" {
		h.Key("X")
		if h.M.OverlayOpen() {
			t.Fatalf("second confirm for the same stop+archive:\n%s", h.View())
		}
	}
	if err := b.SetRunState(runOf(t, b, "ses_synth-b"), contracts.RunExited, "synthetic exit"); err != nil && !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatal(err)
	}
	h.WaitFor("archived after exit", 3*sec, func() bool { return len(rec.Calls("UpdateSession")) == 1 })
	h.SettleFor(150*time.Millisecond, time.Second)
	if h.M.OverlayOpen() {
		t.Fatalf("a dialog opened after the archive:\n%s", h.View())
	}
	if len(rec.Calls("StopSession")) != 1 || rec.Calls("UpdateSession")[0].Request.(contracts.UpdateSessionRequest).SessionID != "ses_synth-b" {
		t.Fatal("wrong or repeated action")
	}
}
