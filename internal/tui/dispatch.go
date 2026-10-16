package tui

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// Key handling, in order: ctrl+c always quits; an open overlay (finder,
// cmdline, composer, pickers) owns the keyboard; the live filter owns it
// while typing; otherwise keys go through the keymap: sequences (leader,
// g g) resolve to actions in the active scopes (view, navigation, global),
// and keys bound to nothing reach the view (typing in the launch flow).

type seqTimeoutMsg struct{ tok int }
type whichKeyMsg struct{ tok int }

// scopes lists the keymap scopes for the active view, highest priority
// first. While a filter is active, n/N cycle matches before view keys.
func (m *Model) scopes() []string {
	var view string
	switch m.view {
	case viewSessions:
		view = "sessions"
	case viewProjects:
		view = "projects"
	case viewDigest:
		view = "digest"
	case viewProject:
		view = "project"
	case viewArtifact:
		view = "artifact"
	case viewDoc:
		view = "doc"
	case viewMessages:
		view = "messages"
	case viewMachines, viewAccounts, viewConnections, viewEnvs, viewActivity:
		view = "manage"
	case viewLaunch:
		view = "launch"
	case viewReader:
		view = "reader"
	}
	if m.filter != "" && m.view.topLevel() {
		return []string{"nav", view, "global"}
	}
	return []string{view, "nav", "global"}
}

func (m *Model) onKey(k tea.KeyPressMsg) tea.Cmd {
	key := k.String()
	if cmd, ok := m.switchKey(k); ok {
		return cmd // session switch chords work from anywhere, the terminal included (switch.go)
	}
	if tv := m.focusedTerm(); tv != nil {
		return m.termKey(tv, k) // the embedded terminal owns the keyboard, ctrl+c included
	}
	if key == "ctrl+c" {
		return tea.Quit
	}
	if m.overlay != nil {
		return m.overlay.update(m, k)
	}
	if m.filtering {
		return m.updateFilter(k)
	}
	if m.view == viewLaunch && m.launch != nil && m.launch.editing != "" {
		return m.keyLaunch(k) // inline text field owns the keyboard
	}
	if m.view == viewReader && m.reader.searching {
		return m.keyReaderSearch(k)
	}
	if key == "esc" && len(m.pending) > 0 {
		m.clearPending()
		return nil
	}
	if m.view == viewGrid && len(m.pending) == 0 {
		if id, _ := m.km.lookup([]string{"sessions"}, []string{key}); id == "sessions.view" {
			return m.cycleSessionsMode() // v works in every Sessions view
		}
		if cmd, ok := m.gridKey(k); ok {
			return cmd // tiles: ←→ h l move across, tab pages
		}
	}
	m.pending = append(m.pending, key)
	exact, more := m.km.lookup(m.scopes(), m.pending)
	if more {
		m.pendingTok++
		tok := m.pendingTok
		if m.pending[0] == m.km.Leader {
			// Leader sequences wait for the next key; the which-key popup
			// appears after a short delay.
			return tea.Tick(m.km.WhichKey, func(time.Time) tea.Msg { return whichKeyMsg{tok} })
		}
		return tea.Tick(m.km.SeqTimeout, func(time.Time) tea.Msg { return seqTimeoutMsg{tok} })
	}
	pend := m.pending
	m.clearPending()
	if exact != "" {
		return m.runAction(exact)
	}
	if len(pend) > 1 {
		// An abandoned sequence: the last key starts over on its own.
		return m.onKey(k)
	}
	return m.viewKey(k)
}

func (m *Model) clearPending() {
	m.pending = nil
	m.whichKey = false
	m.pendingTok++
}

// onSeqTimeout fires a pending exact binding that was waiting for a
// possible longer sequence, or drops the prefix.
func (m *Model) onSeqTimeout(msg seqTimeoutMsg) tea.Cmd {
	if msg.tok != m.pendingTok || len(m.pending) == 0 {
		return nil
	}
	exact, _ := m.km.lookup(m.scopes(), m.pending)
	m.clearPending()
	if exact != "" {
		return m.runAction(exact)
	}
	return nil
}

