package tui_test

import (
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/tui"
	"github.com/jhh3/pyre/internal/tui/tuitest"
)

func leaseRuns(rec *tuitest.Recorder) []string {
	var out []string
	for _, c := range rec.Calls("AcquireLease") {
		out = append(out, c.Request.(contracts.LeaseRequest).RunID)
	}
	return out
}

// Switching with alt+down while typing in the embedded terminal: the alt
// key never reaches the harness, the old session's writable attach and
// input lease are released at once, typing focus follows to the new
// session's terminal, and its lease is taken on the next key, which then
// reaches the new session.
func TestSwitchWhileTypingMovesTheLease(t *testing.T) {
	h, rec := startEmbed(t, 160, 40, nil)
	selectSession(t, h, "ses_synth-a")
	waitTerm(t, h, "ses_synth-a", true)
	h.Key("enter")
	oldTerm := waitTerm(t, h, "ses_synth-a", false)
	h.WaitFor("typing in a", 3*sec, func() bool { return h.M.TermFocused() && len(rec.Calls("AcquireLease")) == 1 })
	h.Type("hi")
	h.WaitFor("typed in a", 3*sec, func() bool { return oldTerm.Input() == "hi" })

	h.Key("alt+down")
	next := h.M.SelectedSession()
	if next == "ses_synth-a" || next == "" {
		t.Fatalf("alt+down did not switch (sel %q)", next)
	}
	if !h.M.TermFocused() || !h.M.LazyTyping() {
		t.Fatalf("typing focus did not follow to %s (focused %v lazy %v)", next, h.M.TermFocused(), h.M.LazyTyping())
	}
	h.WaitFor("old lease released, writable attach closed", 3*sec, func() bool {
		return len(rec.Calls("ReleaseLease")) == 1 && oldTerm.Closed()
	})
	if got := oldTerm.Input(); got != "hi" {
		t.Fatalf("alt+down reached the old harness: %q", got)
	}
	ro := waitTerm(t, h, next, true) // the new session shows at once, watching
	if n := len(rec.Calls("AcquireLease")); n != 1 {
		t.Fatalf("the new session's lease was taken before a key (%d acquires)", n)
	}
	if !h.Contains("next key takes the input lease") {
		t.Fatalf("status line does not say the next key takes the lease:\n%s", h.View())
	}

	h.Type("x")
	inter := waitTerm(t, h, next, false)
	h.WaitFor("lease of the new session, key delivered", 3*sec, func() bool {
		return len(rec.Calls("AcquireLease")) == 2 && inter.Input() == "x"
	})
	if runs := leaseRuns(rec); runs[0] == runs[1] {
		t.Fatalf("the second lease is for the same run: %v", runs)
	}
	if ro.Input() != "" {
		t.Fatalf("keys reached the read-only client: %q", ro.Input())
	}

	// alt+left goes back (tmux last-session); the lease follows again.
	h.Key("alt+left")
	if h.M.SelectedSession() != "ses_synth-a" || !h.M.LazyTyping() {
		t.Fatalf("alt+left: sel %s lazy %v", h.M.SelectedSession(), h.M.LazyTyping())
	}
	h.WaitFor("second release", 3*sec, func() bool { return len(rec.Calls("ReleaseLease")) == 2 && inter.Closed() })
	if inter.Input() != "x" {
		t.Fatalf("alt+left reached the harness: %q", inter.Input())
	}
	// ctrl+] leaves the lazily focused terminal without taking a lease.
	h.Key("ctrl+]")
	h.Settle()
	if h.M.TermFocused() || len(rec.Calls("AcquireLease")) != 2 {
		t.Fatalf("ctrl+] after a switch: focused %v acquires %d", h.M.TermFocused(), len(rec.Calls("AcquireLease")))
	}
}

