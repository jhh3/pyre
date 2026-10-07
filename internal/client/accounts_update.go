package client

import (
	"context"
	"errors"

	c "github.com/jhh3/pyre/internal/contracts"
)

var _ c.AccountUpdateAPI = (*Client)(nil)

// UpdateAccount calls accounts.update. A hub that predates it answers 404,
// reported as unsupported.
func (cl *Client) UpdateAccount(ctx context.Context, req c.UpdateAccountRequest) (c.Account, error) {
	var out c.Account
	err := cl.mutate(ctx, c.OpUpdateAccount, &req.Mutation, &req, &out)
	if c.IsCode(err, c.ErrNotFound) && !isAccountNotFound(err) {
		return out, c.Errorf(c.ErrUnsupported, "this hub does not offer %s; upgrade pyre-hub", c.OpUpdateAccount)
	}
	return out, err
}

// isAccountNotFound: the hub knows the op but not the account.
func isAccountNotFound(err error) bool {
	var e *c.Error
	return errors.As(err, &e) && e.Ref != nil && e.Ref.Kind == c.KindAccount
}
