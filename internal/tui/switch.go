package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/contracts"
)

// Switching between sessions from anywhere, including while typing in the
// embedded terminal:
//
//   - alt+up / alt+down (sessions.prev / sessions.next) move to the
//     previous / next session in the Sessions list's visible order (pins,
//     grouping and filter respected; folded groups, machine rows and
//     archived sessions skipped). The ends stop with a quiet hint.
//   - alt+left (sessions.last) goes back to the previously viewed session,
//     toggling like tmux's last-session; alt+right (sessions.attention)
//     cycles through the sessions that need you.
//   - alt+s or <leader>s (global.switcher) opens a small quick switcher,
//     like tmux's prefix+s: type to fuzzy-filter, most recent first.
//
// A single modifier chord bound to one of these actions is checked before
// anything else gets the key, the embedded terminal included, so its bytes
// never reach the harness. When the switch starts in the terminal, typing
// focus follows to the new session's terminal: the old session's writable
// attach and input lease are released at once (reconcileTerms), and the new
// one is taken on the next key (or click, wheel or paste), so flicking
// through sessions never churns leases. ESC b / ESC f (macOS Terminal.app's
// option+left/right) act as alt+left/right outside the terminal only
// (Keymap.OptionArrows).

// switchActions are the actions whose modifier chords work from anywhere.
var switchActions = []string{"sessions.prev", "sessions.next", "sessions.last", "sessions.attention", "global.switcher"}

// chordAction is the switch action bound to key as a single modifier chord.
func (m *Model) chordAction(key string) string {
	for _, id := range switchActions {
		for _, seq := range m.km.bind[id] {
			if len(seq) == 1 && isChord(seq[0]) && seq[0] == key {
				return id
			}
		}
	}
	return ""
}

func isChord(k string) bool {
	for _, mod := range []string{"ctrl+", "alt+", "meta+", "super+", "hyper+"} {
		if strings.Contains(k, mod) {
			return true
		}
	}
	return false
}

// metaAsAlt spells a key with Meta (CSI 1;9 from iTerm2-style encodings) as
// Alt, so "alt+up" bindings match what those terminals send.
func metaAsAlt(k tea.KeyPressMsg) tea.KeyPressMsg {
	if k.Mod&tea.ModMeta != 0 {
		k.Mod = k.Mod&^tea.ModMeta | tea.ModAlt
	}
	return k
}

// textInputOwnsKeys reports whether a text field has the keyboard (where
// alt+arrows are word motions and stay with the field).
func (m *Model) textInputOwnsKeys() bool {
	return m.filtering || (m.view == viewLaunch && m.launch != nil && m.launch.editing != "") ||
		(m.view == viewReader && m.reader.searching)
}

// switchKey runs a switch chord before the key reaches anything else; ok is
// false when k is not one (or a text input or another overlay owns it).
func (m *Model) switchKey(k tea.KeyPressMsg) (tea.Cmd, bool) {
	if k.Mod&(tea.ModAlt|tea.ModMeta|tea.ModCtrl|tea.ModSuper|tea.ModHyper) == 0 {
		return nil, false
	}
	inTerm := m.focusedTerm() != nil
	if !inTerm && (m.textInputOwnsKeys() || (m.overlay != nil && !m.isSwitcherOpen())) {
		return nil, false
	}
	id := m.chordAction(k.String())
	if id == "" {
		id = m.chordAction(metaAsAlt(k).String())
	}
	if id == "" && (m.km.OptionArrows == "always" || (m.km.OptionArrows == "list" && !inTerm)) {
		switch metaAsAlt(k).String() {
		case "alt+b":
			id = m.chordAction("alt+left")
		case "alt+f":
			id = m.chordAction("alt+right")
		}
	}
	if id == "" {
		return nil, false
	}
	if m.isSwitcherOpen() {
		return m.overlay.(*switcherOverlay).chord(m, id), true
	}
	m.clearPending()
	return m.runSwitch(id), true
}

func (m *Model) isSwitcherOpen() bool {
	_, ok := m.overlay.(*switcherOverlay)
	return ok
}

// runSwitch performs a switch action (also reached through the dispatcher
// for non-chord bindings such as <leader>s).
func (m *Model) runSwitch(id string) tea.Cmd {
	typing := m.focusedTerm() != nil
	switch id {
	case "sessions.prev":
		return m.switchStep(-1, typing)
	case "sessions.next":
		return m.switchStep(1, typing)
	case "sessions.last":
		return m.switchLast(typing)
	case "sessions.attention":
		return m.switchAttention(typing)
	case "global.switcher":
		return m.openSwitcher(typing)
	}
	return nil
}

