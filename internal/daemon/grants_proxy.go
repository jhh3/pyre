package daemon

import (
	"context"

	"github.com/jhh3/pyre/internal/contracts"
)

var _ contracts.StandingGrantsAPI = (*LocalAPI)(nil)

// Standing grants (W-grants) are forwarded to the hub with the caller's
// identity, like asks: the hub decides scope, inheritance and who may
// record or revoke.

func (l *LocalAPI) grants(ctx context.Context) (contracts.StandingGrantsAPI, error) {
	a, err := l.d.upstream(bearerFrom(ctx))
	if err != nil {
		return nil, err
	}
	u, ok := a.(contracts.StandingGrantsAPI)
	if !ok {
		return nil, contracts.Errorf(contracts.ErrUnsupported, "standing grants are not available through this node's hub client")
	}
	return u, nil
}

// CreateGrant forwards grants.create.
func (l *LocalAPI) CreateGrant(ctx context.Context, req contracts.CreateGrantRequest) (contracts.StandingGrant, error) {
	u, err := l.grants(ctx)
	if err != nil {
		return contracts.StandingGrant{}, err
	}
	return u.CreateGrant(l.d.upstreamCtx(ctx), req)
}

// ListGrants forwards grants.list.
func (l *LocalAPI) ListGrants(ctx context.Context, req contracts.ListGrantsRequest) (contracts.ListGrantsResponse, error) {
	u, err := l.grants(ctx)
	if err != nil {
		return contracts.ListGrantsResponse{}, err
	}
	return u.ListGrants(l.d.upstreamCtx(ctx), req)
}

// RevokeGrant forwards grants.revoke.
func (l *LocalAPI) RevokeGrant(ctx context.Context, req contracts.RevokeGrantRequest) (contracts.StandingGrant, error) {
	u, err := l.grants(ctx)
	if err != nil {
		return contracts.StandingGrant{}, err
	}
	return u.RevokeGrant(l.d.upstreamCtx(ctx), req)
}
