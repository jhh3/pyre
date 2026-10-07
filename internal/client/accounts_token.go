package client

import (
	"context"

	c "github.com/jhh3/pyre/internal/contracts"
)

var (
	_ c.AccountTokenAPI     = (*Client)(nil)
	_ c.AccountTokenFetcher = (*Client)(nil)
)

// tokenUnsupported maps a 404 for the operation itself (an older hub) to
// unsupported; a 404 naming an account stays not found.
func tokenUnsupported(op string, err error) error {
	if c.IsCode(err, c.ErrNotFound) && !isAccountNotFound(err) {
		return c.Errorf(c.ErrUnsupported, "this hub does not offer %s; upgrade pyre-hub", op)
	}
	return err
}

// SetAccountToken calls accounts.set_token. The token travels only in the
// request body over the authenticated (TLS) channel.
func (cl *Client) SetAccountToken(ctx context.Context, req c.SetAccountTokenRequest) (c.AccountTokenStatus, error) {
	var out c.AccountTokenStatus
	err := cl.mutate(ctx, c.OpSetAccountToken, &req.Mutation, &req, &out)
	return out, tokenUnsupported(c.OpSetAccountToken, err)
}

// RemoveAccountToken calls accounts.remove_token.
func (cl *Client) RemoveAccountToken(ctx context.Context, req c.RemoveAccountTokenRequest) (c.AccountTokenStatus, error) {
	var out c.AccountTokenStatus
	err := cl.mutate(ctx, c.OpRemoveAccountToken, &req.Mutation, &req, &out)
	return out, tokenUnsupported(c.OpRemoveAccountToken, err)
}

// ListAccountTokens calls accounts.tokens (status only).
func (cl *Client) ListAccountTokens(ctx context.Context, req c.ListAccountTokensRequest) ([]c.AccountTokenStatus, error) {
	var out []c.AccountTokenStatus
	err := cl.read(ctx, c.OpListAccountTokens, req, &out)
	return out, tokenUnsupported(c.OpListAccountTokens, err)
}

// FetchAccountToken calls accounts.token.fetch (node credential only).
func (cl *Client) FetchAccountToken(ctx context.Context, req c.FetchAccountTokenRequest) (c.FetchedAccountToken, error) {
	var out c.FetchedAccountToken
	err := cl.read(ctx, c.OpFetchAccountToken, req, &out)
	if c.IsCode(err, c.ErrNotFound) && !isAccountNotFound(err) {
		return out, c.Errorf(c.ErrUnsupported, "this hub does not offer %s; upgrade pyre-hub", c.OpFetchAccountToken)
	}
	return out, err
}
