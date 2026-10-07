package tui

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// Mouse selection and copy, like a native terminal. Pyre turns mouse
// reporting on (clicks, the wheel), which takes the outer terminal's own
// selection away, so Pyre selects itself: a left drag highlights cells and
// the release copies them to the system clipboard with OSC 52 (works
// locally, over ssh, and inside tmux with `set -g set-clipboard on`). A
// double-click selects a word. In the embedded terminal a plain click
// (press and release on one cell) still reaches the harness, so Claude
// Code's and Codex's clickable elements keep working; a drag never does,
// matching native Claude Code where dragging selects. The wheel is
// unchanged. Elsewhere (lists, detail panes, panels) the drag selects the
// drawn frame's text.

// doubleClickWithin is the longest gap between the clicks of a double-click.
const doubleClickWithin = 400 * time.Millisecond

// cellSel is a stream selection from anchor (ax, ay) to the cell under the
// pointer (bx, by), both inclusive.
type cellSel struct {
	on             bool
	ax, ay, bx, by int
}

// norm orders the ends so (x0, y0) comes first in reading order.
func (s cellSel) norm() (x0, y0, x1, y1 int) {
	if s.ay < s.by || (s.ay == s.by && s.ax <= s.bx) {
		return s.ax, s.ay, s.bx, s.by
	}
	return s.bx, s.by, s.ax, s.ay
}

func (s cellSel) contains(x, y int) bool {
	if !s.on {
		return false
	}
	x0, y0, x1, y1 := s.norm()
	if y < y0 || y > y1 || (y == y0 && x < x0) || (y == y1 && x > x1) {
		return false
	}
	return true
}

// span is the [from, to) column range of row y in the selection, for rows
// w cells wide.
func (s cellSel) span(y, w int) (from, to int, ok bool) {
	if !s.on {
		return 0, 0, false
	}
	x0, y0, x1, y1 := s.norm()
	if y < y0 || y > y1 {
		return 0, 0, false
	}
	from, to = 0, w
	if y == y0 {
		from = x0
	}
	if y == y1 {
		to = x1 + 1
	}
	from, to = max(0, from), min(w, to)
	return from, to, from < to
}

// ---- embedded terminal ----

// termPress is a left press over the embedded terminal, held back until
// we know whether it is a click (forwarded) or a drag (a selection).
type termPress struct {
	sid   string
	x, y  int // emulator cell
	ms    tea.Mouse
	moved bool // dragged off the pressed cell: a selection
	word  bool // second click of a double-click: a word was selected
}

type lastClick struct {
	sid  string
	x, y int
	at   time.Time
}

func (t *termView) setSel(s cellSel) {
	t.mu.Lock()
	t.sel = s
	t.mu.Unlock()
}

func (t *termView) clearSel() {
	t.mu.Lock()
	t.sel = cellSel{}
	t.mu.Unlock()
}

// selectedText returns the selected cells as text, trailing blanks of each
// row trimmed. Caller must not hold mu.
func (t *termView) selectedText() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.selTextLocked()
}

func (t *termView) selTextLocked() string {
	if !t.sel.on {
		return ""
	}
	_, y0, _, y1 := t.sel.norm()
	var rows []string
	for y := max(0, y0); y <= y1 && y < t.rows; y++ {
		from, to, ok := t.sel.span(y, t.cols)
		if !ok {
			rows = append(rows, "")
			continue
		}
		var b strings.Builder
		for x := from; x < to; x++ {
			c := t.em.CellAt(x, y)
			switch {
			case c == nil:
				b.WriteByte(' ')
			case c.Width == 0 && c.Content == "":
				// a wide character's continuation cell
			case c.Content == "":
				b.WriteByte(' ')
			default:
				b.WriteString(c.Content)
			}
		}
		rows = append(rows, strings.TrimRight(b.String(), " "))
	}
	return strings.Join(rows, "\n")
}

// selectWord selects the run of non-blank cells around (x, y); false on a
// blank cell.
func (t *termView) selectWord(x, y int) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	blank := func(x int) bool {
		c := t.em.CellAt(x, y)
		return c == nil || strings.TrimSpace(c.Content) == ""
	}
	if y < 0 || y >= t.rows || x < 0 || x >= t.cols || blank(x) {
		return false
	}
	x0, x1 := x, x
	for x0 > 0 && !blank(x0-1) {
		x0--
	}
	for x1 < t.cols-1 && !blank(x1+1) {
		x1++
	}
	t.sel = cellSel{on: true, ax: x0, ay: y, bx: x1, by: y}
	return true
}

