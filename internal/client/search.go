package client

import (
	"context"

	c "github.com/jhh3/pyre/internal/contracts"
)

var _ c.SearchAPI = (*Client)(nil)

// Search calls the hub's fan-out search (search.query). A hub that
// predates it answers 404, reported as unsupported.
func (cl *Client) Search(ctx context.Context, req c.SearchRequest) (c.SearchResponse, error) {
	var out c.SearchResponse
	err := cl.read(ctx, c.OpSearch, req, &out)
	if c.IsCode(err, c.ErrNotFound) {
		return out, c.Errorf(c.ErrUnsupported, "this hub does not offer search (%s); upgrade pyre-hub", c.OpSearch)
	}
	return out, err
}
