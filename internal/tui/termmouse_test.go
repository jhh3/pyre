package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestEncodeMouse(t *testing.T) {
	sgr := mouseModes{normal: true, button: true, sgr: true}
	anyM := mouseModes{normal: true, any: true, sgr: true}
	x10 := mouseModes{normal: true, button: true}
	press := tea.Mouse{Button: tea.MouseLeft}
	for _, c := range []struct {
		name string
		mm   mouseModes
		ev   mouseEvent
		ms   tea.Mouse
		x, y int
		want string
	}{
		{"sgr press", sgr, mousePress, press, 4, 2, "\x1b[<0;5;3M"},
		{"sgr release keeps button", sgr, mouseRelease, press, 4, 2, "\x1b[<0;5;3m"},
		{"sgr right press", sgr, mousePress, tea.Mouse{Button: tea.MouseRight}, 0, 0, "\x1b[<2;1;1M"},
		{"sgr wheel up", sgr, mouseWheel, tea.Mouse{Button: tea.MouseWheelUp}, 9, 4, "\x1b[<64;10;5M"},
		{"sgr wheel down", sgr, mouseWheel, tea.Mouse{Button: tea.MouseWheelDown}, 9, 4, "\x1b[<65;10;5M"},
		{"sgr drag", sgr, mouseMotion, press, 6, 2, "\x1b[<32;7;3M"},
		{"sgr ctrl+shift click", sgr, mousePress, tea.Mouse{Button: tea.MouseLeft, Mod: tea.ModCtrl | tea.ModShift}, 0, 0, "\x1b[<20;1;1M"},
		{"sgr wide coordinates", sgr, mousePress, press, 299, 99, "\x1b[<0;300;100M"},
		{"button mode drops hover", sgr, mouseMotion, tea.Mouse{}, 1, 1, ""},
		{"any mode reports hover", anyM, mouseMotion, tea.Mouse{}, 1, 1, "\x1b[<35;2;2M"},
		{"x10 press", x10, mousePress, press, 4, 2, "\x1b[M\x20\x25\x23"},
		{"x10 release", x10, mouseRelease, press, 4, 2, "\x1b[M\x23\x25\x23"},
		{"x10 wheel", x10, mouseWheel, tea.Mouse{Button: tea.MouseWheelUp}, 0, 0, "\x1b[M\x60\x21\x21"},
		{"x10 clamps at 223", x10, mousePress, press, 500, 300, "\x1b[M\x20\xff\xff"},
		{"x10-only mode: presses, no modifiers", mouseModes{x10: true}, mousePress, tea.Mouse{Button: tea.MouseLeft, Mod: tea.ModCtrl}, 0, 0, "\x1b[M\x20\x21\x21"},
		{"x10-only mode: no release", mouseModes{x10: true}, mouseRelease, press, 0, 0, ""},
		{"no tracking", mouseModes{sgr: true}, mousePress, press, 0, 0, ""},
	} {
		if got := string(encodeMouse(c.mm, c.ev, c.ms, c.x, c.y)); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// TestMouseModesFromEmulator: the modes tmux enables on its client reach
// the encoder through the emulator's mode callbacks.
func TestMouseModesFromEmulator(t *testing.T) {
	tv := newTermView("s", "r", 20, 5, nil)
	defer tv.close()
	if mm, _ := tv.mouseState(); mm.tracking() {
		t.Fatalf("tracking before any DECSET: %+v", mm)
	}
	tv.feed([]byte("\x1b[?1000h\x1b[?1002h\x1b[?1006h"))
	if mm, _ := tv.mouseState(); !mm.normal || !mm.button || !mm.sgr || mm.any {
		t.Fatalf("modes %+v", mm)
	}
	tv.feed([]byte("\x1b[?1002l\x1b[?1006l\x1b[?1000l"))
	if mm, _ := tv.mouseState(); mm.tracking() || mm.sgr {
		t.Fatalf("modes not reset: %+v", mm)
	}
}
