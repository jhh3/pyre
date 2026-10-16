package tui

import (
	"context"
	"path"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/repoindex"
)

// The inline folder browser of the new-session panel (W-browse): "📁
// Browse folders…" at the bottom of the Where list opens it on the chosen
// machine at ~ (or the folder typed). ↑/↓ move, → or Enter opens a folder,
// ← or Backspace goes up, typing filters the listing, "Use this folder"
// (Enter, or ^s anywhere) chooses, "＋ New folder…" creates one through
// fs.mkdir, Esc returns to the list. Choosing sets Where to the folder and
// fixes the machine to the browsed one; nothing else changes.

const browseKey = "browse"

// Browser row kinds.
const (
	brUse = iota
	brNew
	brDir
)

type nsBrowse struct {
	node   string
	path   string // "~", "~/x" or absolute
	filter string
	hlName string // highlighted: "" = Use this folder, "\x00new" = New folder, else a folder name
	naming bool
	name   string // the new folder's name
	err    string
	busy   bool
}

type nsBRow struct {
	kind int
	name string
}

const brNewKey = "\x00new"

func (b *nsBrowse) key() string { return qlDirKey(b.node, b.path) }

// openBrowse starts the browser at the folder typed (when the Where text
// is a path) or ~, on the plan's machine.
func (q *nsPanel) openBrowse(m *Model, p nsPlan) tea.Cmd {
	node := p.nodeID
	if !p.connected[node] {
		node = p.base
	}
	start := "~"
	if p.pathMode {
		pth, dir := repoindex.NormPath(strings.TrimSpace(q.whereQuery()), q.cwd, q.home)
		start = pth
		if !dir {
			start, _ = repoindex.SplitPath(pth, false)
		}
	}
	q.browse = &nsBrowse{node: node, path: start}
	q.focus = nsWhere
	return q.loadBrowse(m)
}

func (q *nsPanel) loadBrowse(m *Model) tea.Cmd {
	b := q.browse
	if b == nil {
		return nil
	}
	if n := m.node(b.node); n == nil || n.Connection.State != contracts.ConnConnected {
		return nil
	}
	api, nid, dir := m.api, b.node, b.path
	return m.ensure(b.key(), 15*time.Second, func(ctx context.Context) (any, error) {
		var out contracts.DirListResult
		err := nsInvoke(ctx, api, nid, contracts.NewID(contracts.PrefixOperation), contracts.ActionDirList, contracts.DirListParams{Path: dir}, &out, 5)
		return out, err
	})
}

// browseRows: Use this folder, New folder, then the (filtered) folders.
func (q *nsPanel) browseRows(m *Model) (rows []nsBRow, res contracts.DirListResult, st *loadState) {
	b := q.browse
	res, st = get[contracts.DirListResult](m, b.key())
	rows = []nsBRow{{kind: brUse}, {kind: brNew}}
	f := strings.ToLower(b.filter)
	for _, d := range res.Dirs {
		if f == "" || strings.Contains(strings.ToLower(d), f) {
			rows = append(rows, nsBRow{kind: brDir, name: d})
		}
	}
	return rows, res, st
}

func (q *nsPanel) browseSel(rows []nsBRow) int {
	b := q.browse
	for i, r := range rows {
		switch {
		case r.kind == brUse && b.hlName == "":
			return i
		case r.kind == brNew && b.hlName == brNewKey:
			return i
		case r.kind == brDir && r.name == b.hlName:
			return i
		}
	}
	return 0
}

func (b *nsBrowse) highlight(r nsBRow) {
	switch r.kind {
	case brUse:
		b.hlName = ""
	case brNew:
		b.hlName = brNewKey
	default:
		b.hlName = r.name
	}
}

// into opens a child folder; up goes to the parent (from ~, the parent of
// the node's home as dir.list reported it), highlighting where it came from.
func (b *nsBrowse) into(name string) {
	b.path, b.filter, b.hlName, b.err = repoindex.JoinPath(b.path, name), "", "", ""
}

