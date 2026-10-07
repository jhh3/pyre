package tui

import (
	"strconv"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// Mouse input for the embedded terminal. The tmux client in the pane
// enables xterm mouse tracking on its terminal (our emulator) when the
// server has mouse on; Pyre encodes Bubble Tea's mouse events the way a
// real terminal would for the modes enabled, at pane-relative cells, and
// tmux passes them to the application (Claude Code and Codex scroll their
// transcript with the wheel and take clicks) or scrolls its own history.

// mouseModes are the DEC private mouse modes the inner terminal enabled.
type mouseModes struct {
	x10    bool // ?9: presses only
	normal bool // ?1000: presses, releases, wheel
	button bool // ?1002: plus motion while a button is held
	any    bool // ?1003: plus all motion
	sgr    bool // ?1006: SGR encoding
}

func (mm *mouseModes) set(m ansi.Mode, on bool) {
	switch m {
	case ansi.ModeMouseX10:
		mm.x10 = on
	case ansi.ModeMouseNormal:
		mm.normal = on
	case ansi.ModeMouseButtonEvent:
		mm.button = on
	case ansi.ModeMouseAnyEvent:
		mm.any = on
	case ansi.ModeMouseExtSgr:
		mm.sgr = on
	}
}

// tracking reports whether any mouse reporting is on.
func (mm mouseModes) tracking() bool { return mm.x10 || mm.normal || mm.button || mm.any }

type mouseEvent int

const (
	mousePress mouseEvent = iota
	mouseRelease
	mouseMotion
	mouseWheel
)

// encodeMouse encodes one event at cell (x, y) (0-based) for the modes in
// mm, or returns nil when those modes do not report it. Without SGR the
// legacy X10 form (CSI M Cb Cx Cy) is used; its coordinates stop at 223.
func encodeMouse(mm mouseModes, ev mouseEvent, ms tea.Mouse, x, y int) []byte {
	if !mm.tracking() || x < 0 || y < 0 {
		return nil
	}
	x10Only := mm.x10 && !mm.normal && !mm.button && !mm.any
	switch ev {
	case mouseRelease:
		if x10Only {
			return nil
		}
	case mouseMotion:
		if !mm.any && !(mm.button && ms.Button != tea.MouseNone) {
			return nil
		}
	}
	shift, alt, ctrl := ms.Mod&tea.ModShift != 0, ms.Mod&tea.ModAlt != 0, ms.Mod&tea.ModCtrl != 0
	if x10Only {
		shift, alt, ctrl = false, false, false // X10 mode reports no modifiers
	}
	btn := ms.Button
	if ev == mouseRelease && !mm.sgr {
		btn = tea.MouseNone // legacy releases do not say which button
	}
	cb := ansi.EncodeMouseButton(btn, ev == mouseMotion, shift, alt, ctrl)
	if cb == 0xff {
		return nil
	}
	if mm.sgr {
		final := byte('M')
		if ev == mouseRelease {
			final = 'm'
		}
		b := []byte("\x1b[<")
		b = strconv.AppendInt(b, int64(cb), 10)
		b = append(b, ';')
		b = strconv.AppendInt(b, int64(x+1), 10)
		b = append(b, ';')
		b = strconv.AppendInt(b, int64(y+1), 10)
		return append(b, final)
	}
	if cb > 255-32 {
		return nil // button 8-11 has no legacy encoding
	}
	return []byte{0x1b, '[', 'M', cb + 32, byte(min(x, 222) + 33), byte(min(y, 222) + 33)}
}