// copySelection copies the selection's text, if any.
func (m *Model) copySelection(text string) tea.Cmd {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	n := len([]rune(text))
	return m.copyText(text, "copied "+plural(n, "character", "characters"))
}

// termSelectMouse handles the left button over the embedded terminal:
// press (held back), drag (select), release (copy, or forward the click).
// x, y are the emulator cell. forward replays a held-back click.
func (m *Model) termSelectMouse(ev mouseEvent, ms tea.Mouse, tv *termView, x, y int, forward func(press tea.Mouse, x, y int) tea.Cmd) tea.Cmd {
	switch ev {
	case mousePress:
		tv.clearSel()
		p := &termPress{sid: tv.sid, x: x, y: y, ms: ms}
		lc := m.lastTermClick
		if lc.sid == tv.sid && lc.x == x && lc.y == y && time.Since(lc.at) < doubleClickWithin {
			m.lastTermClick = lastClick{}
			if tv.selectWord(x, y) {
				p.word = true
				m.termPress = p
				return m.copySelection(tv.selectedText())
			}
		}
		m.termPress = p
		return nil
	case mouseMotion:
		p := m.termPress
		if p == nil || p.sid != tv.sid || p.word {
			return nil
		}
		if x != p.x || y != p.y {
			p.moved = true
		}
		if p.moved {
			tv.setSel(cellSel{on: true, ax: p.x, ay: p.y, bx: x, by: y})
		}
		return nil
	case mouseRelease:
		p := m.termPress
		m.termPress = nil
		if p == nil || p.sid != tv.sid || p.word {
			return nil
		}
		if p.moved {
			tv.setSel(cellSel{on: true, ax: p.x, ay: p.y, bx: x, by: y})
			return m.copySelection(tv.selectedText())
		}
		m.lastTermClick = lastClick{sid: tv.sid, x: p.x, y: p.y, at: time.Now()}
		return forward(p.ms, p.x, p.y)
	}
	return nil
}

// ---- the rest of the frame ----

// framePress is a left press outside the embedded terminal; a drag from it
// selects the drawn frame's text.
type framePress struct {
	x, y  int
	moved bool
}

// frameMouse tracks a left drag outside the terminal; ok reports that the
// event was part of one.
func (m *Model) frameMouse(ev mouseEvent, ms tea.Mouse) (tea.Cmd, bool) {
	p := m.framePress
	if p == nil {
		return nil, false
	}
	switch ev {
	case mouseMotion:
		if ms.X != p.x || ms.Y != p.y {
			p.moved = true
		}
		if p.moved {
			m.frameSel = cellSel{on: true, ax: p.x, ay: p.y, bx: ms.X, by: ms.Y}
		}
		return nil, true
	case mouseRelease:
		m.framePress = nil
		if !p.moved {
			return nil, true
		}
		m.frameSel = cellSel{on: true, ax: p.x, ay: p.y, bx: ms.X, by: ms.Y}
		return m.copySelection(m.frameSelText()), true
	}
	return nil, false
}

// frameSelText is the selected text of the last frame drawn.
func (m *Model) frameSelText() string {
	if !m.frameSel.on {
		return ""
	}
	lines := strings.Split(m.lastFrame, "\n")
	_, y0, _, y1 := m.frameSel.norm()
	var rows []string
	for y := max(0, y0); y <= y1 && y < len(lines); y++ {
		w := ansi.StringWidth(lines[y])
		from, to, ok := m.frameSel.span(y, max(w, m.width))
		if !ok {
			rows = append(rows, "")
			continue
		}
		rows = append(rows, strings.TrimRight(ansi.Strip(ansi.Cut(lines[y], from, to)), " "))
	}
	return strings.Join(rows, "\n")
}

// highlightFrame draws the frame selection in reverse video.
func (m *Model) highlightFrame(frame string) string {
	if !m.frameSel.on {
		return frame
	}
	lines := strings.Split(frame, "\n")
	for y, line := range lines {
		w := ansi.StringWidth(line)
		from, to, ok := m.frameSel.span(y, w)
		if !ok {
			continue
		}
		mid := ansi.TruncateLeft(ansi.Truncate(line, to, ""), from, "")
		mid = strings.ReplaceAll(mid, "\x1b[m", "\x1b[m\x1b[7m")
		mid = strings.ReplaceAll(mid, "\x1b[0m", "\x1b[0m\x1b[7m")
		lines[y] = ansi.Truncate(line, from, "") + "\x1b[7m" + mid + "\x1b[27m" + ansi.TruncateLeft(line, to, "")
	}
	return strings.Join(lines, "\n")
}
