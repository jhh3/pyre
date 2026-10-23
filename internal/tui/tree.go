package tui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/contracts"
)

// Session trees. A session started by another session (lineage parent, or
// created by a session principal) sits directly under its parent inside the
// parent's group in the Sessions list and table, indented two cells per
// level behind a faint tree glyph ("├ ", "└ ", with "│ " guides for deeper
// levels). Parents keep their place in the group's sort; children follow
// the same sort under their parent. A child whose parent is not in the same
// group (another project or machine, ended and hidden, or archived) stays
// at the top level with a faint "↳ parent title" hint. Children of a pinned
// parent follow it into the Pinned group. za on a parent, or a click on a
// child's glyph, folds the parent's children (a folded parent shows a faint
// "+N"; a click on it unfolds). A lead (a top-level session with children
// in its group) has a bold title and a faint descendant count, "▾N" while
// open; a single session (no parent, no children) is plain. ⌥↑/⌥↓ and the
// Grid follow the tree order.

// treeRow is one session's place in its group's tree (last layout).
type treeRow struct {
	depth  int
	prefix string // guides and glyph, two cells per level ("│ └ ")
	parent string // parent session id in the same group ("" at the top)
	kids   int    // descendants in the same group
	hint   string // the parent's title when it is not in the same group
}

// lineageParent is the session that started s ("" when none).
// A reparented session sits under its new parent (W-reparent).
func lineageParent(s contracts.Session) string { return s.Parent() }

// treeLayout orders ss (already sorted, each group contiguous; groups[i] is
// ss[i]'s group) as trees: within each group a child follows its parent
// (depth first), roots and siblings keep their sorted order. Lineage cycles
// are cut. It returns the new order and each session's tree row.
func treeLayout(ss []contracts.SessionView, groups []string) ([]contracts.SessionView, map[string]treeRow) {
	out := make([]contracts.SessionView, 0, len(ss))
	rows := make(map[string]treeRow, len(ss))
	for start := 0; start < len(ss); {
		end := start + 1
		for end < len(ss) && groups[end] == groups[start] {
			end++
		}
		run := ss[start:end]
		start = end
		idx := make(map[string]int, len(run))
		for i, s := range run {
			idx[s.Session.SessionID] = i
		}
		parent := make([]int, len(run))
		linked := false
		for i, s := range run {
			parent[i] = -1
			if p, ok := idx[lineageParent(s.Session)]; ok && p != i {
				parent[i], linked = p, true
			}
		}
		if !linked {
			for _, s := range run {
				rows[s.Session.SessionID] = treeRow{}
			}
			out = append(out, run...)
			continue
		}
		for i := range run { // cut cycles: the session that closes one becomes a root
			seen := map[int]bool{i: true}
			for p := parent[i]; p >= 0; p = parent[p] {
				if p == i {
					parent[i] = -1
					break
				}
				if seen[p] {
					break // a cycle above; cut when its own member is visited
				}
				seen[p] = true
			}
		}
		kids := make([][]int, len(run))
		for i := range run {
			if p := parent[i]; p >= 0 {
				kids[p] = append(kids[p], i)
			}
		}
		var walk func(i, depth int, guides string, last bool) int
		walk = func(i, depth int, guides string, last bool) int {
			s := run[i]
			r := treeRow{depth: depth}
			childGuides := ""
			if depth > 0 {
				glyph, guide := "├ ", "│ "
				if last {
					glyph, guide = "└ ", "  "
				}
				r.prefix = guides + glyph
				r.parent = run[parent[i]].Session.SessionID
				childGuides = guides + guide
			}
			out = append(out, s)
			n := 0
			for k, c := range kids[i] {
				n += 1 + walk(c, depth+1, childGuides, k == len(kids[i])-1)
			}
			r.kids = n
			rows[s.Session.SessionID] = r
			return n
		}
		for i := range run {
			if parent[i] < 0 {
				walk(i, 0, "", false)
			}
		}
	}
	return out, rows
}

