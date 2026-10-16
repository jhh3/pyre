package tui

import (
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/contracts"
)

// Grid (G, 5): mission control. Every live session on every machine is a
// small live terminal, tiled to fit the screen (up to 12 per page), each
// under a one-line caption: title · state · harness@machine. Sessions that
// need you (approval, question, open inbox items, failed) get an amber
// border that breathes slowly; working sessions show a small moving
// indicator while their terminal draws. Arrows / hjkl move, a click
// selects, enter or a double-click zooms into the Sessions view's live
// pane with the keyboard; ctrl+] there comes back. `/` filters (shared with
// Sessions), `.` shows ended sessions, tab pages when there are more.

// gridState is the Grid view's state (Update goroutine only).
type gridState struct {
	list     listState // the current page's tiles, in display order
	page     int
	pages    int
	total    int // sessions in the grid, all pages
	pageSize int

	tiles     map[string]*gridTile
	notify    chan struct{}
	waiting   bool
	lastFrame time.Time

	pulseOn  bool // needs-you borders bright (true) or soft
	pulsing  bool // a pulse tick is pending
	settling bool // a settle tick is pending (activity indicators stop)
	probing  bool // a window-size re-read tick is pending

	relayWaiting bool // a relay retry tick is pending (relay.go)

	zoomed string // session zoomed into the Sessions view ("" when none)

	seen   map[string]string    // last observed state per session (recency)
	active map[string]time.Time // last time a session's state changed or its tile drew
}

func (m *Model) gridInit() {
	g := &m.grid
	if g.tiles == nil {
		g.tiles = map[string]*gridTile{}
		g.notify = make(chan struct{}, 1)
		g.seen = map[string]string{}
		g.active = map[string]time.Time{}
	}
}

// ---- layout ----

// gridLayout places n tiles in a w x h body: cols x rows cells separated
// by a one-column gap; the last row's tiles share the full width.
type gridLayout struct {
	n, cols, rows, w, h int
}

const (
	gridGap     = 1
	gridMinW    = 28 // smallest tile (border included) worth drawing
	gridMinH    = 10
	gridAspect  = 3.0 // a terminal-shaped tile is about three cells wide per row
	gridEmptyPx = 0.5 // score penalty per empty cell
)

// layoutGrid picks the arrangement whose tiles are the largest
// terminal-shaped regions (min(width/3, height)), preferring no empty
// cells: 2 tiles side by side, 4 as 2x2, 6 as 3x2, 12 as 4x3 on a wide
// screen.
func layoutGrid(n, w, h int) gridLayout {
	if n <= 0 {
		return gridLayout{w: w, h: h}
	}
	best, bestScore := gridLayout{n: n, cols: 1, rows: n, w: w, h: h}, -1.0
	for c := 1; c <= n; c++ {
		r := (n + c - 1) / c
		if (r-1)*c >= n {
			continue // a whole empty row
		}
		tw := float64(w-(c-1)*gridGap) / float64(c)
		th := float64(h) / float64(r)
		s := min(tw/gridAspect, th) - gridEmptyPx*float64(c*r-n)
		if s > bestScore {
			best, bestScore = gridLayout{n: n, cols: c, rows: r, w: w, h: h}, s
		}
	}
	return best
}

// span splits total cells into k parts with gap between them; part i.
func span(total, k, gap, i int) (off, size int) {
	room := total - (k-1)*gap
	base, rem := room/k, room%k
	off = i*(base+gap) + min(i, rem)
	size = base
	if i < rem {
		size++
	}
	return off, size
}

// rowOf is the row and column of tile i, and how many tiles share its row.
func (l gridLayout) rowOf(i int) (row, col, inRow int) {
	row, col = i/l.cols, i%l.cols
	inRow = l.cols
	if row == l.rows-1 {
		inRow = l.n - (l.rows-1)*l.cols
	}
	return row, col, inRow
}

// rect is tile i's position and size in the body.
func (l gridLayout) rect(i int) (x, y, w, h int) {
	row, col, inRow := l.rowOf(i)
	x, w = span(l.w, inRow, gridGap, col)
	y, h = span(l.h, l.rows, 0, row)
	return x, y, w, h
}