// viewKey hands a key to the active view's handler.
func (m *Model) viewKey(k tea.KeyPressMsg) tea.Cmd {
	switch m.view {
	case viewSessions:
		return m.keySessions(k)
	case viewProjects:
		return m.keyProjects(k)
	case viewDigest:
		return m.keyDigest(k)
	case viewProject:
		return m.keyProject(k)
	case viewArtifact:
		return m.keyArtifact(k)
	case viewDoc:
		return m.keyDoc(k)
	case viewMessages:
		return m.keyMessages(k)
	case viewMachines, viewAccounts, viewConnections, viewEnvs:
		return m.keyManage(k)
	case viewActivity:
		return m.keyActivity(k)
	case viewLaunch:
		return m.keyLaunch(k)
	case viewReader:
		return m.keyReader(k)
	case viewGrid:
		return m.keyGrid(k)
	}
	return nil
}

// canon is the built-in key an action used to be hard-wired to; the view
// handlers keep matching these, so any binding can be remapped.
var canon = map[string]string{
	"nav.down": "down", "nav.up": "up", "nav.top": "home", "nav.bottom": "end",
	"nav.half_down": "pgdown", "nav.half_up": "pgup",
	"sessions.attach": "enter", "sessions.message": "m", "sessions.history": "M", "sessions.interrupt": "!",
	"sessions.attach_full": "A", "sessions.info": "i",
	"sessions.stop": "x", "sessions.project": "p", "sessions.archive": "X", "sessions.toggle_ended": ".",
	"sessions.dismiss": "d", "digest.open": "enter", "digest.transcript": "r",
	"projects.open": "enter", "projects.archive": "X",
	"project.next_section": "right", "project.prev_section": "left", "project.open": "enter",
	"project.edit": "e", "project.comment": "c", "project.refresh": "r",
	"artifact.comment": "c", "artifact.scroll_down": "J", "artifact.scroll_up": "K",
	"doc.revisions": "v", "doc.edit": "e",
	"messages.new": "m", "messages.reconcile": "R", "messages.cancel": "c",
	"manage.refresh": "r",
}

func keyFor(name string) tea.KeyPressMsg {
	switch name {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "left":
		return tea.KeyPressMsg{Code: tea.KeyLeft}
	case "right":
		return tea.KeyPressMsg{Code: tea.KeyRight}
	case "home":
		return tea.KeyPressMsg{Code: tea.KeyHome}
	case "end":
		return tea.KeyPressMsg{Code: tea.KeyEnd}
	case "pgup":
		return tea.KeyPressMsg{Code: tea.KeyPgUp}
	case "pgdown":
		return tea.KeyPressMsg{Code: tea.KeyPgDown}
	}
	r := []rune(name)
	return tea.KeyPressMsg{Code: r[0], Text: name}
}

// hasPanes reports whether the view shows a list beside a detail pane.
func (m *Model) hasPanes() bool {
	switch m.view {
	case viewSessions, viewProjects, viewDigest, viewArtifact, viewMessages,
		viewMachines, viewAccounts, viewConnections, viewEnvs, viewActivity:
		return true
	case viewProject:
		return m.projTab != tabOverview && m.projTab != tabSessions
	case viewDoc:
		return m.docRevs
	}
	return false
}

func (m *Model) halfPage() int { return max(3, (m.height-6)/2) }

