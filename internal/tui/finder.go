package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/contracts"
)

// The finder (<leader><leader>, ctrl+p) is a Telescope-style picker over
// everything Pyre knows: sessions, attention items, projects, documents,
// artifacts, machines and commands — fuzzy-ranked with fzf scoring plus a
// small recency bonus, matched characters highlighted, a live preview.

type findItem struct {
	kind   string // session, attention, project, doc, artifact, machine, command
	id     string
	title  string
	sub    string
	fields []string
	at     time.Time
	pid    string // project, for docs and artifacts
	open   func(m *Model) tea.Cmd
	attach func(m *Model) tea.Cmd
}

var kindOrder = map[string]int{"session": 0, "attention": 1, "project": 2, "doc": 3, "artifact": 4, "machine": 5, "command": 6}
var kindBadge = map[string]string{"session": "sess", "attention": "attn", "project": "proj", "doc": "doc", "artifact": "art", "machine": "node", "command": "cmd"}

type finderOverlay struct {
	query   string
	sel     string // selected item key (kind:id) — stable while typing
	idx     int
	top     int
	preview int // preview scroll
	jk      jkState
}

type scoredItem struct {
	it    findItem
	score int
	pos   []int
}

func (it findItem) key() string { return it.kind + ":" + it.id }

func (m *Model) openFinder() tea.Cmd {
	m.overlay = &finderOverlay{}
	// Documents and artifacts of every project feed the finder; load them
	// in the background (results appear as they arrive).
	var cmds []tea.Cmd
	for _, p := range m.projects() {
		if !p.Archived {
			cmds = append(cmds, m.loadProjectLists(p.ProjectID)...)
		}
	}
	return tea.Batch(cmds...)
}

func (m *Model) loadProjectLists(pid string) []tea.Cmd {
	api := m.api
	pidp := &pid
	docs := m.ensure("proj:"+pid+":docs", 30*time.Second, func(ctx context.Context) (any, error) {
		return all(ctx, func(ctx context.Context, cur string) (contracts.Page[contracts.Document], error) {
			return api.ListDocuments(ctx, contracts.ListDocumentsRequest{ProjectID: pidp, PageRequest: contracts.PageRequest{Cursor: cur, Limit: 200}})
		})
	})
	arts := m.ensure("proj:"+pid+":arts", 30*time.Second, func(ctx context.Context) (any, error) {
		return all(ctx, func(ctx context.Context, cur string) (contracts.Page[contracts.Artifact], error) {
			return api.ListArtifacts(ctx, contracts.ListArtifactsRequest{ProjectID: pidp, PageRequest: contracts.PageRequest{Cursor: cur, Limit: 200}})
		})
	})
	return []tea.Cmd{docs, arts}
}