func isSessionRow(id string) bool {
	if id == "" {
		return false
	}
	if _, ok := groupOfRow(id); ok {
		return false
	}
	_, ok := machineOfRow(id)
	return !ok
}

// switchStep moves d sessions along the visible list, skipping header rows.
func (m *Model) switchStep(d int, typing bool) tea.Cmd {
	ids := m.sessList.ids
	if len(ids) == 0 {
		return m.setToast("no sessions", toneMuted)
	}
	cur := m.sessList.idx
	if cur < 0 || cur >= len(ids) || ids[cur] != m.sessList.sel {
		cur = -1
		for i, id := range ids {
			if id == m.sessList.sel {
				cur = i
			}
		}
	}
	if m.view != viewSessions && isSessionRow(m.sessList.sel) {
		return m.switchTo(m.sessList.sel, typing) // elsewhere: show the selected session first
	}
	for i := cur + d; i >= 0 && i < len(ids); i += d {
		if isSessionRow(ids[i]) {
			return m.switchTo(ids[i], typing)
		}
	}
	if d < 0 {
		return m.setToast("top of the list", toneMuted)
	}
	return m.setToast("end of the list", toneMuted)
}

// switchLast toggles to the previously viewed session.
func (m *Model) switchLast(typing bool) tea.Cmd {
	if m.view == viewSessions && isSessionRow(m.sessList.sel) {
		m.noteViewed(m.sessList.sel)
	}
	prev := m.viewPrev
	if prev == "" || prev == m.sessList.sel && m.view == viewSessions {
		return m.setToast("no previous session yet", toneMuted)
	}
	if s, ok := m.session(prev); !ok || s.Session.Archived {
		m.viewPrev = ""
		return m.setToast("the previous session is gone", toneMuted)
	}
	return m.switchTo(prev, typing)
}

// switchAttention cycles through the sessions that need you, in list order
// (sessions inside folded groups included: their group opens).
func (m *Model) switchAttention(typing bool) tea.Cmd {
	vs := m.visibleSessions()
	cur := -1
	for i, s := range vs {
		if s.Session.SessionID == m.sessList.sel {
			cur = i
		}
	}
	for i := 1; i <= len(vs); i++ {
		s := vs[(cur+i+len(vs))%len(vs)]
		if gridNeedsYou(s, m.nodeOf(s)) {
			if s.Session.SessionID == m.sessList.sel && m.view == viewSessions {
				return m.setToast("only this session needs you", toneMuted)
			}
			return m.switchTo(s.Session.SessionID, typing)
		}
	}
	return m.setToast("nothing needs you", toneMuted)
}

// noteViewed records id as the session being looked at (for sessions.last
// and the switcher's recent-first order).
func (m *Model) noteViewed(id string) {
	if !isSessionRow(id) {
		return
	}
	if id != m.viewCur {
		if m.viewCur != "" {
			m.viewPrev = m.viewCur
		}
		m.viewCur = id
	}
	for i, x := range m.mru {
		if x == id {
			copy(m.mru[1:i+1], m.mru[:i])
			m.mru[0] = id
			return
		}
	}
	m.mru = append([]string{id}, m.mru...)
	if len(m.mru) > 200 {
		m.mru = m.mru[:200]
	}
}

// revealSession selects id in the Sessions list, clearing a filter that
// hides it and opening its folded group; false when it cannot be listed
// (archived, or ended while ended sessions are hidden).
func (m *Model) revealSession(id string) (bool, tea.Cmd) {
	if m.sessList.selectID(id) {
		return true, nil
	}
	s, ok := m.session(id)
	if !ok || s.Session.Archived || (sessionEnded(s) && !m.showEnded) {
		return false, nil
	}
	var cmd tea.Cmd
	if m.filter != "" && !m.sessionMatches(s, m.filter) {
		m.applyFilter("")
	}
	if g := m.sessionGroup(s); m.groupCollapsed(g) {
		delete(m.grouping.collapsed, string(m.groupMode())+":"+g)
		cmd = m.saveState()
	}
	if m.revealInTree(id) { // under a folded parent (tree.go)
		cmd = m.saveState()
	}
	m.syncSessions()
	return m.sessList.selectID(id), cmd
}

