package tui

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/contracts"
)

// Per-session refresh (W-delta). Events that name a session mark
// "session:<id>" dirty instead of the whole "sessions" key; flushEvents
// fetches just those sessions with one sessions.get_many and patches the
// cached list in place. Re-listing every session on every run tick pinned
// the hub (~80 live sessions, 180KB per sessions.list, 2026-10-08).
//
// A hub without sessions.get_many (404 / unsupported) falls back to the
// full reload for the rest of the TUI's life.

const sessionKeyPrefix = "session:"

// sessionsSafetyNet is how old the full session list may get while the
// event stream is connected before the periodic tick re-lists it anyway.
func (m *Model) sessionsSafetyNet() time.Duration {
	return max(5*time.Minute, m.opts.RefreshInterval*6)
}

// sessPatchState tracks the per-session refresh path.
type sessPatchState struct {
	unsupported bool // the hub lacks sessions.get_many: full reloads only
	inflight    bool
	// overlay holds rows patched while a full load was in flight (for load
	// seq overlaySeq): that load's snapshot may predate them, so they are
	// re-applied when it lands.
	overlay    map[string]sessPatchRec
	overlaySeq int
}

type sessPatchRec struct {
	v    contracts.SessionView
	gone bool
}

type sessPatchMsg struct {
	ids  []string
	resp contracts.GetManySessionsResponse
	err  error
}

// takeSessionIDs removes the "session:<id>" keys from dirty and returns
// their ids, sorted for a stable request.
func takeSessionIDs(dirty map[string]bool) []string {
	var ids []string
	for k := range dirty {
		if id, ok := strings.CutPrefix(k, sessionKeyPrefix); ok {
			delete(dirty, k)
			if id != "" {
				ids = append(ids, id)
			}
		}
	}
	sort.Strings(ids)
	return ids
}

// patchSessions starts one sessions.get_many for ids. It reports false
// when the caller must fall back to a full reload of "sessions".
func (m *Model) patchSessions(ids []string) (tea.Cmd, bool) {
	b, ok := m.api.(contracts.SessionBatchAPI)
	if !ok || m.sessPatch.unsupported || len(ids) > contracts.MaxGetManySessions {
		return nil, false
	}
	st := m.loads["sessions"]
	if st == nil || !st.have {
		return nil, false
	}
	if m.sessPatch.inflight {
		// One patch at a time: the ids ride the next debounce window,
		// scheduled when the current patch lands.
		if m.ev.dirty == nil {
			m.ev.dirty = map[string]bool{}
		}
		for _, id := range ids {
			m.ev.dirty[sessionKeyPrefix+id] = true
		}
		return nil, true
	}
	m.sessPatch.inflight = true
	m.stats.Patches++
	parent, timeout := m.ctx, m.opts.LoadTimeout
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, timeout)
		defer cancel()
		resp, err := b.GetManySessions(ctx, contracts.GetManySessionsRequest{IDs: ids})
		if err != nil && errors.Is(err, context.DeadlineExceeded) {
			err = fmt.Errorf("timed out after %s", timeout)
		}
		return sessPatchMsg{ids: ids, resp: resp, err: err}
	}, true
}

// getManyUnsupported reports whether err means the hub lacks the op (a 404
// without an object ref is an unknown route, not an unknown session).
func getManyUnsupported(err error) bool {
	if contracts.IsCode(err, contracts.ErrUnsupported) {
		return true
	}
	var ce *contracts.Error
	return contracts.IsCode(err, contracts.ErrNotFound) && !(errors.As(err, &ce) && ce.Ref != nil)
}

func (m *Model) onSessPatch(msg sessPatchMsg) tea.Cmd {
	m.sessPatch.inflight = false
	var cmds []tea.Cmd
	if msg.err != nil {
		if getManyUnsupported(msg.err) {
			m.sessPatch.unsupported = true
		}
		// Unsupported: reload now (once). Anything else (a loaded hub timing
		// out): mark the list stale so the next periodic tick re-lists it,
		// rather than adding a full list request to an overloaded hub.
		if st := m.loads["sessions"]; st != nil {
			st.stale = true
			if m.sessPatch.unsupported {
				cmds = append(cmds, m.reload("sessions", nil))
			}
		}
	} else {
		m.applySessionPatch(msg.resp)
		cmds = append(cmds, m.afterData("sessions"))
	}
	if !m.ev.pending {
		for k := range m.ev.dirty {
			if strings.HasPrefix(k, sessionKeyPrefix) {
				m.ev.pending = true
				cmds = append(cmds, tea.Tick(m.opts.Debounce, func(time.Time) tea.Msg { return debounceMsg{} }))
				break
			}
		}
	}
	return tea.Batch(cmds...)
}

// applySessionPatch merges a get_many answer into the cached list.
func (m *Model) applySessionPatch(resp contracts.GetManySessionsResponse) {
	st := m.loads["sessions"]
	if st == nil || !st.have {
		return
	}
	ss, _ := st.val.([]contracts.SessionView)
	recs := make(map[string]sessPatchRec, len(resp.Items)+len(resp.Missing))
	order := make([]string, 0, len(resp.Items))
	for _, v := range resp.Items {
		id := v.Session.SessionID
		// The list excludes archived sessions (ListSessions default).
		recs[id] = sessPatchRec{v: v, gone: v.Session.Archived}
		order = append(order, id)
	}
	for _, id := range resp.Missing {
		recs[id] = sessPatchRec{gone: true}
	}
	st.val = patchSessionList(ss, recs, order)
	st.rev++
	if st.inflight {
		if m.sessPatch.overlay == nil || m.sessPatch.overlaySeq != st.seq {
			m.sessPatch.overlay = map[string]sessPatchRec{}
			m.sessPatch.overlaySeq = st.seq
		}
		for id, r := range recs {
			m.sessPatch.overlay[id] = r
		}
	}
}

// reapplyOverlay patches a just-landed full list with the rows patched
// while it was in flight.
func (m *Model) reapplyOverlay(seq int, ss []contracts.SessionView) []contracts.SessionView {
	ov := m.sessPatch.overlay
	match := ov != nil && m.sessPatch.overlaySeq == seq
	m.sessPatch.overlay = nil
	if !match {
		return ss
	}
	order := make([]string, 0, len(ov))
	for id, r := range ov {
		if !r.gone {
			order = append(order, id)
		}
	}
	sort.Strings(order)
	return patchSessionList(ss, ov, order)
}

// patchSessionList returns ss with recs applied: matching rows replaced,
// gone rows removed, unknown rows (in order) added; the result keeps the
// hub's list order (newest first).
func patchSessionList(ss []contracts.SessionView, recs map[string]sessPatchRec, order []string) []contracts.SessionView {
	out := make([]contracts.SessionView, 0, len(ss)+len(order))
	seen := make(map[string]bool, len(recs))
	for _, s := range ss {
		id := s.Session.SessionID
		r, ok := recs[id]
		if !ok {
			out = append(out, s)
			continue
		}
		seen[id] = true
		if !r.gone {
			out = append(out, r.v)
		}
	}
	added := false
	for _, id := range order {
		if r := recs[id]; !seen[id] && !r.gone {
			seen[id] = true
			out = append(out, r.v)
			added = true
		}
	}
	if added {
		sort.SliceStable(out, func(i, j int) bool {
			a, b := out[i].Session, out[j].Session
			if !a.CreatedAt.Equal(b.CreatedAt) {
				return a.CreatedAt.After(b.CreatedAt)
			}
			return a.SessionID > b.SessionID
		})
	}
	return out
}