// findItems gathers every searchable object from loaded data.
func (m *Model) findItems() []findItem {
	var out []findItem
	for _, s := range m.sessions() {
		if s.Session.Archived {
			continue
		}
		s := s
		st := sessionStatus(s, m.nodeOf(s))
		sid := s.Session.SessionID
		fields := []string{s.NodeName, string(s.Session.Harness), st.label, m.projectName(s.Session.ProjectID), sid}
		fields = append(fields, userTags(s.Session.Tags)...)
		out = append(out, findItem{kind: "session", id: sid, title: s.Session.Title,
			sub: st.String() + " · " + s.NodeName, fields: fields, at: observedAt(s),
			open:   func(m *Model) tea.Cmd { return m.focusSession(sid) },
			attach: func(m *Model) tea.Cmd { m.focusSessionQuiet(sid); return m.attach(s) }})
	}
	for _, a := range m.openItems() {
		a := a
		st := attentionStatus(a.Kind)
		out = append(out, findItem{kind: "attention", id: a.AttentionID, title: m.humanIDs(a.Title),
			sub: st.String() + " · " + m.sessionTitle(a.SessionID), fields: []string{st.label, m.sessionTitle(a.SessionID), a.Severity, a.AttentionID}, at: a.UpdatedAt,
			open: func(m *Model) tea.Cmd { return m.openAttentionTarget(a) }})
	}
	for _, p := range m.projects() {
		if p.Archived {
			continue
		}
		pid := p.ProjectID
		var repos []string
		for _, r := range p.Repos {
			repos = append(repos, r.Name)
		}
		out = append(out, findItem{kind: "project", id: pid, title: p.Name, sub: plural(len(p.Repos), "repo", "repos"),
			fields: []string{p.Description, strings.Join(repos, " "), pid}, at: p.UpdatedAt,
			open: func(m *Model) tea.Cmd { return m.openProject(pid) }})
		docs, _ := get[[]contracts.Document](m, "proj:"+pid+":docs")
		for _, d := range docs {
			did := d.DocumentID
			out = append(out, findItem{kind: "doc", id: did, pid: pid, title: d.Title, sub: p.Name + fmt.Sprintf(" · r%d", d.HeadSeq),
				fields: []string{d.Slug, p.Name, did}, at: d.UpdatedAt,
				open: func(m *Model) tea.Cmd {
					m.curProject = pid
					m.view, m.back, m.projTab = viewProject, nil, tabDocs
					m.listFor("proj:" + pid + ":docs").sel = did
					return m.openDoc(did)
				}})
		}
		arts, _ := get[[]contracts.Artifact](m, "proj:"+pid+":arts")
		for _, a := range arts {
			aid := a.ArtifactID
			out = append(out, findItem{kind: "artifact", id: aid, pid: pid, title: a.Title, sub: p.Name + fmt.Sprintf(" · v%d", a.VersionCount),
				fields: []string{a.Description, p.Name, aid}, at: a.UpdatedAt,
				open: func(m *Model) tea.Cmd {
					m.curProject, m.projTab, m.curArtifact, m.docScroll = pid, tabArtifacts, aid, 0
					m.view, m.back = viewProject, nil
					return m.push(viewArtifact)
				}})
		}
	}
	for _, n := range m.sortedNodes() {
		nid := n.NodeID
		ns := nodeStatus(n)
		out = append(out, findItem{kind: "machine", id: nid, title: n.Name, sub: ns.String() + " · " + n.Platform,
			fields: []string{ns.label, n.Hostname, nid},
			open: func(m *Model) tea.Cmd {
				m.view, m.back = viewSessions, nil
				m.mgmtList = listState{sel: nid}
				return m.push(viewMachines)
			}})
	}
	for _, c := range m.commands() {
		c := c
		out = append(out, findItem{kind: "command", id: c.name, title: ":" + c.name, sub: c.desc, fields: c.aliases,
			open: func(m *Model) tea.Cmd {
				if c.arg != "" || c.args != "" {
					return m.openCmdline(c.name + " ")
				}
				return c.run(m, "")
			}})
	}
	return out
}

// rank scores items for the query: fzf score + recency bonus; empty query
// lists by kind, most recent first.
func (m *Model) rank(query string, items []findItem) []scoredItem {
	now := m.now()
	var out []scoredItem
	for _, it := range items {
		fields := append([]string{it.title, it.sub}, it.fields...)
		score, pos, ok := matchTerms(query, fields...)
		if !ok {
			continue
		}
		if !it.at.IsZero() && strings.TrimSpace(query) != "" {
			// up to +8 for things touched in the last hour, fading over a day
			if age := now.Sub(it.at); age < 24*time.Hour {
				score += int(8 * (1 - float64(age)/float64(24*time.Hour)))
			}
		}
		out = append(out, scoredItem{it, score, pos})
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if strings.TrimSpace(query) != "" && a.score != b.score {
			return a.score > b.score
		}
		if kindOrder[a.it.kind] != kindOrder[b.it.kind] {
			return kindOrder[a.it.kind] < kindOrder[b.it.kind]
		}
		if !a.it.at.Equal(b.it.at) {
			return a.it.at.After(b.it.at)
		}
		if len(a.it.title) != len(b.it.title) && strings.TrimSpace(query) != "" {
			return len(a.it.title) < len(b.it.title)
		}
		return a.it.key() < b.it.key()
	})
	return out
}