// switchTo shows session id in the Sessions view; with typing, the keyboard
// stays with the terminal (the new session's input lease is taken on the
// next key).
func (m *Model) switchTo(id string, typing bool) tea.Cmd {
	if m.view != viewSessions {
		m.view, m.back = viewSessions, nil
	}
	ok, save := m.revealSession(id)
	if !ok {
		return m.setToast("that session is not in the list (. shows ended sessions)", toneMuted)
	}
	m.detailOff, m.termLazy, m.focus = 0, "", 0
	m.noteViewed(id)
	cmds := []tea.Cmd{save, m.schedulePreview()}
	if s, ok := m.session(id); ok && typing {
		if emb, _ := m.embeddable(s); emb && m.actionReason(s, "attach") == "" {
			m.focus, m.termLazy = 1, id
			m.termViewFor(s)
		}
	}
	cmds = append(cmds, m.openTermView(), m.termWait())
	return tea.Batch(cmds...)
}

// lazyFocus reports whether tv holds the keyboard without a writable attach
// yet (focus followed a switch; the next key takes the lease).
func (m *Model) lazyFocus(tv *termView) bool {
	if m.termLazy == "" || m.termLazy != tv.sid || tv.wantInteractive() {
		return false
	}
	s, ok := m.session(tv.sid)
	if !ok {
		return false
	}
	emb, _ := m.embeddable(s)
	return emb
}

// activateLazy swaps a lazily focused pane to a writable attach with the
// input lease; keys sent meanwhile wait in tv.pending. Another client's
// lease is never taken over silently here: the refusal is explained and
// the next enter offers the takeover, as for enter.
func (m *Model) activateLazy(tv *termView) tea.Cmd {
	if !m.lazyFocus(tv) {
		return nil
	}
	m.termLazy = ""
	s, ok := m.session(tv.sid)
	if !ok {
		return nil
	}
	tv.takeover = tv.takeoverOffered()
	tv.askTakeover, tv.focusNote = "", ""
	if live, mode := tv.live(); live && mode == termInteractive {
		return nil
	}
	if st, _, _ := tv.status(); st == termStarting {
		tv.reqMode = termInteractive
		return m.termWait()
	}
	return tea.Batch(m.requestTerm(s, tv, termInteractive), m.termWait())
}

// LazyTyping reports whether the focused terminal has not taken its input
// lease yet (tests).
func (m *Model) LazyTyping() bool {
	tv := m.focusedTerm()
	return tv != nil && m.lazyFocus(tv)
}

// switchHint is the footer hint for the switch keys ("alt+↑↓"), or "".
func (m *Model) switchHint() string {
	if m.km.keysFor("sessions.prev") == "alt+up" && m.km.keysFor("sessions.next") == "alt+down" {
		return "alt+↑↓"
	}
	return ""
}

// ---- quick switcher ----

type switchItem struct {
	s      contracts.SessionView
	st     status
	fields []string // title first (highlighted), then machine, harness, …
	recent int      // position in the recent-first order
}

type switchRow struct {
	it    *switchItem
	score int
	pos   []int
}

type switcherOverlay struct {
	query  string
	items  []switchItem
	res    []switchRow
	rev    int // sessions value revision the items were built from
	sel    string
	idx    int
	top    int
	typing bool   // opened from the embedded terminal
	origin string // session selected when it opened
	// Pick mode (W-reparent, reparent.go): enter calls pick with the chosen
	// session ID instead of switching; exclude hides sessions; none adds a
	// "top level" row (ID reparentNone); curParent is marked.
	pick      func(m *Model, id string) tea.Cmd
	exclude   map[string]bool
	none      bool
	curParent string
	boxTitle  string
}

// openSwitcher opens the quick switcher; typing remembers that the keyboard
// was in the embedded terminal (enter keeps it there, esc returns to it).
func (m *Model) openSwitcher(typing bool) tea.Cmd {
	if m.view == viewSessions && isSessionRow(m.sessList.sel) {
		m.noteViewed(m.sessList.sel)
	}
	o := &switcherOverlay{typing: typing, origin: m.sessList.sel}
	o.build(m)
	o.rank(m)
	if len(o.res) > 1 && o.res[0].it.s.Session.SessionID == o.origin {
		o.idx = 1 // like alt-tab: the cursor starts on the previous session
	}
	if len(o.res) > 0 {
		o.sel = o.res[o.idx].it.s.Session.SessionID
	}
	m.overlay = o
	return nil
}

