package tui_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/tui"
	"github.com/jhh3/pyre/internal/tui/tuitest"
)

// startEmbed is start() over an API whose attach returns embeddable
// local_tmux descriptors; the harness runs them as fake terminals.
func startEmbed(t *testing.T, w, ht int, mod func(*tui.Options)) (*tuitest.Harness, *tuitest.Recorder) {
	t.Helper()
	var rec *tuitest.Recorder
	h, _ := start(t, w, ht, func(a contracts.API) contracts.API {
		rec = tuitest.NewRecorder(tuitest.NewEmbeddable(a))
		return rec
	}, mod)
	return h, rec
}

// termOf is the newest fake terminal started for session sid (nil if none);
// tuitest.Embeddable puts the session ID in the synthetic command.
func termOf(h *tuitest.Harness, sid string) *tuitest.FakeTerm {
	ts := h.Terms()
	for i := len(ts) - 1; i >= 0; i-- {
		if len(ts[i].D.Command) > 1 && ts[i].D.Command[1] == sid {
			return ts[i]
		}
	}
	return nil
}

// waitTerm waits for session sid's newest terminal to be live in the
// wanted mode and returns it.
func waitTerm(t *testing.T, h *tuitest.Harness, sid string, readOnly bool) *tuitest.FakeTerm {
	t.Helper()
	var f *tuitest.FakeTerm
	h.WaitFor("terminal of "+sid, 3*sec, func() bool {
		f = termOf(h, sid)
		return f != nil && f.D.ReadOnly == readOnly && !f.Closed()
	})
	return f
}

// The right pane is the live terminal: one header line, no Activity block,
// no frame, no tmux status line; watching is a read-only attach that never
// takes the input lease.
func TestEmbeddedTerminalWatchesReadOnly(t *testing.T) {
	h, rec := startEmbed(t, 160, 40, nil)
	selectSession(t, h, "ses_synth-a")
	h.WaitFor("read-only view", 3*sec, func() bool {
		return strings.Contains(h.M.TermText("ses_synth-a"), "Welcome to Claude Code")
	})
	h.Settle()
	v := h.View()
	for _, want := range []string{"coordinator · ● working · Claude on synth-mac", "Welcome to Claude Code (synthetic)", "? for shortcuts", "live · enter to type"} {
		if !strings.Contains(v, want) {
			t.Errorf("pane lacks %q:\n%s", want, v)
		}
	}
	for _, bad := range []string{"Activity", "╭─ terminal", "[synthetic] 0:claude", "Directory", "Project  "} {
		if strings.Contains(v, bad) {
			t.Errorf("pane shows %q:\n%s", bad, v)
		}
	}
	ro := waitTerm(t, h, "ses_synth-a", true)
	if !ro.D.ReadOnly || ro.D.Command[len(ro.D.Command)-1] != "-r" {
		t.Fatalf("watching must be a read-only attach: %+v", ro.D)
	}
	if n := len(rec.Calls("AcquireLease")); n != 0 {
		t.Fatalf("watching took the input lease (%d acquires)", n)
	}
	// The pty follows the pane: header row off, one hidden status row on.
	cols, rows := ro.Size()
	if cols < 80 || rows < 30 {
		t.Fatalf("pty %dx%d does not fill the pane", cols, rows)
	}
	h.Key("i")
	if !h.Contains("Directory") || !h.Contains("read-only (tmux attach -r") {
		t.Fatalf("info overlay lacks the details:\n%s", h.View())
	}
	h.Key("esc")
	if ro.Input() != "" {
		t.Fatalf("keys reached a watched terminal: %q", ro.Input())
	}
}

// Focus (enter) swaps in a writable attach holding the input lease; keys,
// ctrl+c and paste go to the session; the escape chord returns focus to the
// list, releases the lease and goes back to a read-only view.
func TestEmbeddedTerminalFocusTakesLeaseAndEscapes(t *testing.T) {
	h, rec := startEmbed(t, 160, 40, nil)
	selectSession(t, h, "ses_synth-a")
	ro := waitTerm(t, h, "ses_synth-a", true)

	h.Key("enter")
	inter := waitTerm(t, h, "ses_synth-a", false)
	h.WaitFor("interactive attach", 3*sec, func() bool { return h.M.TermFocused() && len(rec.Calls("AcquireLease")) == 1 })
	h.WaitFor("read-only client closed", 3*sec, ro.Closed)
	if got := writableAttaches(rec); len(got) != 1 || got[0].SessionID != "ses_synth-a" {
		t.Fatalf("writable attach requests = %+v", got)
	}
	if !h.Contains("back to the list") || !h.Contains("keys go to the session") {
		t.Fatalf("focused hints missing:\n%s", h.View())
	}
	h.Type("echo hi")
	h.Key("enter", "ctrl+c", "shift+enter", "q", "?")
	h.Paste("pasted text")
	want := "echo hi\r\x03\x1b\rq?pasted text"
	h.WaitFor("keys typed", 3*sec, func() bool { return inter.Input() == want })
	if h.M.ActiveView() != "Sessions" || h.M.OverlayOpen() {
		t.Fatalf("keys leaked to the UI: view=%s overlay=%v", h.M.ActiveView(), h.M.OverlayOpen())
	}
	// The wheel scrolls tmux history through copy-mode.
	x, y := at(t, h, "Welcome to Claude Code")
	h.Send(tea.MouseWheelMsg{X: x, Y: y, Button: tea.MouseWheelUp})
	h.WaitFor("copy-mode keys", 3*sec, func() bool { return strings.HasSuffix(inter.Input(), "\x02[\x1b[1;5A\x1b[1;5A\x1b[1;5A") })

	h.Key("ctrl+]")
	h.WaitFor("unfocused", 3*sec, func() bool {
		return !h.M.TermFocused() && len(rec.Calls("ReleaseLease")) == 1 && inter.Closed()
	})
	waitTerm(t, h, "ses_synth-a", true) // back to watching
	rel := rec.Calls("ReleaseLease")[0].Request.(contracts.LeaseReleaseRequest)
	if rel.LeaseID == "" {
		t.Fatal("released an empty lease id")
	}
	before := h.M.SelectedSession()
	typed := inter.Input()
	h.Key("j")
	if h.M.SelectedSession() == before {
		t.Fatal("after ctrl+] keys should drive the list again")
	}
	if inter.Input() != typed {
		t.Fatal("a key reached the session after unfocus")
	}
}

