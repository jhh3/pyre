package tui

import (
	"strings"
)

// The frame's bottom (W-browse, owner: "give me the bottom of the screen
// back"). In the Sessions sidenav the status summary, toasts, the filter
// input and the key hints live at the bottom of the left sidebar, so the
// session's terminal pane reaches the screen's last row. The Sessions
// table and the grid (no sidebar) keep one slim bottom line. Every other
// screen, narrow layouts and open panels keep the status line and the
// hint line.

const (
	footFull    = iota // status line + hint line, full width
	footSidebar        // both inside the sidenav's bottom; no full-width rows
	footSlim           // one line: status (when any) or hints, plus the summary
)

var footRows = map[int]int{footFull: 2, footSidebar: 0, footSlim: 1}

func (m *Model) chooseFoot(w, h int) int {
	if m.overlay != nil || h < 12 {
		return footFull
	}
	switch {
	case m.view == viewSessions && m.tableShown():
		return footSlim
	case m.view == viewSessions && w >= splitMin:
		return footSidebar
	case m.view == viewGrid:
		return footSlim
	}
	return footFull
}

// sidebarList is the sidenav with its footer at the bottom.
func (m *Model) sidebarList(w, h int) []string {
	foot := m.sidebarFoot(w)
	lh := max(3, h-len(foot))
	return append(fit(m.viewSessionList(w, lh), w, lh), foot...)
}

// sidebarFoot is the sidenav's fixed footer (W-archive2: nothing wraps
// mid-item). Top to bottom:
//
//	───────────────────────────────  a faint rule
//	Needs you  ! deploy prep  …      the needs-you / fleet summary (one line)
//	✓ archived "x" — :archived …     a toast or status (one line, truncated
//	                                 with …; a click expands a toast)
//	n new session  enter type  F …   the key hints as an aligned grid
//
// The grid has up to three columns and three rows sized to the sidebar
// width; lower-priority hints drop when they do not fit, and "? keys"
// always stays.
func (m *Model) sidebarFoot(w int) []string {
	inner := max(10, w-2)
	out := []string{" " + m.th.faint.Render(strings.Repeat("─", inner))}
	if s := m.needsYouStrip(w, 100); len(s) > 0 {
		out = append(out, trunc(s[len(s)-1], w))
	}
	out = append(out, m.sidebarStatus(inner)...)
	hs, global := m.footerHints()
	return append(out, m.hintGrid(hs, global, inner)...)
}

// toastExpandLines bounds an expanded toast.
const toastExpandLines = 4

// sidebarStatus is the footer's status: the filter input, a toast, the
// terminal focus note or an unfinished key sequence. One line each; a
// clicked toast shows its full text (wrapped, at most toastExpandLines).
func (m *Model) sidebarStatus(inner int) []string {
	dim := func(s string) string { return m.th.dim.Render(s) }
	var showcmd string
	if len(m.pending) > 0 {
		showcmd = m.th.key.Render(displaySeq(m.pendingDisplay())) + dim(" …")
	}
	line := func(text string, render func(string) string) []string {
		room := inner
		if showcmd != "" {
			room = max(10, inner-width(showcmd)-2)
		}
		l := " " + render(trunc(text, room))
		if showcmd != "" {
			l += strings.Repeat(" ", max(2, inner+1-width(l)-width(showcmd))) + showcmd
		}
		return []string{l}
	}
	switch {
	case m.filtering:
		m.filterIn.SetWidth(max(10, min(40, inner-4)))
		out := []string{" " + m.filterIn.View()}
		if l := m.activeList(); l != nil {
			out = append(out, " "+dim(trunc(plural(len(l.ids), "match", "matches")+" · enter keeps · esc clears", inner)))
		}
		return out
	case m.toast != nil:
		tn := m.toast.tone
		render := func(s string) string { return m.th.tone(tn, s) }
		if m.toast.open && width(m.toast.text) > inner {
			ls := wrap(m.toast.text, inner)
			if len(ls) > toastExpandLines {
				ls = ls[:toastExpandLines]
				ls[toastExpandLines-1] = trunc(ls[toastExpandLines-1]+" …", inner)
			}
			out := make([]string, len(ls))
			for i, l := range ls {
				out[i] = " " + m.mark(zoneDef{kind: zoneToast}, render(l))
			}
			return out
		}
		l := line(m.toast.text, render)
		l[0] = " " + m.mark(zoneDef{kind: zoneToast}, strings.TrimPrefix(l[0], " "))
		return l
	case m.ev.err != "":
		return line("events: "+m.ev.err, dim)
	case m.LazyTyping():
		return line("the next key takes the input lease — "+m.unfocusLabel()+" returns to the list", dim)
	case m.focusedTerm() != nil:
		return line("keys go to the session — "+m.unfocusLabel()+" returns to the list", dim)
	case m.focus == 1 && m.hasPanes():
		return line("detail pane focused — j/k scroll · h or esc returns", dim)
	case showcmd != "":
		return []string{" " + showcmd}
	}
	return nil
}

