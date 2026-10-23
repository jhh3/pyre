package tui_test

import (
	"fmt"
	"strings"
	"testing"
)

// TestGoldenScreens snapshots key screens over the synthetic fixture data
// (mock backend, fake clock). Regenerate with:
//
//	go test ./tests/tui -run TestGoldenScreens -update
func TestGoldenScreens(t *testing.T) {
	type screen struct {
		name   string
		keys   []string
		widths []int
	}
	all := []int{60, 100, 160}
	screens := []screen{
		{"sessions", nil, all},
		// The sidebar footer at sidebar widths 56, 64, 72 and 80 (W-archive2).
		{"sidebar-foot", nil, []int{100, 128, 144, 223}},
		{"sessions-filtered", []string{"/", "l", "i", "n", "u", "x", "enter"}, []int{100}},
		{"sessions-table", []string{"v"}, []int{100, 120, 200}},
		{"sessions-attention", []string{":", "group attention", "enter"}, []int{100}},
		{"sessions-sort-attention", []string{"g", "s"}, []int{100}},
		{"session-approval", []string{"ctrl+p", "A", "p", "p", "r", "o", "v", "e", "enter", "a"}, all},
		{"projects", []string{"2"}, []int{100}},
		{"project-home", []string{"2", "enter"}, all},
		{"project-docs", []string{"2", "enter", "right"}, []int{100}},
		{"artifact-review", []string{"2", "enter", "right", "right", "enter", "down"}, []int{100, 160}},
		{"messages-indeterminate", []string{"/", "w", "o", "r", "k", "e", "r", "enter", "M"}, []int{160}},
		{"palette", []string{":"}, all},
		{"help", []string{"?"}, []int{100}},
		{"help-agent-commands", []string{"?", "agent commands"}, all},
		{"machines", []string{":", "M", "a", "c", "h", "i", "n", "e", "s", "enter"}, []int{100}},
		{"machines-stats", []string{":", "M", "a", "c", "h", "i", "n", "e", "s", "enter", "down"}, []int{160}},
		{"sessions-usage", []string{"u"}, []int{160}},
		{"accounts", []string{":", "A", "c", "c", "o", "u", "n", "t", "s", "enter"}, []int{100, 120, 160, 200}},
		{"activity", []string{":", "A", "c", "t", "i", "v", "i", "t", "y", "enter"}, []int{100, 160}},
		{"activity-accounts", []string{":", "A", "c", "c", "o", "u", "n", "t", "s", "enter", "a", "down"}, []int{100, 160}},
		{"connections", []string{":", "C", "o", "n", "n", "e", "c", "t", "enter"}, []int{100}},
		{"environments", []string{":", "E", "n", "v", "i", "r", "enter"}, []int{100}},
		{"launch-review", []string{"n", "ctrl+a", "down", "enter", "enter", "enter", "enter"}, []int{100}},
		{"reconcile", []string{"/", "w", "o", "r", "k", "e", "r", "enter", "M", "down", "down", "down", "down", "down", "R", "d"}, []int{100}},
	}
	for _, sc := range screens {
		for _, w := range sc.widths {
			name := fmt.Sprintf("%s-%d", sc.name, w)
			t.Run(name, func(t *testing.T) {
				t.Parallel() // each screen has its own harness and mock; the time is all Settle waits
				h, _ := start(t, w, 30, nil, nil)
				for _, k := range sc.keys {
					h.Key(k)
					h.Settle()
				}
				h.Settle()
				v := h.View()
				if strings.Contains(v, "loading") {
					h.Settle()
					v = h.View()
				}
				golden(t, name, v)
			})
		}
	}
}
