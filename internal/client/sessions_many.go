package client

import (
	"context"

	c "github.com/jhh3/pyre/internal/contracts"
)

var _ c.SessionBatchAPI = (*Client)(nil)

// GetManySessions calls sessions.get_many. A hub that predates it answers
// 404, reported as unsupported.
func (cl *Client) GetManySessions(ctx context.Context, req c.GetManySessionsRequest) (c.GetManySessionsResponse, error) {
	var out c.GetManySessionsResponse
	err := cl.read(ctx, c.OpGetManySessions, req, &out)
	return out, reposUnsupported(c.OpGetManySessions, err)
}
