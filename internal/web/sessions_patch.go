package web

import (
	"context"
	"sort"
	"sync"

	c "github.com/jhh3/pyre/internal/contracts"
)

// Per-session refresh. A hub event that names a session marks just that
// session dirty; the next reader fetches the dirty sessions with one
// sessions.get_many and patches the cached list instead of re-listing
// every session. Run observations alone arrive about once a second across
// the fleet, and each used to cost a ~150 KB sessions.list as soon as a
// browser refetched (2026-10-23: ~2700 lists an hour). Events without a
// session, a full fetch due by age, too many dirty sessions, or a hub
// without get_many take the full list as before.

// sessPatch tracks dirty sessions between reads.
type sessPatch struct {
	mu      sync.Mutex // serialises patches (one get_many at a time)
	dirtyMu sync.Mutex
	dirty   map[string]bool
	noBatch bool // the hub lacks sessions.get_many (guarded by dirtyMu)
}

// markSessionDirty records an event about one session.
func (s *Server) markSessionDirty(id string) {
	s.sessPatch.dirtyMu.Lock()
	noBatch := s.sessPatch.noBatch
	if !noBatch {
		if s.sessPatch.dirty == nil {
			s.sessPatch.dirty = map[string]bool{}
		}
		s.sessPatch.dirty[id] = true
	}
	s.sessPatch.dirtyMu.Unlock()
	if noBatch {
		s.sessions.invalidate()
	}
}

// takeDirty empties the dirty set and returns its ids, sorted.
func (s *Server) takeDirty() []string {
	s.sessPatch.dirtyMu.Lock()
	defer s.sessPatch.dirtyMu.Unlock()
	ids := make([]string, 0, len(s.sessPatch.dirty))
	for id := range s.sessPatch.dirty {
		ids = append(ids, id)
	}
	s.sessPatch.dirty = nil
	sort.Strings(ids)
	return ids
}

// patchedSessions returns the cached list with the dirty sessions
// refreshed by one get_many; ok=false means the caller takes the full
// path (cache refetch when stale or invalidated).
func (s *Server) patchedSessions(ctx context.Context) ([]c.SessionView, bool) {
	s.sessPatch.mu.Lock()
	defer s.sessPatch.mu.Unlock()
	list, gen, fresh := s.sessions.peekFresh(s.now())
	if !fresh {
		return nil, false // a full fetch is due anyway (and clears the dirty set)
	}
	ids := s.takeDirty()
	if len(ids) == 0 {
		return list, true
	}
	b, ok := s.api.(c.SessionBatchAPI)
	if !ok || len(ids) > c.MaxGetManySessions {
		s.sessions.invalidate()
		return nil, false
	}
	resp, err := b.GetManySessions(ctx, c.GetManySessionsRequest{IDs: ids})
	if err != nil {
		if c.IsCode(err, c.ErrUnsupported) || (c.IsCode(err, c.ErrNotFound) && c.AsError(err).Ref == nil) {
			s.sessPatch.dirtyMu.Lock()
			s.sessPatch.noBatch = true
			s.sessPatch.dirtyMu.Unlock()
		}
		s.sessions.invalidate()
		return nil, false
	}
	patched := patchSessionList(list, resp)
	if !s.sessions.patch(gen, patched) {
		return nil, false // invalidated meanwhile: the full fetch covers these ids
	}
	return patched, true
}

// patchSessionList returns list with resp applied: rows replaced, archived,
// monitor and missing rows removed, new rows added in the hub's order
// (newest first). list itself is not modified.
func patchSessionList(list []c.SessionView, resp c.GetManySessionsResponse) []c.SessionView {
	type rec struct {
		v    c.SessionView
		gone bool
	}
	recs := make(map[string]rec, len(resp.Items)+len(resp.Missing))
	for _, v := range resp.Items {
		recs[v.Session.SessionID] = rec{v: v, gone: v.Session.Archived || v.Session.IsMonitor()}
	}
	for _, id := range resp.Missing {
		recs[id] = rec{gone: true}
	}
	out := make([]c.SessionView, 0, len(list)+len(resp.Items))
	seen := make(map[string]bool, len(recs))
	for _, v := range list {
		id := v.Session.SessionID
		r, ok := recs[id]
		if !ok {
			out = append(out, v)
			continue
		}
		seen[id] = true
		if !r.gone {
			out = append(out, r.v)
		}
	}
	added := false
	for _, v := range resp.Items {
		id := v.Session.SessionID
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
