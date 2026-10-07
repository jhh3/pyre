package tui

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/digest"
)

// "While you were away" (W-digest): on launch the TUI builds the digest
// since this client's last-seen mark (internal/digest, the same mark
// `pyre digest` uses, kept in digest-state.json beside the TUI state file)
// and, when anything happened, says so in a toast. :digest (<leader>w)
// opens it as its own view (the Inbox it used to sit on is gone, W-attn):
// one line per agent, needs you → done → still working. Selecting a line
// shows what changed in the detail pane; enter follows the line's deep link
// (the session that needs you, else the transcript at that moment). esc
// marks everything seen and goes back; quitting after the digest was shown
// marks it seen too.

type digestState struct {
	path      string // digest-state.json ("" = no digest, as in tests without a state file)
	loading   bool
	loaded    bool
	err       error
	d         digest.Digest
	hidden    bool // dismissed (or nothing to show)
	shown     bool // rendered at least once (quitting then marks it seen)
	list      listState
	readerSid string // pending jump: open the reader at this transcript moment
	readerSeq int64
}

type digestLoadedMsg struct {
	d   digest.Digest
	err error
}

// digestInit starts the launch digest (Init hook).
func (m *Model) digestInit() tea.Cmd {
	p := m.opts.StateFile
	if p == "" || p == "none" {
		return nil
	}
	m.dg.path = filepath.Join(filepath.Dir(p), "digest-state.json")
	opts, err := digest.Cutoff("last", m.now(), m.dg.path)
	if err != nil {
		return nil
	}
	opts.CallTimeout = 3 * time.Second
	m.dg.loading = true
	api, parent := m.api, m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, 20*time.Second)
		defer cancel()
		d, err := digest.Fetch(ctx, api, opts)
		return resultMsg{val: digestLoadedMsg{d: d, err: err}, then: func(m *Model, v any, _ error) tea.Cmd {
			return m.onDigest(v.(digestLoadedMsg))
		}}
	}
}

func (m *Model) onDigest(msg digestLoadedMsg) tea.Cmd {
	m.dg.loading = false
	if msg.err != nil {
		m.dg.err = msg.err
		m.dg.hidden = true
		return nil
	}
	m.dg.loaded, m.dg.d = true, msg.d
	ids := make([]string, len(msg.d.Agents))
	for i, a := range msg.d.Agents {
		ids[i] = a.SessionID
	}
	m.dg.list.setIDs(ids)
	if msg.d.Empty() {
		m.dg.hidden = true
		return nil
	}
	if m.view == viewDigest {
		return m.digestPreview()
	}
	if m.showTips || m.toast != nil {
		return nil // first launch (the tips bar speaks) or a warning on screen: :digest still shows it
	}
	n := msg.d.Count(digest.RankNeedsYou)
	text := "While you were away: " + plural(len(msg.d.Agents), "agent", "agents")
	if n > 0 {
		text += ", " + itoa(n) + " need you"
	}
	tok := m.toastSeq + 1
	m.toastSeq = tok
	m.toast = &toast{text: text + " — :digest shows it", tone: toneAccent, token: tok}
	return tea.Tick(12*time.Second, func(time.Time) tea.Msg { return clearToastMsg{token: tok} })
}

// digestVisible reports whether the digest view shows a digest.
func (m *Model) digestVisible() bool {
	return m.view == viewDigest && m.dg.loaded && len(m.dg.d.Agents)+len(m.dg.d.Other) > 0
}

// digestFocused reports whether a digest line holds the cursor.
func (m *Model) digestFocused() bool {
	return m.digestVisible() && len(m.dg.list.ids) > 0
}

// openDigest shows the digest view (:digest, <leader>w), also after it was
// dismissed: asking for it is looking at it again.
func (m *Model) openDigest() tea.Cmd {
	switch {
	case m.dg.path == "" && !m.dg.loaded:
		return m.setToast("no digest here (no state file); pyre digest --since 12h prints one", toneMuted)
	case m.dg.loaded && m.dg.d.Empty():
		return m.setToast("nothing happened since you last looked — pyre digest --since 12h shows more", toneMuted)
	}
	m.dg.hidden = false
	m.focus = 0
	if !m.view.topLevel() {
		m.view, m.back = viewSessions, nil
	}
	return m.push(viewDigest)
}

