package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/usage"
)

// Sessions has three views, switched inside the Sessions tab and
// remembered in the state file:
//
//   - sidenav (default): the grouped list on the left, the selected
//     session's live terminal on the right;
//   - table: a full-width table, one row per session with more columns
//     (project, machine, harness, account with its 5h / weekly usage,
//     last activity); enter opens the session's terminal in the sidenav
//     layout and leaving the terminal (ctrl+]) returns to the table;
//   - grid: every live session as a live tile (grid.go; the Grid used to
//     be its own tab).
//
// v cycles sidenav → table → grid, G toggles the grid, :view sets one. The
// three share the grouping, pins, filter and selection.

type sessMode string

const (
	modeSidenav sessMode = "sidenav"
	modeTable   sessMode = "table"
	modeGrid    sessMode = "grid"
)

var sessModes = []sessMode{modeSidenav, modeTable, modeGrid}

// sessViewState is the Sessions view mode (Model.sessView).
type sessViewState struct {
	mode     sessMode // "" until chosen: sidenav
	lastList sessMode // the non-grid mode G returns to
	zoomed   string   // session opened from the table into the sidenav terminal
}

func parseSessMode(s string) (sessMode, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "sidenav", "list", "side", "nav", "split":
		return modeSidenav, true
	case "table", "wide", "full":
		return modeTable, true
	case "grid", "tiles":
		return modeGrid, true
	}
	return "", false
}

// sessionsMode is the current Sessions view.
func (m *Model) sessionsMode() sessMode {
	if m.sessView.mode == "" {
		return modeSidenav
	}
	return m.sessView.mode
}

// SessionsMode is the Sessions view ("sidenav", "table", "grid"; tests).
func (m *Model) SessionsMode() string { return string(m.sessionsMode()) }

// sessionsView is the viewID that shows Sessions in its current mode.
func (m *Model) sessionsView() viewID {
	if m.sessionsMode() == modeGrid {
		return viewGrid
	}
	return viewSessions
}

// tableShown reports whether the Sessions view draws the table (not while
// a session opened from it has the terminal).
func (m *Model) tableShown() bool {
	return m.view == viewSessions && m.sessionsMode() == modeTable && m.sessView.zoomed == ""
}

// setSessionsMode switches the Sessions view and remembers it.
func (m *Model) setSessionsMode(md sessMode) tea.Cmd {
	m.sessView.mode, m.sessView.zoomed = md, ""
	if md != modeGrid {
		m.sessView.lastList = md
	}
	m.focus = 0
	label := map[sessMode]string{modeSidenav: "Sessions: list and terminal", modeTable: "Sessions: table", modeGrid: "Sessions: grid of live tiles"}[md]
	return tea.Batch(m.goTop(m.sessionsView()), m.saveState(), m.setToast(label, toneMuted), m.schedulePreview())
}

// cycleSessionsMode moves to the next view (v).
func (m *Model) cycleSessionsMode() tea.Cmd {
	cur := m.sessionsMode()
	for i, md := range sessModes {
		if md == cur {
			return m.setSessionsMode(sessModes[(i+1)%len(sessModes)])
		}
	}
	return m.setSessionsMode(modeSidenav)
}

// toggleGrid is G: the grid, or back to the list view it came from.
func (m *Model) toggleGrid() tea.Cmd {
	if m.view == viewGrid {
		back := m.sessView.lastList
		if back == "" {
			back = modeSidenav
		}
		return m.setSessionsMode(back)
	}
	return m.setSessionsMode(modeGrid)
}

// viewCommand is :view [sidenav|table|grid]; no argument cycles.
func (m *Model) viewCommand() command {
	return command{name: "view", args: "[mode]", desc: "Sessions view: sidenav (list + terminal), table or grid (no mode: next)",
		run: func(m *Model, args string) tea.Cmd {
			if strings.TrimSpace(args) == "" {
				return m.cycleSessionsMode()
			}
			md, ok := parseSessMode(args)
			if !ok {
				return m.setToast("view sidenav, table or grid", toneWarn)
			}
			return m.setSessionsMode(md)
		}}
}

