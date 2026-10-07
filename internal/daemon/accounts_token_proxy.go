package daemon

import (
	"context"

	"github.com/jhh3/pyre/internal/contracts"
)

// The node-local API forwards the owner-facing token operations to the hub
// with the caller's identity (the hub decides). accounts.token.fetch is
// deliberately NOT offered here: only the daemon's own node credential may
// fetch a token, never a local caller.
var _ contracts.AccountTokenAPI = (*LocalAPI)(nil)

func (l *LocalAPI) tokenAPI(ctx context.Context) (contracts.AccountTokenAPI, error) {
	a, err := l.d.upstream(bearerFrom(ctx))
	if err != nil {
		return nil, err
	}
	t, ok := a.(contracts.AccountTokenAPI)
	if !ok {
		return nil, contracts.Errorf(contracts.ErrUnsupported, "account tokens are not available through this node's hub client")
	}
	return t, nil
}

// SetAccountToken forwards accounts.set_token.
func (l *LocalAPI) SetAccountToken(ctx context.Context, req contracts.SetAccountTokenRequest) (contracts.AccountTokenStatus, error) {
	t, err := l.tokenAPI(ctx)
	if err != nil {
		return contracts.AccountTokenStatus{}, err
	}
	return t.SetAccountToken(l.d.upstreamCtx(ctx), req)
}

// RemoveAccountToken forwards accounts.remove_token.
func (l *LocalAPI) RemoveAccountToken(ctx context.Context, req contracts.RemoveAccountTokenRequest) (contracts.AccountTokenStatus, error) {
	t, err := l.tokenAPI(ctx)
	if err != nil {
		return contracts.AccountTokenStatus{}, err
	}
	return t.RemoveAccountToken(l.d.upstreamCtx(ctx), req)
}

// ListAccountTokens forwards accounts.tokens.
func (l *LocalAPI) ListAccountTokens(ctx context.Context, req contracts.ListAccountTokensRequest) ([]contracts.AccountTokenStatus, error) {
	t, err := l.tokenAPI(ctx)
	if err != nil {
		return nil, err
	}
	return t.ListAccountTokens(l.d.upstreamCtx(ctx), req)
}