func (m *Model) runAction(id string) tea.Cmd {
	if cmd, ok := m.groupingAction(id); ok { // grouping.go, pinning.go
		return cmd
	}
	if cmd, ok := m.usageAction(id); ok { // stats.go
		return cmd
	}
	if cmd, ok := m.tableKey(id); ok { // sessions_modes.go
		return cmd
	}
	// Detail pane focus: movement scrolls the detail instead of the list.
	if m.focus == 1 && strings.HasPrefix(id, "nav.") && m.hasPanes() {
		switch id {
		case "nav.down":
			m.detailOff++
			return nil
		case "nav.up":
			m.detailOff = max(0, m.detailOff-1)
			return nil
		case "nav.half_down":
			m.detailOff += m.halfPage()
			return nil
		case "nav.half_up":
			m.detailOff = max(0, m.detailOff-m.halfPage())
			return nil
		case "nav.top":
			m.detailOff = 0
			return nil
		case "nav.bottom":
			m.detailOff = 1 << 20 // clamped when drawn
			return nil
		}
	}
	switch id {
	case "global.quit":
		switch {
		case m.view.topLevel():
			return tea.Quit
		case m.view == viewLaunch:
			return m.launchBack()
		}
		return m.goBack()
	case "global.back":
		if m.focus == 1 {
			m.focus = 0
			return nil
		}
		if m.view.topLevel() {
			if m.filter != "" {
				m.applyFilter("")
				return m.schedulePreview()
			}
			return m.dismissTips()
		}
		return m.viewKey(keyFor("esc"))
	case "global.help":
		m.overlay = &helpOverlay{}
		return nil
	case "global.cmdline":
		return m.openCmdline("")
	case "global.finder":
		return m.openFinder()
	case "global.switcher", "sessions.prev", "sessions.next", "sessions.last", "sessions.attention":
		return m.runSwitch(id) // switch.go
	case "global.filter":
		if !m.view.topLevel() && m.view != viewReader {
			m.view, m.back = viewSessions, nil
		}
		if m.view == viewReader {
			return m.startReaderSearch()
		}
		return m.startFilter()
	case "global.search":
		return m.openSearch("")
	case "global.search_transcript":
		s, ok := m.selectedSession()
		if !ok {
			return m.setToast("select a session first", toneMuted)
		}
		return tea.Batch(m.openReader(s), m.startReaderSearch())
	case "global.sessions":
		return m.goTop(m.sessionsView()) // in its current view (sessions_modes.go)
	case "sessions.view":
		return m.cycleSessionsMode()
	case "global.projects":
		return m.goTop(viewProjects)
	case "global.digest":
		return m.openDigest()
	case "global.inbox":
		return m.openWebInbox() // the inbox is in the web app (weblink.go)
	case "global.grid":
		return m.toggleGrid() // the grid is a Sessions view (sessions_modes.go)
	case "global.machines":
		return m.openManage(viewMachines)
	case "global.accounts":
		return m.openManage(viewAccounts)
	case "global.connections":
		return m.openManage(viewConnections)
	case "global.environments":
		return m.openManage(viewEnvs)
	case "global.activity":
		return m.openActivity("") // activity.go
	case "global.new", "sessions.new", "projects.new":
		return m.openQuickLaunch() // W-quicklaunch: tab inside opens the step-by-step launch
	case "global.refresh":
		return tea.Batch(m.refreshVisible(true), m.setToast("refreshing…", toneMuted))
	case "nav.pane_next", "nav.pane_prev":
		if m.view == viewSessions {
			if s, ok := m.selectedSession(); ok {
				if emb, _ := m.embeddable(s); emb {
					if id == "nav.pane_next" && m.focus == 0 {
						return m.focusTerm()
					}
					m.focus = 0
					return nil
				}
			}
		}
		if m.view == viewProject && !m.hasPanes() {
			if id == "nav.pane_next" {
				return m.viewKey(keyFor("right"))
			}
			return m.viewKey(keyFor("left"))
		}
		if m.hasPanes() {
			m.focus = 1 - m.focus
			m.detailOff = 0
		}
		return nil
	case "nav.match_next", "nav.match_prev":
		if m.view == viewReader {
			if id == "nav.match_next" {
				return m.readerJump(1)
			}
			return m.readerJump(-1)
		}
		if m.filter == "" || !m.view.topLevel() {
			// n without a filter: the view's own meaning (new session).
			if id == "nav.match_next" && (m.view == viewSessions || m.view == viewProjects) {
				return m.openQuickLaunch()
			}
			return nil
		}
		d := 1
		if id == "nav.match_prev" {
			d = -1
		}
		return m.cycleMatch(d)
	case "sessions.answer":
		return m.answerSelected()
	case "sessions.read":
		s, ok := m.selectedSession()
		if !ok {
			return nil
		}
		return m.openReader(s)
	case "sessions.yank":
		s, ok := m.selectedSession()
		if !ok {
			return nil
		}
		return m.copyText(s.Session.SessionID, "copied session ID "+s.Session.SessionID)
	case "reader.search":
		return m.startReaderSearch()
	case "sessions.reparent":
		return m.reparentSelected() // reparent.go
	case "sessions.rename":
		return m.renameOrRelaunch() // relaunch.go: R relaunches a never-started session
	case "sessions.worktrees":
		return m.openWorktreeOffer() // worktrees.go
	case "manage.rename":
		return m.renameAccountSelected() // account_rename.go
	case "manage.login":
		return m.loginOnMachine() // login.go
	case "manage.activity":
		return m.activityFromManage() // activity.go
	case "manage.filter":
		return m.cycleActivityFilter()
	}
	if k, ok := canon[id]; ok {
		before := m.selectionKey()
		cmd := m.viewKey(keyFor(k))
		if m.selectionKey() != before {
			m.detailOff = 0
		}
		return cmd
	}
	return nil
}

