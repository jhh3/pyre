package tui

import (
	"errors"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/jhh3/pyre/internal/contracts"

	tea "charm.land/bubbletea/v2"
)

// overlay is a modal panel that takes key focus and replaces the body.
type overlay interface {
	update(m *Model, k tea.KeyPressMsg) tea.Cmd
	view(m *Model, w, h int) []string
}

// msgUpdater lets an overlay receive non-key messages (cursor blink).
type msgUpdater interface {
	updateMsg(m *Model, msg tea.Msg) tea.Cmd
}

type hint struct{ key, label string }

// Spacing scale: one space between columns, two between groups of
// related items, gutter of one space around the pane separator.
const (
	gap1 = " "
	gap2 = "  "
)

// View implements tea.Model: the full-screen frame in the alternate screen.
// Bubble Tea v2 downsamples colors to the terminal's profile, wraps frames
// in synchronized output, and enables bracketed paste and basic keyboard
// disambiguation (shift+enter, ctrl+j vs enter) where the terminal has them.
func (m *Model) View() tea.View {
	m.zdefs = m.zdefs[:0]
	frame := m.render()
	m.frameDefs = append(m.frameDefs[:0], m.zdefs...)
	frame = m.indexZones(frame)
	m.lastFrame = frame
	v := tea.NewView(m.highlightFrame(frame))
	v.AltScreen = true
	// Cell-motion mouse: clicks, drags and the wheel. A drag selects and
	// copies (termselect.go); holding shift (option in iTerm2) still lets
	// the terminal select text itself.
	v.MouseMode = tea.MouseModeCellMotion
	v.WindowTitle = "Pyre"
	return v
}

// render draws the frame: header, faint rule, body, status line, key hints.
func (m *Model) render() string {
	w, h := m.width, m.height
	if w < 20 || h < 6 {
		return "Pyre: terminal too small"
	}
	rule := h >= 16
	bodyH := h - 3
	if rule {
		bodyH--
	}
	tips := m.showTips && m.overlay == nil && h >= 12
	if tips {
		bodyH--
	}
	var body []string
	if m.overlay != nil {
		m.boxAt = nil
		body = m.overlay.view(m, w, bodyH)
		if g := m.boxAt; g != nil && !m.th.plain {
			// A boxed panel floats over the dimmed view it belongs to, so
			// the session stays in sight while you answer.
			body = composite(m.dimmed(fit(m.viewBody(w, bodyH), w, bodyH)), fit(body, w, bodyH), *g)
		}
	} else {
		body = m.viewBody(w, bodyH)
	}
	lines := make([]string, 0, h)
	lines = append(lines, pad(m.viewHeader(w), w))
	if rule {
		lines = append(lines, m.th.faint.Render(strings.Repeat("─", w)))
	}
	body = fit(body, w, bodyH)
	if m.whichKey && m.overlay == nil {
		if pop := m.whichKeyLines(w); len(pop) > 0 && len(pop) <= bodyH {
			copy(body[bodyH-len(pop):], pop)
		}
	}
	lines = append(lines, body...)
	if tips {
		lines = append(lines, pad(m.viewTips(w), w))
	}
	lines = append(lines, pad(m.viewStatus(w), w))
	lines = append(lines, pad(m.viewHints(w), w))
	return strings.Join(lines, "\n")
}

func (m *Model) viewBody(w, h int) []string {
	switch m.view {
	case viewSessions:
		return m.viewSessions(w, h)
	case viewProjects:
		return m.viewProjects(w, h)
	case viewDigest:
		return m.viewDigest(w, h)
	case viewProject:
		return m.viewProjectHome(w, h)
	case viewArtifact:
		return m.viewArtifact(w, h)
	case viewDoc:
		return m.viewDoc(w, h)
	case viewMessages:
		return m.viewMessages(w, h)
	case viewMachines:
		return m.viewMachines(w, h)
	case viewAccounts:
		return m.viewAccounts(w, h)
	case viewConnections:
		return m.viewConnections(w, h)
	case viewEnvs:
		return m.viewEnvs(w, h)
	case viewLaunch:
		return m.viewLaunch(w, h)
	case viewReader:
		return m.viewReader(w, h)
	case viewGrid:
		return m.viewGrid(w, h)
	case viewActivity:
		return m.viewActivity(w, h)
	}
	return nil
}

