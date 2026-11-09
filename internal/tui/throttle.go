package tui

import (
	"time"

	tea "charm.land/bubbletea/v2"
)

// Event-driven re-lists of the big base lists are spaced out (ReloadGap).
// Usage observations alone emit an account.* event every few seconds
// across a fleet, and node heartbeats a node.* event; re-listing every
// account (~150 KB) or every session on each one kept two owner TUIs at
// ~550 accounts.list calls an hour each (2026-10-23). The first event
// after a quiet spell still reloads at once; a burst lands at most one
// re-list per ReloadGap, the last one after the burst. Per-session patches
// (sessions_patch.go) are never delayed.

// throttledKeys are the base keys whose event-driven reloads are spaced.
var throttledKeys = map[string]bool{"accounts": true, "nodes": true, "sessions": true}

type throttleMsg struct{}

// reloadWait is how long key must wait before an event-driven reload (0:
// reload now).
func (m *Model) reloadWait(key string, st *loadState) time.Duration {
	if m.opts.ReloadGap <= 0 || !throttledKeys[key] || st == nil || !st.have || st.loadedWall.IsZero() {
		return 0
	}
	if since := time.Since(st.loadedWall); since < m.opts.ReloadGap {
		return m.opts.ReloadGap - since
	}
	return 0
}

// deferReload schedules one throttleMsg after d (the earliest one pending
// covers every throttled key: each re-checks its own wait).
func (m *Model) deferReload(d time.Duration) tea.Cmd {
	if m.throttlePending {
		return nil
	}
	m.throttlePending = true
	return tea.Tick(d, func(time.Time) tea.Msg { return throttleMsg{} })
}

// reloadStaleBase reloads the stale base keys, deferring throttled ones
// that reloaded within ReloadGap.
func (m *Model) reloadStaleBase() []tea.Cmd {
	var cmds []tea.Cmd
	var wait time.Duration
	for _, k := range baseKeys {
		st := m.loads[k]
		if st == nil || !st.stale {
			continue
		}
		// In flight: reload marks it to go again after (a deferral could
		// let that load clear the stale mark it predates).
		if w := m.reloadWait(k, st); w > 0 && !st.inflight {
			if wait == 0 || w < wait {
				wait = w
			}
			continue
		}
		cmds = append(cmds, m.reload(k, nil))
	}
	if wait > 0 {
		cmds = append(cmds, m.deferReload(wait))
	}
	return cmds
}

func (m *Model) onThrottle() tea.Cmd {
	m.throttlePending = false
	return tea.Batch(m.reloadStaleBase()...)
}