// selectionKey identifies the current selection, to reset detail scroll
// when it changes.
func (m *Model) selectionKey() string {
	return m.sessList.sel + "|" + m.dg.list.sel + "|" + m.projList.sel + "|" + m.mgmtList.sel
}

func (m *Model) openManage(v viewID) tea.Cmd {
	m.mgmtList = listState{}
	m.focus = 0
	if m.view == v {
		return nil
	}
	if !m.view.topLevel() {
		m.view, m.back = viewSessions, nil
	}
	return m.push(v)
}

// cycleMatch moves to the next/previous row of the filtered list, wrapping.
func (m *Model) cycleMatch(d int) tea.Cmd {
	l := m.activeList()
	if l == nil || len(l.ids) == 0 {
		return nil
	}
	l.idx = (l.idx + d + len(l.ids)) % len(l.ids)
	l.sel = l.ids[l.idx]
	if m.view == viewSessions {
		return m.schedulePreview()
	}
	return nil
}

func (m *Model) activeList() *listState {
	switch m.view {
	case viewSessions:
		return &m.sessList
	case viewProjects:
		return &m.projList
	case viewGrid:
		return &m.grid.list
	}
	return nil
}

// copyText puts text on the clipboard (OSC 52, or Options.Clipboard).
func (m *Model) copyText(text, note string) tea.Cmd {
	if m.opts.Clipboard != nil {
		m.opts.Clipboard(text)
		return m.setToast(note, toneGood)
	}
	return tea.Batch(tea.SetClipboard(text), m.setToast(note+" (OSC 52)", toneGood))
}

// whichKeyLines renders the leader popup for the pending prefix.
func (m *Model) whichKeyLines(w int) []string {
	entries := m.km.next(m.scopes(), m.pending)
	if len(entries) == 0 {
		return nil
	}
	colW := 0
	for _, e := range entries {
		colW = max(colW, width(displayKey(e.key))+2+width(e.desc))
	}
	colW = min(colW+4, 40)
	cols := max(1, (w-4)/colW)
	var lines []string
	title := " " + m.th.key.Render(displaySeq(m.pendingDisplay())) + m.th.dim.Render(" … ") + m.th.faint.Render("esc cancels") + " "
	lines = append(lines, m.th.faint.Render("╭─")+title+m.th.faint.Render(strings.Repeat("─", max(0, w-width(title)-3))+"╮"))
	for i := 0; i < len(entries); i += cols {
		row := ""
		for j := i; j < min(i+cols, len(entries)); j++ {
			e := entries[j]
			desc := e.desc
			if e.group {
				desc = m.th.accent.Render(desc)
			}
			row += pad(m.th.key.Render(displayKey(e.key))+"  "+desc, colW-3) + "   "
		}
		lines = append(lines, m.th.faint.Render("│")+" "+pad(row, w-4)+" "+m.th.faint.Render("│"))
	}
	lines = append(lines, m.th.faint.Render("╰"+strings.Repeat("─", w-2)+"╯"))
	return lines
}

func (m *Model) pendingDisplay() []string {
	out := make([]string, len(m.pending))
	for i, k := range m.pending {
		if k == m.km.Leader {
			k = "<leader>"
		}
		out[i] = k
	}
	return out
}

func displayKey(k string) string {
	if k == "space" {
		return "␣"
	}
	return k
}