// ---- header ----

// plainTab renders a section tab of fixed width (label plus one cell
// either side) so the layout is identical in every color mode.
func (m *Model) plainTab(label string, on bool) string {
	if on {
		if m.th.plain {
			return "[" + label + "]"
		}
		return m.th.onSel(m.th.tabOn.Render(" " + label + " "))
	}
	return m.th.tabOff.Render(" " + label + " ")
}

// tab renders a clickable navigation tab: its key and label, padded by one
// cell either side when active (background, or [brackets] without colour,
// so the layout is identical in every color mode); count (sessions that need you) is amber text, never colour
// alone.
func (m *Model) tab(v viewID, key, label string, count int, on bool) string {
	cnt, cntPlain := "", ""
	if count > 0 {
		cntPlain = " " + itoa(count)
		cnt = " " + m.th.tone(toneWarn, itoa(count))
	}
	var s string
	switch {
	case on && m.th.plain:
		s = "[" + key + " " + label + cntPlain + "]"
	case on:
		s = m.th.onSel(m.th.tabOn.Render(" "+key+" "+label) + cnt + m.th.tabOn.Render(" "))
	case label == "": // compact: the key alone
		s = m.th.key.Render(key) + cnt
	default:
		s = m.th.key.Render(key) + m.th.tabOff.Render(" "+label) + cnt
	}
	return m.mark(zoneDef{kind: zoneTab, view: v}, s)
}

// headerTabs are the header's tabs in order; 1–4 follow the same order.
// The grid and table are views inside Sessions (sessions_modes.go), and
// the Inbox is no longer a tab (attention lives on the sessions; I still
// opens it).
var headerTabs = []struct {
	v          viewID
	key, label string
}{{viewSessions, "S", "Sessions"}, {viewProjects, "P", "Projects"}, {viewMachines, "M", "Machines"}, {viewAccounts, "A", "Accounts"}}

// isTab reports whether the header tab v is the active one.
func (m *Model) isTab(v viewID) bool {
	return m.view == v || (v == viewProjects && (m.view == viewProject || m.view == viewArtifact || m.view == viewDoc)) ||
		(v == viewMachines && (m.view == viewConnections || m.view == viewEnvs)) ||
		(v == viewSessions && (m.view == viewMessages || m.view == viewGrid))
}

// viewToggle is the clickable Sessions view switch beside the Sessions
// tab: "v sidenav" / "v table" / "v grid" (a click or v cycles).
func (m *Model) viewToggle() string {
	return m.mark(zoneDef{kind: zoneHint, name: "v"}, m.th.key.Render("v")+m.th.faint.Render(" "+string(m.sessionsMode())))
}