// viewDigest is the digest's own view: the panel beside the selected line's
// detail (and, wide, what its session is doing now).
func (m *Model) viewDigest(w, h int) []string {
	if !m.digestVisible() {
		msg := "loading what happened since you last looked…"
		switch {
		case m.dg.err != nil:
			msg = "✗ digest unavailable: " + errText(m.dg.err)
		case m.dg.loaded:
			msg = "Nothing happened since you last looked."
		}
		return []string{"", "  " + m.th.dim.Render(trunc(msg, w-2))}
	}
	m.dg.shown = true
	if m.toast != nil && strings.HasPrefix(m.toast.text, "While you were away") {
		m.toast = nil // the launch toast pointed here
	}
	// The panel spans the width on top (its rows carry a facts column);
	// the selected line's detail is below it, beside (wide) what its
	// session is doing now.
	panel := m.viewDigestPanel(w, max(6, h/2))
	rest := h - len(panel)
	if rest < 3 {
		return fit(panel, w, h)
	}
	detail := func(w, h int) []string { return m.digestDetail(w) }
	if w >= wideMin {
		dw := w * 55 / 100
		rw := w - dw - 3
		return append(panel, hjoin(fit(m.scrolled(detail, dw, rest), dw, rest), dw, m.paneSep(), fit(m.digestRelated(rw, rest), rw, rest), rw)...)
	}
	return append(panel, fit(m.scrolled(detail, w, rest), w, rest)...)
}

func (m *Model) digestAgent() (digest.Agent, bool) {
	for _, a := range m.dg.d.Agents {
		if a.SessionID == m.dg.list.sel {
			return a, true
		}
	}
	return digest.Agent{}, false
}

// digestDismiss marks everything seen and leaves the view (esc).
func (m *Model) digestDismiss() tea.Cmd {
	m.dg.hidden = true
	return tea.Batch(m.digestMark(), m.setToast("marked seen — :digest or pyre digest --since 12h shows it again", toneMuted), m.goBack())
}

// digestMark records the last-seen mark in the background.
func (m *Model) digestMark() tea.Cmd {
	path, ls := m.dg.path, m.digestSeenMark()
	if path == "" || ls.Cursor <= 0 {
		return nil
	}
	return func() tea.Msg {
		_ = digest.SaveLastSeen(path, ls)
		return nil
	}
}

func (m *Model) digestSeenMark() digest.LastSeen {
	return digest.LastSeen{Cursor: max(m.dg.d.HeadCursor, m.ev.cursor), At: m.now()}
}

// digestClose marks the digest seen on quit when the panel was shown
// (Close hook). A digest never looked at stays for the next launch.
func (m *Model) digestClose() {
	if m.dg.path == "" || !m.dg.loaded || !m.dg.shown || m.dg.hidden {
		return
	}
	_ = digest.SaveLastSeen(m.dg.path, m.digestSeenMark())
}

