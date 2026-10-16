package hub

import (
	"context"
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
)

var _ contracts.SessionBatchAPI = (*Hub)(nil)

// GetManySessions implements contracts.OpGetManySessions: the views of the
// requested sessions (archived included) in request order, and the ids that
// do not exist. One query for the sessions; node lookups share a request
// memo, so N ids on one node parse its status once.
func (h *Hub) GetManySessions(ctx context.Context, req contracts.GetManySessionsRequest) (contracts.GetManySessionsResponse, error) {
	out := contracts.GetManySessionsResponse{Items: []contracts.SessionView{}, Missing: []string{}}
	if _, err := principal(ctx); err != nil {
		return out, err
	}
	if len(req.IDs) > contracts.MaxGetManySessions {
		return out, contracts.Errorf(contracts.ErrInvalid, "at most %d ids per sessions.get_many (got %d)", contracts.MaxGetManySessions, len(req.IDs))
	}
	ids := make([]string, 0, len(req.IDs))
	seen := make(map[string]bool, len(req.IDs))
	for _, id := range req.IDs {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := h.db.QueryContext(ctx, `SELECT `+sesCols+` FROM sessions WHERE session_id IN (?`+strings.Repeat(",?", len(ids)-1)+`)`, args...)
	if err != nil {
		return out, err
	}
	found := make(map[string]contracts.Session, len(ids))
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			rows.Close()
			return out, err
		}
		found[s.SessionID] = s
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	ctx = withNodeMemo(ctx)
	for _, id := range ids {
		s, ok := found[id]
		if !ok {
			out.Missing = append(out.Missing, id)
			continue
		}
		v, err := h.sessionView(ctx, h.db, s, true)
		if err != nil {
			return out, err
		}
		out.Items = append(out.Items, v)
	}
	return out, nil
}
