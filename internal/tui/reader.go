package tui

import (
	"context"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/jhh3/pyre/internal/contracts"
)

// readerState is the full-screen transcript reader (r on a session):
// comfortable line width, / incremental search with n/N.
type readerState struct {
	sid       string
	scroll    int
	searching bool
	input     string
	query     string
	matches   []int // line indexes
	cur       int
	jk        jkState
	at        readerJump // open at a search hit (search.go); zero: tail
}

func (m *Model) openReader(s contracts.SessionView) tea.Cmd {
	m.reader = readerState{sid: s.Session.SessionID}
	m.focus = 0
	return m.push(viewReader)
}

func (m *Model) loadTranscript(force bool) tea.Cmd {
	sid := m.reader.sid
	s, ok := m.session(sid)
	if !ok || s.Run == nil {
		return nil
	}
	api := m.api
	req := m.reader.at.request(sid)
	fn := func(ctx context.Context) (any, error) {
		return api.ReadSession(ctx, req)
	}
	if force {
		return m.reload(m.readerKey(), fn)
	}
	return m.ensure(m.readerKey(), 5*time.Second, fn)
}

func (m *Model) startReaderSearch() tea.Cmd {
	m.reader.searching = true
	m.reader.input = m.reader.query
	return nil
}

// readerLines renders the transcript at width w.
func (m *Model) readerLines(w int) []string {
	resp, st := get[contracts.ReadSessionResponse](m, m.readerKey())
	if !st.ok() {
		return []string{m.loadingOr(st, "no transcript")}
	}
	var out []string
	if resp.Gap {
		out = append(out, m.th.dim.Render("… earlier activity missing (gap)"), "")
	}
	es := shownEntries(resp.Entries)
	m.reader.at.lines = map[int64]int{}
	for i, e := range es {
		m.reader.at.lines[e.Seq] = len(out)
		out = append(out, m.transcriptEntry(e, w)...)
		if i+1 < len(es) && e.Kind == "tool" && es[i+1].Kind == "tool" {
			continue // consecutive tool calls read as one block
		}
		out = append(out, "")
	}
	if screen := compactScreen(resp.Screen); len(screen) > 0 {
		for i := range screen {
			screen[i] = m.th.term.Render(clip(screen[i], w-4))
		}
		out = append(out, m.frame("terminal now", screen, w)...)
	}
	if len(resp.Entries) == 0 && resp.Screen == "" {
		out = append(out, m.th.dim.Render("No activity captured yet."))
	}
	return out
}

func (m *Model) readerMatches(lines []string) []int {
	q := strings.ToLower(m.reader.query)
	if q == "" {
		return nil
	}
	var out []int
	for i, l := range lines {
		if strings.Contains(strings.ToLower(ansiStrip(l)), q) {
			out = append(out, i)
		}
	}
	return out
}

func (m *Model) viewReader(w, h int) []string {
	s, ok := m.session(m.reader.sid)
	if !ok {
		return []string{m.th.dim.Render("session not loaded")}
	}
	tw := min(w-2, textMax+8)
	margin := max(0, (w-tw)/2)
	if w < 120 {
		margin = 1
	}
	stt := sessionStatus(s, m.nodeOf(s))
	head := m.th.title.Render(iconTitle(s)) + m.th.dim.Render("  ·  ") + m.th.status(stt) + m.th.dim.Render(" · "+titleCase(string(s.Session.Harness))+" on "+s.NodeName)
	lines := m.readerLines(tw)
	m.applyReaderJump(lines, h)
	m.reader.matches = m.readerMatches(lines)
	info := ""
	switch {
	case m.reader.searching:
		info = m.th.key.Render("/") + m.reader.input + m.th.accent.Render("▏")
	case m.reader.query != "":
		n := len(m.reader.matches)
		pos := 0
		if n > 0 {
			pos = m.reader.cur%n + 1
		}
		info = m.th.dim.Render("/"+m.reader.query+"  ") + m.th.accent.Render(itoa(pos)+"/"+itoa(n)) + m.th.dim.Render("  n/N")
	}
	out := []string{strings.Repeat(" ", margin) + trunc(head, tw), strings.Repeat(" ", margin) + m.th.faint.Render(strings.Repeat("─", tw))}
	bodyH := h - len(out) - 1
	if l, ok := m.digestReaderJump(tw); ok {
		m.reader.scroll = l
	}
	maxScroll := max(0, len(lines)-bodyH)
	m.reader.scroll = max(0, min(m.reader.scroll, maxScroll))
	q := m.reader.query
	if m.reader.searching {
		q = m.reader.input
	}
	for i := m.reader.scroll; i < min(len(lines), m.reader.scroll+bodyH); i++ {
		l := lines[i]
		if q != "" {
			cur := len(m.reader.matches) > 0 && m.reader.cur >= 0 && m.reader.matches[m.reader.cur%len(m.reader.matches)] == i
			l = m.markQuery(l, q, cur)
		}
		out = append(out, strings.Repeat(" ", margin)+l)
	}
	out = fit(out, w, h-1)
	pct := 100
	if maxScroll > 0 {
		pct = m.reader.scroll * 100 / maxScroll
	}
	foot := pad(" "+info, w-8) + m.th.dim.Render(padLeft(itoa(pct)+"%", 6)) + "  "
	return append(out, foot)
}

