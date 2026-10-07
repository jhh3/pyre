package tui

import (
	"reflect"
	"strings"
	"testing"
)

func TestLayoutGridShapes(t *testing.T) {
	// A full-screen body (240x65 terminal: 240 x 60 for tiles).
	cases := []struct{ n, cols, rows int }{
		{1, 1, 1}, {2, 2, 1}, {3, 2, 2}, {4, 2, 2}, {5, 3, 2}, {6, 3, 2},
		{7, 4, 2}, {8, 4, 2}, {9, 3, 3}, {10, 4, 3}, {11, 4, 3}, {12, 4, 3},
	}
	for _, c := range cases {
		l := layoutGrid(c.n, 240, 60)
		if l.cols != c.cols || l.rows != c.rows {
			t.Errorf("n=%d: got %dx%d, want %dx%d", c.n, l.cols, l.rows, c.cols, c.rows)
		}
	}
}

// Tiles cover the body exactly: no overlap, one-column gaps, the last
// row's tiles share the full width.
func TestLayoutGridRects(t *testing.T) {
	for _, sz := range [][2]int{{240, 60}, {200, 50}, {119, 29}, {97, 23}} {
		for n := 1; n <= 12; n++ {
			l := layoutGrid(n, sz[0], sz[1])
			cells := make([][]int, sz[1])
			for y := range cells {
				cells[y] = make([]int, sz[0])
			}
			rowEnd := map[int]int{}
			for i := 0; i < n; i++ {
				x, y, w, h := l.rect(i)
				if w <= 0 || h <= 0 || x+w > sz[0] || y+h > sz[1] {
					t.Fatalf("%v n=%d tile %d out of the body: %d,%d %dx%d", sz, n, i, x, y, w, h)
				}
				for yy := y; yy < y+h; yy++ {
					for xx := x; xx < x+w; xx++ {
						if cells[yy][xx] != 0 {
							t.Fatalf("%v n=%d tiles overlap at %d,%d", sz, n, xx, yy)
						}
						cells[yy][xx] = i + 1
					}
				}
				rowEnd[y] = max(rowEnd[y], x+w)
			}
			for y, end := range rowEnd {
				if end != sz[0] {
					t.Errorf("%v n=%d row at %d ends at %d, not the full width", sz, n, y, end)
				}
			}
		}
	}
}

func TestGridPageSize(t *testing.T) {
	for _, c := range []struct{ w, h, want int }{
		{240, 60, 12}, {200, 50, 12}, {120, 29, 8}, {100, 19, 3}, {60, 18, 2}, {30, 8, 1},
	} {
		if got := gridPageSize(c.w, c.h); got != c.want {
			t.Errorf("gridPageSize(%d, %d) = %d, want %d", c.w, c.h, got, c.want)
		}
	}
}

func TestGridLocalAttachIgnoresSize(t *testing.T) {
	d := []string{"/usr/bin/tmux", "-u", "-S", "/run/pyre/tmux.sock", "attach-session", "-t", "=pyre_run_x", "-r"}
	tc, ok := parseTmuxAttach(d)
	if !ok {
		t.Fatal("local tmux attach not recognised")
	}
	got := tc.attachCommand(true)
	want := []string{"/usr/bin/tmux", "-u", "-S", "/run/pyre/tmux.sock", "attach-session", "-t", "=pyre_run_x", "-r", "-f", "ignore-size"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("attach argv:\n got %q\nwant %q", got, want)
	}
	if old := tc.attachCommand(false); strings.Contains(strings.Join(old, " "), "ignore-size") || old[len(old)-1] != "-r" {
		t.Fatalf("tmux < 3.2 has no -f: %q", old)
	}
	probe := tc.probeCommand()
	if !reflect.DeepEqual(probe[:5], []string{"/usr/bin/tmux", "-u", "-S", "/run/pyre/tmux.sock", "display-message"}) || probe[len(probe)-1] != gridSizeFormat {
		t.Fatalf("probe argv: %q", probe)
	}
}