func (m *Model) viewHeader(w int) string {
	sep := gap1 + m.th.faint.Render("·") + gap1
	brand := " " + m.th.brand.Render("Pyre")
	build := func(toggle, compact bool) string {
		var tabs []string
		for _, t := range headerTabs {
			on := m.isTab(t.v)
			label := t.label
			if compact && !on {
				label = ""
			}
			n := 0
			if t.v == viewSessions {
				n = m.headerNeedsYou()
			}
			s := m.tab(t.v, t.key, label, n, on)
			if t.v == viewSessions && toggle && on && (m.view == viewSessions || m.view == viewGrid) {
				s += gap1 + m.viewToggle()
			}
			tabs = append(tabs, s)
		}
		gap := gap2
		if w < 64 {
			gap = gap1
		}
		return brand + gap + strings.Join(tabs, sep)
	}
	// Narrow terminals drop the view toggle, then the inactive tabs'
	// labels, before the connection state loses its words.
	full, short := m.freshnessLabel()
	need := max(8, width(full)+3)
	left := build(true, false)
	if width(left) > w-need {
		left = build(false, false)
	}
	if width(left) > w-need {
		left = build(false, true)
	}
	if (!m.view.topLevel() || m.view == viewDigest) && m.view != viewAccounts && m.view != viewMachines { // tabs: no crumb; the digest is not a tab
		crumb := m.th.faint.Render(" › ") + m.th.bold.Render(m.view.name())
		if width(left)+width(crumb) < w-12 {
			left += crumb
		}
	}
	var filterTag string
	if m.filter != "" && !m.filtering {
		filterTag = m.th.accent.Render("/" + trunc(m.filter, 16))
	}
	room := w - width(left) - 2
	pool := m.poolSummary() // usage_view.go
	var right string
	for _, cand := range []string{joinNonEmpty(gap2+gap1, filterTag, pool, full), joinNonEmpty(gap2+gap1, pool, full), joinNonEmpty(gap2, filterTag, full), full, joinNonEmpty(gap2, filterTag, short), short} {
		if width(cand) <= room {
			right = cand
			break
		}
	}
	if right == "" {
		return trunc(left, w)
	}
	return left + strings.Repeat(" ", w-width(left)-width(right)-1) + right + " "
}

func joinNonEmpty(sep string, parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, sep)
}

// unreachableLabel names what could not be reached: the local daemon (a
// client in PYRE_SOCKET mode) or the hub.
func unreachableLabel(err error) string {
	var ce *contracts.Error
	// "unreachable" is client.DetailUnreachable.
	if errors.As(err, &ce) && ce.Details["unreachable"] == "local_daemon" {
		return "local pyred unreachable"
	}
	return "hub unreachable"
}

// freshnessLabel summarizes connection and data freshness, in a full and a
// short form for narrow terminals.
func (m *Model) freshnessLabel() (full, short string) {
	st := m.loads["sessions"]
	now := m.now()
	switch {
	case st == nil || (!st.have && st.inflight):
		l := m.th.dim.Render("loading…")
		return l, l
	case !st.have && st.err != nil:
		return m.th.tone(toneBad, "✗ "+unreachableLabel(st.err)), m.th.tone(toneBad, "✗ offline")
	case st.err != nil:
		return m.th.tone(toneWarn, "! "+unreachableLabel(st.err)) + m.th.dim.Render(" · showing data from "+ago(now, st.at)),
			m.th.tone(toneWarn, "! "+age(now, st.at))
	}
	// Fixed width while fresh, so the header never reflows on ticks; the
	// age shows only once the data is older than staleAfter.
	dot, label := m.th.dim.Render("○"), "synced"
	if m.ev.connected {
		dot, label = m.th.tone(toneGood, "●"), "live"
	}
	if now.Sub(st.at) > staleAfter {
		return dot + " " + m.th.tone(toneWarn, label+" · stale "+age(now, st.at)), dot + " " + m.th.tone(toneWarn, age(now, st.at))
	}
	return dot + " " + m.th.dim.Render(label), dot + " " + m.th.dim.Render(label)
}

// staleAfter is how old the sessions data may get before the header says so.
const staleAfter = 30 * time.Second