// hintGrid lays hints out as an aligned grid of "key label" cells: up to
// three columns of equal width and three rows. The view's hints keep their
// order (most important first); when they do not all fit the last ones
// drop, and the help hint ("? keys") always stays, in the last cell.
func (m *Model) hintGrid(hs, global []hint, w int) []string {
	const maxRows, gap = 3, 2
	var tail []hint
	for _, h := range global {
		if h.key == "?" {
			h.label = "keys"
		}
		tail = append(tail, h)
	}
	cellW := func(h hint) int { return width(h.key) + 1 + width(h.label) }
	for cols := 3; cols >= 1; cols-- {
		colW := (w - gap*(cols-1)) / cols
		var cells []hint
		for _, h := range append(append([]hint{}, hs...), tail...) {
			if cellW(h) <= colW {
				cells = append(cells, h)
			}
		}
		if len(cells) < len(hs)+len(tail) && cols > 1 {
			continue // a hint does not fit this many columns
		}
		if room := maxRows * cols; len(cells) > room {
			keep := cells[:room-len(tail)]
			cells = append(append([]hint{}, keep...), tail...)
		}
		natural := 0
		for _, h := range cells {
			natural = max(natural, cellW(h))
		}
		colW = min(colW, natural+4) // compact on wide sidebars
		var out []string
		for i := 0; i < len(cells); i += cols {
			var b strings.Builder
			b.WriteString(" ")
			for j := i; j < min(i+cols, len(cells)); j++ {
				seg := m.hintSeg(cells[j])
				b.WriteString(seg)
				if j < min(i+cols, len(cells))-1 {
					b.WriteString(strings.Repeat(" ", colW-width(seg)+gap))
				}
			}
			out = append(out, b.String())
		}
		return out
	}
	return nil
}

// slimFooter is the one bottom line of the table and the grid: the hints
// (or the filter input / a key-focus note) on the left; a toast, else the
// needs-you summary, on the right.
func (m *Model) slimFooter(w int) string {
	right := m.compactSummary()
	if m.toast != nil {
		right = m.th.tone(m.toast.tone, trunc(m.toast.text, max(20, w/2)))
	}
	room := w - width(right) - 3
	var left string
	if m.filtering || len(m.pending) > 0 || m.focusedTerm() != nil || m.LazyTyping() {
		saved := m.toast
		m.toast = nil
		left = trunc(m.viewStatus(room), room)
		m.toast = saved
	} else {
		hs, global := m.footerHints()
		left = m.joinHints(hs, global, room)
	}
	if right == "" {
		return left
	}
	return left + strings.Repeat(" ", max(2, w-width(left)-width(right)-1)) + right
}

// compactSummary: "! 3 need you · ✉ 2" or "✓ ● 9 working  ○ 5 idle".
func (m *Model) compactSummary() string {
	if !m.Loaded("sessions") {
		return ""
	}
	n := len(m.needsYouSessions())
	_, msgs := flagCounts(m.visibleSessions())
	s := m.th.tone(toneGood, "✓") + " " + m.fleetSummary()
	if n > 0 {
		s = m.th.tone(toneWarn, "! "+plural(n, "needs you", "need you"))
	}
	if msgs > 0 {
		s += m.th.dim.Render(" · ") + m.th.tone(toneAccent, "✉") + m.th.dim.Render(" "+itoa(msgs))
	}
	return s
}
