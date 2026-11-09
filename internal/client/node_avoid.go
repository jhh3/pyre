package client

import (
	"context"

	c "github.com/jhh3/pyre/internal/contracts"
)

var _ c.NodeAvoidAPI = (*Client)(nil)

// AvoidNode calls nodes.avoid. A hub that predates it answers 404,
// reported as unsupported.
func (cl *Client) AvoidNode(ctx context.Context, req c.AvoidNodeRequest) (c.AvoidNodeResponse, error) {
	var out c.AvoidNodeResponse
	err := cl.mutate(ctx, c.OpNodesAvoid, &req.Mutation, &req, &out)
	return out, reposUnsupported(c.OpNodesAvoid, err)
}
