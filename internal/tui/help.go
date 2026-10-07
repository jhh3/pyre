package tui

import (
	"fmt"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// mouseHelp is the help screen's note on the mouse and number keys.
const mouseHelp = "Mouse: click a tab, row or footer key · click the selected row (or double-click) to open · wheel scrolls · " +
	"drag selects and copies (OSC 52), double-click a word; shift (option in iTerm2) leaves selection to the terminal. 1–4 switch tabs (Sessions, Projects, Machines, Accounts); v cycles the Sessions view (sidenav, table, grid), G toggles the grid; in an answer box 1–9 answer."

// helpGroupOrder puts the everyday groups first.
var helpGroupOrder = []string{"Move", "Switch", "Session", "Terminal", "Find", "Go to", "Digest", "Act", "Composer", "Pickers",
	"Projects", "Project", "Review", "Document", "Messages", "Reader", "Manage", "Help"}

// helpOverlay is a searchable cheat-sheet generated from the effective
// keymap (so keys.toml overrides show up), grouped like which-key.
type helpOverlay struct {
	query  string
	scroll int
	jk     jkState
}

func (h *helpOverlay) hints() []hint {
	return []hint{{"type", "search"}, {"↑↓", "scroll"}, {"esc", "close"}}
}

func (h *helpOverlay) update(m *Model, k tea.KeyPressMsg) tea.Cmd {
	if h.jk.escape(m, k, &h.query) {
		m.overlay = nil
		return nil
	}
	switch k.String() {
	case "esc", "?":
		m.overlay = nil
	case "down", "ctrl+j", "ctrl+n":
		h.scroll++
	case "up", "ctrl+k", "ctrl+p":
		h.scroll = max(0, h.scroll-1)
	case "pgdown", "ctrl+d":
		h.scroll += 10
	case "pgup", "ctrl+u":
		h.scroll = max(0, h.scroll-10)
	case "backspace":
		if r := []rune(h.query); len(r) > 0 {
			h.query = string(r[:len(r)-1])
			h.scroll = 0
		}
	default:
		if k.Text != "" && k.Mod&^tea.ModShift == 0 {
			h.query += k.Text
			h.scroll = 0
		}
	}
	return nil
}

func (h *helpOverlay) view(m *Model, w, hh int) []string {
	bw := min(168, w-2)
	inner := bw - 4
	type entry struct{ keys, desc string }
	groups := map[string][]entry{}
	var order []string
	defs := append([]actionDef(nil), actionDefs...)
	rank := map[string]int{}
	for i, g := range helpGroupOrder {
		rank[g] = i
	}
	sort.SliceStable(defs, func(i, j int) bool { return rank[defs[i].group] < rank[defs[j].group] })
	for _, a := range defs {
		keys := m.km.keysFor(a.id)
		if keys == "" {
			continue
		}
		if h.query != "" && !filterOK(h.query, a.desc, keys, a.id, a.group) {
			continue
		}
		if _, ok := groups[a.group]; !ok {
			order = append(order, a.group)
		}
		groups[a.group] = append(groups[a.group], entry{keys, a.desc})
	}
	keyW := 6
	for _, g := range order {
		for _, e := range groups[g] {
			keyW = max(keyW, width(e.keys))
		}
	}
	keyW = min(keyW+1, 18)
	block := func(g string) []string {
		out := []string{m.th.heading.Render(g)}
		for _, e := range groups[g] {
			out = append(out, " "+m.th.key.Render(pad(e.keys, keyW))+" "+m.th.dim.Render(e.desc))
		}
		return append(out, "")
	}
	var blocks [][]string
	total := 0
	for _, g := range order {
		b := block(g)
		blocks = append(blocks, b)
		total += len(b)
	}
	cols := 1
	switch {
	case inner >= 150:
		cols = 3
	case inner >= 100:
		cols = 2
	}
	lines := packColumns(blocks, total, cols, (inner-3*(cols-1))/cols)
	if len(lines) == 0 {
		lines = []string{m.th.dim.Render("no keys match " + h.query)}
	}
	legend := []string{
		m.th.status(status{"●", "working", toneGood}), m.th.status(status{"○", "idle", toneNone}),
		m.th.status(status{"!", "approval", toneWarn}), m.th.status(status{"◌", "offline/stale", toneMuted}),
		m.th.status(status{"✗", "failed", toneBad}), m.th.status(status{"?", "unknown", toneMuted}),
	}
	search := m.th.key.Render("›") + " " + h.query + m.th.accent.Render("▏")
	if h.query == "" {
		search += m.th.dim.Render("type to search keys")
	}
	leader := "leader " + displayKey(m.km.Leader)
	if m.km.Source != "" {
		leader += " · overrides from " + m.km.Source
	}
	head := []string{search + strings.Repeat(" ", max(1, inner-width(search)-width(leader))) + m.th.dim.Render(leader), m.th.faint.Render(strings.Repeat("─", inner))}
	foot := []string{m.th.faint.Render(strings.Repeat("─", inner))}
	foot = append(foot, wrapJoin(legend, gap2, inner)...)
	foot = append(foot, wrap(mouseHelp, inner)...)
	room := max(3, hh-6-len(head)-len(foot))
	h.scroll = max(0, min(h.scroll, len(lines)-room))
	body := append([]string(nil), lines[h.scroll:min(len(lines), h.scroll+room)]...)
	if more := len(lines) - h.scroll - room; more > 0 {
		body[len(body)-1] = m.th.dim.Render(fmt.Sprintf("↓ %d more lines · j/k or ctrl+d scroll", more))
	}
	for len(body) < room && h.query == "" {
		body = append(body, "") // stable box height: the panel does not jump
	}
	out := append(append(head, body...), foot...)
	return m.box("Keys & help", out, bw, w, hh)
}

// packColumns flows whole blocks (a heading and its keys) down cols columns
// of width colW, balancing their heights; a block never splits.
func packColumns(blocks [][]string, total, cols, colW int) []string {
	if cols <= 1 {
		var out []string
		for _, b := range blocks {
			out = append(out, b...)
		}
		return out
	}
	target := (total + cols - 1) / cols
	columns := make([][]string, cols)
	c := 0
	for _, b := range blocks {
		if c < cols-1 && len(columns[c]) > 0 && len(columns[c])+len(b) > target+2 {
			c++
		}
		columns[c] = append(columns[c], b...)
	}
	rows := 0
	for _, col := range columns {
		rows = max(rows, len(col))
	}
	out := make([]string, rows)
	for i := range out {
		var b strings.Builder
		for ci, col := range columns {
			cell := ""
			if i < len(col) {
				cell = col[i]
			}
			if ci < cols-1 {
				b.WriteString(pad(cell, colW) + "   ")
			} else {
				b.WriteString(cell)
			}
		}
		out[i] = b.String()
	}
	return out
}