// viewDigestPanel renders the header, one row per agent (scrolling when
// there are many) and a quiet footer line, then a blank separator.
func (m *Model) viewDigestPanel(w, h int) []string {
	d := m.dg.d
	now := m.now()
	head := " " + m.th.accent.Render("While you were away")
	meta := ""
	if d.SinceLabel == "last seen" {
		meta = "since " + d.Since.In(time.Local).Format("15:04") + " (" + ago(now, d.Since) + ")"
	} else if d.SinceLabel != "" {
		meta = d.SinceLabel
	}
	cnt := plural(len(d.Agents), "agent", "agents")
	if n := d.Count(digest.RankNeedsYou); n > 0 {
		cnt += ", " + m.th.tone(toneWarn, itoa(n)+" need you")
	}
	head += m.th.dim.Render(" · "+meta+" · ") + m.th.dim.Render(cnt)
	hide := m.hintSeg(hint{"esc", "mark seen"})
	if width(head)+width(hide)+3 > w {
		head = trunc(head, max(0, w-width(hide)-3))
	}
	out := []string{head + strings.Repeat(" ", max(1, w-width(head)-width(hide)-1)) + hide}

	rows := min(len(d.Agents), max(3, h-3))
	start, end := m.dg.list.window(rows)
	titleW := 12
	for _, a := range d.Agents {
		titleW = max(titleW, width(a.Title))
	}
	titleW = min(titleW, 26, max(12, w/5))
	ageW := 7
	factW := 0
	if w >= 110 {
		factW = min(40, w/4)
	}
	sumW := max(10, w-2-2-titleW-2-factW-ageW-3)
	for i := start; i < end; i++ {
		a := d.Agents[i]
		sel := i == m.dg.list.idx
		glyph := m.th.tone(digestTone(a), a.Glyph())
		title := m.titleCell(sel, trunc(a.Title, titleW), titleW)
		sum := trunc(a.Summary, sumW)
		switch {
		case a.Rank == digest.RankNeedsYou && len(a.NeedsYou) > 0:
			sum = m.th.tone(toneWarn, sum)
		case a.Rank == digest.RankNeedsYou:
			sum = m.th.tone(toneBad, sum)
		case sel:
			sum = m.th.selText.Render(sum)
		}
		row := m.cursor(sel) + glyph + " " + title + gap2 + pad(sum, sumW)
		if factW > 0 {
			f := compactFacts(a, factW)
			if a.Rank == digest.RankNeedsYou || f == "" {
				f = a.Where()
			}
			row += " " + m.th.dim.Render(pad(trunc(f, factW), factW))
		}
		row += " " + m.th.dim.Render(padLeft(digestAge(now, a.LastActivity), ageW))
		out = append(out, m.markRow(&m.dg.list, i, sel, row, w))
	}
	var tail []string
	if more := len(d.Agents) - (end - start); more > 0 {
		tail = append(tail, plural(more, "more agent", "more agents")+" (j/k)")
	}
	if d.Quiet > 0 {
		tail = append(tail, plural(d.Quiet, "quiet session", "quiet sessions"))
	}
	for _, r := range d.Other {
		tail = append(tail, m.th.tone(toneWarn, "! ")+m.th.dim.Render(m.humanIDs(r.Title)))
	}
	if d.Truncated {
		tail = append(tail, "older history not read")
	}
	if len(tail) > 0 {
		out = append(out, "    "+trunc(joinStyled(m, tail), w-4))
	}
	return append(out, "")
}

// compactFacts is the panel's facts column: turns, files and published
// results, as many as fit in w.
func compactFacts(a digest.Agent, w int) string {
	var parts []string
	add := func(n int, one, many string) {
		if n <= 0 {
			return
		}
		p := plural(n, one, many)
		if one == "turn" && a.Partial {
			p = itoa(n) + "+ " + many
		}
		if s := strings.Join(append(parts, p), " · "); width(s) <= w {
			parts = append(parts, p)
		}
	}
	add(a.Turns, "turn", "turns")
	add(len(a.Files), "file", "files")
	add(len(a.Artifacts), "artifact", "artifacts")
	add(len(a.Docs), "doc", "docs")
	add(len(a.Messages), "message", "messages")
	return strings.Join(parts, " · ")
}

func joinStyled(m *Model, parts []string) string {
	for i, p := range parts {
		if !strings.Contains(p, "\x1b[") {
			parts[i] = m.th.faint.Render(p)
		}
	}
	return strings.Join(parts, m.th.faint.Render(" · "))
}

func digestTone(a digest.Agent) tone {
	switch {
	case a.Rank == digest.RankNeedsYou && len(a.NeedsYou) == 0:
		return toneBad
	case a.Rank == digest.RankNeedsYou:
		return toneWarn
	case a.Rank == digest.RankWorking:
		return toneGood
	}
	return toneMuted
}

func digestAge(now, t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return age(now, t)
}

