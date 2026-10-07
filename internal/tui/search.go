package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/search"
)

// Global search (␣/, or ?text in the finder): one query over every session
// transcript on every machine plus memory, notes and docs (the hub's
// search.query). Results are grouped by session with a highlighted snippet
// per match; enter opens the transcript reader at that turn with the match
// highlighted, a attaches. Machines that were not searched say so.

const searchDebounce = 450 * time.Millisecond

type searchOverlay struct {
	input    string
	editing  bool // the query line has the keyboard
	archived bool // include archived sessions
	typed    int  // keystroke generation (debounce)
	gen      int  // request generation
	running  bool
	ran      string // query of the shown results
	resp     *contracts.SearchResponse
	err      error
	rows     []searchRow
	hits     []int // indexes of selectable rows
	idx      int   // into hits
	top      int
	jk       jkState
}

type searchRow struct {
	header  bool
	section string // non-session section header ("Memory, notes & docs")
	hit     contracts.SearchHit
}

type searchDueMsg struct{ typed int }

func (m *Model) openSearch(query string) tea.Cmd {
	o := &searchOverlay{input: query, editing: true}
	m.overlay = o
	if strings.TrimSpace(query) != "" {
		return o.run(m)
	}
	return nil
}

func (o *searchOverlay) run(m *Model) tea.Cmd {
	q := strings.TrimSpace(o.input)
	if len(search.Terms(q)) == 0 {
		return nil
	}
	sapi, ok := m.api.(contracts.SearchAPI)
	if !ok {
		o.err = contracts.Errorf(contracts.ErrUnsupported, "this connection does not offer search")
		return nil
	}
	o.gen++
	gen, archived := o.gen, o.archived
	o.running = true
	return m.do(func(ctx context.Context) (any, error) {
		return sapi.Search(ctx, contracts.SearchRequest{Query: q, IncludeArchived: archived, Limit: 60})
	}, func(m *Model, val any, err error) tea.Cmd {
		cur, ok := m.overlay.(*searchOverlay)
		if !ok || cur != o || o.gen != gen {
			return nil // superseded or closed
		}
		o.running = false
		o.err = err
		if err != nil {
			return nil
		}
		resp, _ := val.(contracts.SearchResponse)
		o.resp, o.ran = &resp, q
		o.build()
		return nil
	})
}

// build groups transcript hits by session (in rank order of each session's
// best hit), then the other sources.
func (o *searchOverlay) build() {
	o.rows, o.hits, o.idx, o.top = nil, nil, 0, 0
	if o.resp == nil {
		return
	}
	order := []string{}
	bySession := map[string][]contracts.SearchHit{}
	var others []contracts.SearchHit
	for _, h := range o.resp.Hits {
		if h.Source != contracts.SearchSourceTranscript || h.SessionID == "" {
			others = append(others, h)
			continue
		}
		if _, ok := bySession[h.SessionID]; !ok {
			order = append(order, h.SessionID)
		}
		bySession[h.SessionID] = append(bySession[h.SessionID], h)
	}
	for _, sid := range order {
		hs := bySession[sid]
		o.rows = append(o.rows, searchRow{header: true, hit: hs[0]})
		for _, h := range hs {
			o.hits = append(o.hits, len(o.rows))
			o.rows = append(o.rows, searchRow{hit: h})
		}
	}
	if len(others) > 0 {
		o.rows = append(o.rows, searchRow{header: true, section: "Memory, notes & docs"})
		for _, h := range others {
			o.hits = append(o.hits, len(o.rows))
			o.rows = append(o.rows, searchRow{hit: h})
		}
	}
}

func (o *searchOverlay) selected() (contracts.SearchHit, bool) {
	if len(o.hits) == 0 {
		return contracts.SearchHit{}, false
	}
	o.idx = max(0, min(o.idx, len(o.hits)-1))
	return o.rows[o.hits[o.idx]].hit, true
}

func (o *searchOverlay) hints() []hint {
	if o.editing {
		return []hint{{"enter", "search"}, {"↓", "results"}, {"ctrl+t", "archived"}, {"esc", "close"}}
	}
	return []hint{{"enter", "open at match"}, {"a", "attach"}, {"j/k", "move"}, {"/", "edit query"}, {"esc", "close"}}
}

