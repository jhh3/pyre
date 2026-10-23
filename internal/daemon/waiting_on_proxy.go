package daemon

import (
	"context"

	"github.com/jhh3/pyre/internal/contracts"
)

var _ contracts.WaitingOnAPI = (*LocalAPI)(nil)

// SetWaitingOn forwards sessions.waiting_on to the hub with the caller's
// identity (a session's CLI/MCP talks to pyred).
func (l *LocalAPI) SetWaitingOn(ctx context.Context, req contracts.SetWaitingOnRequest) (contracts.SetWaitingOnResponse, error) {
	a, err := l.d.upstream(bearerFrom(ctx))
	if err != nil {
		return contracts.SetWaitingOnResponse{}, err
	}
	u, ok := a.(contracts.WaitingOnAPI)
	if !ok {
		return contracts.SetWaitingOnResponse{}, contracts.Errorf(contracts.ErrUnsupported, "waiting_on is not available through this node's hub client")
	}
	return u.SetWaitingOn(l.d.upstreamCtx(ctx), req)
}