// markQuery highlights case-insensitive occurrences of q in a rendered
// line, keeping the line's own styling around and outside the matches (role
// colours, frame borders). The current match (n/N) is shown reversed.
func (m *Model) markQuery(line, q string, current bool) string {
	plain := ansiStrip(line)
	lp, lq := []rune(strings.ToLower(plain)), []rune(strings.ToLower(q))
	if len(lq) == 0 || len(lp) != len([]rune(plain)) {
		return line
	}
	// cell offset of each rune (wide runes take two cells)
	cells := make([]int, len(lp)+1)
	for i, r := range []rune(plain) {
		cells[i+1] = cells[i] + ansi.StringWidth(string(r))
	}
	st := m.th.match
	if current {
		st = st.Reverse(true)
	}
	var rs []lipgloss.Range
	for i := 0; i+len(lq) <= len(lp); {
		if string(lp[i:i+len(lq)]) == string(lq) {
			rs = append(rs, lipgloss.NewRange(cells[i], cells[i+len(lq)], st))
			i += len(lq)
			continue
		}
		i++
	}
	if len(rs) == 0 {
		return line
	}
	return lipgloss.StyleRanges(line, rs...)
}

func (m *Model) readerJump(d int) tea.Cmd {
	n := len(m.reader.matches)
	if n == 0 {
		return m.setToast("no matches for /"+m.reader.query, toneMuted)
	}
	m.reader.cur = (m.reader.cur + d + n) % n
	m.reader.scroll = max(0, m.reader.matches[m.reader.cur]-(m.height-8)/3)
	return nil
}

func (m *Model) keyReader(k tea.KeyPressMsg) tea.Cmd {
	switch k.String() {
	case "down":
		m.reader.scroll++
	case "up":
		m.reader.scroll = max(0, m.reader.scroll-1)
	case "pgdown":
		m.reader.scroll += m.halfPage()
	case "pgup":
		m.reader.scroll = max(0, m.reader.scroll-m.halfPage())
	case "home":
		m.reader.scroll = 0
	case "end":
		m.reader.scroll = 1 << 20
	case "esc":
		if m.reader.query != "" {
			m.reader.query = ""
			return nil
		}
		return m.goBack()
	case "r":
		return m.loadTranscript(true)
	}
	return nil
}

func (m *Model) keyReaderSearch(k tea.KeyPressMsg) tea.Cmd {
	if m.reader.jk.escape(m, k, &m.reader.input) {
		m.reader.searching = false
		return nil
	}
	switch k.String() {
	case "esc":
		m.reader.searching = false
		return nil
	case "enter":
		m.reader.searching = false
		m.reader.query = m.reader.input
		m.reader.cur = -1
		m.reader.matches = m.readerMatches(m.readerLines(min(m.width-2, textMax+8)))
		return m.readerJump(1)
	case "backspace":
		if r := []rune(m.reader.input); len(r) > 0 {
			m.reader.input = string(r[:len(r)-1])
		}
	default:
		if k.Text != "" && k.Mod&^tea.ModShift == 0 {
			m.reader.input += k.Text
		}
	}
	// incremental: scroll to the first match as you type
	m.reader.query = m.reader.input
	m.reader.matches = m.readerMatches(m.readerLines(min(m.width-2, textMax+8)))
	if len(m.reader.matches) > 0 {
		m.reader.cur = 0
		m.reader.scroll = max(0, m.reader.matches[0]-(m.height-8)/3)
	}
	return nil
}