// sessionByID finds a listed (non-archived or archived) session quickly:
// an index over the sessions snapshot, rebuilt when the snapshot changes.
func (m *Model) sessionByID(id string) (contracts.SessionView, bool) {
	ss := m.sessions()
	if id == "" || len(ss) == 0 {
		return contracts.SessionView{}, false
	}
	t := &m.tree
	if t.idxOf != &ss[0] || len(t.idx) != len(ss) {
		t.idxOf, t.idx = &ss[0], make(map[string]int, len(ss))
		for i, s := range ss {
			t.idx[s.Session.SessionID] = i
		}
	}
	if i, ok := t.idx[id]; ok && i < len(ss) && ss[i].Session.SessionID == id {
		return ss[i], true
	}
	for _, s := range ss { // the snapshot was edited in place
		if s.Session.SessionID == id {
			t.idxOf = nil
			return s, true
		}
	}
	return contracts.SessionView{}, false
}

// pinState reports whether s sits in the Pinned group and its pin time:
// pinned itself, or a descendant of a pinned session that is listed (not
// archived, not ended while ended sessions are hidden).
func (m *Model) pinState(s contracts.SessionView) (bool, int64) {
	if sessionPinned(s) {
		return true, pinnedAt(s.Session.Tags)
	}
	seen := map[string]bool{s.Session.SessionID: true}
	for p := lineageParent(s.Session); p != "" && !seen[p] && len(seen) < 64; {
		seen[p] = true
		a, ok := m.sessionByID(p)
		if !ok || a.Session.Archived || (!m.showEnded && sessionEnded(a)) {
			return false, 0
		}
		if sessionPinned(a) {
			return true, pinnedAt(a.Session.Tags)
		}
		p = lineageParent(a.Session)
	}
	return false, 0
}

// pinned reports whether s sits in the Pinned group (pinState).
func (m *Model) pinned(s contracts.SessionView) bool {
	p, _ := m.pinState(s)
	return p
}

// treeState is the last tree layout of the Sessions list (Model.tree);
// the folded parents live in groupingState.trees (remembered).
type treeState struct {
	rows  map[string]treeRow
	idx   map[string]int // sessionByID's index
	idxOf *contracts.SessionView
}

// treeFolded reports whether parent id hides its children (never while
// filtering: every match shows).
func (m *Model) treeFolded(id string) bool {
	return m.filter == "" && m.grouping.trees[id]
}

// treeHidden reports whether a folded ancestor hides id.
func (m *Model) treeHidden(id string) bool {
	seen := 0
	for p := m.tree.rows[id].parent; p != "" && seen < 64; p = m.tree.rows[p].parent {
		if m.treeFolded(p) {
			return true
		}
		seen++
	}
	return false
}

// layoutTrees records the tree rows of the listed sessions vs (in list
// order, groups[i] is vs[i]'s group) and the "↳ parent" hints.
func (m *Model) layoutTrees(vs []contracts.SessionView, groups []string) {
	_, rows := treeLayout(vs, groups)
	for _, s := range vs {
		r := rows[s.Session.SessionID]
		if r.depth > 0 {
			continue
		}
		if p := lineageParent(s.Session); p != "" {
			if ps, ok := m.sessionByID(p); ok && ps.Session.Title != "" {
				r.hint = ps.Session.Title
				rows[s.Session.SessionID] = r
			} else if ps, ok := m.session(p); ok && ps.Session.Title != "" {
				r.hint = ps.Session.Title
				rows[s.Session.SessionID] = r
			}
		}
	}
	m.tree.rows = rows
}

// toggleTree folds or unfolds parent id's children and selects it.
func (m *Model) toggleTree(id string) tea.Cmd {
	if m.filter != "" {
		return m.setToast("trees stay open while filtering", toneMuted)
	}
	if m.grouping.trees == nil {
		m.grouping.trees = map[string]bool{}
	}
	if m.grouping.trees[id] {
		delete(m.grouping.trees, id)
	} else if m.tree.rows[id].kids > 0 {
		m.grouping.trees[id] = true
	}
	m.syncSessions()
	m.sessList.selectID(id)
	return tea.Batch(m.saveState(), m.schedulePreview())
}

// revealInTree unfolds the folded ancestors of id; true when one opened.
func (m *Model) revealInTree(id string) bool {
	opened := false
	seen := 0
	for p := m.tree.rows[id].parent; p != "" && seen < 64; p = m.tree.rows[p].parent {
		if m.grouping.trees[p] {
			delete(m.grouping.trees, p)
			opened = true
		}
		seen++
	}
	return opened
}

