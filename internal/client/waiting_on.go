package client

import (
	"context"

	c "github.com/jhh3/pyre/internal/contracts"
)

var _ c.WaitingOnAPI = (*Client)(nil)

// SetWaitingOn calls sessions.waiting_on (a hub that predates it answers
// 404, reported as unsupported).
func (cl *Client) SetWaitingOn(ctx context.Context, req c.SetWaitingOnRequest) (c.SetWaitingOnResponse, error) {
	var out c.SetWaitingOnResponse
	err := cl.mutate(ctx, c.OpSessionsWaitingOn, &req.Mutation, &req, &out)
	return out, reposUnsupported(c.OpSessionsWaitingOn, err)
}