func (m *Model) readerHints() []hint {
	return []hint{{"j/k", "scroll"}, {"/", "search"}, {"n/N", "match"}, {"gg/G", "top/end"}}
}

// ---- answer the selected session's pending request (a) ----

type answerOverlay struct {
	a contracts.AttentionItem
}

func (m *Model) pendingRequest(sid string) (contracts.AttentionItem, bool) {
	for _, a := range m.openItems() {
		if a.SessionID == sid && isAnswerable(a) {
			return a, true
		}
	}
	return contracts.AttentionItem{}, false
}

func (m *Model) answerSelected() tea.Cmd {
	s, ok := m.selectedSession()
	if !ok {
		return nil
	}
	a, ok := m.pendingRequest(s.Session.SessionID)
	if !ok {
		if s.Run != nil && s.Run.State == contracts.RunWaitingApproval {
			return m.setToast(trunc(s.Session.Title, 40)+" is waiting on a prompt in its own terminal — enter types into it to answer", toneWarn)
		}
		return m.setToast(trunc(s.Session.Title, 40)+" has no pending request", toneMuted)
	}
	m.overlay = &answerOverlay{a: a}
	return m.loadApproval(a, false)
}

func (m *Model) answerSelectedWith(n int) tea.Cmd {
	cmd := m.answerSelected()
	ov, ok := m.overlay.(*answerOverlay)
	if !ok {
		return cmd
	}
	ap, st := get[contracts.Approval](m, "approval:"+ov.a.AttentionID)
	if !st.ok() {
		return tea.Batch(cmd, m.setToast("request loading — press "+itoa(n)+" again", toneMuted))
	}
	return ov.choose(m, ap, n)
}

func (o *answerOverlay) choose(m *Model, ap contracts.Approval, n int) tea.Cmd {
	if ap.State != contracts.ApprovalPending {
		return m.setToast("request is "+string(ap.State)+"; nothing to answer", toneMuted)
	}
	if n < 1 || n > len(ap.Options) {
		return nil
	}
	return m.answer(o.a, ap, ap.Options[n-1].ID, "")
}

func (o *answerOverlay) hintsFor(m *Model) []hint {
	if ap, st := get[contracts.Approval](m, "approval:"+o.a.AttentionID); st.ok() && ap.State != contracts.ApprovalPending {
		return []hint{{"esc", "close"}}
	}
	return []hint{{"1-9", "answer"}, {"t", "text"}, {"esc", "close"}}
}

func (o *answerOverlay) update(m *Model, k tea.KeyPressMsg) tea.Cmd {
	key := k.String()
	ap, st := get[contracts.Approval](m, "approval:"+o.a.AttentionID)
	switch {
	case key == "esc" || key == "q":
		m.overlay = nil
		return nil
	case key == "t":
		if !st.ok() || !ap.AllowText || ap.State != contracts.ApprovalPending {
			return m.setToast("free-text answer not available for this request", toneWarn)
		}
		a := o.a
		return m.openTextPrompt("Answer: "+ap.Title, "your answer", func(m *Model, text string) tea.Cmd {
			return m.answer(a, ap, "", text)
		})
	case len(key) == 1 && key[0] >= '1' && key[0] <= '9':
		if !st.ok() {
			return m.setToast("request not loaded yet", toneMuted)
		}
		return o.choose(m, ap, int(key[0]-'0'))
	}
	return nil
}

func (o *answerOverlay) view(m *Model, w, h int) []string {
	bw := min(84, w-2)
	inner := bw - 4
	lines := []string{m.th.dim.Render(trunc(m.sessionTitle(o.a.SessionID)+" · "+o.a.Title, inner)), ""}
	lines = append(lines, m.approvalLines(o.a, inner)...)
	if msg := m.answerResult[o.a.AttentionID]; msg != "" {
		lines = append(lines, "")
		lines = append(lines, wrap(msg, inner)...)
	}
	return m.box("Answer request", lines, bw, w, h)
}