func (o *searchOverlay) update(m *Model, k tea.KeyPressMsg) tea.Cmd {
	key := k.String()
	if key == "ctrl+t" {
		o.archived = !o.archived
		return o.run(m)
	}
	if o.editing {
		return o.updateEditing(m, k)
	}
	move := func(d int) tea.Cmd {
		if len(o.hits) > 0 {
			o.idx = max(0, min(len(o.hits)-1, o.idx+d))
		}
		return nil
	}
	switch key {
	case "esc", "q":
		m.overlay = nil
		return nil
	case "down", "j", "ctrl+j", "ctrl+n", "tab":
		return move(1)
	case "up", "k", "ctrl+k", "ctrl+p", "shift+tab":
		if o.idx == 0 {
			o.editing = true
			return nil
		}
		return move(-1)
	case "pgdown", "ctrl+d":
		return move(10)
	case "pgup", "ctrl+u":
		return move(-10)
	case "g", "home":
		o.idx = 0
		return nil
	case "G", "end":
		return move(len(o.hits))
	case "enter":
		if h, ok := o.selected(); ok {
			return m.openSearchHit(h, o.ran)
		}
		return nil
	case "a", "ctrl+a":
		if h, ok := o.selected(); ok {
			return m.attachSearchHit(h)
		}
		return nil
	case "/", "i":
		o.editing = true
		return nil
	case "backspace":
		o.editing = true
		return o.updateEditing(m, k)
	}
	if k.Text != "" && k.Mod&^tea.ModShift == 0 { // typing refines the query
		o.editing = true
		return o.updateEditing(m, k)
	}
	return nil
}

func (o *searchOverlay) updateEditing(m *Model, k tea.KeyPressMsg) tea.Cmd {
	if o.jk.escape(m, k, &o.input) {
		m.overlay = nil
		return nil
	}
	switch k.String() {
	case "esc":
		m.overlay = nil
		return nil
	case "enter":
		o.editing = len(o.hits) == 0 && o.ran == strings.TrimSpace(o.input)
		if strings.TrimSpace(o.input) != o.ran || o.err != nil {
			o.editing = false
			return o.run(m)
		}
		o.editing = false
		return nil
	case "down", "ctrl+j", "ctrl+n", "tab":
		if len(o.hits) > 0 {
			o.editing = false
		}
		return nil
	case "backspace":
		if r := []rune(o.input); len(r) > 0 {
			o.input = string(r[:len(r)-1])
		}
	case "ctrl+w":
		t := strings.TrimRight(o.input, " ")
		if i := strings.LastIndexByte(t, ' '); i >= 0 {
			o.input = t[:i+1]
		} else {
			o.input = ""
		}
	case "ctrl+u":
		o.input = ""
	default:
		if k.Text == "" || k.Mod&^tea.ModShift != 0 {
			return nil
		}
		o.input += k.Text
	}
	o.typed++
	typed := o.typed
	return tea.Tick(searchDebounce, func(time.Time) tea.Msg { return searchDueMsg{typed: typed} })
}

func (o *searchOverlay) updateMsg(m *Model, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case searchDueMsg:
		q := strings.TrimSpace(o.input)
		if msg.typed == o.typed && len([]rune(q)) >= 3 && q != o.ran {
			return o.run(m) // live results while typing; the query keeps the keyboard
		}
	case tea.PasteMsg:
		o.input += strings.Join(strings.Fields(msg.Content), " ")
		o.editing = true
		o.typed++
		typed := o.typed
		return tea.Tick(searchDebounce, func(time.Time) tea.Msg { return searchDueMsg{typed: typed} })
	}
	return nil
}

// ---- opening a hit ----

func (m *Model) openSearchHit(h contracts.SearchHit, query string) tea.Cmd {
	switch h.Source {
	case contracts.SearchSourceDocument:
		if h.ProjectID != "" {
			m.overlay = nil
			pid, did := h.ProjectID, h.Ref
			m.curProject = pid
			m.view, m.back, m.projTab = viewProject, nil, tabDocs
			m.listFor("proj:" + pid + ":docs").sel = did
			return m.openDoc(did)
		}
	case contracts.SearchSourceArtifact:
		if h.ProjectID != "" {
			m.overlay = nil
			m.curProject, m.projTab, m.curArtifact, m.docScroll = h.ProjectID, tabArtifacts, h.Ref, 0
			m.view, m.back = viewProject, nil
			return m.push(viewArtifact)
		}
	}
	if h.SessionID == "" {
		return m.setToast(trunc(h.Source+": "+h.Provenance, 80), toneMuted)
	}
	jump := readerJump{run: h.RunID, seq: h.Seq, terms: search.MarkedTerms(h.Snippet)}
	if len(jump.terms) == 0 {
		jump.terms = search.Terms(query)
	}
	if h.Source != contracts.SearchSourceTranscript {
		jump = readerJump{terms: jump.terms}
	}
	return m.withSearchSession(h.SessionID, func(m *Model, s contracts.SessionView) tea.Cmd {
		m.overlay = nil
		m.reader = readerState{sid: s.Session.SessionID, at: jump}
		m.focus = 0
		return m.push(viewReader)
	})
}