func (m *Model) viewStatus(w int) string {
	if m.filtering {
		m.filterIn.SetWidth(max(10, min(40, w-50)))
		n := ""
		if l := m.activeList(); l != nil {
			n = m.th.dim.Render("  " + plural(len(l.ids), "match", "matches") + " · enter keeps · esc clears")
		}
		return " " + m.filterIn.View() + n
	}
	// showcmd: an unfinished key sequence, right-aligned like Vim
	var showcmd string
	if len(m.pending) > 0 {
		showcmd = m.th.key.Render(displaySeq(m.pendingDisplay())) + m.th.dim.Render(" …")
	}
	left := ""
	switch {
	case m.toast != nil:
		left = " " + m.th.tone(m.toast.tone, trunc(m.toast.text, w-2-width(showcmd)-2))
	case m.ev.err != "":
		left = " " + m.th.dim.Render(trunc("events: "+m.ev.err, w-2-width(showcmd)-2))
	case m.LazyTyping():
		left = " " + m.th.dim.Render("keys go to the session (the next key takes the input lease) — "+m.unfocusLabel()+" returns to the list")
	case m.focusedTerm() != nil:
		left = " " + m.th.dim.Render("keys go to the session (input lease held; Pyre's messages wait) — "+m.unfocusLabel()+" returns to the list")
	case m.focus == 1 && m.hasPanes() && m.overlay == nil:
		left = " " + m.th.dim.Render("detail pane focused — j/k scroll · h or esc returns to the list")
	}
	if showcmd == "" {
		return left
	}
	return left + strings.Repeat(" ", max(1, w-width(left)-width(showcmd)-1)) + showcmd
}

// ---- key hints ----

func (m *Model) hintSeg(h hint) string {
	return m.mark(zoneDef{kind: zoneHint, name: h.key}, m.th.key.Render(h.key)+" "+m.th.dim.Render(h.label))
}

// viewHints renders view-specific hints on the left and the global ones on
// the right, dropping globals before view hints when space is short.
func (m *Model) viewHints(w int) string {
	var hs, global []hint
	switch {
	case m.overlay != nil:
		if ho, ok := m.overlay.(interface{ hintsFor(*Model) []hint }); ok {
			hs = ho.hintsFor(m)
		} else if ho, ok := m.overlay.(interface{ hints() []hint }); ok {
			hs = ho.hints()
		} else {
			hs = []hint{{"esc", "close"}}
		}
	case m.filtering:
		hs = []hint{{"enter", "apply"}, {"esc", "clear"}, {"↑↓", "move"}}
	default:
		// A short footer: the view's most useful keys, then help. Everything
		// else is one ? (or ␣ for the which-key popup) away.
		hs = m.viewHintsFor()
		if len(hs) > 7 {
			hs = hs[:7]
		}
		global = []hint{{"?", "help"}}
		if !m.view.topLevel() {
			global = []hint{{"esc", "back"}, {"?", "help"}}
		}
		if m.focusedTerm() != nil {
			global = nil // every key but the escape chord goes to the session
		}
	}
	join := func(hs []hint, limit int) (string, int) {
		var b strings.Builder
		used := 0
		for i, h := range hs {
			seg := m.hintSeg(h)
			sw := width(seg)
			if i > 0 {
				sw += 3
			}
			if used+sw > limit {
				break
			}
			if i > 0 {
				b.WriteString("   ")
			}
			b.WriteString(seg)
			used += sw
		}
		return b.String(), used
	}
	avail := w - 2
	r, rw := join(global, avail)
	l, lw := join(hs, avail-rw-4)
	if len(global) > 0 && len(hs) > 0 && lw == 0 {
		// Too narrow for both: keep the view's hints plus help.
		r, rw = join([]hint{{"?", "help"}}, avail)
		l, lw = join(hs, avail-rw-3)
	}
	if rw == 0 {
		return " " + l
	}
	return " " + l + strings.Repeat(" ", max(1, w-2-lw-rw)) + r
}

func (m *Model) viewHintsFor() []hint {
	switch m.view {
	case viewSessions:
		return m.sessionHints()
	case viewProjects:
		return []hint{{"enter", "open"}, {"/", "filter"}, {"n", "new session"}, {"␣␣", "find"}}
	case viewDigest:
		return m.digestHints()
	case viewProject:
		return m.projectHints()
	case viewArtifact:
		return []hint{{"↑↓", "version"}, {"c", "comment"}, {"J/K", "scroll"}}
	case viewDoc:
		return []hint{{"↑↓", "scroll"}, {"v", "revisions"}, {"e", "edit"}}
	case viewMessages:
		return m.messageHints()
	case viewAccounts:
		return []hint{{"↑↓", "select"}, {"r", "refresh"}, {"a", "activity"}}
	case viewActivity:
		return []hint{{"↑↓", "select"}, {"f", "filter"}, {"r", "refresh"}}
	case viewMachines, viewConnections, viewEnvs:
		return []hint{{"↑↓", "select"}, {"r", "refresh"}}
	case viewLaunch:
		return m.launchHints()
	case viewReader:
		return m.readerHints()
	case viewGrid:
		return m.gridHints()
	}
	return nil
}

