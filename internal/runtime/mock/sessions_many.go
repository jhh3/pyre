package mock

import (
	"context"

	c "github.com/jhh3/pyre/internal/contracts"
)

var _ c.SessionBatchAPI = (*Backend)(nil)

// GetManySessions mirrors the hub's sessions.get_many: views in request
// order (archived included), unknown ids in Missing.
func (b *Backend) GetManySessions(ctx context.Context, req c.GetManySessionsRequest) (c.GetManySessionsResponse, error) {
	return query(b, ctx, func(p c.Principal) (c.GetManySessionsResponse, error) {
		if len(req.IDs) > c.MaxGetManySessions {
			return c.GetManySessionsResponse{}, c.Errorf(c.ErrInvalid, "at most %d ids per sessions.get_many (got %d)", c.MaxGetManySessions, len(req.IDs))
		}
		out := c.GetManySessionsResponse{Items: []c.SessionView{}, Missing: []string{}}
		seen := map[string]bool{}
		for _, id := range req.IDs {
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			s, ok := b.sessions[id]
			if !ok {
				out.Missing = append(out.Missing, id)
				continue
			}
			out.Items = append(out.Items, b.viewLocked(s))
		}
		return out, nil
	})
}
