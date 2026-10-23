package client

import (
	"context"

	c "github.com/jhh3/pyre/internal/contracts"
)

var _ c.StandingGrantsAPI = (*Client)(nil)

// Standing grants (W-grants). A hub that predates them answers 404,
// reported as unsupported.

// CreateGrant calls grants.create.
func (cl *Client) CreateGrant(ctx context.Context, req c.CreateGrantRequest) (c.StandingGrant, error) {
	var out c.StandingGrant
	err := cl.mutate(ctx, c.OpGrantsCreate, &req.Mutation, &req, &out)
	return out, reposUnsupported(c.OpGrantsCreate, err)
}

// ListGrants calls grants.list.
func (cl *Client) ListGrants(ctx context.Context, req c.ListGrantsRequest) (c.ListGrantsResponse, error) {
	var out c.ListGrantsResponse
	err := cl.read(ctx, c.OpGrantsList, req, &out)
	return out, reposUnsupported(c.OpGrantsList, err)
}

// RevokeGrant calls grants.revoke.
func (cl *Client) RevokeGrant(ctx context.Context, req c.RevokeGrantRequest) (c.StandingGrant, error) {
	var out c.StandingGrant
	err := cl.mutate(ctx, c.OpGrantsRevoke, &req.Mutation, &req, &out)
	return out, reposUnsupported(c.OpGrantsRevoke, err)
}