// Leaving the pane any other way (a click on the list) also gives up the
// lease and the writable client.
func TestEmbeddedTerminalClickOnListReleasesLease(t *testing.T) {
	h, rec := startEmbed(t, 160, 40, nil)
	selectSession(t, h, "ses_synth-a")
	waitTerm(t, h, "ses_synth-a", true)
	h.Key("enter")
	inter := waitTerm(t, h, "ses_synth-a", false)
	h.WaitFor("focus", 3*sec, func() bool { return h.M.TermFocused() && len(rec.Calls("AcquireLease")) == 1 })
	click(t, h, "STATUS")
	h.WaitFor("released", 3*sec, func() bool {
		return !h.M.TermFocused() && len(rec.Calls("ReleaseLease")) == 1 && inter.Closed()
	})
}

// Switching sessions switches the embedded view; recently viewed
// sessions stay warm (their read-only attach keeps running) up to
// tui.MaxWarmTerms, beyond which the least recently viewed are closed.
func TestEmbeddedTerminalKeepsWarmTermsBounded(t *testing.T) {
	h, _ := startEmbed(t, 160, 40, nil)
	selectSession(t, h, "ses_synth-a")
	seen := map[string]bool{}
	for i := 0; i < 12; i++ {
		h.Key("down")
		h.Settle()
		if n := h.M.LiveTerms(); n > tui.MaxWarmTerms {
			t.Fatalf("%d embedded terminals live after %d moves", n, i+1)
		}
		seen[h.M.SelectedSession()] = true
	}
	if started := len(h.Terms()); started < 3 {
		t.Fatalf("only %d terminals started while moving over %d sessions", started, len(seen))
	}
	closed := 0
	for _, f := range h.Terms() {
		if f.Closed() {
			closed++
		}
	}
	if closed < len(h.Terms())-tui.MaxWarmTerms {
		t.Fatalf("%d of %d terminals closed; want all but %d", closed, len(h.Terms()), tui.MaxWarmTerms)
	}
}

// The escape chord is configurable (term.unfocus in keys.toml).
func TestEmbeddedTerminalEscapeChordConfigurable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.toml")
	if err := os.WriteFile(path, []byte("[keys]\n\"term.unfocus\" = \"ctrl+g\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h, _ := startEmbed(t, 160, 40, func(o *tui.Options) { o.KeysFile = path })
	selectSession(t, h, "ses_synth-a")
	h.Key("enter")
	inter := waitTerm(t, h, "ses_synth-a", false)
	if !h.Contains("ctrl+g back to the list") {
		t.Fatalf("hint should name the configured chord:\n%s", h.View())
	}
	h.Key("ctrl+]")
	h.WaitFor("ctrl+] typed", 3*sec, func() bool { return inter.Input() == "\x1d" })
	if !h.M.TermFocused() {
		t.Fatal("ctrl+] must reach the session when the chord is ctrl+g")
	}
	h.Key("ctrl+g")
	if h.M.TermFocused() {
		t.Fatal("ctrl+g should return focus to the list")
	}
}

// A session without an embeddable terminal (the plain mock: snapshots
// only) shows its last screen; enter falls back to the full-screen attach.
func TestNonEmbeddableSessionFallsBack(t *testing.T) {
	h, _ := start(t, 160, 40, nil, nil)
	selectSession(t, h, "ses_synth-a")
	h.WaitFor("snapshot", 3*sec, func() bool { return h.Contains("[mock screen]") && h.Contains("read-only snapshot") })
	h.Key("F")
	h.WaitFor("full-screen attach", 3*sec, func() bool { return len(h.Attaches()) == 1 })
}