// digestDetail is the digest's detail pane for the selected line:
// the whole summary, what changed and where each line leads.
func (m *Model) digestDetail(w int) []string {
	a, ok := m.digestAgent()
	if !ok {
		return []string{"", m.th.dim.Render("Nothing selected")}
	}
	now := m.now()
	tw := min(w, textMax)
	lw := 11
	meta := m.th.tone(digestTone(a), a.Glyph()+" "+a.Word()) + m.th.dim.Render(" · "+a.Where()+" · "+ago(now, a.LastActivity))
	out := []string{m.th.title.Render(trunc(a.Title, w)), trunc(meta, w), ""}
	style := func(s ...string) string { return strings.Join(s, " ") }
	if a.Rank == digest.RankNeedsYou && len(a.NeedsYou) == 0 {
		style = func(s ...string) string { return m.th.tone(toneBad, strings.Join(s, " ")) }
	}
	for _, l := range wrap(m.humanIDs(a.Summary), tw) {
		out = append(out, style(l))
	}
	out = append(out, "")
	var act []string
	if a.Turns > 0 {
		act = append(act, plural(a.Turns, "turn", "turns"))
	}
	if a.Tools > 0 {
		act = append(act, plural(a.Tools, "tool call", "tool calls"))
	}
	if a.Replies > 0 {
		act = append(act, plural(a.Replies, "reply", "replies"))
	}
	if len(act) > 0 {
		f := strings.Join(act, " · ")
		if a.Partial {
			f += m.th.dim.Render(" (at least; older entries not read)")
		}
		out = append(out, m.kvFit("Activity", f, lw, w)...)
	}
	for i, r := range a.NeedsYou {
		lbl := ""
		if i == 0 {
			lbl = "Needs you"
		}
		out = append(out, m.kvl(lbl, m.th.tone(toneWarn, trunc(m.humanIDs(r.Title), w-lw)), lw))
	}
	if a.Blocked != "" {
		out = append(out, m.kvWrap("Blocked", upper1(a.Blocked), func(s ...string) string { return m.th.tone(toneBad, strings.Join(s, " ")) }, lw, w)...)
	}
	list := func(label string, items []string) {
		for i, it := range items {
			if i == 6 {
				out = append(out, m.kvl("", m.th.dim.Render("+"+itoa(len(items)-6)+" more"), lw))
				return
			}
			l := ""
			if i == 0 {
				l = label
			}
			out = append(out, m.kvl(l, trunc(it, w-lw), lw))
		}
	}
	list("Files", a.Files)
	refs := func(rs []digest.Ref) []string {
		var s []string
		for _, r := range rs {
			t := m.humanIDs(r.Title)
			switch r.Direction {
			case "sent":
				t = "→ " + t
			case "received":
				t = "← " + t
			}
			s = append(s, t)
		}
		return s
	}
	list("Artifacts", refs(a.Artifacts))
	list("Docs", refs(a.Docs))
	list("Notes", refs(a.Notes))
	list("Messages", refs(a.Messages))
	out = append(out, "", m.th.key.Render("enter")+" "+m.th.dim.Render(m.digestEnterLabel(a)))
	out = append(out, m.th.faint.Render(trunc(a.Link, w)))
	return out
}

