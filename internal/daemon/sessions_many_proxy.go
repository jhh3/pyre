package daemon

import (
	"context"

	"github.com/jhh3/pyre/internal/contracts"
)

var _ contracts.SessionBatchAPI = (*LocalAPI)(nil)

// GetManySessions forwards sessions.get_many to the hub with the caller's
// identity. There is no node-local fallback: a client that gets an error
// here falls back to sessions.list, which has one.
func (l *LocalAPI) GetManySessions(ctx context.Context, req contracts.GetManySessionsRequest) (contracts.GetManySessionsResponse, error) {
	a, err := l.d.upstream(bearerFrom(ctx))
	if err != nil {
		return contracts.GetManySessionsResponse{}, err
	}
	u, ok := a.(contracts.SessionBatchAPI)
	if !ok {
		return contracts.GetManySessionsResponse{}, contracts.Errorf(contracts.ErrUnsupported, "sessions.get_many is not available through this node's hub client")
	}
	return u.GetManySessions(l.d.upstreamCtx(ctx), req)
}
