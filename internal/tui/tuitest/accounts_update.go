package tuitest

import (
	"context"

	"github.com/jhh3/pyre/internal/contracts"
)

// The test wrappers pass accounts.update through to the wrapped API when
// it offers it (contracts.AccountUpdateAPI is not part of contracts.API).

func updateAccountVia(api contracts.API, ctx context.Context, req contracts.UpdateAccountRequest) (contracts.Account, error) {
	if u, ok := api.(contracts.AccountUpdateAPI); ok {
		return u.UpdateAccount(ctx, req)
	}
	return contracts.Account{}, contracts.Errorf(contracts.ErrUnsupported, "accounts.update not offered")
}

func (e *Embeddable) UpdateAccount(ctx context.Context, req contracts.UpdateAccountRequest) (contracts.Account, error) {
	return updateAccountVia(e.API, ctx, req)
}

func (r *Recorder) UpdateAccount(ctx context.Context, req contracts.UpdateAccountRequest) (contracts.Account, error) {
	r.rec("UpdateAccount", req.OperationID, req)
	return updateAccountVia(r.API, ctx, req)
}
