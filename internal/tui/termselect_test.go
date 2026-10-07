package tui

import "testing"

func TestCellSelSpanBothDirections(t *testing.T) {
	fwd := cellSel{on: true, ax: 3, ay: 1, bx: 2, by: 3}
	back := cellSel{on: true, ax: 2, ay: 3, bx: 3, by: 1}
	for _, s := range []cellSel{fwd, back} {
		for _, c := range []struct{ y, from, to int }{{1, 3, 10}, {2, 0, 10}, {3, 0, 3}} {
			f, to, ok := s.span(c.y, 10)
			if !ok || f != c.from || to != c.to {
				t.Errorf("%+v row %d: %d..%d %v", s, c.y, f, to, ok)
			}
		}
		if _, _, ok := s.span(0, 10); ok {
			t.Errorf("row 0 selected")
		}
	}
}

func TestTermSelectedText(t *testing.T) {
	tv := newTermView("ses_synth", "run_synth", 20, 3, nil)
	defer tv.close()
	tv.feed([]byte("hello synthetic\r\nsecond line"))
	tv.setSel(cellSel{on: true, ax: 6, ay: 0, bx: 5, by: 1})
	if got := tv.selectedText(); got != "synthetic\nsecond" {
		t.Fatalf("selected %q", got)
	}
	if !tv.selectWord(8, 0) || tv.selectedText() != "synthetic" {
		t.Fatalf("word %q", tv.selectedText())
	}
}

func TestFitItemsNeverWraps(t *testing.T) {
	id := func(s ...string) string { return s[0] }
	items := []string{"server ● 5", "mac ●", "laptop ●", "builder ●"}
	if got := fitItems("machines  ", items, " · ", 80, id); got != "machines  server ● 5 · mac ● · laptop ● · builder ●" {
		t.Fatalf("wide: %q", got)
	}
	if got := fitItems("machines  ", items, " · ", 36, id); got != "machines  server ● 5 · mac ● · +2" {
		t.Fatalf("narrow: %q", got)
	}
}
