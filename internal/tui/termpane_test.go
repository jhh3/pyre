package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestTermPaneRendersCells(t *testing.T) {
	tv := newTermView("sess_synth", "run_synth", 20, 6, nil)
	defer tv.close()
	tv.resize(20, 6)
	tv.feed([]byte("hello\r\n\x1b[1;31mred\x1b[m world"))
	out := tv.render(20, 5, false, false)
	if len(out) != 5 {
		t.Fatalf("got %d rows, want 5", len(out))
	}
	// No status row: the bottom 5 of the 6 emulator rows are shown, so the
	// first row ("hello") scrolls out of the pane only if it is row 0.
	plain := ansi.Strip(strings.Join(tv.render(20, 6-statusRows, false, true), "\n"))
	if strings.Contains(plain, "hello") {
		t.Logf("pane shows the top rows: %q", plain)
	}
	full := tv.text()
	if !strings.Contains(full, "hello") || !strings.Contains(full, "red world") {
		t.Fatalf("emulator text = %q", full)
	}
	styled := strings.Join(out, "\n")
	if !strings.Contains(styled, "red") || !strings.Contains(styled, "\x1b[") {
		t.Fatalf("styled render lost colour: %q", styled)
	}
}

func TestTermPaneHidesTmuxStatusLine(t *testing.T) {
	tv := newTermView("sess_synth", "run_synth", 30, 5, nil)
	defer tv.close()
	// The pane is 4 rows; the emulator gets 5 (one for the status line).
	tv.render(30, 4, false, true)
	tv.feed([]byte("\x1b[2J\x1b[1;1Htop line\x1b[4;1Hprompt> \x1b[5;1H\x1b[42;30m[synth] 0:claude*" + strings.Repeat(" ", 13) + "\x1b[m\x1b[4;9H"))
	out := ansi.Strip(strings.Join(tv.render(30, 4, true, true), "\n"))
	if strings.Contains(out, "0:claude") {
		t.Fatalf("status line drawn:\n%s", out)
	}
	if !strings.Contains(out, "top line") || !strings.Contains(out, "prompt>") {
		t.Fatalf("content missing:\n%s", out)
	}
	// Without a status line the bottom rows win (the prompt stays in view).
	tv2 := newTermView("sess_synth2", "run_synth2", 30, 5, nil)
	defer tv2.close()
	tv2.render(30, 4, false, true)
	tv2.feed([]byte("\x1b[2J\x1b[1;1Hrow1\x1b[5;1Hlast row"))
	out2 := ansi.Strip(strings.Join(tv2.render(30, 4, false, true), "\n"))
	if !strings.Contains(out2, "last row") || strings.Contains(out2, "row1") {
		t.Fatalf("without status line want bottom rows:\n%s", out2)
	}
}

func TestTermPaneCursorAndModes(t *testing.T) {
	tv := newTermView("sess_synth", "run_synth", 10, 3, nil)
	defer tv.close()
	tv.render(10, 2, true, false)
	tv.feed([]byte("\x1b[?1h\x1b[?2004h\x1b[3;1Hab"))
	ac, bp := tv.keyModes()
	if !ac || !bp {
		t.Fatalf("modes: appCursor=%v bracketed=%v", ac, bp)
	}
	out := strings.Join(tv.render(10, 2, true, false), "\n")
	if !strings.Contains(out, "\x1b[7m") {
		t.Fatalf("focused render has no reverse-video cursor: %q", out)
	}
	if strings.Contains(strings.Join(tv.render(10, 2, false, false), "\n"), "\x1b[7m") {
		t.Fatal("unfocused render draws a cursor")
	}
	tv.feed([]byte("\x1b[?1l\x1b[?2004l"))
	if ac, bp := tv.keyModes(); ac || bp {
		t.Fatal("modes not reset")
	}
}

func TestTermPaneCopyModeIndicator(t *testing.T) {
	tv := newTermView("sess_synth", "run_synth", 40, 4, nil)
	defer tv.close()
	tv.resize(40, 4)
	if tv.inCopyMode() {
		t.Fatal("copy mode on an empty screen")
	}
	tv.feed([]byte("\x1b[1;30H\x1b[43m[12/340]\x1b[m"))
	if !tv.inCopyMode() {
		t.Fatalf("copy-mode indicator not seen: %q", tv.text())
	}
}

func TestEncodeKey(t *testing.T) {
	cases := []struct {
		k    tea.KeyPressMsg
		app  bool
		want string
	}{
		{tea.KeyPressMsg{Code: 'a', Text: "a"}, false, "a"},
		{tea.KeyPressMsg{Code: 'a', Text: "A", Mod: tea.ModShift}, false, "A"},
		{tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}, false, "\x03"},
		{tea.KeyPressMsg{Code: 'd', Mod: tea.ModCtrl}, false, "\x04"},
		{tea.KeyPressMsg{Code: 'x', Mod: tea.ModAlt}, false, "\x1bx"},
		{tea.KeyPressMsg{Code: tea.KeyEnter}, false, "\r"},
		{tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModShift}, false, "\x1b\r"},
		{tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModAlt}, false, "\x1b\r"},
		{tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl}, false, "\n"},
		{tea.KeyPressMsg{Code: tea.KeyUp}, false, "\x1b[A"},
		{tea.KeyPressMsg{Code: tea.KeyUp}, true, "\x1bOA"},
		{tea.KeyPressMsg{Code: tea.KeyUp, Mod: tea.ModCtrl}, true, "\x1b[1;5A"},
		{tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}, false, "\x1b[Z"},
		{tea.KeyPressMsg{Code: tea.KeyBackspace}, false, "\x7f"},
		{tea.KeyPressMsg{Code: tea.KeyEscape}, false, "\x1b"},
		{tea.KeyPressMsg{Code: tea.KeyPgUp}, false, "\x1b[5~"},
		{tea.KeyPressMsg{Code: tea.KeyDelete}, false, "\x1b[3~"},
		{tea.KeyPressMsg{Code: tea.KeyF5}, false, "\x1b[15~"},
		{tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}, false, " "},
		{tea.KeyPressMsg{Code: '[', Mod: tea.ModCtrl}, false, "\x1b"},
		{tea.KeyPressMsg{Code: 'é', Text: "é"}, false, "é"},
	}
	for _, c := range cases {
		if got := string(encodeKey(c.k, c.app)); got != c.want {
			t.Errorf("encodeKey(%s, app=%v) = %q, want %q", c.k.String(), c.app, got, c.want)
		}
	}
}

func TestBracketPaste(t *testing.T) {
	if got := string(bracketPaste("hi", false)); got != "hi" {
		t.Fatalf("raw paste = %q", got)
	}
	got := string(bracketPaste("a\x1b[201~b", true))
	if got != "\x1b[200~ab\x1b[201~" {
		t.Fatalf("bracketed paste = %q", got)
	}
}
