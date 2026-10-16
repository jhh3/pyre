package tui

import (
	"context"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/contracts"
)

// evState tracks the live event watcher and pending invalidations.
type evState struct {
	ch        chan contracts.Event
	done      chan error
	cursor    int64
	connected bool
	err       string
	pending   bool            // a debounce tick is scheduled
	dirty     map[string]bool // keys/prefixes to refresh at the next flush
	gen       int
}

type eventsMsg struct {
	gen    int
	events []contracts.Event
}

type watchEndedMsg struct {
	gen int
	err error
}

type watchRestartMsg struct{}

const maxBatch = 512

// startWatch launches the watcher goroutine (if a WatchFunc is configured)
// and returns a command that waits for the first batch of events.
func (m *Model) startWatch() tea.Cmd {
	if m.opts.Watch == nil {
		return nil
	}
	m.ev.gen++
	gen := m.ev.gen
	ch := make(chan contracts.Event, 256)
	done := make(chan error, 1)
	m.ev.ch, m.ev.done = ch, done
	watch, ctx, api, cursor := m.opts.Watch, m.ctx, m.api, m.ev.cursor
	go func() {
		if cursor == 0 {
			// Start at the current head: the snapshot loads already show
			// current state, so replaying history would only add noise.
			hctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			if resp, err := api.ListEvents(hctx, contracts.ListEventsRequest{AfterCursor: 0, Limit: 1}); err == nil {
				cursor = resp.HeadCursor
			}
			cancel()
		}
		err := watch(ctx, cursor, func(ev contracts.Event) error {
			select {
			case ch <- ev:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		done <- err
	}()
	m.ev.connected = true
	m.ev.err = ""
	return waitEvents(gen, ch, done)
}

// waitEvents blocks for at least one event, then drains what is already
// queued so a burst arrives as a single message.
func waitEvents(gen int, ch chan contracts.Event, done chan error) tea.Cmd {
	return func() tea.Msg {
		var batch []contracts.Event
		select {
		case ev := <-ch:
			batch = append(batch, ev)
		case err := <-done:
			return watchEndedMsg{gen: gen, err: err}
		}
		for len(batch) < maxBatch {
			select {
			case ev := <-ch:
				batch = append(batch, ev)
			default:
				return eventsMsg{gen: gen, events: batch}
			}
		}
		return eventsMsg{gen: gen, events: batch}
	}
}

func (m *Model) onEvents(msg eventsMsg) tea.Cmd {
	if msg.gen != m.ev.gen {
		return nil
	}
	if m.ev.dirty == nil {
		m.ev.dirty = map[string]bool{}
	}
	for _, ev := range msg.events {
		m.stats.Events++
		if ev.Cursor > m.ev.cursor {
			m.ev.cursor = ev.Cursor
		}
		for _, k := range invalidationKeys(ev) {
			m.ev.dirty[k] = true
		}
	}
	m.ev.connected = true
	m.ev.err = ""
	cmds := []tea.Cmd{waitEvents(msg.gen, m.ev.ch, m.ev.done)}
	if !m.ev.pending {
		m.ev.pending = true
		cmds = append(cmds, tea.Tick(m.opts.Debounce, func(time.Time) tea.Msg { return debounceMsg{} }))
	}
	return tea.Batch(cmds...)
}

// invalidationKeys maps an event to the cached data it may change. Events
// are only invalidation signals: canonical state is always re-read, never
// reconstructed from (possibly stale or replayed) payloads.
func invalidationKeys(ev contracts.Event) []string {
	var keys []string
	add := func(k ...string) { keys = append(keys, k...) }
	// sessions marks the one session the event names (patched in place by
	// flushEvents, sessions_patch.go), or the whole list when it names none.
	sessions := func() {
		if ev.SessionID != "" {
			add(sessionKeyPrefix + ev.SessionID)
		} else {
			add("sessions")
		}
	}
	k := string(ev.Kind)
	switch {
	case strings.HasPrefix(k, "node."):
		add("nodes", "sessions")
	case strings.HasPrefix(k, "session.") || strings.HasPrefix(k, "run.") || k == string(contracts.EvLeaseChanged):
		sessions()
		if ev.SessionID != "" {
			add("preview:" + ev.SessionID)
		}
	case strings.HasPrefix(k, "message."):
		sessions()
		if ev.SessionID != "" {
			add("messages:" + ev.SessionID)
		}
		add("messages:")
		if ev.ProjectID != nil {
			add("proj:" + *ev.ProjectID + ":thread")
		}
	case strings.HasPrefix(k, "approval."):
		add("attention", "approval:")
		sessions()
	case k == string(contracts.EvAttentionChanged):
		// Session flags are computed from attention items by the hub.
		add("attention", "approval:", ownerInboxKey)
		sessions()
	case k == string(contracts.EvAskChanged):
		add(asksKey) // the ask signals (asks.go)
	case k == string(contracts.EvProjectChanged):
		add("projects")
	case strings.HasPrefix(k, "document."):
		add("doc:")
		if ev.ProjectID != nil {
			add("proj:" + *ev.ProjectID + ":docs")
		}
	case strings.HasPrefix(k, "artifact."):
		add("art:", "artifact:")
		if ev.ProjectID != nil {
			add("proj:" + *ev.ProjectID + ":arts")
		}
	case strings.HasPrefix(k, "comment."):
		add("art:")
		if ev.ProjectID != nil {
			add("proj:" + *ev.ProjectID + ":comments")
		}
	case strings.HasPrefix(k, "account."):
		add("accounts", "attention", "fallbacks:")
	case strings.HasPrefix(k, "environment.") || strings.HasPrefix(k, "update."):
		add("env:")
	}
	if strings.HasPrefix(k, "account.") || strings.HasPrefix(k, "session.") || strings.HasPrefix(k, "node.") {
		add("audit:") // Activity and RECENT SWITCHES (activity.go)
	}
	return keys
}

// flushEvents runs at most once per debounce window: it marks every cached
// key matching a dirty key/prefix as stale and reloads the ones that are
// on screen (base data plus the current view). Off-screen keys reload
// lazily when next shown.
func (m *Model) flushEvents() tea.Cmd {
	m.ev.pending = false
	if len(m.ev.dirty) == 0 {
		return nil
	}
	m.stats.Refreshes++
	dirty := m.ev.dirty
	m.ev.dirty = nil
	var cmds []tea.Cmd
	if ids := takeSessionIDs(dirty); len(ids) > 0 && !dirty["sessions"] {
		if cmd, ok := m.patchSessions(ids); ok {
			cmds = append(cmds, cmd)
		} else {
			dirty["sessions"] = true
		}
	}
	matches := func(key string) bool {
		if dirty[key] {
			return true
		}
		for d := range dirty {
			if strings.HasSuffix(d, ":") && strings.HasPrefix(key, d) {
				return true
			}
		}
		return false
	}
	for key, st := range m.loads {
		if matches(key) {
			st.stale = true
		}
	}
	for _, k := range baseKeys {
		if st := m.loads[k]; st != nil && st.stale {
			cmds = append(cmds, m.reload(k, nil))
		}
	}
	cmds = append(cmds, m.refreshView(false)...)
	return tea.Batch(cmds...)
}

func (m *Model) onWatchEnded(msg watchEndedMsg) tea.Cmd {
	if msg.gen != m.ev.gen || m.ctx.Err() != nil {
		return nil
	}
	m.ev.connected = false
	if msg.err == nil {
		m.ev.err = "stream ended; reconnecting"
	} else {
		m.ev.err = errText(msg.err) + "; periodic refresh continues"
	}
	var cmds []tea.Cmd
	if contracts.IsCode(msg.err, contracts.ErrResetRequired) {
		// Our cursor is older than retention: re-snapshot everything and
		// resume from the snapshot cursor the hub reported.
		ce := contracts.AsError(msg.err)
		m.ev.cursor = 0
		if v, ok := ce.Details["snapshot_cursor"]; ok {
			switch n := v.(type) {
			case float64:
				m.ev.cursor = int64(n)
			case int64:
				m.ev.cursor = n
			case int:
				m.ev.cursor = int64(n)
			}
		}
		for _, st := range m.loads {
			st.stale = true
		}
		cmds = append(cmds, m.refreshVisible(true))
		m.ev.err = "event history reset; re-synced"
	}
	cmds = append(cmds, tea.Tick(m.opts.WatchRetry, func(time.Time) tea.Msg { return watchRestartMsg{} }))
	return tea.Batch(cmds...)
}
