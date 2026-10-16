package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/persistence"
)

// Batched reads for session lists (W-dbhygiene). sessionView used to issue
// one query per listed session for its run, its pending approvals and its
// input lease; a list now reads each of them once per batch and the views
// take them from the request memo. Paths without a memo are unchanged.

// listBatch holds prefetched per-run data for one batch of a list.
type listBatch struct {
	runs      map[string]*contracts.Run // run ID -> run (nil: no such run)
	approvals map[string][]string       // run ID -> pending approval IDs, oldest first
	leases    map[string]*contracts.InputLease
}

// extraScan appends destinations to a row scan (the list query selects the
// session columns plus the current run's body).
type extraScan struct {
	sc    interface{ Scan(...any) error }
	extra []any
}

func (e extraScan) Scan(dest ...any) error { return e.sc.Scan(append(dest, e.extra...)...) }

// prefetchList loads the current runs (bodies already read with the
// sessions), pending approvals and active leases of a batch into the memo.
func (h *Hub) prefetchList(ctx context.Context, q queryer, sessions []contracts.Session, bodies []sql.NullString) {
	m := nodeMemoFrom(ctx)
	if m == nil {
		return
	}
	b := &listBatch{runs: map[string]*contracts.Run{}, approvals: map[string][]string{}, leases: map[string]*contracts.InputLease{}}
	var ids []any
	for i, s := range sessions {
		if s.CurrentRunID == "" {
			continue
		}
		var r *contracts.Run
		if bodies[i].Valid {
			var run contracts.Run
			if json.Unmarshal([]byte(bodies[i].String), &run) == nil {
				r = &run
			}
		}
		if r == nil {
			continue // not found or unreadable: sessionView reads it itself
		}
		b.runs[s.CurrentRunID] = r
		ids = append(ids, s.CurrentRunID)
	}
	if len(ids) > 0 {
		ph := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
		args := append(append([]any{}, ids...), string(contracts.ApprovalPending))
		if rows, err := q.QueryContext(ctx, `SELECT run_id, approval_id FROM approvals WHERE run_id IN (`+ph+`) AND state=? ORDER BY created_at`, args...); err == nil {
			for rows.Next() {
				var run, id string
				if rows.Scan(&run, &id) == nil {
					b.approvals[run] = append(b.approvals[run], id)
				}
			}
			rows.Close()
		} else {
			return // fall back to per-session reads
		}
		args = append(append([]any{}, ids...), persistence.Time(h.now()))
		rows, err := q.QueryContext(ctx, `SELECT `+leaseCols+` FROM leases WHERE run_id IN (`+ph+`) AND released_at IS NULL AND expires_at > ? ORDER BY acquired_at DESC`, args...)
		if err != nil {
			return
		}
		for rows.Next() {
			l, _, err := scanLease(rows)
			if err != nil {
				continue
			}
			if _, ok := b.leases[l.RunID]; !ok {
				b.leases[l.RunID] = &l
			}
		}
		rows.Close()
	}
	m.mu.Lock()
	m.batch = b
	m.mu.Unlock()
}

// batchFrom returns the prefetched batch of the list request ctx belongs to.
func batchFrom(ctx context.Context) *listBatch {
	m := nodeMemoFrom(ctx)
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.batch
}

// batchRun returns a prefetched run (a copy).
func (b *listBatch) run(id string) (contracts.Run, bool) {
	if b == nil {
		return contracts.Run{}, false
	}
	r, ok := b.runs[id]
	if !ok || r == nil {
		return contracts.Run{}, false
	}
	return *r, true
}

// decorated reports the prefetched lease and approvals of a run.
func (b *listBatch) decorated(id string) (lease *contracts.InputLease, approvals []string, ok bool) {
	if b == nil {
		return nil, nil, false
	}
	if _, ok := b.runs[id]; !ok {
		return nil, nil, false
	}
	if l := b.leases[id]; l != nil {
		cp := *l
		lease = &cp
	}
	return lease, append([]string(nil), b.approvals[id]...), true
}
