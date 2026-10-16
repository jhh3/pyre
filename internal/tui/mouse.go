package tui

import (
	"regexp"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// Mouse support (cell-motion mode). Clickable regions are marked while a
// frame renders: mark wraps a segment in two private, zero-width CSI
// sequences that the layout helpers (width, pad, trunc, Cut) treat like any
// other escape sequence. View locates them, records each region's cells and
// strips them before the frame reaches the terminal, so hit testing always
// matches what was drawn: header tabs, footer key hints, list rows and
// panes. Holding shift lets the terminal select text as usual.

type zoneKind int

const (
	zoneTab      zoneKind = iota // header tab: switch view
	zoneHint                     // a key hint: run that key
	zoneRow                      // list row: select / open
	zonePane                     // list or detail pane: focus
	zoneTipsHide                 // the first-launch tips bar's hide button
	zoneSection                  // a project home section tab
	zoneTerm                     // a row of the embedded terminal (idx: pane row)
	zoneGroup                    // a Sessions group header: fold / unfold (name: group key)
	zoneOverlay                  // a control inside a panel (name: passed to the overlay's click)
	zoneTree                     // a session tree glyph or "+N": fold / unfold (name: parent session id)
	zoneToast                    // the sidebar's toast: expand to the full text / dismiss (W-archive2)
	zoneLink                     // a link to the web app (name: the URL; weblink.go)
)

type zoneDef struct {
	kind zoneKind
	name string     // hint key, pane name ("list"/"detail")
	view viewID     // tab target
	list *listState // row: the list it belongs to
	idx  int        // row: index in list.ids
}

// zoneHit is where a marked region was drawn in the last frame.
type zoneHit struct {
	def    int
	y      int
	x0, x1 int // [x0, x1) cells
}

var zoneRE = regexp.MustCompile("\x1b\\[([56]);([0-9]+)z")

// mark registers a clickable region around s for the frame being rendered.
func (m *Model) mark(d zoneDef, s string) string {
	i := itoa(len(m.zdefs))
	m.zdefs = append(m.zdefs, d)
	return "\x1b[5;" + i + "z" + s + "\x1b[6;" + i + "z"
}

// markRow renders a list row (rowSel) and makes it clickable.
func (m *Model) markRow(l *listState, i int, sel bool, row string, w int) string {
	return m.mark(zoneDef{kind: zoneRow, list: l, idx: i}, m.rowSel(sel, row, w))
}

// markPane makes every (already fitted) line of a pane clickable.
func (m *Model) markPane(name string, lines []string) []string {
	for i, l := range lines {
		lines[i] = m.mark(zoneDef{kind: zonePane, name: name}, l)
	}
	return lines
}

// indexZones records the regions marked in frame and returns the frame
// without the markers.
func (m *Model) indexZones(frame string) string {
	m.zhits = m.zhits[:0]
	if !strings.Contains(frame, "\x1b[5;") {
		return frame
	}
	lines := strings.Split(frame, "\n")
	for y, line := range lines {
		locs := zoneRE.FindAllStringSubmatchIndex(line, -1)
		if len(locs) == 0 {
			continue
		}
		var b strings.Builder
		open := map[int]int{}
		col, prev := 0, 0
		for _, loc := range locs {
			seg := line[prev:loc[0]]
			b.WriteString(seg)
			col += ansi.StringWidth(seg)
			prev = loc[1]
			id := atoiSafe(line[loc[4]:loc[5]])
			if line[loc[2]] == '5' {
				open[id] = col
				continue
			}
			if x0, ok := open[id]; ok {
				if col > x0 {
					m.zhits = append(m.zhits, zoneHit{def: id, y: y, x0: x0, x1: col})
				}
				delete(open, id)
			}
		}
		rest := line[prev:]
		b.WriteString(rest)
		col += ansi.StringWidth(rest)
		for id, x0 := range open { // cut off by truncation: runs to the line end
			if col > x0 {
				m.zhits = append(m.zhits, zoneHit{def: id, y: y, x0: x0, x1: col})
			}
		}
		lines[y] = b.String()
	}
	return strings.Join(lines, "\n")
}

func atoiSafe(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
		if n > 1<<24 {
			return -1
		}
	}
	return n
}