// ---- panels ----

// box draws a rounded panel of width w around lines, centered in a region
// of width rw and height rh, with the title set into the top border.
func (m *Model) box(title string, lines []string, w, rw, rh int) []string {
	if w > rw-2 {
		w = rw - 2
	}
	if w < 20 {
		w = rw
	}
	inner := w - 4
	maxLines := rh - 4
	if maxLines < 1 {
		maxLines = 1
	}
	if len(lines) > maxLines {
		lines = lines[:maxLines]
	}
	b := m.th.box.Render
	t := " " + m.th.brand.Render(trunc(title, inner-2)) + " "
	top := b("╭─") + t + b(strings.Repeat("─", max(0, w-width(t)-3))+"╮")
	side := b("│")
	blank := side + strings.Repeat(" ", w-2) + side
	out := []string{top, blank}
	for _, l := range lines {
		out = append(out, side+" "+pad(l, inner)+" "+side)
	}
	out = append(out, blank, b("╰"+strings.Repeat("─", w-2)+"╯"))
	if len(out) > rh {
		out = append(out[:rh-1], out[len(out)-1])
	}
	left := (rw - w) / 2
	var res []string
	topPad := (rh - len(out)) / 3
	m.boxAt = &boxGeom{top: max(0, topPad), left: left, w: w, h: len(out)}
	for i := 0; i < topPad; i++ {
		res = append(res, "")
	}
	for _, l := range out {
		res = append(res, strings.Repeat(" ", left)+l)
	}
	return res
}

// frame draws a full-width rounded frame with a small title, used for the
// terminal snapshot in previews.
func (m *Model) frame(title string, lines []string, w int) []string {
	if w < 8 {
		return lines
	}
	b := m.th.box.Render
	t := ""
	if title != "" {
		t = " " + m.th.dim.Render(title) + " "
	}
	out := []string{b("╭─") + t + b(strings.Repeat("─", max(0, w-width(t)-3))+"╮")}
	for _, l := range lines {
		out = append(out, b("│")+" "+pad(l, w-4)+" "+b("│"))
	}
	return append(out, b("╰"+strings.Repeat("─", w-2)+"╯"))
}

// sectionTitle is a bold section label (whitespace, not rules, separates
// sections).
func (m *Model) sectionTitle(s string, w int) string {
	return m.th.heading.Render(trunc(s, w))
}

// colHeader renders faint column-header text.
func (m *Model) colHeader(s string, w int) string {
	return m.th.faint.Render(trunc(s, w))
}

// kvl renders a key/value row with a dimmed label column.
func (m *Model) kvl(label, value string, lw int) string {
	if width(label) >= lw {
		lw = width(label) + 1 // a label never runs into its value
	}
	return m.th.label.Render(pad(label, lw)) + value
}

// narrowValue is the value width below which a label/value pair puts the
// value on its own line under the label.
const narrowValue = 24

// kvFit renders a label and a styled value in w cells: beside the label
// (truncated) when there is room, else under it, indented.
func (m *Model) kvFit(label, value string, lw, w int) []string {
	if w-lw >= narrowValue {
		return []string{m.kvl(label, trunc(value, w-lw), lw)}
	}
	return []string{m.th.label.Render(trunc(label, w)), "  " + trunc(value, max(1, w-2))}
}

