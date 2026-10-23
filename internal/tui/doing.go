package tui

import (
	"fmt"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// "Doing now": what a working session is actually doing ("running tests",
// "editing main.go"), from Run.Activity (node-derived, ≤40 chars). Shown
// as a faint suffix after the title in the Sessions list and table (the
// status column is too narrow for it), and with its elapsed time in the
// session header and info overlay.

// sessionDoing returns the phrase and when it began for a session that is
// working right now (not offline, stale, a shell, or idle).
func sessionDoing(s contracts.SessionView, node *contracts.Node) (string, time.Time, bool) {
	if s.Run == nil || s.Run.Activity == nil || s.Run.Activity.Text == "" {
		return "", time.Time{}, false
	}
	if sessionStatus(s, node).label != "working" {
		return "", time.Time{}, false
	}
	return s.Run.Activity.Text, s.Run.Activity.Since, true
}

// titleDoingCell is titleCell plus a faint " · <doing>" suffix fitted into
// the same width w. The title keeps at least three fifths of the cell; the
// suffix is dropped when too little room is left for it to read.
func (m *Model) titleDoingCell(sel bool, s contracts.SessionView, w int) string {
	title := iconTitle(s)
	phrase, ok := m.rowPhrase(s) // doing, then "watching N" (monitors.go)
	if !ok {
		return m.titleCell(sel, title, w)
	}
	suffix := " · " + phrase
	tw := min(width(title), max(w-width(suffix), w*3/5))
	rest := w - tw
	if rest < 8 {
		return m.titleCell(sel, title, w)
	}
	return m.titleCell(sel, title, tw) + m.th.faint.Render(pad(suffix, rest))
}

// doingStatus is the status for a session header: "● running tests ·
// 1m 20s" while it works on something known, else the plain status.
func (m *Model) doingStatus(s contracts.SessionView, st status) string {
	phrase, since, ok := sessionDoing(s, m.nodeOf(s))
	if !ok {
		return m.th.status(st)
	}
	out := m.th.tone(st.tone, st.sym) + " " + phrase
	if !since.IsZero() {
		out += m.th.dim.Render(" · " + elapsed(m.now().Sub(since)))
	}
	return out
}

// elapsed renders a running duration: "45s", "1m 20s", "2h 5m".
func elapsed(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm %ds", int(d/time.Minute), int(d%time.Minute/time.Second))
	}
	return fmt.Sprintf("%dh %dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
}