// zoneAt returns the innermost region drawn at (x, y) in the last frame.
func (m *Model) zoneAt(x, y int) (zoneDef, bool) {
	best, bw := -1, 1<<30
	for _, h := range m.zhits {
		if h.y == y && x >= h.x0 && x < h.x1 && h.x1-h.x0 < bw && h.def >= 0 && h.def < len(m.frameDefs) {
			best, bw = h.def, h.x1-h.x0
		}
	}
	if best < 0 {
		return zoneDef{}, false
	}
	return m.frameDefs[best], true
}

// zoneKindAt is zoneAt restricted to one kind (a pane under a row).
func (m *Model) zoneKindAt(x, y int, kind zoneKind) (zoneDef, bool) {
	for _, h := range m.zhits {
		if h.y == y && x >= h.x0 && x < h.x1 && h.def >= 0 && h.def < len(m.frameDefs) && m.frameDefs[h.def].kind == kind {
			return m.frameDefs[h.def], true
		}
	}
	return zoneDef{}, false
}

func (m *Model) onMouseClick(ms tea.Mouse) tea.Cmd {
	if ms.Mod&tea.ModShift != 0 {
		return nil
	}
	m.frameSel, m.framePress = cellSel{}, nil
	if cmd, ok := m.termMouse(mousePress, ms); ok {
		return cmd
	}
	if ms.Button != tea.MouseLeft {
		return nil
	}
	m.framePress = &framePress{x: ms.X, y: ms.Y} // a drag from here selects (termselect.go)
	z, ok := m.zoneAt(ms.X, ms.Y)
	if !ok {
		return nil
	}
	if z.kind == zoneHint {
		return m.clickHint(z.name)
	}
	if z.kind == zoneToast && m.toast != nil {
		if m.toast.open {
			m.toast = nil // a second click dismisses it
		} else {
			m.toast.open = true
		}
		return nil
	}
	if z.kind == zoneOverlay {
		if c, ok := m.overlay.(interface{ click(*Model, string) tea.Cmd }); ok {
			return c.click(m, z.name)
		}
		return nil
	}
	if m.overlay != nil || m.filtering {
		return nil // the panel owns the screen; only its key hints click
	}
	if len(m.pending) > 0 {
		m.clearPending()
	}
	switch z.kind {
	case zoneGroup:
		m.focus = 0
		return m.toggleGroup(z.name)
	case zoneTree:
		m.focus = 0
		return m.toggleTree(z.name) // tree.go
	case zoneTab:
		m.focus = 0
		if z.view == viewMachines || z.view == viewAccounts {
			return m.openManage(z.view)
		}
		if z.view == viewSessions {
			return m.goTop(m.sessionsView()) // in its current view (sessions_modes.go)
		}
		return m.goTop(z.view)
	case zoneLink:
		return m.openWebURL(z.name, "web inbox")
	case zoneTipsHide:
		return m.dismissTips()
	case zoneSection:
		var cmds []tea.Cmd
		for j := 0; j < 16 && m.view == viewProject && m.projTab != z.idx; j++ {
			before := m.projTab
			if z.idx > m.projTab {
				cmds = append(cmds, m.viewKey(keyFor("right")))
			} else {
				cmds = append(cmds, m.viewKey(keyFor("left")))
			}
			if m.projTab == before {
				break
			}
		}
		return tea.Batch(cmds...)
	case zonePane:
		if z.name == "detail" && m.view == viewSessions {
			if s, ok := m.selectedSession(); ok {
				if emb, _ := m.embeddable(s); emb {
					return m.focusTerm() // a click on the terminal types into it
				}
			}
		}
		if z.name == "detail" && m.hasPanes() {
			if m.focus != 1 {
				m.focus, m.detailOff = 1, 0
			}
		} else {
			m.focus = 0
		}
		return nil
	case zoneRow:
		if cmd, ok := m.digestClick(z); ok {
			return cmd
		}
		l := z.list
		if l == nil || z.idx < 0 || z.idx >= len(l.ids) {
			return nil
		}
		m.focus = 0
		if l.idx == z.idx && l.sel == l.ids[z.idx] {
			// A click on the selected row (or the second click of a
			// double-click) opens it, like enter.
			return m.viewKey(keyFor("enter"))
		}
		return m.selectRow(l, z.idx)
	}
	return nil
}

