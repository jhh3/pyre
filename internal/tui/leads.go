package tui

import (
	"sort"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/contracts"
)

// Lead signals (owner request 2026-10-14: "I should know when a project
// lead is worth clicking on"). A lead is a top-level session (no listed,
// unarchived parent); its signal sums up the lead and every descendant,
// wherever they are listed. Exactly one signal per lead, the highest wins:
//
//	● asks N    open asks from the subtree (red); the detail names the
//	            soonest deadline
//	◆ waiting   something in the subtree needs you: a waiting-on-you,
//	            approval or question flag, or any other needs-you flag
//	            (the needs-you rule, attention_flag.go)
//	• new       your answer is ready: you typed or sent the lead something,
//	            it has finished since, and you have not looked since you
//	            sent it (or the lead left a message for you after you last
//	            looked). Turns the lead takes on its own (children's
//	            reports, Pyre mail, background tasks) are not new: leads
//	            wake for those all day.
//	◌ working   something in the subtree is running
//	(nothing)   the lead row reads dim
//
// The chip sits at the right of the lead's title cell (children rows are
// unchanged); a project header (project grouping) sums its leads in the
// same words, worst first; the Sessions tab counts leads that ask or wait.
//
// "Last looked" is the latest of the session's OwnerActiveAt (the owner
// typed or messaged), the last time this TUI showed it selected for at
// least Options.SeenDwell (2s), and the seen baseline (the first launch
// with lead signals, so nothing old reads new). A session on screen past
// the dwell counts as looked at now. The viewed times persist in the
// state file (bounded, archived sessions pruned).
//
// Turn finish time: the hub keeps no "state since"; a run's
// Freshness.ObservedAt is the hub's time of the node's last report, and a
// node reports a run on its state changes, so for an idle run
// (waiting_input) it is when the turn ended (a node reconnect re-reports
// and may read as new once). An ended run uses EndedAt. A message flag
// uses the flag's Since.

type leadKind int

const (
	leadQuiet leadKind = iota
	leadWorking
	leadNew
	leadWaiting
	leadAsks
)

// leadSig is one lead's signal and what it is made of.
type leadSig struct {
	kind     leadKind
	asks     int             // open asks from the subtree
	soonest  *contracts.Ask  // the open ask with the nearest deadline
	waiting  int             // sessions in the subtree that need you
	newAt    time.Time       // the latest unseen finish
	working  int             // sessions in the subtree that run
	sessions map[string]bool // the lead and its descendants
	reason   string          // the first waiting session's reason
}

// leadState memoizes the signals for one frame (cleared on every Update).
type leadState struct {
	valid bool
	root  map[string]string  // session → its lead
	sigs  map[string]leadSig // lead → signal
}

// seenState is when this TUI last showed each session (dwell ≥
// Options.SeenDwell), persisted in the state file.
type seenState struct {
	at    map[string]time.Time
	since time.Time // baseline: finishes before it never read new
	cur   string    // the session on screen
	tok   int
	dwelt bool // cur has been on screen past the dwell
	fresh bool // the baseline is not in the state file yet (Init saves it)
}

// seenDwellMsg fires when a session has been on screen for the dwell.
type seenDwellMsg struct {
	id  string
	tok int
}

// seenMax bounds the remembered viewed times.
const seenMax = 500

// leadRoot is s's lead: the topmost listed, unarchived ancestor (s itself
// when it has none). Lineage cycles stop the walk.
func leadRoot(s contracts.SessionView, byID map[string]contracts.SessionView) string {
	id := s.Session.SessionID
	seen := map[string]bool{id: true}
	for p := lineageParent(s.Session); p != "" && !seen[p] && len(seen) < 64; {
		seen[p] = true
		a, ok := byID[p]
		if !ok || a.Session.Archived {
			break
		}
		id = a.Session.SessionID
		p = lineageParent(a.Session)
	}
	return id
}

// viewedAt is when the owner last had session id on screen (the dwell or
// the seen baseline), not counting typing to it.
func (m *Model) viewedAt(s contracts.SessionView) time.Time {
	id := s.Session.SessionID
	if id == m.seen.cur && m.seen.dwelt {
		return m.now()
	}
	t := m.seen.since
	if a, ok := m.seen.at[id]; ok && a.After(t) {
		t = a
	}
	return t
}