// treeTitle decorates a row's title cell of w cells: the faint tree prefix
// (a click folds the parent), then the title (with the doing suffix), then
// a faint "+N" on a folded parent (a click unfolds) or "↳ parent" hint.
// A lead (a top-level row with descendants in its group and no "↳ parent"
// elsewhere) has a bold title and always shows its descendant count: "▾N"
// open (a click folds), "+N" folded.
//
// A top-level row (no "↳ parent") carries its lead signal (leads.go) as a
// chip at the right of the cell, unless the row's own status already says
// it (the lead itself works, or itself needs you); a quiet lead has a dim
// title instead.
func (m *Model) treeTitle(sel bool, s contracts.SessionView, w int) string {
	r := m.tree.rows[s.Session.SessionID]
	if r.depth > 0 || r.hint != "" {
		return m.treeTitleCell(sel, false, s, w)
	}
	g, ok := m.leadSignal(s.Session.SessionID)
	if !ok {
		return m.treeTitleCell(sel, false, s, w)
	}
	if (g.kind == leadWorking && m.sessionWorking(s)) || (g.kind == leadWaiting && needsYou(s, m.nodeOf(s))) {
		return m.treeTitleCell(sel, false, s, w) // its own status already says it
	}
	chip, cw := m.leadChip(g)
	if g.kind == leadAsks { // a link to the lead's asks in the web inbox (weblink.go)
		chip = m.askLink("session", s.Session.SessionID, chip)
	}
	if cw == 0 || w-cw-1 < 10 {
		return m.treeTitleCell(sel, g.kind == leadQuiet && !sel, s, w)
	}
	return m.treeTitleCell(sel, false, s, w-cw-1) + " " + chip
}

// treeTitleCell is treeTitle without the lead chip; quiet dims the title.
func (m *Model) treeTitleCell(sel, quiet bool, s contracts.SessionView, w int) string {
	r := m.tree.rows[s.Session.SessionID]
	lead := r.depth == 0 && r.kids > 0 && r.hint == "" && !quiet
	bold := sel || lead // titleCell's bold is the selected row's
	title := func(tw int) string {
		if quiet {
			return m.highlight(pad(iconTitle(s), tw), m.filterPos(iconTitle(s)), m.th.dim)
		}
		return m.titleCell(bold, iconTitle(s), tw)
	}
	pre := ""
	if r.prefix != "" {
		p := r.prefix
		for width(p) > max(2, w/3) { // deep trees keep the title readable
			p = string([]rune(p)[2:])
		}
		pre = m.mark(zoneDef{kind: zoneTree, name: r.parent}, m.th.faint.Render(p))
		w -= width(p)
	}
	post, postW := "", 0
	switch {
	case r.kids > 0 && (lead || m.treeFolded(s.Session.SessionID)):
		t := " +" + itoa(r.kids)
		if !m.treeFolded(s.Session.SessionID) {
			t = " ▾" + itoa(r.kids)
		}
		post, postW = m.mark(zoneDef{kind: zoneTree, name: s.Session.SessionID}, m.th.faint.Render(t)), width(t)
	case r.hint != "":
		room := w - width(iconTitle(s)) // the hint never cuts the title
		if room >= 8 {
			t := trunc(" ↳ "+r.hint, min(room, 28))
			post, postW = m.th.faint.Render(t), width(t)
		}
	}
	if postW == 0 || postW >= w {
		if quiet {
			return pre + title(w)
		}
		return pre + m.titleDoingCell(bold, s, w)
	}
	tw := min(width(iconTitle(s)), w-postW)
	if tw < width(iconTitle(s)) || tw+postW >= w {
		return pre + title(tw) + post + pad("", w-tw-postW)
	}
	// The title fits with its decoration: the doing suffix takes the rest.
	rest := w - tw - postW
	phrase, ok := m.rowPhrase(s) // monitors.go
	tail := pad("", rest)
	if ok && rest >= 8 {
		tail = m.th.faint.Render(pad(" · "+phrase, rest))
	}
	return pre + title(tw) + post + tail
}

// ---- accessors for tests ----

// TreeDepth is the session's depth in its group's tree (0 at the top).
func (m *Model) TreeDepth(id string) int { return m.tree.rows[id].depth }