func (b *nsBrowse) up(res contracts.DirListResult) {
	cur := b.path
	parent, name := repoindex.SplitPath(cur, false)
	if cur == "~" || parent == cur {
		if cur == "~" && strings.HasPrefix(res.Path, "/") && res.Path != "/" {
			parent, name = path.Dir(res.Path), path.Base(res.Path)
		} else {
			return
		}
	}
	b.path, b.filter, b.hlName, b.err = parent, "", name, ""
}

// chooseBrowse sets Where to the browsed folder on the browsed machine.
func (q *nsPanel) chooseBrowse(m *Model, dir string) tea.Cmd {
	b := q.browse
	q.browse = nil
	q.machine = b.node
	q.query = dir
	if dir != "/" && dir != "~" {
		q.query += "/"
	} else if dir == "~" {
		q.query = "~/"
	}
	q.hlKey = "dir:" + dir
	q.focus = nsMachine
	q.errLine = ""
	return q.load(m, q.plan(m))
}

func (q *nsPanel) mkdirBrowse(m *Model) tea.Cmd {
	b := q.browse
	name := strings.TrimSpace(b.name)
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		b.err = "a folder name, without /"
		return nil
	}
	target := repoindex.JoinPath(b.path, name)
	api, nid := m.api, b.node
	op := m.opID("ns:" + q.instance + ":browse-mkdir:" + nid + ":" + target)
	b.busy, b.err = true, ""
	return m.doLong(time.Minute, func(ctx context.Context) (any, error) {
		var mk contracts.FsMkdirResult
		err := nsInvoke(ctx, api, nid, op, contracts.ActionFsMkdir, contracts.FsMkdirParams{Path: target}, &mk, 30)
		return mk, err
	}, func(m *Model, _ any, err error) tea.Cmd {
		if q.browse != b {
			return nil
		}
		b.busy = false
		if err != nil {
			b.err = "not created: " + errText(err)
			return nil
		}
		parentKey := b.key()
		b.naming, b.name = false, ""
		b.into(name)
		return tea.Batch(m.reload(parentKey, nil), q.loadBrowse(m))
	})
}

// browseKey handles every key while the browser is open.
func (q *nsPanel) browseKey(m *Model, k tea.KeyPressMsg) tea.Cmd {
	b := q.browse
	key := k.String()
	if b.busy {
		return nil
	}
	if b.naming {
		switch key {
		case "esc":
			b.naming, b.name, b.err = false, "", ""
		case "enter":
			return q.mkdirBrowse(m)
		default:
			q.editText(&b.name, k)
		}
		return nil
	}
	rows, res, _ := q.browseRows(m)
	sel := q.browseSel(rows)
	switch key {
	case "esc":
		q.browse = nil
		return nil
	case "ctrl+c":
		m.overlay = nil
		return nil
	case "ctrl+s", "ctrl+enter":
		return q.chooseBrowse(m, b.path)
	case "tab", "shift+tab":
		q.browse = nil
		if key == "tab" {
			q.move(1)
		} else {
			q.move(-1)
		}
		return nil
	case "up", "ctrl+p", "ctrl+k":
		b.highlight(rows[max(0, sel-1)])
	case "down", "ctrl+n", "ctrl+j":
		b.highlight(rows[min(len(rows)-1, sel+1)])
	case "pgup":
		b.highlight(rows[max(0, sel-8)])
	case "pgdown":
		b.highlight(rows[min(len(rows)-1, sel+8)])
	case "right":
		if rows[sel].kind == brDir {
			b.into(rows[sel].name)
		}
	case "enter":
		switch rows[sel].kind {
		case brUse:
			return q.chooseBrowse(m, b.path)
		case brNew:
			b.naming, b.name, b.err = true, b.filter, ""
			return nil
		default:
			b.into(rows[sel].name)
		}
	case "left":
		b.up(res)
	case "backspace":
		if b.filter == "" {
			b.up(res)
		} else {
			q.editText(&b.filter, k)
			q.browseFirstMatch(m)
		}
	default:
		if q.editText(&b.filter, k) {
			q.browseFirstMatch(m)
		}
	}
	return q.loadBrowse(m)
}