func (m *Model) attachSearchHit(h contracts.SearchHit) tea.Cmd {
	if h.SessionID == "" {
		return m.setToast("a attaches sessions only", toneMuted)
	}
	return m.withSearchSession(h.SessionID, func(m *Model, s contracts.SessionView) tea.Cmd {
		m.overlay = nil
		m.focusSessionQuiet(s.Session.SessionID)
		return m.attach(s)
	})
}

// withSearchSession runs fn with the session's view, fetching it first
// when it is not in the session list (archived, or not loaded yet).
func (m *Model) withSearchSession(sid string, fn func(m *Model, s contracts.SessionView) tea.Cmd) tea.Cmd {
	if s, ok := m.session(sid); ok {
		return fn(m, s)
	}
	api := m.api
	return m.do(func(ctx context.Context) (any, error) {
		return api.GetSession(ctx, contracts.GetRequest{ID: sid})
	}, func(m *Model, val any, err error) tea.Cmd {
		if err != nil {
			return m.setToast("cannot open "+sid+": "+contracts.AsError(err).Message, toneWarn)
		}
		s, _ := val.(contracts.SessionView)
		m.loads["sess:"+sid] = &loadState{val: s, have: true, at: m.now()}
		return fn(m, s)
	})
}

// sessionOutsideList answers session lookups for sessions opened from
// search that the list does not hold (archived ones).
func (m *Model) sessionOutsideList(id string) (contracts.SessionView, bool) {
	s, st := get[contracts.SessionView](m, "sess:"+id)
	return s, st != nil && st.have
}

// ---- reader jump ----

// readerJump opens the reader at a search hit: the run's entries from a
// little before the hit, scrolled to the hit's entry with its match
// highlighted (n/N keep working).
type readerJump struct {
	run     string
	seq     int64
	terms   []string
	done    bool
	missing bool          // the hit's entry is not in the loaded window
	lines   map[int64]int // entry seq -> first rendered line (readerLines)
}

func (j readerJump) active() bool { return j.run != "" || len(j.terms) > 0 }

func (j readerJump) request(sid string) contracts.ReadSessionRequest {
	if j.run == "" || j.seq <= 0 {
		return contracts.ReadSessionRequest{SessionID: sid, Tail: true, Limit: 500}
	}
	return contracts.ReadSessionRequest{SessionID: sid, RunID: j.run, Cursor: itoa(int(max(0, j.seq-60))), Limit: 500}
}

func (m *Model) readerKey() string {
	j := m.reader.at
	if j.run == "" || j.seq <= 0 {
		return "transcript:" + m.reader.sid
	}
	return fmt.Sprintf("transcript:%s@%s#%d", m.reader.sid, j.run, max(0, j.seq-60))
}

// applyReaderJump positions the reader once the transcript has loaded: the
// match nearest the hit's entry becomes the current match.
func (m *Model) applyReaderJump(lines []string, h int) {
	j := &m.reader.at
	if !j.active() || j.done {
		return
	}
	if _, st := get[contracts.ReadSessionResponse](m, m.readerKey()); st == nil || !st.ok() {
		return
	}
	j.done = true
	if len(j.terms) == 0 {
		return
	}
	m.reader.query = j.terms[0]
	matches := m.readerMatches(lines)
	if len(matches) == 0 {
		j.missing = true
		return
	}
	target := 0
	if line, ok := j.lines[j.seq]; ok && j.seq > 0 {
		target = line
	} else if j.seq > 0 {
		j.missing = true
	}
	best := 0
	for i, l := range matches {
		if l >= target {
			best = i
			break
		}
		best = i
	}
	m.reader.cur = best
	m.reader.scroll = max(0, matches[best]-(h-4)/3)
}

// ---- view ----

