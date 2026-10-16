package hub

import (
	"context"
	"database/sql"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/persistence"
)

// Owner activity (additive): Session.OwnerActiveAt is when the owner last
// messaged or typed into the session, so clients can mark the sessions the
// owner recently worked with. Typing calls this per keystroke batch, so the
// write is throttled and runs off the caller's path.

// ownerActiveEvery is the most often one session's mark is written.
const ownerActiveEvery = time.Minute

// ownerActiveState remembers the last scheduled mark per session.
type ownerActiveState struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// ownerActive records, off the caller's path and at most once a minute per
// session, that the owner messaged or typed into sessionID. Only a direct
// owner principal counts: a session acting on the owner's behalf is not the
// owner. It reports whether a write was scheduled.
func (h *Hub) ownerActive(p contracts.Principal, sessionID string) bool {
	if p.Kind != contracts.PrincipalOwner || sessionID == "" {
		return false
	}
	now := h.now()
	h.ownerAct.mu.Lock()
	if last, ok := h.ownerAct.last[sessionID]; ok && now.Sub(last) < ownerActiveEvery {
		h.ownerAct.mu.Unlock()
		return false
	}
	if h.ownerAct.last == nil {
		h.ownerAct.last = map[string]time.Time{}
	}
	if len(h.ownerAct.last) >= 1024 { // keep the map bounded: stale marks throttle nothing
		for id, t := range h.ownerAct.last {
			if now.Sub(t) >= ownerActiveEvery {
				delete(h.ownerAct.last, id)
			}
		}
	}
	h.ownerAct.last[sessionID] = now
	h.ownerAct.mu.Unlock()
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), engageTimeout)
		defer cancel()
		go func() {
			select {
			case <-h.stop:
				cancel()
			case <-ctx.Done():
			}
		}()
		_, _ = h.markOwnerActive(ctx, sessionID, now)
	}()
	return true
}

// markOwnerActive stores at as the session's owner activity unless a mark
// from the last minute is already there (the guard still throttles after a
// restart empties the in-memory map), and emits session.updated when it
// changed. It opens its own transaction, never nested in another (I27).
func (h *Hub) markOwnerActive(ctx context.Context, sessionID string, at time.Time) (bool, error) {
	changed := false
	err := h.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE sessions SET owner_active_at=? WHERE session_id=? AND (owner_active_at IS NULL OR owner_active_at < ?)`,
			persistence.Time(at), sessionID, persistence.Time(at.Add(-ownerActiveEvery)))
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		s, err := h.getSession(ctx, tx, sessionID)
		if err != nil {
			return err
		}
		changed = true
		return h.emitSession(ctx, tx, contracts.EvSessionUpdated, s, "hub", 0)
	})
	return changed, err
}