func (o *switcherOverlay) build(m *Model) {
	if st := m.loads["sessions"]; st != nil {
		o.rev = st.rev
	}
	rank := make(map[string]int, len(m.mru))
	for i, id := range m.mru {
		rank[id] = i
	}
	type act struct {
		i  int
		at time.Time
	}
	o.items = o.items[:0]
	if o.none {
		top := contracts.SessionView{Session: contracts.Session{SessionID: reparentNone, Title: "top level (no parent)"}}
		o.items = append(o.items, switchItem{s: top, st: status{sym: "↑", tone: toneMuted}, fields: []string{top.Session.Title, "none"}, recent: -1})
	}
	var rest []act
	for _, s := range m.sessions() {
		if s.Session.Archived || (!m.showEnded && sessionEnded(s)) || o.exclude[s.Session.SessionID] {
			continue
		}
		nd := m.nodeOf(s)
		st := listStatus(s, nd)
		f := []string{s.Session.Title, s.NodeName, string(s.Session.Harness), st.label, m.projectName(s.Session.ProjectID), sessionRepo(s), s.Session.SessionID}
		f = append(f, userTags(s.Session.Tags)...)
		it := switchItem{s: s, st: st, fields: f, recent: -1}
		if r, ok := rank[s.Session.SessionID]; ok {
			it.recent = r
		}
		o.items = append(o.items, it)
		if it.recent < 0 {
			rest = append(rest, act{len(o.items) - 1, m.lastActivity(s)})
		}
	}
	// Recent-first: what you viewed (most recent first), then the rest by
	// their latest activity.
	sort.SliceStable(rest, func(i, j int) bool { return rest[i].at.After(rest[j].at) })
	base := len(m.mru)
	for n, r := range rest {
		o.items[r.i].recent = base + n
	}
}

func (o *switcherOverlay) rank(m *Model) {
	q := strings.TrimSpace(o.query)
	o.res = o.res[:0]
	for i := range o.items {
		it := &o.items[i]
		score, pos, ok := matchTerms(q, it.fields...)
		if !ok {
			continue
		}
		if q != "" {
			score += max(0, 12-it.recent) // a small bonus for what you just used
		}
		o.res = append(o.res, switchRow{it: it, score: score, pos: pos})
	}
	sort.SliceStable(o.res, func(i, j int) bool {
		a, b := o.res[i], o.res[j]
		if q != "" && a.score != b.score {
			return a.score > b.score
		}
		return a.it.recent < b.it.recent
	})
	o.idx = 0
	for i, r := range o.res {
		if r.it.s.Session.SessionID == o.sel {
			o.idx = i
		}
	}
	if len(o.res) > 0 {
		o.sel = o.res[o.idx].it.s.Session.SessionID
	} else {
		o.sel = ""
	}
}

// refresh rebuilds the rows when the session list changed while open.
func (o *switcherOverlay) refresh(m *Model) {
	if st := m.loads["sessions"]; st != nil && st.rev != o.rev {
		o.build(m)
		o.rank(m)
	}
}

func (o *switcherOverlay) hints() []hint {
	if o.pick != nil {
		return []hint{{"enter", "move here"}, {"↑↓", "move"}, {"esc", "cancel"}}
	}
	return []hint{{"enter", "switch"}, {"↑↓", "move"}, {"ctrl+j/k", "move"}, {"esc", "close"}}
}

func (o *switcherOverlay) move(d int) {
	if len(o.res) == 0 {
		return
	}
	o.idx = max(0, min(len(o.res)-1, o.idx+d))
	o.sel = o.res[o.idx].it.s.Session.SessionID
}

// chord handles a switch chord while the switcher is open: the arrows move,
// alt+s closes it, alt+left/right switch as usual.
func (o *switcherOverlay) chord(m *Model, id string) tea.Cmd {
	switch id {
	case "sessions.prev":
		o.move(-1)
		return nil
	case "sessions.next":
		o.move(1)
		return nil
	case "global.switcher":
		return o.close(m)
	}
	m.overlay = nil
	return m.runSwitchFrom(id, o.typing)
}

func (m *Model) runSwitchFrom(id string, typing bool) tea.Cmd {
	switch id {
	case "sessions.last":
		return m.switchLast(typing)
	case "sessions.attention":
		return m.switchAttention(typing)
	}
	return nil
}

// close leaves the switcher; the keyboard goes back where it was.
func (o *switcherOverlay) close(m *Model) tea.Cmd {
	m.overlay = nil
	if o.typing && m.sessList.sel == o.origin {
		if s, ok := m.session(o.origin); ok {
			if emb, _ := m.embeddable(s); emb {
				m.focus, m.termLazy = 1, o.origin
				return tea.Batch(m.openTermView(), m.termWait())
			}
		}
	}
	return nil
}