// lookedAt is when the owner last looked at session id (see above).
func (m *Model) lookedAt(s contracts.SessionView) time.Time {
	t := m.viewedAt(s)
	if s.Session.OwnerActiveAt != nil && s.Session.OwnerActiveAt.After(t) {
		t = *s.Session.OwnerActiveAt
	}
	return t
}

// finishedAt is when s's run last finished a turn or ended (zero when it
// is working or unknown).
func finishedAt(s contracts.SessionView) time.Time {
	r := s.Run
	if r == nil || r.Freshness.Stale {
		return time.Time{}
	}
	switch {
	case r.State == contracts.RunWaitingInput:
		return r.Freshness.ObservedAt
	case r.State.Terminal() && r.EndedAt != nil:
		return *r.EndedAt
	}
	return time.Time{}
}

// sessionWorking: the run is busy (working, starting, background work).
func (m *Model) sessionWorking(s contracts.SessionView) bool {
	if s.Session.Archived {
		return false
	}
	switch sessionStatus(s, m.nodeOf(s)).label {
	case "working", "starting", "background":
		return true
	}
	return false
}

// leadSignals computes (once per frame) every lead's signal.
func (m *Model) leadSignals() *leadState {
	ls := &m.leads
	if ls.valid {
		return ls
	}
	ls.valid = true
	ss := m.sessions()
	byID := make(map[string]contracts.SessionView, len(ss))
	for _, s := range ss {
		byID[s.Session.SessionID] = s
	}
	ls.root = make(map[string]string, len(ss))
	ls.sigs = map[string]leadSig{}
	for _, s := range ss {
		if s.Session.Archived {
			continue
		}
		ls.root[s.Session.SessionID] = leadRoot(s, byID)
	}
	sig := func(lead string) leadSig {
		g, ok := ls.sigs[lead]
		if !ok {
			g.sessions = map[string]bool{}
		}
		return g
	}
	for _, s := range ss {
		id := s.Session.SessionID
		lead, ok := ls.root[id]
		if !ok {
			continue
		}
		g := sig(lead)
		g.sessions[id] = true
		if needsYou(s, m.nodeOf(s)) {
			g.waiting++
			if g.reason == "" {
				_, g.reason = sessionAttention(s)
				if g.reason == "" {
					g.reason = sessionStatus(s, m.nodeOf(s)).label
				}
			}
		} else if m.sessionWorking(s) {
			g.working++
		}
		ls.sigs[lead] = g
	}
	// A waiting-on-you item whose session flag has not caught up yet.
	waitingItem := map[string]bool{}
	for _, a := range m.attention() {
		if (a.State == contracts.AttnOpen || a.State == contracts.AttnRead) && a.SessionID != "" &&
			(a.Kind == contracts.AttnWaiting || a.Kind == contracts.AttnApproval || a.Kind == contracts.AttnQuestion) {
			waitingItem[a.SessionID] = true
		}
	}
	for sid := range waitingItem {
		if s, ok := byID[sid]; ok && !s.Session.Archived && !needsYou(s, m.nodeOf(s)) {
			if lead, ok := ls.root[sid]; ok {
				g := ls.sigs[lead]
				g.waiting++
				if g.reason == "" {
					g.reason = "waiting"
				}
				ls.sigs[lead] = g
			}
		}
	}
	for _, a := range m.asks() {
		if a.State != contracts.AskOpen || a.SessionID == "" {
			continue
		}
		lead, ok := ls.root[a.SessionID]
		if !ok {
			continue
		}
		g := ls.sigs[lead]
		g.asks++
		if a.DeadlineAt != nil && (g.soonest == nil || g.soonest.DeadlineAt == nil || a.DeadlineAt.Before(*g.soonest.DeadlineAt)) {
			a := a
			g.soonest = &a
		} else if g.soonest == nil {
			a := a
			g.soonest = &a
		}
		ls.sigs[lead] = g
	}
	for lead, g := range ls.sigs {
		l, ok := byID[lead]
		if ok && !m.sessionWorking(l) {
			// New: you sent the lead something, it (idle or ended) finished
			// since, and you have not looked since you sent it; or the lead
			// left you a message after you last looked.
			if oa := l.Session.OwnerActiveAt; oa != nil && oa.After(m.viewedAt(l)) {
				if f := finishedAt(l); f.After(*oa) {
					g.newAt = f
				}
			}
			if fl := l.Flag; fl != nil && fl.Level == contracts.FlagMessage && fl.Since.After(m.lookedAt(l)) && fl.Since.After(g.newAt) {
				g.newAt = fl.Since
			}
		}
		switch {
		case g.asks > 0:
			g.kind = leadAsks
		case g.waiting > 0:
			g.kind = leadWaiting
		case !g.newAt.IsZero():
			g.kind = leadNew
		case g.working > 0:
			g.kind = leadWorking
		}
		ls.sigs[lead] = g
	}
	return ls
}

