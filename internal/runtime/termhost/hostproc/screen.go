package hostproc

import (
	"fmt"
	"io"
	"strings"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
)

// screen is a session's terminal state: everything the program drew,
// emulated by charmbracelet/x/vt (the emulator the TUI's embedded terminal
// uses), with scrollback. Captures and the repaint sent to an attaching
// client are rendered from it. Not safe for concurrent use: the session
// holds its lock around every call.
type screen struct {
	em    *vt.Emulator
	modes map[ansi.DECMode]bool
}

// HistoryLimit is the scrollback kept per session (tmux history-limit).
const HistoryLimit = 10000

// repaintModes are the DEC private modes an attaching client's terminal
// must mirror for keys, paste and mouse to reach the program the way it
// asked (cursor keys, bracketed paste, mouse tracking and encoding, focus).
var repaintModes = []ansi.DECMode{
	ansi.ModeCursorKeys, ansi.ModeBracketedPaste,
	ansi.ModeMouseNormal, ansi.ModeMouseButtonEvent, ansi.ModeMouseAnyEvent,
	ansi.ModeMouseExtSgr, ansi.ModeFocusEvent,
}

// newScreen returns an emulator of cols x rows. Its replies to the
// program's queries (device attributes, cursor position, colours) go to
// respond, which must not block (it runs on the reply reader goroutine).
func newScreen(cols, rows int, respond func([]byte)) *screen {
	s := &screen{em: vt.NewEmulator(cols, rows), modes: map[ansi.DECMode]bool{}}
	s.em.SetScrollbackSize(HistoryLimit)
	s.em.SetCallbacks(vt.Callbacks{
		EnableMode:       func(m ansi.Mode) { s.setMode(m, true) },
		DisableMode:      func(m ansi.Mode) { s.setMode(m, false) },
		CursorVisibility: func(v bool) { s.modes[ansi.ModeTextCursorEnable] = v },
	})
	s.modes[ansi.ModeTextCursorEnable] = true
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := s.em.Read(buf)
			if n > 0 && respond != nil {
				respond(append([]byte(nil), buf[:n]...))
			}
			if err != nil {
				return
			}
		}
	}()
	return s
}

func (s *screen) setMode(m ansi.Mode, on bool) {
	if d, ok := m.(ansi.DECMode); ok {
		s.modes[d] = on
	}
}

func (s *screen) write(b []byte)        { _, _ = s.em.Write(b) }
func (s *screen) resize(cols, rows int) { s.em.Resize(cols, rows) }

// close ends the reply reader by closing the emulator's reply pipe
// (Emulator.Close would write its closed flag unsynchronised with Read).
func (s *screen) close() {
	if c, ok := s.em.InputPipe().(io.Closer); ok {
		_ = c.Close()
	}
}
func (s *screen) mode(m ansi.DECMode) bool { return s.modes[m] }

// line returns visible row y as a line of cells.
func (s *screen) line(y int) uv.Line {
	w := s.em.Width()
	l := make(uv.Line, w)
	for x := 0; x < w; x++ {
		if c := s.em.CellAt(x, y); c != nil {
			l[x] = *c
		} else {
			l[x] = uv.EmptyCell
		}
	}
	return l
}

func renderLine(l uv.Line, escapes bool) string {
	if escapes {
		return l.Render()
	}
	return l.String()
}

// capture renders the screen like `tmux capture-pane -p`: one line per
// row, each ending in "\n", trailing spaces trimmed; History scrollback
// lines first; SGR sequences kept with escapes. Wrapped lines are not
// joined (the emulator does not record soft wraps).
func (s *screen) capture(history int, escapes bool) string {
	var b strings.Builder
	if history > 0 {
		if sb := s.em.Scrollback(); sb != nil {
			ls := sb.Lines()
			start := max(0, len(ls)-history)
			for _, l := range ls[start:] {
				b.WriteString(renderLine(l, escapes))
				b.WriteByte('\n')
			}
		}
	}
	for y := 0; y < s.em.Height(); y++ {
		b.WriteString(renderLine(s.line(y), escapes))
		b.WriteByte('\n')
	}
	return b.String()
}

// repaint draws the current screen on a freshly attached client's
// terminal and puts that terminal into the modes the program set, so the
// raw output that follows lands where the program expects it.
func (s *screen) repaint() []byte {
	var b strings.Builder
	if s.em.IsAltScreen() {
		b.WriteString("\x1b[?1049h")
	}
	b.WriteString("\x1b[0m\x1b[H\x1b[2J")
	for y := 0; y < s.em.Height(); y++ {
		fmt.Fprintf(&b, "\x1b[%d;1H", y+1)
		b.WriteString(renderLine(s.line(y), true))
	}
	for _, m := range repaintModes {
		if s.modes[m] {
			fmt.Fprintf(&b, "\x1b[?%dh", int(m))
		}
	}
	pos := s.em.CursorPosition()
	fmt.Fprintf(&b, "\x1b[%d;%dH", pos.Y+1, pos.X+1)
	if s.modes[ansi.ModeTextCursorEnable] {
		b.WriteString("\x1b[?25h")
	} else {
		b.WriteString("\x1b[?25l")
	}
	return []byte(b.String())
}

// ResetSequence leaves every mode a session may have put an attached
// terminal into (written by the attach client when it detaches).
const ResetSequence = "\x1b[0m\x1b[?1l\x1b[?2004l\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?1004l\x1b[?25h\x1b[?1049l"
