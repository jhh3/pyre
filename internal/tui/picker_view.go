package tui

import "strings"

// view renders a picker: an optional query line, then items (grouped under
// section headers when they carry a group) with right-aligned details. The
// panel height is capped relative to the terminal.
func (p *pickerOverlay) view(m *Model, w, h int) []string {
	bw := min(84, max(60, w*50/100))
	inner := min(bw, w-2) - 4
	vis := p.sync()
	var lines []string
	if p.filter {
		q := m.th.key.Render("›") + " " + p.query + m.th.accent.Render("▏")
		if p.query == "" {
			q = m.th.key.Render("›") + " " + m.th.accent.Render("▏") + m.th.dim.Render("type to filter")
		}
		lines = append(lines, q, m.th.faint.Render(strings.Repeat("─", inner)))
	}
	rows := min(h-6-len(lines), max(8, h*60/100))
	rows = max(rows, 3)
	if len(vis) == 0 {
		lines = append(lines, m.th.dim.Render("  no matches"))
		return m.box(p.title, lines, bw, w, h)
	}
	groups := make([]string, len(vis))
	for i, it := range vis {
		groups[i] = it.group
	}
	grouped := false
	for _, g := range groups {
		if g != "" {
			grouped = true
		}
	}
	var dl []dline
	if grouped && strings.TrimSpace(p.query) == "" {
		dl = groupedWindow(groups, p.l.idx, rows, &p.top)
	} else {
		start, end := p.l.window(rows)
		for i := start; i < end; i++ {
			dl = append(dl, dline{item: i})
		}
	}
	for _, d := range dl {
		if d.item < 0 {
			lines = append(lines, m.th.dim.Render(d.group))
			continue
		}
		it := vis[d.item]
		sel := d.item == p.l.idx
		right := it.detail
		if it.reason != "" {
			right = "unavailable: " + it.reason
		}
		label := trunc(it.label, inner-4)
		lab := m.primary(sel, label)
		if it.reason != "" {
			lab = m.th.dim.Render(label)
		}
		row := m.cursor(sel) + lab
		if right != "" {
			rw := inner - width(row) - 2
			if rw > 8 {
				r := trunc(right, rw)
				row += strings.Repeat(" ", inner-width(row)-width(r)) + m.th.dim.Render(r)
			}
		}
		lines = append(lines, m.rowSel(sel, row, inner))
	}
	if len(vis) > rows {
		lines = append(lines, m.th.faint.Render(itoa(p.l.idx+1)+"/"+itoa(len(vis))))
	}
	return m.box(p.title, lines, bw, w, h)
}
