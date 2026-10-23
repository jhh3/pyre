package tui

import (
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
)

// Monitors (contracts/monitors.go) are not rows: the hub leaves them out
// of sessions.list and lists the live ones on the session they wake, which
// shows "watching N" after its title and their titles in its facts.

// withoutMonitors drops monitor rows (a hub older than include_monitors
// lists them).
func withoutMonitors(ss []contracts.SessionView) []contracts.SessionView {
	kept := ss[:0]
	for _, s := range ss {
		if !s.Session.IsMonitor() {
			kept = append(kept, s)
		}
	}
	return kept
}

// watchPhrase is the row badge for a session that monitors wake.
func watchPhrase(s contracts.SessionView) string {
	if n := len(s.Monitors); n > 0 {
		return "watching " + itoa(n)
	}
	return ""
}

// rowPhrase is the faint suffix after a row's title: what the session is
// doing, then its monitor badge.
func (m *Model) rowPhrase(s contracts.SessionView) (string, bool) {
	phrase, _, ok := sessionDoing(s, m.nodeOf(s))
	if w := watchPhrase(s); w != "" {
		if ok {
			return phrase + " · " + w, true
		}
		return w, true
	}
	return phrase, ok
}

// monitorFacts are the facts lines about monitors: the ones that wake s,
// or (s is a monitor) the session it wakes.
func (m *Model) monitorFacts(s contracts.SessionView, lw, w int) []string {
	var out []string
	if len(s.Monitors) > 0 {
		titles := make([]string, len(s.Monitors))
		for i, mon := range s.Monitors {
			titles[i] = strings.TrimPrefix(mon.Title, contracts.MonitorTitlePrefix)
		}
		out = append(out, m.kvFit("Watching", strings.Join(titles, m.th.dim.Render(" · ")), lw, w)...)
	}
	if t := s.Session.MonitorTarget(); t != "" {
		out = append(out, m.kvFit("Monitor", "wakes "+m.sessionTitle(t), lw, w)...)
	}
	return out
}

// patchMonitorBadges updates, in ss, the Monitors of the sessions that the
// patched monitor views mons wake: a live monitor is added or updated, an
// ended or archived one removed.
func patchMonitorBadges(ss []contracts.SessionView, mons []contracts.SessionView) {
	for _, mon := range mons {
		target := mon.Session.MonitorTarget()
		if target == "" {
			continue
		}
		for i := range ss {
			if ss[i].Session.SessionID != target {
				continue
			}
			live := !mon.Session.Archived && mon.Run != nil && contracts.MonitorLive(mon.Run.State)
			next := make([]contracts.MonitorRef, 0, len(ss[i].Monitors)+1)
			found := false
			for _, r := range ss[i].Monitors {
				if r.SessionID != mon.Session.SessionID {
					next = append(next, r)
					continue
				}
				found = true
				if live {
					next = append(next, contracts.MonitorRef{SessionID: r.SessionID, Title: mon.Session.Title, State: mon.Run.State})
				}
			}
			if live && !found {
				next = append(next, contracts.MonitorRef{SessionID: mon.Session.SessionID, Title: mon.Session.Title, State: mon.Run.State})
			}
			if len(next) == 0 {
				next = nil
			}
			ss[i].Monitors = next
			break
		}
	}
}