// tableZoom opens the selected session's terminal from the table, typing.
func (m *Model) tableZoom() tea.Cmd {
	s, ok := m.selectedSession()
	if !ok {
		return nil
	}
	if emb, _ := m.embeddable(s); !emb {
		return m.focusTerm() // attaches full-screen instead
	}
	m.sessView.zoomed = s.Session.SessionID
	return tea.Batch(m.openTermView(), m.focusTerm())
}

// tableAfter returns to the table once a zoomed terminal lets go of the
// keyboard (or the view changed).
func (m *Model) tableAfter() {
	z := m.sessView.zoomed
	if z == "" {
		return
	}
	if m.view != viewSessions || m.sessionsMode() != modeTable || m.sessList.sel != z {
		m.sessView.zoomed = ""
		return
	}
	if m.focus == 0 && m.overlay == nil && !m.filtering {
		m.sessView.zoomed = ""
	}
}

// tableKey handles keys that mean something else in the table: enter and
// l / tab open the terminal (there is no pane beside the table).
func (m *Model) tableKey(id string) (tea.Cmd, bool) {
	if !m.tableShown() {
		return nil, false
	}
	if _, ok := m.selectedSession(); !ok {
		return nil, false
	}
	switch id {
	case "sessions.attach", "nav.pane_next":
		return m.tableZoom(), true
	}
	return nil, false
}

// ---- state ----

func (v *sessViewState) load(st tuiState) {
	if md, ok := parseSessMode(st.SessionsView); ok {
		v.mode = md
		if md != modeGrid {
			v.lastList = md
		}
	}
}

func (v *sessViewState) save(st *tuiState) {
	if v.mode != "" {
		st.SessionsView = string(v.mode)
	}
}

// ---- table ----

// tableCols are the table's columns for a width; 0 drops a column.
type tableCols struct {
	titleW, projW, machW, harnW, acctW int
}

// tableColumns fits the columns into w: the title first (up to its
// longest), then machine, project, account, harness; the column the list is
// grouped by is left out (its header names it).
func (m *Model) tableColumns(vs []contracts.SessionView, w int) tableCols {
	longest, proj, mach, acct := 16, 0, 0, 0
	for _, s := range vs {
		longest = max(longest, width(iconTitle(s)))
		proj = max(proj, width(m.projectName(s.Session.ProjectID)))
		mach = max(mach, width(s.NodeName))
		acct = max(acct, width(m.tableAccountPlain(s)))
	}
	mode := m.groupMode()
	c := tableCols{harnW: 7, projW: min(24, max(7, proj)), machW: min(14, max(7, mach)), acctW: min(34, max(7, acct))}
	if mode == groupProject || proj == 0 {
		c.projW = 0
	}
	if mode == groupMachine {
		c.machW = 0
	}
	fixed := 2 + 12 + 1 + 1 + 6 + 1 // cursor status _ title _ active _
	used := func() int {
		n := fixed
		for _, x := range []int{c.projW, c.machW, c.harnW, c.acctW} {
			if x > 0 {
				n += x + 1
			}
		}
		return n
	}
	// Narrow tables drop columns, least useful first, keeping 20 for titles.
	for _, drop := range []*int{&c.harnW, &c.projW, &c.acctW, &c.machW} {
		if w-used() >= 20 {
			break
		}
		*drop = 0
	}
	// The title column takes the rest of the width (owner: use the whole
	// screen); its spare room carries the session's doing-now phrase.
	_ = longest
	c.titleW = max(10, w-used())
	return c
}

// tableAccountPlain is the account cell without styles (for widths).
func (m *Model) tableAccountPlain(s contracts.SessionView) string {
	return ansiStrip(m.tableAccount(s))
}

