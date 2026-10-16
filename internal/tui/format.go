package tui

import (
	"fmt"
	"github.com/jhh3/pyre/internal/contracts"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// age renders a compact duration since t ("now", "4s", "7m", "3h", "2d").
func age(now, t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := now.Sub(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Second:
		return "now"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
}

// ago is age with an " ago" suffix where meaningful.
func ago(now, t time.Time) string {
	a := age(now, t)
	if a == "—" || a == "now" {
		return a
	}
	return a + " ago"
}

// until renders time remaining until t ("in 3h"), or "passed".
func until(now, t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	if !t.After(now) {
		return "passed"
	}
	return "in " + age(now, now.Add(-t.Sub(now)))
}

func ansiStrip(s string) string { return ansi.Strip(s) }

// width is the printable width of s (ANSI- and wide-rune-aware). Every
// frame measures the same long lines (terminal rows, list rows) several
// times while fitting panes, and grapheme-aware measuring dominates a
// frame's cost, so long lines' widths are memoised in a small bounded cache.
func width(s string) int {
	if len(s) < widthMemoMin || len(s) > widthMemoMax {
		return ansi.StringWidth(s)
	}
	widthMemo.Lock()
	n, ok := widthMemo.m[s]
	widthMemo.Unlock()
	if ok {
		return n
	}
	n = ansi.StringWidth(s)
	widthMemo.Lock()
	if widthMemo.m == nil || len(widthMemo.m) >= widthMemoCap {
		widthMemo.m = make(map[string]int, widthMemoCap)
	}
	widthMemo.m[s] = n
	widthMemo.Unlock()
	return n
}

const (
	widthMemoMin = 24
	widthMemoMax = 8 << 10
	widthMemoCap = 1024 // a few frames' worth of lines; bounded memory
)

var widthMemo struct {
	sync.Mutex
	m map[string]int
}

// trunc cuts s to at most w cells, ending with "…" when shortened.
func trunc(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if width(s) <= w {
		return s
	}
	return ansi.Truncate(s, w, "…")
}

// pad truncates or right-pads s to exactly w cells.
func pad(s string, w int) string {
	n := width(s)
	if n == w {
		return s
	}
	if n > w {
		s = trunc(s, w)
		n = width(s)
	}
	if n < w {
		s += strings.Repeat(" ", w-n)
	}
	return s
}

// padLeft truncates or left-pads s to exactly w cells.
func padLeft(s string, w int) string {
	s = trunc(s, w)
	if n := w - width(s); n > 0 {
		s = strings.Repeat(" ", n) + s
	}
	return s
}

// wrap word-wraps text to w cells, preserving paragraph breaks.
func wrap(text string, w int) []string {
	if w <= 0 {
		return nil
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\t", "    ")
	var out []string
	for _, para := range strings.Split(text, "\n") {
		if para == "" {
			out = append(out, "")
			continue
		}
		out = append(out, strings.Split(ansi.Wrap(para, w, ""), "\n")...)
	}
	return out
}

// fit returns exactly h lines of exactly w cells each.
func fit(lines []string, w, h int) []string {
	out := make([]string, 0, h)
	for i := 0; i < h; i++ {
		if i < len(lines) {
			out = append(out, pad(lines[i], w))
		} else {
			out = append(out, strings.Repeat(" ", w))
		}
	}
	return out
}

// hjoin places two fitted columns side by side with a separator.
func hjoin(left []string, lw int, sep string, right []string, rw int) []string {
	h := len(left)
	if len(right) > h {
		h = len(right)
	}
	l := fit(left, lw, h)
	r := fit(right, rw, h)
	out := make([]string, h)
	for i := range out {
		out[i] = l[i] + sep + r[i]
	}
	return out
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

func shortID(id string) string {
	if len(id) <= 18 {
		return id
	}
	return id[:17] + "…"
}

func humanBytes(n int64) string {
	const k = 1024
	switch {
	case n < k:
		return fmt.Sprintf("%d B", n)
	case n < k*k:
		return fmt.Sprintf("%.1f KiB", float64(n)/k)
	case n < k*k*k:
		return fmt.Sprintf("%.1f MiB", float64(n)/(k*k))
	}
	return fmt.Sprintf("%.1f GiB", float64(n)/(k*k*k))
}

// matchQuery reports whether every space-separated term of q appears
// (case-insensitively) in one of the fields.
func matchQuery(q string, fields ...string) bool {
	q = strings.TrimSpace(strings.ToLower(q))
	if q == "" {
		return true
	}
	hay := strings.ToLower(strings.Join(fields, " \x00 "))
	for _, term := range strings.Fields(q) {
		if !strings.Contains(hay, term) {
			return false
		}
	}
	return true
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// iconTitle is a session's title with its emoji, if any, before it
// (W-archive2). Width math goes through width(), which counts a wide emoji
// as two cells.
func iconTitle(s contracts.SessionView) string {
	if s.Session.Icon == "" {
		return s.Session.Title
	}
	return s.Session.Icon + " " + s.Session.Title
}