func (f *finderOverlay) results(m *Model) []scoredItem {
	res := m.rank(f.query, m.findItems())
	f.idx = 0
	for i, r := range res {
		if r.it.key() == f.sel {
			f.idx = i
		}
	}
	if len(res) > 0 {
		f.sel = res[f.idx].it.key()
	}
	return res
}

func (f *finderOverlay) hints() []hint {
	return []hint{{"enter", "open"}, {"ctrl+a", "attach"}, {"ctrl+j/k", "move"}, {"ctrl+d/u", "preview"}, {"esc", "close"}}
}

func (f *finderOverlay) update(m *Model, k tea.KeyPressMsg) tea.Cmd {
	if f.jk.escape(m, k, &f.query) {
		m.overlay = nil
		return nil
	}
	res := f.results(m)
	move := func(d int) tea.Cmd {
		if len(res) == 0 {
			return nil
		}
		f.idx = max(0, min(len(res)-1, f.idx+d))
		f.sel = res[f.idx].it.key()
		f.preview = 0
		return f.loadPreview(m, res[f.idx].it)
	}
	switch k.String() {
	case "esc":
		m.overlay = nil
		return nil
	case "enter":
		if len(res) == 0 {
			return nil
		}
		m.overlay = nil
		return res[f.idx].it.open(m)
	case "ctrl+a":
		if len(res) == 0 || res[f.idx].it.attach == nil {
			return m.setToast("ctrl+a attaches sessions only", toneMuted)
		}
		m.overlay = nil
		return res[f.idx].it.attach(m)
	case "down", "ctrl+j", "ctrl+n":
		return move(1)
	case "up", "ctrl+k", "ctrl+p":
		return move(-1)
	case "pgdown":
		return move(10)
	case "pgup":
		return move(-10)
	case "ctrl+d":
		f.preview += 5
		return nil
	case "ctrl+u":
		f.preview = max(0, f.preview-5)
		return nil
	case "backspace":
		if r := []rune(f.query); len(r) > 0 {
			f.query = string(r[:len(r)-1])
		}
	case "ctrl+w":
		t := strings.TrimRight(f.query, " ")
		if i := strings.LastIndexByte(t, ' '); i >= 0 {
			f.query = t[:i+1]
		} else {
			f.query = ""
		}
	default:
		if k.Text == "" || k.Mod&^tea.ModShift != 0 {
			return nil
		}
		f.query += k.Text
		if strings.HasPrefix(f.query, "?") { // ?query searches inside every transcript (search.go)
			return m.openSearch(strings.TrimPrefix(f.query, "?"))
		}
	}
	f.sel, f.top, f.preview = "", 0, 0 // new query: best match first
	res = f.results(m)
	if len(res) > 0 {
		return f.loadPreview(m, res[0].it)
	}
	return nil
}

func (f *finderOverlay) updateMsg(m *Model, msg tea.Msg) tea.Cmd {
	if p, ok := msg.(tea.PasteMsg); ok {
		f.query += strings.Join(strings.Fields(p.Content), " ")
		f.sel = ""
	}
	return nil
}

func (f *finderOverlay) loadPreview(m *Model, it findItem) tea.Cmd {
	switch it.kind {
	case "session":
		if s, ok := m.session(it.id); ok {
			return m.loadPreviewFor(s, false)
		}
	case "doc":
		return m.loadDoc(it.id, "", false)
	case "project":
		if p := m.project(it.id); p != nil && p.BriefDocumentID != "" {
			return m.loadDoc(p.BriefDocumentID, "", false)
		}
	case "attention":
		for _, a := range m.attention() {
			if a.AttentionID == it.id {
				return m.loadApproval(a, false)
			}
		}
	}
	return nil
}

