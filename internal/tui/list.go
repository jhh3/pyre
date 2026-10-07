package tui

// listState is a cursor over a list of stable IDs. Selection is by ID, so
// re-sorting, inserting or refreshing items never moves the cursor to a
// different item. When the selected item disappears, the cursor stays at
// the same position (clamped), selecting its neighbour.
type listState struct {
	ids []string
	sel string
	idx int
	top int
}

// setIDs replaces the visible items, preserving selection by ID.
func (l *listState) setIDs(ids []string) {
	l.ids = ids
	if len(ids) == 0 {
		l.sel, l.idx, l.top = "", 0, 0
		return
	}
	if l.sel != "" {
		for i, id := range ids {
			if id == l.sel {
				l.idx = i
				return
			}
		}
	}
	if l.idx >= len(ids) {
		l.idx = len(ids) - 1
	}
	if l.idx < 0 {
		l.idx = 0
	}
	l.sel = ids[l.idx]
}

// selectID moves the cursor to id if present; reports success.
func (l *listState) selectID(id string) bool {
	for i, x := range l.ids {
		if x == id {
			l.idx, l.sel = i, id
			return true
		}
	}
	return false
}

// move shifts the cursor by d, clamped.
func (l *listState) move(d int) {
	if len(l.ids) == 0 {
		return
	}
	l.idx += d
	if l.idx < 0 {
		l.idx = 0
	}
	if l.idx >= len(l.ids) {
		l.idx = len(l.ids) - 1
	}
	l.sel = l.ids[l.idx]
}

func (l *listState) home() { l.move(-len(l.ids)) }
func (l *listState) end()  { l.move(len(l.ids)) }

// window returns the [start,end) range of rows to draw in h lines, keeping
// the cursor visible with minimal scrolling.
func (l *listState) window(h int) (int, int) {
	if h <= 0 {
		return 0, 0
	}
	if l.idx < l.top {
		l.top = l.idx
	}
	if l.idx >= l.top+h {
		l.top = l.idx - h + 1
	}
	if max := len(l.ids) - h; l.top > max {
		l.top = max
	}
	if l.top < 0 {
		l.top = 0
	}
	end := l.top + h
	if end > len(l.ids) {
		end = len(l.ids)
	}
	return l.top, end
}
