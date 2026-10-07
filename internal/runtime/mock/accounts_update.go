package mock

import (
	"context"
	"strings"

	c "github.com/jhh3/pyre/internal/contracts"
)

var _ c.AccountUpdateAPI = (*Backend)(nil)

// UpdateAccount mirrors the hub's accounts.update.
func (b *Backend) UpdateAccount(ctx context.Context, req c.UpdateAccountRequest) (c.Account, error) {
	return mutate(b, ctx, c.OpUpdateAccount, req.Mutation, req, func(p c.Principal) (c.Account, error) {
		if err := requireOwner(p); err != nil {
			return c.Account{}, err
		}
		if req.Label == nil && req.Disabled == nil {
			return c.Account{}, c.Errorf(c.ErrInvalid, "nothing to update (label or disabled)")
		}
		a, ok := b.accounts[req.AccountID]
		if !ok {
			return c.Account{}, notFound(c.KindAccount, req.AccountID)
		}
		if req.Label != nil {
			l := strings.TrimSpace(*req.Label)
			if l == "" {
				return c.Account{}, c.Errorf(c.ErrInvalid, "label is required")
			}
			a.Account.Label = l
		}
		if req.Disabled != nil {
			a.Account.Disabled = *req.Disabled
		}
		return a.Account, nil
	})
}