func (f *finderOverlay) view(m *Model, w, h int) []string {
	res := f.results(m)
	lw := w
	showPreview := w >= 90
	if showPreview {
		lw = min(96, w*45/100)
	}
	count := m.th.dim.Render(fmt.Sprintf("%d/%d", min(len(res), f.idx+1), len(res)))
	prompt := " " + m.th.key.Render("Find ›") + " " + f.query + m.th.accent.Render("▏")
	if f.query == "" {
		prompt += m.th.dim.Render(trunc("sessions, needs-you items, projects, docs, artifacts, machines, :commands, ?inside transcripts", max(0, lw-24)))
	}
	head := pad(prompt, lw-width(count)-1) + count + " "
	out := []string{head, m.th.faint.Render(strings.Repeat("─", lw))}
	rows := h - len(out)
	if f.idx < f.top {
		f.top = f.idx
	}
	if f.idx >= f.top+rows {
		f.top = f.idx - rows + 1
	}
	if len(res) == 0 {
		out = append(out, m.th.dim.Render("  no matches"))
	}
	for i := f.top; i < min(len(res), f.top+rows); i++ {
		r := res[i]
		sel := i == f.idx
		badge := m.th.dim.Render(pad(kindBadge[r.it.kind], 5))
		subW := min(32, max(0, lw-2-6-24))
		titleW := lw - 2 - 6 - subW - 1
		row := m.cursor(sel) + badge + " " + m.highlight(pad(r.it.title, titleW), r.pos, styleIf(sel, m.th.selText))
		if subW > 6 {
			row += " " + m.th.dim.Render(padLeft(trunc(r.it.sub, subW), subW))
		}
		out = append(out, m.rowSel(sel, row, lw))
	}
	if !showPreview {
		return out
	}
	var prev []string
	if len(res) > 0 {
		prev = m.findPreview(res[f.idx].it, w-lw-3, h+f.preview)
		if f.preview > 0 && f.preview < len(prev) {
			prev = prev[f.preview:]
		}
	}
	return hjoin(fit(out, lw, h), lw, " "+m.th.faint.Render("│")+" ", fit(prev, w-lw-3, h), w-lw-3)
}

func (m *Model) findPreview(it findItem, w, h int) []string {
	switch it.kind {
	case "session":
		if s, ok := m.session(it.id); ok {
			return m.sessionDetail(s, w, h)
		}
	case "attention":
		for _, a := range m.attention() {
			if a.AttentionID == it.id {
				return m.attentionDetail(a, w)
			}
		}
	case "project":
		if p := m.project(it.id); p != nil {
			return m.projectOverview(*p, w, h, false)
		}
	case "doc":
		dr, st := get[contracts.DocumentWithRevision](m, "doc:"+it.id+"@")
		out := []string{m.th.title.Render(trunc(it.title, w)), m.th.dim.Render(trunc(it.sub, w)), ""}
		if !st.ok() {
			return append(out, m.loadingOr(st, ""))
		}
		return append(out, m.markdownLines(dr.Revision.Content, min(w, textMax))...)
	case "artifact":
		arts, _ := get[[]contracts.Artifact](m, "proj:"+it.pid+":arts")
		for _, a := range arts {
			if a.ArtifactID == it.id {
				out := []string{m.th.title.Render(trunc(a.Title, w)), m.th.dim.Render(trunc(it.sub, w)), ""}
				if a.Description != "" {
					out = append(out, wrap(a.Description, min(w, textMax))...)
					out = append(out, "")
				}
				out = append(out, m.kvl("Versions", itoa(a.VersionCount), 10), m.kvl("By", trunc(m.whoFull(a.CreatedBy), w-10), 10),
					m.kvl("Updated", ago(m.now(), a.UpdatedAt), 10), "", m.th.dim.Render("enter opens versions, content and comments"))
				return out
			}
		}
	case "machine":
		if n := m.node(it.id); n != nil {
			ns := nodeStatus(*n)
			out := []string{m.th.title.Render(n.Name) + "  " + m.th.status(ns), ""}
			out = append(out, m.kvl("Platform", n.Platform+"/"+n.Arch, 11), m.kvl("Harnesses", m.harnessChips(*n), 11))
			r := n.Resources
			out = append(out, m.kvl("Resources", trunc(fmt.Sprintf("load %.1f/%d · %s free · %d runs", r.Load1, r.CPUCount, humanBytes(r.MemAvailMB<<20), r.ActiveRuns), w-11), 11))
			return out
		}
	case "command":
		return []string{m.th.title.Render(it.title), "", wrap(it.sub, w)[0], "", m.th.dim.Render("enter runs it (commands that take an argument open the command line)")}
	}
	return nil
}