func upper1(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func (m *Model) digestEnterLabel(a digest.Agent) string {
	l, _ := digest.ParseLink(a.Link)
	switch {
	case a.SummarySource == "attention" && len(a.NeedsYou) > 0 && m.digestLiveInList(a):
		return "open the session (a answers)"
	case l.Kind == contracts.KindSession && l.Seq > 0:
		return "open the transcript at this moment"
	case l.Kind == contracts.KindSession && m.digestLiveInList(a):
		return "open the session"
	case l.Kind == contracts.KindSession:
		return "open the transcript"
	}
	return "open what needs you"
}

// digestLiveInList reports whether the Sessions list shows the agent's
// session (ended sessions are hidden unless shown with .).
func (m *Model) digestLiveInList(a digest.Agent) bool {
	s, ok := m.session(a.SessionID)
	return ok && (m.showEnded || !sessionEnded(s))
}

// digestRelated is the wide digest's right column for a line: what
// the agent's session is doing now.
func (m *Model) digestRelated(w, h int) []string {
	a, _ := m.digestAgent()
	s, ok := m.session(a.SessionID)
	if !ok {
		return []string{m.sectionTitle("Context", w), m.th.dim.Render(trunc("Session "+a.Title+" is not listed (archived or ended).", w))}
	}
	stt := sessionStatus(s, m.nodeOf(s))
	out := []string{m.th.bold.Render(trunc(s.Session.Title, w)), trunc(m.th.status(stt)+m.th.dim.Render(" · "+titleCase(string(s.Session.Harness))+" on "+s.NodeName), w), ""}
	return append(out, m.activityLines(s, w, h-len(out))...)
}

// digestPreview loads the related session's activity for the selected line.
func (m *Model) digestPreview() tea.Cmd {
	a, ok := m.digestAgent()
	if !ok || m.width < wideMin {
		return nil
	}
	if s, ok := m.session(a.SessionID); ok {
		return m.loadPreviewFor(s, false)
	}
	return nil
}

func (m *Model) digestHints() []hint {
	return []hint{{"↑↓", "select"}, {"enter", "open"}, {"r", "transcript"}, {"esc", "mark seen"}}
}

// keyDigest handles keys in the digest view.
func (m *Model) keyDigest(k tea.KeyPressMsg) tea.Cmd {
	key := k.String()
	if key == "esc" {
		if m.digestVisible() {
			return m.digestDismiss()
		}
		return m.goBack()
	}
	if !m.digestVisible() || len(m.dg.list.ids) == 0 {
		return nil
	}
	cmd, _ := m.digestKey(key)
	return cmd
}

// digestKey moves through and opens digest lines.
func (m *Model) digestKey(key string) (tea.Cmd, bool) {
	switch key {
	case "down":
		m.dg.list.move(1)
	case "up":
		m.dg.list.move(-1)
	case "pgdown":
		m.dg.list.move(m.halfPage())
	case "pgup":
		m.dg.list.move(-m.halfPage())
	case "home":
		m.dg.list.home()
	case "end":
		m.dg.list.end()
	case "enter":
		return m.digestOpen(), true
	case "r":
		a, _ := m.digestAgent()
		return m.digestOpenMoment(a, true), true
	default:
		return nil, false
	}
	m.detailOff = 0
	return m.digestPreview(), true
}

// digestClick handles a click on a digest row (mouse hook): the first click
// selects it, a click on the selected row opens it.
func (m *Model) digestClick(z zoneDef) (tea.Cmd, bool) {
	if z.kind != zoneRow || z.list != &m.dg.list || z.idx < 0 || z.idx >= len(m.dg.list.ids) {
		return nil, false
	}
	m.focus = 0
	if m.dg.list.idx == z.idx {
		return m.digestOpen(), true
	}
	m.dg.list.idx, m.dg.list.sel = z.idx, m.dg.list.ids[z.idx]
	m.detailOff = 0
	return m.digestPreview(), true
}

// digestOpen follows the selected line's deep link.
func (m *Model) digestOpen() tea.Cmd {
	a, ok := m.digestAgent()
	if !ok {
		return nil
	}
	l, _ := digest.ParseLink(a.Link)
	if a.SummarySource == "attention" && len(a.NeedsYou) > 0 && m.digestLiveInList(a) {
		// The line is about something that needs you: the session, where
		// a answers it.
		return m.focusSession(a.SessionID)
	}
	if l.Kind == contracts.KindSession && l.Seq == 0 && m.digestLiveInList(a) {
		return m.focusSession(a.SessionID) // its live terminal
	}
	return m.digestOpenMoment(a, true)
}

// digestOpenMoment opens the transcript reader scrolled to the line's
// moment (the entry behind its summary); without one, the session itself.
func (m *Model) digestOpenMoment(a digest.Agent, always bool) tea.Cmd {
	s, ok := m.session(a.SessionID)
	if !ok {
		return m.setToast(a.Title+" is archived or no longer listed; pyre read "+a.SessionID+" shows its transcript", toneMuted)
	}
	if a.Seq == 0 && !always {
		return m.focusSession(a.SessionID)
	}
	m.dg.readerSid, m.dg.readerSeq = a.SessionID, a.Seq
	cmd := m.openReader(s)
	if a.Seq > 0 {
		note := "at the agent's last message"
		if a.SummarySource == "assistant" {
			note = "at the moment of: " + trunc(a.Summary, 70)
		}
		return tea.Batch(cmd, m.setToast(note, toneMuted))
	}
	return cmd
}

// digestReaderJump returns the reader line of the pending digest moment
// once the transcript is loaded (viewReader hook), then forgets it.
func (m *Model) digestReaderJump(tw int) (int, bool) {
	if m.dg.readerSid == "" || m.dg.readerSid != m.reader.sid {
		return 0, false
	}
	resp, st := get[contracts.ReadSessionResponse](m, "transcript:"+m.reader.sid)
	if !st.ok() {
		return 0, false
	}
	seq := m.dg.readerSeq
	m.dg.readerSid, m.dg.readerSeq = "", 0
	if seq <= 0 {
		return 0, false
	}
	n := 0
	if resp.Gap {
		n += 2
	}
	es := shownEntries(resp.Entries)
	for i, e := range es {
		if e.Seq >= seq {
			return max(0, n-1), true
		}
		n += len(m.transcriptEntry(e, tw))
		if !(i+1 < len(es) && e.Kind == "tool" && es[i+1].Kind == "tool") {
			n++
		}
	}
	return 0, false
}

// DigestLoaded reports whether the launch digest finished loading (tests).
func (m *Model) DigestLoaded() bool { return m.dg.loaded || m.dg.err != nil }

// DigestFocused reports whether a "While you were away" line holds the
// cursor (tests).
func (m *Model) DigestFocused() bool { return m.digestFocused() }