func (o *switcherOverlay) update(m *Model, k tea.KeyPressMsg) tea.Cmd {
	o.refresh(m)
	switch k.String() {
	case "esc", "ctrl+c":
		return o.close(m)
	case "enter":
		if len(o.res) == 0 {
			return nil
		}
		m.overlay = nil
		if o.pick != nil {
			return o.pick(m, o.res[o.idx].it.s.Session.SessionID)
		}
		return m.switchTo(o.res[o.idx].it.s.Session.SessionID, o.typing)
	case "down", "ctrl+j", "ctrl+n", "tab":
		o.move(1)
		return nil
	case "up", "ctrl+k", "ctrl+p", "shift+tab":
		o.move(-1)
		return nil
	case "pgdown":
		o.move(10)
		return nil
	case "pgup":
		o.move(-10)
		return nil
	case "backspace":
		if r := []rune(o.query); len(r) > 0 {
			o.query = string(r[:len(r)-1])
		}
	case "ctrl+u":
		o.query = ""
	case "ctrl+w":
		t := strings.TrimRight(o.query, " ")
		if i := strings.LastIndexByte(t, ' '); i >= 0 {
			o.query = t[:i+1]
		} else {
			o.query = ""
		}
	default:
		if k.Text == "" || k.Mod&^tea.ModShift != 0 {
			return nil
		}
		o.query += k.Text
	}
	o.sel, o.top = "", 0 // a new query: the best match first
	o.rank(m)
	return nil
}

func (o *switcherOverlay) updateMsg(m *Model, msg tea.Msg) tea.Cmd {
	if p, ok := msg.(tea.PasteMsg); ok {
		o.query += strings.Join(strings.Fields(p.Content), " ")
		o.sel, o.top = "", 0
		o.rank(m)
	}
	return nil
}

func (o *switcherOverlay) view(m *Model, w, h int) []string {
	o.refresh(m)
	bw := min(104, w-4)
	inner := bw - 4
	count := m.th.dim.Render(fmt.Sprintf("%d/%d", min(len(o.res), o.idx+1), len(o.res)))
	prompt := m.th.key.Render("›") + " " + o.query + m.th.accent.Render("▏")
	if o.query == "" {
		prompt += m.th.dim.Render(trunc("type a title, project, machine, harness, repo or status", max(0, inner-width(count)-6)))
	}
	lines := []string{pad(prompt, inner-width(count)) + count, m.th.faint.Render(strings.Repeat("─", inner))}
	rows := max(3, min(16, h-8))
	if o.idx < o.top {
		o.top = o.idx
	}
	if o.idx >= o.top+rows {
		o.top = o.idx - rows + 1
	}
	if len(o.res) == 0 {
		lines = append(lines, m.th.dim.Render("  no matching session"))
	}
	machW := min(16, max(6, inner/6))
	harnW, stW := 6, 11
	titleW := max(10, inner-2-2-machW-harnW-stW-3)
	for i := o.top; i < min(len(o.res), o.top+rows); i++ {
		r := o.res[i]
		sel := i == o.idx
		s := r.it.s
		title := iconTitle(r.it.s)
		if title == "" {
			title = shortID(s.Session.SessionID)
		}
		cur := ""
		if mode := m.groupMode(); mode == groupProject || mode == groupRepo {
			// The list's group, so a switch reads like the left nav.
			if k := m.groupKey(s); k != noGroup {
				cur = m.th.faint.Render(" · " + m.groupLabel(k))
			}
		}
		if s.Session.SessionID == o.origin {
			cur += m.th.faint.Render(" · current")
		}
		if o.curParent != "" && s.Session.SessionID == o.curParent {
			cur += m.th.faint.Render(" · current parent")
		}
		tw := titleW - width(cur)
		row := m.cursor(sel) + m.th.tone(r.it.st.tone, r.it.st.sym) + " " +
			pad(m.highlight(trunc(title, tw), r.pos, styleIf(sel, m.th.selText))+cur, titleW) + " " +
			m.th.dim.Render(pad(trunc(orDash(s.NodeName), machW), machW)) + " " +
			m.th.dim.Render(pad(trunc(string(s.Session.Harness), harnW), harnW)) + " " +
			m.th.tone(r.it.st.tone, pad(trunc(r.it.st.label, stW), stW))
		lines = append(lines, m.rowSel(sel, row, inner))
	}
	for n := max(1, len(o.res)); n < min(rows, max(3, len(o.items))); n++ {
		lines = append(lines, "") // a stable height: the panel does not jump while typing
	}
	head := "Switch session"
	if o.boxTitle != "" {
		head = o.boxTitle
	}
	return m.box(head, lines, bw, w, h)
}