// browseFirstMatch highlights the first folder matching the filter.
func (q *nsPanel) browseFirstMatch(m *Model) {
	rows, _, _ := q.browseRows(m)
	if q.browse.filter == "" {
		q.browse.hlName = ""
		return
	}
	for _, r := range rows {
		if r.kind == brDir {
			q.browse.hlName = r.name
			return
		}
	}
	q.browse.hlName = ""
}

func (q *nsPanel) browseClick(m *Model, val string) tea.Cmd {
	b := q.browse
	if b == nil || b.busy {
		return nil
	}
	rows, _, _ := q.browseRows(m)
	i, _ := strconv.Atoi(val)
	if i < 0 || i >= len(rows) {
		return nil
	}
	switch r := rows[i]; r.kind {
	case brUse:
		return q.chooseBrowse(m, b.path)
	case brNew:
		b.highlight(r)
		b.naming, b.name = true, b.filter
		return nil
	default:
		b.into(r.name)
	}
	return q.loadBrowse(m)
}

func (q *nsPanel) browseHints() []hint {
	if q.browse.naming {
		return []hint{{"enter", "create"}, {"esc", "cancel"}}
	}
	return []hint{{"↑↓", "move"}, {"→", "open"}, {"←", "up"}, {"enter", "open / use"}, {"^s", "use this folder"}, {"esc", "back to list"}}
}

// browseLines renders the browser in the list's place.
func (q *nsPanel) browseLines(m *Model, width_, listH int) []string {
	b := q.browse
	rows, _, st := q.browseRows(m)
	sel := q.browseSel(rows)
	head := m.th.label.Render("Browse ") + m.th.key.Render(b.path) + m.th.dim.Render(" on "+m.nodeName(b.node))
	if b.filter != "" {
		head += m.th.dim.Render("   filter: ") + b.filter + m.th.accent.Render("▏")
	}
	list := []string{trunc(head, width_)}
	selLine := 0
	for i, r := range rows {
		on := i == sel
		var s string
		switch r.kind {
		case brUse:
			s = m.th.key.Render("✓ Use this folder") + "  " + m.th.faint.Render(b.path)
		case brNew:
			s = m.th.dim.Render("＋ New folder…")
			if b.naming {
				s = m.th.key.Render("＋ New folder: ") + b.name + m.th.accent.Render("▏")
				if b.busy {
					s += m.th.dim.Render("  creating…")
				}
			}
		default:
			s = m.highlightSub(r.name, b.filter, styleIf(on, m.th.selText)) + m.th.faint.Render("/")
		}
		if on {
			selLine = len(list)
		}
		list = append(list, m.mark(zoneDef{kind: zoneOverlay, name: "brow:" + strconv.Itoa(i)}, m.rowSel(on, trunc(m.cursor(on)+s, width_), width_)))
	}
	switch {
	case b.err != "":
		list = append(list, m.th.tone(toneWarn, trunc(b.err, width_)))
	case st != nil && st.err != nil && !st.inflight:
		list = append(list, m.th.tone(toneWarn, trunc("cannot list "+b.path+": "+errText(st.err)+" (← goes up)", width_)))
	case st == nil || (!st.have && st.err == nil):
		list = append(list, m.th.dim.Render("  listing…"))
	case len(rows) == 2 && b.filter != "":
		list = append(list, m.th.dim.Render(trunc("  no folder matches “"+b.filter+"” (＋ New folder… creates one)", width_)))
	case len(rows) == 2:
		list = append(list, m.th.dim.Render("  no folders here"))
	}
	if len(list) > listH {
		top := 0
		if selLine >= listH-1 {
			top = selLine - listH + 2
		}
		top = min(top, len(list)-listH)
		list = append([]string{list[0]}, list[top+1:top+listH]...)
	}
	for len(list) < listH {
		list = append(list, "")
	}
	return list
}

// highlightSub marks the filter's first occurrence in s.
func (m *Model) highlightSub(s, filter string, base lipgloss.Style) string {
	i := strings.Index(strings.ToLower(s), strings.ToLower(filter))
	if filter == "" || i < 0 {
		return m.highlight(s, nil, base)
	}
	var pos []int
	start := len([]rune(s[:i]))
	for j := range []rune(filter) {
		pos = append(pos, start+j)
	}
	return m.highlight(s, pos, base)
}