// leadSignal is session id's signal when it is a lead; ok false for a
// session with a listed parent.
func (m *Model) leadSignal(id string) (leadSig, bool) {
	ls := m.leadSignals()
	if ls.root[id] != id {
		return leadSig{}, false
	}
	return ls.sigs[id], true
}

// leadChip is a lead row's chip ("● asks 2", "◆ waiting", "• new",
// "◌ working"), "" when the lead is quiet, and its width.
func (m *Model) leadChip(g leadSig) (string, int) {
	var sym, word string
	var t tone
	switch g.kind {
	case leadAsks:
		sym, word, t = "●", "asks "+itoa(g.asks), toneBad
	case leadWaiting:
		sym, word, t = "◆", "waiting", toneWarn
	case leadNew:
		sym, word, t = "•", "new", toneAccent
	case leadWorking:
		return m.th.tone(toneGood, "◌") + m.th.dim.Render(" working"), width("◌ working")
	default:
		return "", 0
	}
	return m.th.tone(t, sym+" "+word), width(sym + " " + word)
}

// leadWords describes lead id's signal for its facts ("asks you 2 · I
// web inbox · soonest: … → Canary in 12m"), "" when quiet. The asks link
// to the web inbox filtered to the lead (weblink.go).
func (m *Model) leadWords(id string, g leadSig) string {
	switch g.kind {
	case leadAsks:
		out := m.askLink("session", id, m.th.tone(toneBad, "● asks you "+itoa(g.asks))) + m.th.dim.Render(" · ") + m.th.key.Render("I") + m.th.dim.Render(" web inbox")
		if a := g.soonest; a != nil {
			out += m.th.dim.Render(" · soonest: "+trunc(a.Title, 40)+" ") + m.th.tone(m.deadlineTone(*a), m.deadlineWords(*a, true))
		}
		return out
	case leadWaiting:
		return m.th.tone(toneWarn, "◆ waiting on you") + m.th.dim.Render(" · "+plural(g.waiting, "session", "sessions")+" ("+g.reason+")")
	case leadNew:
		return m.th.tone(toneAccent, "• new") + m.th.dim.Render(" · finished "+ago(m.now(), g.newAt)+", after you last looked")
	case leadWorking:
		return m.th.tone(toneGood, "◌ working") + m.th.dim.Render(" · "+itoa(g.working)+" running")
	}
	return ""
}

// projectLeadSummary sums a project group's leads, worst first: " · ● 2
// asks · ◆ 1 waiting · • 1 new · ◌ 1 working" (only non-zero parts), and
// whether every lead is quiet. leads are the group's top-level rows; the
// asks link to the web inbox filtered to project (weblink.go).
func (m *Model) projectLeadSummary(project string, leads []string) ([]string, bool) {
	var asks, waiting, fresh, working int
	for _, id := range leads {
		g, ok := m.leadSignal(id)
		if !ok {
			continue
		}
		switch g.kind {
		case leadAsks:
			asks += g.asks
		case leadWaiting:
			waiting++
		case leadNew:
			fresh++
		case leadWorking:
			working++
		}
	}
	sep := m.th.faint.Render(" · ")
	var out []string
	if asks > 0 {
		key := "project"
		if project == noGroup {
			key = "" // "No project": the whole inbox
		}
		out = append(out, sep+m.askLink(key, project, m.th.tone(toneBad, "● "+itoa(asks)+" "+pluralWord(asks, "ask", "asks"))))
	}
	if waiting > 0 {
		out = append(out, sep+m.th.tone(toneWarn, "◆ "+itoa(waiting)+" waiting"))
	}
	if fresh > 0 {
		out = append(out, sep+m.th.tone(toneAccent, "• "+itoa(fresh)+" new"))
	}
	if working > 0 {
		out = append(out, sep+m.th.tone(toneGood, "◌")+m.th.dim.Render(" "+itoa(working)+" working"))
	}
	return out, len(out) == 0
}