// selectRow moves the cursor of l to i through the ordinary navigation
// actions, so the view's usual side effects (previews, detail loads) run.
func (m *Model) selectRow(l *listState, i int) tea.Cmd {
	d, act := i-l.idx, "nav.down"
	if d < 0 {
		d, act = -d, "nav.up"
	}
	var cmds []tea.Cmd
	for j := 0; j < d && j < 500; j++ {
		before := l.idx
		cmds = append(cmds, m.runAction(act))
		if l.idx == before {
			break // the view does not move this list with nav keys
		}
	}
	if l.idx != i && i < len(l.ids) {
		l.idx, l.sel = i, l.ids[i]
		m.detailOff = 0
	}
	return tea.Batch(cmds...)
}

func (m *Model) onMouseWheel(ms tea.Mouse) tea.Cmd {
	var dir string
	switch ms.Button {
	case tea.MouseWheelUp:
		dir = "up"
	case tea.MouseWheelDown:
		dir = "down"
	default:
		return nil
	}
	if m.overlay != nil {
		if wo, ok := m.overlay.(interface{ wheel(*Model, string) tea.Cmd }); ok {
			return wo.wheel(m, dir)
		}
		return m.overlay.update(m, keyFor(dir))
	}
	if m.filtering {
		return nil
	}
	if m.view == viewReader {
		for i := 0; i < 3; i++ {
			m.keyReader(keyFor(dir))
		}
		return nil
	}
	if m.hasPanes() {
		p, ok := m.zoneKindAt(ms.X, ms.Y, zonePane)
		if !ok {
			return nil
		}
		if p.name == "detail" && m.view == viewSessions {
			if cmd, ok := m.termMouse(mouseWheel, ms); ok {
				return cmd // the wheel over the terminal scrolls the session
			}
			if s, ok := m.selectedSession(); ok {
				if emb, _ := m.embeddable(s); emb {
					return nil // watching: the wheel never takes the keyboard
				}
			}
		}
		if p.name == "detail" {
			if m.focus != 1 {
				m.focus, m.detailOff = 1, 0
			}
			if dir == "down" {
				m.detailOff += 3
			} else {
				m.detailOff = max(0, m.detailOff-3)
			}
			return nil
		}
		m.focus = 0
		return m.runAction("nav." + dir)
	}
	var cmds []tea.Cmd
	for i := 0; i < 3; i++ {
		cmds = append(cmds, m.runAction("nav."+dir))
	}
	return tea.Batch(cmds...)
}

// clickHint runs the key a clicked hint names.
func (m *Model) clickHint(key string) tea.Cmd {
	if m.overlay == nil && !m.filtering {
		switch key {
		case "␣␣":
			return m.runAction("global.finder")
		case "␣n":
			return m.runAction("global.new")
		case "?":
			return m.runAction("global.help")
		}
	}
	if m.focusedTerm() != nil {
		m.focus = 0 // a click outside the terminal leaves it
		if key == m.unfocusLabel() {
			return nil
		}
	}
	k, ok := hintKeyPress(key)
	if !ok {
		return nil
	}
	if m.overlay != nil {
		return m.overlay.update(m, k)
	}
	if m.filtering {
		return m.updateFilter(k)
	}
	m.clearPending()
	return m.onKey(k)
}

// hintKeyPress converts a hint's key label to a key press; labels that name
// several keys (↑↓, 1-9, J/K) are not clickable.
func hintKeyPress(key string) (tea.KeyPressMsg, bool) {
	switch key {
	case "enter", "esc":
		return keyFor(key), true
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}, true
	}
	if r := []rune(key); len(r) == 1 && r[0] > ' ' && r[0] < 0x7f {
		return keyFor(key), true
	}
	if r := []rune(key); len(r) == 2 && r[0] == '^' && r[1] >= 'a' && r[1] <= 'z' {
		return tea.KeyPressMsg{Code: r[1], Mod: tea.ModCtrl}, true // ^o: ctrl+o
	}
	return tea.KeyPressMsg{}, false
}