// kvWrap renders a label and a plain-text value in w cells, wrapping the
// value: beside the label with a hanging indent when there is room, else
// under the label.
func (m *Model) kvWrap(label, value string, style func(...string) string, lw, w int) []string {
	if w-lw >= narrowValue {
		lines := wrap(value, w-lw)
		out := make([]string, len(lines))
		for i, l := range lines {
			if i == 0 {
				out[i] = m.kvl(label, style(l), lw)
				continue
			}
			out[i] = strings.Repeat(" ", max(lw, width(label)+1)) + style(l)
		}
		return out
	}
	out := []string{m.th.label.Render(trunc(label, w))}
	for _, l := range wrap(value, max(1, w-2)) {
		out = append(out, "  "+style(l))
	}
	return out
}

// splitPanes returns list and detail widths for side-by-side layout, or
// zero detail width when the terminal is narrow (stacked layout).
func splitPanes(w int) (lw, rw int) {
	if w < 100 {
		return w, 0
	}
	lw = w * 45 / 100
	if lw < 50 {
		lw = 50
	}
	return lw, w - lw - 3
}

// listDetail lays out a list and its detail either side by side (normal)
// or stacked (narrow).
func (m *Model) listDetail(w, h int, list func(w, h int) []string, detail func(w, h int) []string) []string {
	lw, _ := splitPanes(w)
	return m.listDetailW(w, h, lw, list, detail)
}

// listDetailW is listDetail with an explicit list width (ignored when the
// terminal is too narrow for two panes).
func (m *Model) listDetailW(w, h, lw int, list func(w, h int) []string, detail func(w, h int) []string) []string {
	if w >= 100 {
		rw := w - lw - 3
		return hjoin(m.markPane("list", fit(list(lw, h), lw, h)), lw, m.paneSep(), m.markPane("detail", fit(m.scrolled(detail, rw, h), rw, h)), rw)
	}
	lh := h * 45 / 100
	if lh < 4 {
		lh = 4
	}
	out := m.markPane("list", fit(list(w, lh), w, lh))
	rule := m.th.faint.Render(strings.Repeat("─", w))
	if m.focus == 1 {
		rule = m.th.accent.Render(strings.Repeat("━", w))
	}
	out = append(out, rule)
	out = append(out, m.markPane("detail", fit(m.scrolled(detail, w, h-lh-1), w, h-lh-1))...)
	return out
}

// paneSep is the vertical pane separator; it lights up in the accent when
// the detail pane has focus.
func (m *Model) paneSep() string {
	if m.focus == 1 && m.hasPanes() {
		return " " + m.th.accent.Render("┃") + " "
	}
	return " " + m.th.faint.Render("│") + " "
}

// scrolled renders a detail pane, applying the scroll offset when the
// detail pane has focus.
func (m *Model) scrolled(detail func(w, h int) []string, w, h int) []string {
	if m.focus != 1 || m.detailOff == 0 {
		return detail(w, h)
	}
	lines := detail(w, h+min(m.detailOff, 2000))
	m.detailOff = max(0, min(m.detailOff, len(lines)-h))
	return lines[m.detailOff:]
}

// ---- rows and selection ----

// cursor is the two-cell selection slot that starts every list row. The ›
// marker keeps selection visible without color (NO_COLOR, screenshots).
func (m *Model) cursor(sel bool) string {
	if sel {
		return m.th.marker.Render("›") + " "
	}
	return "  "
}

// rowTone colors s by tone. (Selection no longer inverts the row, so inner
// colors stay meaningful on the selected row too.)
func (m *Model) rowTone(selected bool, tn tone, s string) string {
	return m.th.tone(tn, s)
}

// primary renders a row's main text: bold when selected.
func (m *Model) primary(sel bool, s string) string {
	if sel {
		return m.th.selText.Render(s)
	}
	return s
}

// rowSel pads a selected row to w and lays the subtle selection background
// under it.
func (m *Model) rowSel(selected bool, row string, w int) string {
	if !selected {
		return row
	}
	return m.th.onSel(pad(row, w))
}