func (o *searchOverlay) status(m *Model) string {
	switch {
	case o.running:
		return m.th.dim.Render("searching every machine…")
	case o.err != nil:
		return m.th.tone(toneWarn, trunc(contracts.AsError(o.err).Message, 60))
	case o.resp == nil:
		return ""
	}
	sessions := 0
	for _, r := range o.rows {
		if r.header && r.section == "" {
			sessions++
		}
	}
	s := plural(len(o.resp.Hits), "match", "matches")
	if sessions > 0 {
		s += " in " + plural(sessions, "session", "sessions")
	}
	return m.th.dim.Render(s + fmt.Sprintf(" · %d ms", o.resp.TookMS))
}

func (o *searchOverlay) view(m *Model, w, h int) []string {
	lw := w
	showPreview := w >= 110 && len(o.hits) > 0
	if showPreview {
		lw = min(110, w*58/100)
	}
	caret := ""
	if o.editing {
		caret = m.th.accent.Render("▏")
	}
	prompt := " " + m.th.key.Render("Search ›") + " " + o.input + caret
	if o.input == "" {
		prompt += m.th.dim.Render(trunc("every transcript on every machine, memory and docs", max(0, lw-14)))
	}
	right := o.status(m)
	if o.archived {
		right = m.th.dim.Render("incl. archived · ") + right
	}
	head := pad(prompt, max(0, lw-width(right)-1)) + right + " "
	out := []string{trunc(head, lw), m.th.faint.Render(strings.Repeat("─", lw))}
	if notes := o.notSearched(); len(notes) > 0 { // always in sight, above the results
		out = append(out, "  "+m.th.dim.Render("◌ "+trunc(strings.Join(notes, " · "), max(10, lw-6))), "")
	}
	body := o.listLines(m, lw)
	rows := h - len(out)
	// keep the selected row visible
	selLine := -1
	if len(o.hits) > 0 && !o.editing {
		selLine = o.lineOfRow(o.hits[o.idx])
	}
	if selLine >= 0 {
		if selLine < o.top {
			o.top = max(0, selLine-1)
		}
		if selLine >= o.top+rows {
			o.top = selLine - rows + 1
		}
	}
	o.top = max(0, min(o.top, max(0, len(body)-rows)))
	out = append(out, body[o.top:min(len(body), o.top+rows)]...)
	if !showPreview {
		return out
	}
	hit, _ := o.selected()
	prev := m.searchPreview(hit, o.resp, w-lw-3)
	return hjoin(fit(out, lw, h), lw, " "+m.th.faint.Render("│")+" ", fit(prev, w-lw-3, h), w-lw-3)
}

// lineOfRow maps a row index to its rendered line (headers after the first
// get a blank line above).
func (o *searchOverlay) lineOfRow(ri int) int {
	line := 0
	for i := 0; i < ri && i < len(o.rows); i++ {
		if o.rows[i].header && i > 0 {
			line++
		}
		line++
	}
	if ri < len(o.rows) && o.rows[ri].header && ri > 0 {
		line++
	}
	return line
}

func (o *searchOverlay) listLines(m *Model, w int) []string {
	if o.resp == nil {
		if o.running || o.err != nil {
			return nil
		}
		return []string{"", m.th.dim.Render("  Type words to find in every Pyre session, live or ended, on every machine."),
			m.th.dim.Render("  \"quoted phrase\" for exact · ctrl+t includes archived sessions · enter opens the transcript at the match")}
	}
	var out []string
	sel := -1
	if len(o.hits) > 0 && !o.editing {
		sel = o.hits[o.idx]
	}
	for i, r := range o.rows {
		if r.header {
			if i > 0 {
				out = append(out, "")
			}
			if r.section != "" {
				out = append(out, "  "+m.th.faint.Render(r.section))
				continue
			}
			h := r.hit
			meta := m.searchNodeName(h.NodeID, o.resp) + " · " + age(m.now(), h.At)
			if h.Archived {
				meta = "archived · " + meta
			}
			title := h.Title
			if title == "" {
				title = h.SessionID
			}
			tw := max(8, w-4-width(meta)-2)
			out = append(out, "  "+pad(m.th.title.Render(trunc(title, tw)), tw)+"  "+m.th.dim.Render(meta))
			continue
		}
		h := r.hit
		label := searchRole(h)
		row := m.cursor(i == sel) + "  " + m.th.dim.Render(pad(label, 7)) + m.markedSnippet(h.Snippet, max(10, w-13), styleIf(i == sel, m.th.selText))
		out = append(out, m.rowSel(i == sel, row, w))
	}
	if len(o.resp.Hits) == 0 {
		out = append(out, "", m.th.dim.Render("  no matches"))
	}
	return out
}