// Flicking through several sessions while typing takes no lease at all
// until a key is typed.
func TestSwitchFlickTakesNoLeases(t *testing.T) {
	h, rec := startEmbed(t, 160, 40, nil)
	selectSession(t, h, "ses_synth-a")
	waitTerm(t, h, "ses_synth-a", true)
	h.Key("enter")
	waitTerm(t, h, "ses_synth-a", false)
	h.WaitFor("typing", 3*sec, func() bool { return h.M.TermFocused() && len(rec.Calls("AcquireLease")) == 1 })
	h.Key("alt+down", "alt+down", "alt+up", "alt+down")
	h.Settle()
	if n := len(rec.Calls("AcquireLease")); n != 1 {
		t.Fatalf("flicking took %d leases", n)
	}
	h.WaitFor("the first lease released", 3*sec, func() bool { return len(rec.Calls("ReleaseLease")) == 1 })
	if h.M.LiveTerms() > tui.MaxWarmTerms {
		t.Fatalf("%d live embedded attaches after flicking", h.M.LiveTerms())
	}
}

// alt+s opens the quick switcher from inside the embedded terminal (the
// keys never reach the harness); enter switches and typing focus follows;
// esc returns the keyboard to the terminal it came from.
func TestSwitcherFromTheTerminal(t *testing.T) {
	h, rec := startEmbed(t, 160, 40, nil)
	selectSession(t, h, "ses_synth-a")
	waitTerm(t, h, "ses_synth-a", true)
	h.Key("enter")
	inter := waitTerm(t, h, "ses_synth-a", false)
	h.WaitFor("typing", 3*sec, func() bool { return h.M.TermFocused() && len(rec.Calls("AcquireLease")) == 1 })

	h.Key("alt+s")
	if !h.M.OverlayOpen() || !h.Contains("Switch session") || !h.Contains("· current") {
		t.Fatalf("alt+s did not open the switcher:\n%s", h.View())
	}
	h.Key("esc")
	if h.M.OverlayOpen() || !h.M.TermFocused() || h.M.SelectedSession() != "ses_synth-a" {
		t.Fatalf("esc: overlay %v focused %v sel %s", h.M.OverlayOpen(), h.M.TermFocused(), h.M.SelectedSession())
	}

	h.Key("alt+s")
	h.Type("deploy")
	if !h.Contains("deploy prep") {
		t.Fatalf("switcher filter:\n%s", h.View())
	}
	h.Key("enter")
	if h.M.OverlayOpen() || h.M.SelectedSession() == "ses_synth-a" || !h.M.LazyTyping() {
		t.Fatalf("enter: overlay %v sel %s lazy %v", h.M.OverlayOpen(), h.M.SelectedSession(), h.M.LazyTyping())
	}
	if strings.Contains(inter.Input(), "deploy") || strings.Contains(inter.Input(), "s") {
		t.Fatalf("switcher keys reached the harness: %q", inter.Input())
	}
}

// <leader>s opens the switcher (S and 1 still go to Sessions).
func TestLeaderSOpensSwitcher(t *testing.T) {
	h, _ := start(t, 120, 34, nil, nil)
	h.Key("2", "space", "s")
	if !h.Contains("Switch session") {
		t.Fatalf("␣s did not open the switcher:\n%s", h.View())
	}
	h.Key("esc", "S")
	if h.M.ActiveView() != "Sessions" || h.M.OverlayOpen() {
		t.Fatalf("S: view %s overlay %v", h.M.ActiveView(), h.M.OverlayOpen())
	}
	h.Key("2", "alt+down")
	if h.M.ActiveView() != "Sessions" {
		t.Fatalf("alt+down from Projects stays in %s", h.M.ActiveView())
	}
}

// Golden screens of the quick switcher.
func TestSwitcherGolden(t *testing.T) {
	h, _ := start(t, 120, 34, nil, nil)
	h.Key("down", "down", "alt+s")
	h.Settle()
	golden(t, "switcher-120", h.View())
	h.Key("w", "o", "r")
	h.Settle()
	golden(t, "switcher-typed-120", h.View())
}
