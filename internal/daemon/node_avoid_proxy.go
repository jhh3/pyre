package daemon

import (
	"context"

	"github.com/jhh3/pyre/internal/contracts"
)

var _ contracts.NodeAvoidAPI = (*LocalAPI)(nil)

// AvoidNode forwards nodes.avoid to the hub with the caller's identity:
// the hub attributes the flag and decides what a session may change.
func (l *LocalAPI) AvoidNode(ctx context.Context, req contracts.AvoidNodeRequest) (contracts.AvoidNodeResponse, error) {
	a, err := l.d.upstream(bearerFrom(ctx))
	if err != nil {
		return contracts.AvoidNodeResponse{}, err
	}
	u, ok := a.(contracts.NodeAvoidAPI)
	if !ok {
		return contracts.AvoidNodeResponse{}, contracts.Errorf(contracts.ErrUnsupported, "node avoid flags are not available through this node's hub client")
	}
	return u.AvoidNode(l.d.upstreamCtx(ctx), req)
}