// onSel lays the selection background under s (re-applied after every
// reset emitted by inner styles).
func (t theme) onSel(s string) string {
	if t.selBg == "" {
		return s
	}
	// lipgloss v2 resets with ESC[m (v1 used ESC[0m); re-arm after either.
	s = strings.ReplaceAll(s, "\x1b[0m", "\x1b[m")
	return t.selBg + strings.ReplaceAll(s, "\x1b[m", "\x1b[m"+t.selBg) + "\x1b[m"
}

// dline is one display line of a grouped list: a group header (item < 0)
// or an item row.
type dline struct {
	group string
	item  int
}

// groupedWindow lays items out under group headers (a header whenever the
// group changes) and returns the h display lines that keep the cursor and,
// when possible, its group header visible. top persists the scroll offset.
func groupedWindow(groups []string, sel, h int, top *int) []dline {
	return groupedWindowOpt(groups, sel, h, top, false)
}

// groupedWindowOpt is groupedWindow; spaced puts a blank line (item -2)
// before every group header but the first.
func groupedWindowOpt(groups []string, sel, h int, top *int, spaced bool) []dline {
	return groupedWindowHdr(groups, nil, sel, h, top, spaced)
}

// groupedWindowHdr is groupedWindowOpt where isHdr(i) marks items that are
// themselves their group's (selectable) header: no separate header line is
// drawn before them.
func groupedWindowHdr(groups []string, isHdr func(int) bool, sel, h int, top *int, spaced bool) []dline {
	var all []dline
	selD := -1
	for i, g := range groups {
		if i == 0 || groups[i-1] != g {
			if spaced && i > 0 {
				all = append(all, dline{group: g, item: -2})
			}
			if (g != "" || i > 0) && (isHdr == nil || !isHdr(i)) {
				all = append(all, dline{group: g, item: -1})
			}
		}
		if i == sel {
			selD = len(all)
		}
		all = append(all, dline{group: g, item: i})
	}
	if h <= 0 {
		return nil
	}
	if selD >= 0 {
		if selD < *top {
			*top = selD
		}
		if selD > 0 && all[selD-1].item < 0 && *top == selD {
			*top = selD - 1
		}
		if selD >= *top+h {
			*top = selD - h + 1
		}
	}
	if mx := len(all) - h; *top > mx {
		*top = mx
	}
	if *top < 0 {
		*top = 0
	}
	end := min(len(all), *top+h)
	win := all[*top:end]
	for len(win) > 1 && win[len(win)-1].item < 0 {
		win = win[:len(win)-1] // never end on a group header with no rows under it
	}
	return win
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// wrapJoin joins styled segments with sep, wrapping to width w.
func wrapJoin(segs []string, sep string, w int) []string {
	var out []string
	cur := ""
	for _, s := range segs {
		switch {
		case cur == "":
			cur = s
		case width(cur)+width(sep)+width(s) <= w:
			cur += sep + s
		default:
			out = append(out, cur)
			cur = s
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// boxGeom is where the last box() call placed its panel in the body.
type boxGeom struct{ top, left, w, h int }

// dimmed renders lines faint and colourless (the backdrop of a panel).
func (m *Model) dimmed(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = m.th.faint.Render(ansiStrip(l))
	}
	return out
}

// composite lays the panel rows of over (inside g) onto base, keeping the
// base visible around the panel.
func composite(base, over []string, g boxGeom) []string {
	out := append([]string(nil), base...)
	for y := g.top; y < g.top+g.h && y < len(out) && y < len(over); y++ {
		bw := width(base[y])
		left := ansi.Cut(base[y], 0, g.left)
		mid := ansi.Cut(over[y], g.left, g.left+g.w)
		right := ""
		if g.left+g.w < bw {
			right = ansi.Cut(base[y], g.left+g.w, bw)
		}
		out[y] = left + "\x1b[m" + mid + "\x1b[m" + right
	}
	return out
}