// tableAccount is the account and its usage at a glance: "me 5h 12% wk
// 40%" (amber near the limit, red at it), "own login" when Pyre does not
// manage it, "—" for a shell.
func (m *Model) tableAccount(s contracts.SessionView) string {
	if s.Run == nil || s.Session.Harness == contracts.HarnessShell {
		return m.th.faint.Render("—")
	}
	if s.Run.AccountID == "" {
		return m.th.dim.Render("own login")
	}
	c, ok := m.accountCard(s)
	if !ok {
		return m.accountLabel(s.Run.AccountID)
	}
	out := c.Label()
	if b := m.usageBadge(c); b != "" {
		out += " " + b
	}
	return out
}

// usageBadge is an account's usage, compact: "5h 12% wk 40%", toned like
// the Accounts bars; "at limit" / "re-auth" when that is the news.
func (m *Model) usageBadge(c usage.Card) string {
	switch c.State {
	case usage.AtLimit:
		return m.th.tone(toneBad, "at limit")
	case usage.NeedsReauth:
		return m.th.tone(toneBad, "re-auth")
	}
	if c.Obs == nil {
		return ""
	}
	sp := usage.Split(c.Obs.Windows)
	var parts []string
	for _, x := range []struct {
		label string
		w     *contracts.UsageWindow
	}{{"5h", sp.FiveHour}, {"wk", sp.Weekly}} {
		if x.w == nil {
			continue
		}
		s := fmt.Sprintf("%s %.0f%%", x.label, x.w.UsedPercent)
		switch {
		case c.LastKnown:
			s = m.th.faint.Render(s)
		case x.w.UsedPercent >= 100:
			s = m.th.tone(toneBad, s)
		case x.w.UsedPercent >= usage.NearPercent:
			s = m.th.tone(toneWarn, s)
		default:
			s = m.th.dim.Render(s)
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " ")
}

// viewSessionTable is the table view: the grouped rows full width, no
// terminal pane.
func (m *Model) viewSessionTable(w, h int) []string {
	vs := m.visibleSessions()
	c := m.tableColumns(vs, w)
	hdr := "  " + pad("STATUS", 12) + " " + pad(trunc(m.sessionListTitle(len(vs)), c.titleW), c.titleW)
	cols := []struct {
		w     int
		label string
	}{{c.projW, "PROJECT"}, {c.machW, "MACHINE"}, {c.harnW, "HARNESS"}, {c.acctW, "ACCOUNT · USAGE"}}
	for _, x := range cols {
		if x.w > 0 {
			hdr += " " + pad(trunc(x.label, x.w), x.w)
		}
	}
	hdr += " " + padLeft("ACTIVE", 6)
	now := m.now()
	return m.groupedSessionRows(vs, w, h, hdr, func(s contracts.SessionView, sel bool) string {
		stt := listStatus(s, m.nodeOf(s))
		row := m.sessionCursor(sel, s) + m.statusCell(stt, 10) + " " + m.treeTitle(sel, s, c.titleW)
		if c.projW > 0 {
			p := m.projectName(s.Session.ProjectID)
			if p == "" {
				p = "—"
			}
			row += " " + m.th.dim.Render(pad(trunc(p, c.projW), c.projW))
		}
		if c.machW > 0 {
			row += " " + m.th.dim.Render(pad(trunc(orDash(s.NodeName), c.machW), c.machW))
		}
		if c.harnW > 0 {
			row += " " + m.th.dim.Render(pad(string(s.Session.Harness), c.harnW))
		}
		if c.acctW > 0 {
			row += " " + pad(trunc(m.tableAccount(s), c.acctW), c.acctW)
		}
		// A resources column (CPU / memory per session) slots in here once
		// the hub reports per-run stats.
		return row + " " + m.th.dim.Render(padLeft(age(now, m.lastActivity(s)), 6))
	})
}

// tableHints are the footer keys in the table.
func (m *Model) tableHints() []hint {
	h := []hint{{"n", "new session"}}
	if _, ok := m.selectedSession(); ok {
		h = append(h, hint{"enter", "open terminal"}, hint{"m", "message"}, hint{"i", "info"})
	}
	return append(h, hint{"v", "view"}, hint{"gb", "group by"}, hint{"/", "filter"}, hint{"␣␣", "find"})
}