// notSearched lists machines whose results are missing or partial.
func (o *searchOverlay) notSearched() []string {
	if o.resp == nil {
		return nil
	}
	var notes []string
	for _, n := range o.resp.Nodes {
		switch n.State {
		case contracts.SearchNodeSearched:
			if n.Partial {
				notes = append(notes, n.Name+" still indexing")
			}
		case contracts.SearchNodeSkipped:
		case contracts.SearchNodeOffline:
			notes = append(notes, n.Name+" not searched (offline)")
		default:
			notes = append(notes, n.Name+" not searched ("+n.State+")")
		}
	}
	return notes
}

func searchRole(h contracts.SearchHit) string {
	if h.Source != contracts.SearchSourceTranscript {
		switch h.Source {
		case contracts.SearchSourceDocument:
			return "doc"
		case contracts.SearchSourceArtifact:
			return "artifact"
		}
		return h.Source
	}
	switch h.Kind {
	case "user":
		return "you"
	case "assistant":
		return "agent"
	case "message":
		return "message"
	}
	return h.Kind
}

func (m *Model) searchNodeName(id string, resp *contracts.SearchResponse) string {
	if resp != nil {
		for _, n := range resp.Nodes {
			if n.NodeID == id && n.Name != "" {
				return n.Name
			}
		}
	}
	if n := m.node(id); n != nil {
		return n.Name
	}
	return id
}

// markedSnippet renders a snippet with its «matches» highlighted, cut to w
// cells (the markers take no space).
func (m *Model) markedSnippet(s string, w int, base lipgloss.Style) string {
	var b strings.Builder
	in := false
	used := 0
	seg := []rune{}
	flush := func() {
		if len(seg) == 0 {
			return
		}
		if in {
			b.WriteString(m.th.match.Render(string(seg)))
		} else {
			b.WriteString(base.Render(string(seg)))
		}
		seg = seg[:0]
	}
	rs := []rune(strings.Join(strings.Fields(s), " "))
	open, closeM := []rune(contracts.SearchMarkOpen)[0], []rune(contracts.SearchMarkClose)[0]
	for i, r := range rs {
		switch r {
		case open:
			flush()
			in = true
			continue
		case closeM:
			flush()
			in = false
			continue
		}
		cw := width(string(r))
		if used+cw > w-1 && i < len(rs)-1 {
			seg = append(seg, '…')
			flush()
			return b.String()
		}
		seg = append(seg, r)
		used += cw
	}
	flush()
	return b.String()
}

func (m *Model) searchPreview(h contracts.SearchHit, resp *contracts.SearchResponse, w int) []string {
	if h.Source == "" {
		return nil
	}
	title := h.Title
	if title == "" {
		title = h.SessionID
	}
	out := []string{m.th.title.Render(trunc(title, w))}
	where := strings.TrimSuffix(h.Provenance, " transcript")
	if h.Source == contracts.SearchSourceTranscript {
		where = titleCase(where) + " on " + m.searchNodeName(h.NodeID, resp)
		if s, ok := m.session(h.SessionID); ok && s.Run != nil {
			where = m.th.status(sessionStatus(s, m.nodeOf(s))) + m.th.dim.Render(" · ") + m.th.dim.Render(where)
		} else {
			where = m.th.dim.Render(where)
		}
	} else {
		where = m.th.dim.Render(trunc(where, w))
	}
	out = append(out, where, m.th.dim.Render(trunc(searchRole(h)+" · "+ago(m.now(), h.At)+turnLabel(h), w)), "")
	for _, l := range wrap(search.PlainSnippet(h.Snippet), min(w, textMax)) {
		out = append(out, m.markTerms(l, search.MarkedTerms(h.Snippet)))
	}
	out = append(out, "")
	if h.SessionID != "" {
		out = append(out, m.th.dim.Render(trunc("enter opens the transcript at this match · a attaches", w)))
	}
	return out
}

func turnLabel(h contracts.SearchHit) string {
	if h.Source == contracts.SearchSourceTranscript && h.Seq > 0 {
		return fmt.Sprintf(" · entry %d", h.Seq)
	}
	return ""
}

// markTerms highlights each term in an already wrapped plain line.
func (m *Model) markTerms(line string, terms []string) string {
	for _, t := range terms {
		line = m.markQuery(line, t, false)
	}
	return line
}