func pluralWord(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// headerLeads is the Sessions tab's count: leads that ask you or wait on
// you (the needs-you rule, rolled up to leads; "new" does not count).
func (m *Model) headerLeads() int {
	n := 0
	for _, g := range m.leadSignals().sigs {
		if g.kind >= leadWaiting {
			n++
		}
	}
	return n
}

// ---- seen ----

// onScreenSession is the session the owner is looking at: the selected
// session of the Sessions view (its terminal is on the right), not in the
// table, nor under the Grid.
func (m *Model) onScreenSession() string {
	if m.view != viewSessions || m.tableShown() || !isSessionRow(m.sessList.sel) {
		return ""
	}
	return m.sessList.sel
}

// seenAfter follows the session on screen after every update: leaving one
// that dwelt records when it was last seen; a new one starts the dwell.
func (m *Model) seenAfter() tea.Cmd {
	id := m.onScreenSession()
	s := &m.seen
	if id == s.cur {
		return nil
	}
	var save tea.Cmd
	if s.cur != "" && s.dwelt {
		m.markSeen(s.cur)
		save = m.saveState()
	}
	s.cur, s.dwelt = id, false
	s.tok++
	if id == "" {
		return save
	}
	tok, d := s.tok, m.opts.SeenDwell
	return tea.Batch(save, tea.Tick(d, func(time.Time) tea.Msg { return seenDwellMsg{id: id, tok: tok} }))
}

// onSeenDwell: the session stayed on screen for the dwell.
func (m *Model) onSeenDwell(msg seenDwellMsg) tea.Cmd {
	s := &m.seen
	if msg.tok != s.tok || msg.id != s.cur {
		return nil
	}
	s.dwelt = true
	m.markSeen(msg.id)
	m.leads.valid = false
	return m.saveState()
}

// markSeen records id as seen now and keeps the map bounded: archived
// sessions go, then the oldest beyond seenMax.
func (m *Model) markSeen(id string) {
	s := &m.seen
	if s.at == nil {
		s.at = map[string]time.Time{}
	}
	s.at[id] = m.now()
	for _, v := range m.sessions() {
		if v.Session.Archived {
			delete(s.at, v.Session.SessionID)
		}
	}
	if len(s.at) <= seenMax {
		return
	}
	ids := make([]string, 0, len(s.at))
	for k := range s.at {
		ids = append(ids, k)
	}
	sort.Slice(ids, func(i, j int) bool { return s.at[ids[i]].After(s.at[ids[j]]) })
	for _, k := range ids[seenMax:] {
		delete(s.at, k)
	}
}

func (s *seenState) load(st tuiState, now time.Time) {
	s.at = map[string]time.Time{}
	for k, v := range st.Seen {
		s.at[k] = v
	}
	s.since, s.fresh = now, true // the first launch with lead signals: nothing earlier is new
	if st.SeenSince != nil {
		s.since, s.fresh = *st.SeenSince, false
	}
}

func (s seenState) save(st *tuiState) {
	since := s.since
	st.SeenSince = &since
	if len(s.at) > 0 {
		st.Seen = make(map[string]time.Time, len(s.at))
		for k, v := range s.at {
			st.Seen[k] = v
		}
	}
}

// ---- accessors for tests ----

// SeenDwellDue is the message the dwell timer of the session on screen
// sends when it fires; tests deliver it instead of waiting.
func (m *Model) SeenDwellDue() tea.Msg { return seenDwellMsg{id: m.seen.cur, tok: m.seen.tok} }

// LeadSignal is a lead's signal word ("asks", "waiting", "new", "working",
// "" quiet) and the open asks; ok false when id is not a lead.
func (m *Model) LeadSignal(id string) (kind string, asks int, ok bool) {
	g, ok := m.leadSignal(id)
	if !ok {
		return "", 0, false
	}
	return [...]string{"", "working", "new", "waiting", "asks"}[g.kind], g.asks, true
}