// minTile is the smallest tile size in the layout.
func (l gridLayout) minTile() (w, h int) {
	w, h = l.w, l.h
	for i := 0; i < l.n; i++ {
		_, _, tw, th := l.rect(i)
		w, h = min(w, tw), min(h, th)
	}
	return w, h
}

// gridPageSize is how many tiles fit the body (1–12) at a legible size.
func gridPageSize(w, h int) int {
	for p := gridMaxTiles; p > 1; p-- {
		if tw, th := layoutGrid(p, w, h).minTile(); tw >= gridMinW && th >= gridMinH {
			return p
		}
	}
	return 1
}

// gridBody is the body size the frame gives the Grid (see render), minus
// the summary line.
func (m *Model) gridBody() (w, h int) {
	h = m.height - 3
	if m.height >= 16 {
		h--
	}
	if m.showTips && m.overlay == nil && m.height >= 12 {
		h--
	}
	return m.width, max(1, h-1)
}

// ---- membership ----

// gridNeedsYou: waiting on approval, or an actionable blocking flag
// (needsYou: a failed run or an ended session's failure shows red in its
// tile but needs nothing; a message for you is informational).
func gridNeedsYou(s contracts.SessionView, node *contracts.Node) bool {
	return needsYou(s, node)
}

// gridSessions are the sessions the Grid shows: not archived, live unless
// ended sessions are shown, matching the shared filter.
func (m *Model) gridSessions() []contracts.SessionView {
	var out []contracts.SessionView
	for _, s := range m.sessions() {
		if s.Session.Archived || (!m.showEnded && sessionEnded(s)) {
			continue
		}
		if m.filter != "" && !m.sessionMatches(s, m.filter) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// syncGrid ranks the sessions (needs you, then most recently active),
// splits them into pages and lists the current page in the Sessions list's
// order (Pinned, then the grouping: project, machine, …; then machine and
// title), so tiles do not shuffle while you watch.
func (m *Model) syncGrid() {
	m.gridInit()
	g := &m.grid
	ss := m.gridSessions()
	now := time.Now()
	for _, s := range ss {
		sid := s.Session.SessionID
		level, reason := sessionAttention(s)
		key := sessionStatus(s, m.nodeOf(s)).label + "|" + level + "|" + reason
		if prev, ok := g.seen[sid]; !ok {
			if s.Run != nil {
				g.active[sid] = s.Run.StartedAt
			}
		} else if prev != key {
			g.active[sid] = now
		}
		g.seen[sid] = key
	}
	sort.SliceStable(ss, func(i, j int) bool {
		a, b := ss[i], ss[j]
		if pa, pb := m.pinned(a), m.pinned(b); pa != pb {
			return pa // pinned tiles first (pinning.go)
		}
		na, nb := gridNeedsYou(a, m.nodeOf(a)), gridNeedsYou(b, m.nodeOf(b))
		if na != nb {
			return na
		}
		ta, tb := g.active[a.Session.SessionID], g.active[b.Session.SessionID]
		if !ta.Equal(tb) {
			return ta.After(tb)
		}
		return a.Session.SessionID < b.Session.SessionID
	})
	w, h := m.gridBody()
	g.pageSize = gridPageSize(w, h)
	g.total = len(ss)
	g.pages = max(1, (len(ss)+g.pageSize-1)/g.pageSize)
	g.page = max(0, min(g.page, g.pages-1))
	page := ss[min(len(ss), g.page*g.pageSize):min(len(ss), (g.page+1)*g.pageSize)]
	page = append([]contracts.SessionView(nil), page...)
	m.sortGrouped(page) // grouping.go
	ids := make([]string, len(page))
	for i, s := range page {
		ids[i] = s.Session.SessionID
	}
	g.list.setIDs(ids)
}

// ---- lifecycle ----

// gridAfter runs after every update: it returns from a zoom once the
// terminal lets go of the keyboard, closes every tile client when the Grid
// is not shown, and otherwise keeps the current page's tiles attached and
// the pulse ticking while something needs you.
func (m *Model) gridAfter() tea.Cmd {
	m.gridInit()
	g := &m.grid
	if g.zoomed != "" {
		if m.view != viewSessions {
			g.zoomed = ""
		} else if m.focus == 0 && m.overlay == nil && !m.filtering {
			g.zoomed = ""
			m.view, m.back = viewGrid, nil
		}
	}
	if m.view != viewGrid {
		if len(g.tiles) > 0 {
			m.closeGridTiles()
		}
		return nil
	}
	m.syncGrid()
	var cmds []tea.Cmd
	keep := map[string]bool{}
	w, h := m.gridBody()
	lay := layoutGrid(len(g.list.ids), w, h)
	now := time.Now()
	needs := false
	for i, sid := range g.list.ids {
		keep[sid] = true
		s, ok := m.session(sid)
		if !ok {
			continue
		}
		if gridNeedsYou(s, m.nodeOf(s)) {
			needs = true
		}
		runID := ""
		if s.Run != nil {
			runID = s.Run.RunID
		}
		t := g.tiles[sid]
		if t != nil && t.runID != runID {
			if t.tv != nil {
				t.tv.close()
			}
			delete(g.tiles, sid)
			t = nil
		}
		if emb, why := m.embeddable(s); !emb {
			if t != nil && t.tv != nil {
				t.tv.closeConn() // keep the last screen
			}
			if t == nil {
				t = &gridTile{sid: sid, runID: runID}
				g.tiles[sid] = t
			}
			t.state, t.note = termUnavailable, why
			continue
		}
		if t == nil {
			t = &gridTile{sid: sid, runID: runID}
			g.tiles[sid] = t
		}
		if t.tv != nil {
			if live, _ := t.tv.live(); live {
				continue
			}
			if t.state == termLive {
				t.state, t.retryAt = termEnded, now.Add(gridEndedWait)
			}
		}
		if t.state == termConnecting || now.Before(t.retryAt) {
			continue
		}
		_, _, tw, th := lay.rect(i)
		cmds = append(cmds, m.gridRequest(s, t, max(2, tw-2), max(2, th-2)))
	}
	for sid, t := range g.tiles {
		if !keep[sid] {
			if t.tv != nil {
				t.tv.close()
			}
			delete(g.tiles, sid)
		}
	}
	if !g.probing {
		for _, t := range g.tiles {
			if t.probe != nil {
				g.probing = true
				cmds = append(cmds, tea.Tick(gridProbeEvery, func(time.Time) tea.Msg { return gridProbeTickMsg{} }))
				break
			}
		}
	}
	if needs && !g.pulsing {
		g.pulsing = true
		cmds = append(cmds, tea.Tick(gridPulseHalf, func(time.Time) tea.Msg { return gridPulseMsg{} }))
	}
	cmds = append(cmds, m.gridWait())
	return tea.Batch(cmds...)
}

func (m *Model) onGridMsg(msg tea.Msg) tea.Cmd {
	m.gridInit()
	g := &m.grid
	switch msg := msg.(type) {
	case gridFrameMsg:
		g.waiting = false
		g.lastFrame = time.Now()
		var cmd tea.Cmd
		if !g.settling && m.gridActive() {
			g.settling = true
			cmd = tea.Tick(gridActiveFor, func(time.Time) tea.Msg { return gridSettleMsg{} })
		}
		return tea.Batch(cmd, m.gridWait())
	case gridSettleMsg:
		g.settling = false
	case gridPulseMsg:
		g.pulsing = false
		g.pulseOn = !g.pulseOn
	case gridAttachMsg:
		return m.onGridAttach(msg)
	case gridRelayRetryMsg:
		g.relayWaiting = false // gridAfter re-requests the waiting tiles
	case gridProbeTickMsg:
		g.probing = false
		if m.view == viewGrid {
			return m.gridReprobe()
		}
	case gridSizeMsg:
		if t := g.tiles[msg.sid]; t != nil && t.tok == msg.tok && t.tv != nil {
			t.tv.resize(msg.cols, msg.rows) // the window's own size: never a resize of the window
		}
	}
	return nil
}

// gridActive reports whether a tile drew recently (activity indicators).
func (m *Model) gridActive() bool {
	now := time.Now().UnixNano()
	for _, t := range m.grid.tiles {
		if now-t.lastOut.Load() < int64(gridActiveFor) {
			m.grid.active[t.sid] = time.Now()
			return true
		}
	}
	return false
}

// ---- keys ----

// gridKey handles the Grid's fixed keys before the keymap: left/right and
// h/l move across tiles, tab / shift+tab page.
func (m *Model) gridKey(k tea.KeyPressMsg) (tea.Cmd, bool) {
	g := &m.grid
	switch k.String() {
	case "left", "h":
		g.list.move(-1)
	case "right", "l":
		g.list.move(1)
	case "tab":
		m.gridPage(1)
	case "shift+tab":
		m.gridPage(-1)
	default:
		return nil, false
	}
	return nil, true
}

func (m *Model) gridPage(d int) {
	g := &m.grid
	if g.pages <= 1 {
		return
	}
	g.page = (g.page + d + g.pages) % g.pages
	g.list.sel, g.list.idx = "", 0
	m.syncGrid()
}

// keyGrid is the view handler (keys the keymap resolved to canonical ones).
func (m *Model) keyGrid(k tea.KeyPressMsg) tea.Cmd {
	g := &m.grid
	w, h := m.gridBody()
	lay := layoutGrid(len(g.list.ids), w, h)
	switch k.String() {
	case "down":
		m.gridVertical(lay, 1)
	case "up":
		m.gridVertical(lay, -1)
	case "home":
		g.list.home()
	case "end":
		g.list.end()
	case "pgdown":
		m.gridPage(1)
	case "pgup":
		m.gridPage(-1)
	case "enter":
		return m.gridZoom()
	case ".":
		return m.toggleEnded()
	case "esc":
		if m.filter != "" {
			m.applyFilter("")
		}
	}
	return nil
}

// gridVertical moves to the tile above or below, keeping the column as
// closely as the row's (possibly stretched) tiles allow.
func (m *Model) gridVertical(lay gridLayout, d int) {
	g := &m.grid
	if len(g.list.ids) == 0 {
		return
	}
	x, _, w, _ := lay.rect(g.list.idx)
	mid := x + w/2
	row, _, _ := lay.rowOf(g.list.idx)
	row += d
	if row < 0 || row >= lay.rows {
		return
	}
	best := -1
	for i := row * lay.cols; i < min(lay.n, (row+1)*lay.cols); i++ {
		tx, _, tw, _ := lay.rect(i)
		if mid >= tx && mid < tx+tw+gridGap {
			best = i
			break
		}
		best = i
	}
	if best >= 0 {
		g.list.idx, g.list.sel = best, g.list.ids[best]
	}
}

// gridZoom opens the selected tile's session in the Sessions view with the
// live pane focused for typing; leaving the pane (ctrl+]) returns here.
func (m *Model) gridZoom() tea.Cmd {
	sid := m.grid.list.sel
	if sid == "" {
		return nil
	}
	m.closeGridTiles()
	m.view, m.back, m.focus = viewSessions, nil, 0
	m.syncSessions()
	if !m.sessList.selectID(sid) {
		m.view = viewGrid
		return m.setToast("that session is not in the Sessions list (filter or ended)", toneWarn)
	}
	m.grid.zoomed = sid
	return tea.Batch(tea.Batch(m.refreshView(false)...), m.focusTerm())
}

// GridZoomed is the session zoomed into from the Grid ("" when none; tests).
func (m *Model) GridZoomed() string { return m.grid.zoomed }

// GridSelected is the selected tile's session (tests).
func (m *Model) GridSelected() string { return m.grid.list.sel }

// GridTiles are the current page's session IDs in display order (tests).
func (m *Model) GridTiles() []string { return append([]string(nil), m.grid.list.ids...) }

// GridPage is the current page and the page count (tests).
func (m *Model) GridPage() (int, int) { return m.grid.page, m.grid.pages }

// ---- view ----

var gridSpinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func (m *Model) viewGrid(w, h int) []string {
	g := &m.grid
	if g.tiles == nil {
		m.syncGrid()
	}
	out := []string{m.gridSummary(w)}
	h--
	if len(g.list.ids) == 0 {
		msg := "No live sessions — n starts one"
		if m.filter != "" {
			msg = "No sessions match /" + m.filter + " — esc clears the filter"
		}
		out = append(out, "", "  "+m.th.dim.Render(msg))
		return out
	}
	lay := layoutGrid(len(g.list.ids), w, h)
	body := make([]string, h)
	for row := 0; row < lay.rows; row++ {
		var tiles [][]string
		var widths []int
		y0, rh := span(h, lay.rows, 0, row)
		for i := row * lay.cols; i < min(lay.n, (row+1)*lay.cols); i++ {
			_, _, tw, th := lay.rect(i)
			tiles = append(tiles, m.gridTile(i, tw, th))
			widths = append(widths, tw)
		}
		for y := 0; y < rh; y++ {
			var b strings.Builder
			for j, t := range tiles {
				if j > 0 {
					b.WriteString(strings.Repeat(" ", gridGap))
				}
				l := ""
				if y < len(t) {
					l = t[y]
				}
				b.WriteString(pad(l, widths[j]))
			}
			body[y0+y] = b.String()
		}
	}
	return append(out, body...)
}

// gridSummary is the faint line above the tiles: how many, which page.
func (m *Model) gridSummary(w int) string {
	g := &m.grid
	live := 0
	need := 0
	for _, s := range m.gridSessions() {
		if !sessionEnded(s) {
			live++
		}
		if gridNeedsYou(s, m.nodeOf(s)) {
			need++
		}
	}
	parts := []string{plural(live, "live session", "live sessions")}
	if ended := g.total - live; ended > 0 {
		parts = append(parts, plural(ended, "ended", "ended"))
	}
	if g.pages > 1 {
		rest := g.total - len(g.list.ids)
		parts = append(parts, "page "+itoa(g.page+1)+" of "+itoa(g.pages), "+"+itoa(rest)+" more")
	}
	left := " " + m.th.dim.Render(strings.Join(parts, " · "))
	if g.pages > 1 {
		left += m.th.faint.Render(" — ") + m.hintSeg(hint{"tab", "next page"})
	}
	right := ""
	if need > 0 {
		right = m.th.tone(toneWarn, "! "+itoa(need)+" need you") + " "
	}
	if right == "" {
		return trunc(left, w)
	}
	return left + strings.Repeat(" ", max(1, w-width(left)-width(right))) + right
}

// gridTile draws tile i (w x h, border included).
func (m *Model) gridTile(i, w, h int) []string {
	g := &m.grid
	sid := g.list.ids[i]
	s, ok := m.session(sid)
	sel := i == g.list.idx
	if !ok || w < 4 || h < 2 {
		return nil
	}
	needs := gridNeedsYou(s, m.nodeOf(s))
	tl, tr, bl, br, hz, vt := "╭", "╮", "╰", "╯", "─", "│"
	if sel {
		tl, tr, bl, br, hz, vt = "┏", "┓", "┗", "┛", "━", "┃"
	}
	border := m.th.faint
	switch {
	case needs && !m.th.plain:
		border = m.th.tones[toneWarn]
		if !g.pulseOn {
			border = border.Faint(true)
		}
	case sel:
		border = m.th.accent
	}
	b := func(s string) string { return border.Render(s) }
	inner := w - 2
	caption := m.gridCaption(s, sel, inner-2)
	top := b(tl+hz) + caption + b(strings.Repeat(hz, max(0, inner-1-width(caption)))+tr)
	lines := []string{top}
	content := m.gridContent(s, inner, h-2)
	for y := 0; y < h-2; y++ {
		l := ""
		if y < len(content) {
			l = content[y]
		}
		lines = append(lines, b(vt)+pad(trunc(l, inner), inner)+"\x1b[m"+b(vt))
	}
	lines = append(lines, b(bl+strings.Repeat(hz, inner)+br))
	for y := range lines {
		lines[y] = m.mark(zoneDef{kind: zoneRow, list: &g.list, idx: i}, lines[y])
	}
	return lines
}

// gridCaption is " title · ● working · claude@synth-mac " fitted to w:
// harness@machine drops first, then the state word, then the title
// shortens.
func (m *Model) gridCaption(s contracts.SessionView, sel bool, w int) string {
	if w < 3 {
		return ""
	}
	st := listStatus(s, m.nodeOf(s))
	glyph := st.sym
	if st.label == "working" && m.tileActive(s.Session.SessionID) {
		glyph = gridSpinner[int(time.Now().UnixMilli()/120)%len(gridSpinner)]
	}
	state := m.th.tone(st.tone, glyph)
	word := st.label
	switch st.tone {
	case toneWarn, toneBad:
		word = m.th.tone(st.tone, word)
	default:
		word = m.th.dim.Render(word)
	}
	where := strings.ToLower(string(s.Session.Harness))
	if s.NodeName != "" {
		where += "@" + s.NodeName
	}
	sep := m.th.faint.Render(" · ")
	title := iconTitle(s)
	mark := ""
	if sel && m.th.plain {
		mark = "› "
	}
	style := func(t string) string {
		if sel {
			return m.th.title.Render(t)
		}
		return t
	}
	forms := [][]string{
		{style(title), state + " " + word, m.th.dim.Render(where)},
		{style(title), state + " " + word},
		{style(title), state},
	}
	if you := m.yoursCaption(s); you != "" { // "you 5m" goes before the machine
		forms = append([][]string{{style(title), state + " " + word, you, m.th.dim.Render(where)}, {style(title), state + " " + word, you}}, forms[1:]...)
	}
	for _, segs := range forms {
		c := " " + mark + strings.Join(segs, sep) + " "
		if width(c) <= w {
			return c
		}
	}
	room := w - 4 - width(mark) - width(glyph)
	if room < 1 {
		return trunc(" "+state+" ", w)
	}
	return " " + mark + style(trunc(title, room)) + " " + state + " "
}

func (m *Model) tileActive(sid string) bool {
	t := m.grid.tiles[sid]
	return t != nil && time.Now().UnixNano()-t.lastOut.Load() < int64(gridActiveFor)
}

// gridContent is the tile body: the live crop, the last screen with why it
// is not live, or a short note.
func (m *Model) gridContent(s contracts.SessionView, w, h int) []string {
	if h < 1 {
		return nil
	}
	t := m.grid.tiles[s.Session.SessionID]
	if t != nil && t.tv != nil {
		if _, _, drawn := t.tv.status(); drawn {
			lines := t.tv.cropRender(w, h, m.th.plain)
			if t.state != termLive && len(lines) > 0 {
				lines[len(lines)-1] = m.th.faint.Render(trunc(gridNote(t), w))
			}
			return lines
		}
	}
	note := "connecting…"
	switch {
	case sessionEnded(s):
		note = sessionStatus(s, m.nodeOf(s)).label
		if s.Run != nil && s.Run.EndedAt != nil {
			note += " " + ago(m.now(), *s.Run.EndedAt)
		}
	case t != nil && t.state != termConnecting && t.state != termIdle:
		note = gridNote(t)
	}
	lines := make([]string, h)
	mid := (h - 1) / 2
	for i, l := range wrap(note, max(4, w-2)) {
		if mid+i < h {
			lines[mid+i] = strings.Repeat(" ", max(0, (w-width(l))/2)) + m.th.dim.Render(l)
		}
	}
	return lines
}

func gridNote(t *gridTile) string {
	switch t.state {
	case termFailed:
		return "✗ " + t.note
	case termEnded:
		return "disconnected · reconnecting"
	case termUnavailable:
		return t.note
	}
	return "connecting…"
}

func (m *Model) gridHints() []hint {
	h := []hint{{"enter", "zoom"}, {"←↑↓→", "move"}}
	if m.grid.pages > 1 {
		h = append(h, hint{"tab", "more"})
	}
	h = append(h, hint{"v", "view"}, hint{"/", "filter"})
	if m.showEnded {
		h = append(h, hint{".", "hide ended"})
	} else {
		h = append(h, hint{".", "show ended"})
	}
	return append(h, hint{"␣␣", "find"})
}