func TestGridRemoteAttachIgnoresSize(t *testing.T) {
	// What tmux.RemoteAttachArgv builds.
	d := []string{"ssh", "-t", "server", `bash -lc '/usr/bin/tmux -u -S /home/u/.local/state/pyre/tmux.sock attach-session -t =pyre_run_y -r'`}
	tc, ok := parseTmuxAttach(d)
	if !ok {
		t.Fatal("ssh tmux attach not recognised")
	}
	got := tc.attachCommand(true)
	if got[0] != "ssh" || got[1] != "-t" || got[2] != "server" {
		t.Fatalf("ssh prefix: %q", got)
	}
	words, ok := shellWords(got[3])
	if !ok || len(words) != 3 || words[0] != "bash" || words[1] != "-lc" {
		t.Fatalf("remote command: %q", got[3])
	}
	inner, _ := shellWords(words[2])
	want := []string{"/usr/bin/tmux", "-u", "-S", "/home/u/.local/state/pyre/tmux.sock", "attach-session", "-t", "=pyre_run_y", "-r", "-f", "ignore-size"}
	if !reflect.DeepEqual(inner, want) {
		t.Fatalf("remote tmux argv:\n got %q\nwant %q", inner, want)
	}
	probe := tc.probeCommand()
	if strings.Contains(strings.Join(probe[:len(probe)-1], " "), "-t server") || probe[1] != "-o" || probe[2] != "BatchMode=yes" {
		t.Fatalf("probe runs without a tty, non-interactively: %q", probe)
	}
	w, _ := shellWords(probe[len(probe)-1])
	in, _ := shellWords(w[2])
	if in[len(in)-1] != gridSizeFormat || in[4] != "display-message" {
		t.Fatalf("remote probe: %q", in)
	}
}

func TestGridUnrecognisedCommands(t *testing.T) {
	for _, c := range [][]string{
		{"pyre-synthetic-term", "ses_x", "run_x", "-r"},
		{"tmux", "new-session", "-s", "x"},
		{"tmux", "attach-session"}, // no target
		{"ssh", "-t", "host", "echo hi"},
		{},
	} {
		if _, ok := parseTmuxAttach(c); ok {
			t.Errorf("%q should not parse as a tmux attach", c)
		}
	}
}

func TestParseWindowSize(t *testing.T) {
	for _, c := range []struct {
		in         string
		cols, rows int
		ignore     bool
	}{
		{"200 50 on 3.4\n", 200, 51, true},
		{"120 40 off 3.6", 120, 40, true},
		{"80 24 2 3.2a", 80, 26, true},
		{"80 24 on 3.1c", 80, 25, false},
		{"80 24 on ", 80, 25, false},
		{"80 24 on next-3.7", 80, 25, true},
	} {
		cols, rows, ig, err := parseWindowSize(c.in)
		if err != nil || cols != c.cols || rows != c.rows || ig != c.ignore {
			t.Errorf("parseWindowSize(%q) = %d %d %v %v", c.in, cols, rows, ig, err)
		}
	}
	if _, _, _, err := parseWindowSize("no server running"); err == nil {
		t.Error("garbage must fail (never attach at a guessed size)")
	}
}

func TestShellWords(t *testing.T) {
	got, ok := shellWords(`a 'b c' "d \"e\"" f\ g 'it'"'"'s'`)
	want := []string{"a", "b c", `d "e"`, "f g", "it's"}
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("shellWords = %q %v", got, ok)
	}
	if _, ok := shellWords(`'unbalanced`); ok {
		t.Fatal("unbalanced quotes must fail")
	}
	round, _ := shellWords(shellJoinArgs([]string{"x y", "it's", "#{a} #{b}", "plain"}))
	if !reflect.DeepEqual(round, []string{"x y", "it's", "#{a} #{b}", "plain"}) {
		t.Fatalf("round trip: %q", round)
	}
}
