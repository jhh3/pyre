package daemon

import (
	"context"

	"github.com/jhh3/pyre/internal/contracts"
)

var _ contracts.AccountUpdateAPI = (*LocalAPI)(nil)

// UpdateAccount forwards accounts.update to the hub with the caller's
// identity.
func (l *LocalAPI) UpdateAccount(ctx context.Context, req contracts.UpdateAccountRequest) (contracts.Account, error) {
	a, err := l.d.upstream(bearerFrom(ctx))
	if err != nil {
		return contracts.Account{}, err
	}
	u, ok := a.(contracts.AccountUpdateAPI)
	if !ok {
		return contracts.Account{}, contracts.Errorf(contracts.ErrUnsupported, "accounts.update is not available through this node's hub client")
	}
	return u.UpdateAccount(l.d.upstreamCtx(ctx), req)
}
