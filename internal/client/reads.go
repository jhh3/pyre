package client

import (
	"context"

	c "github.com/jhh3/pyre/internal/contracts"
)

var _ c.MessageReadAPI = (*Client)(nil)

// MarkMessagesRead calls messages.mark_read. A hub that predates it answers
// 404, reported as unsupported.
func (cl *Client) MarkMessagesRead(ctx context.Context, req c.MarkMessagesReadRequest) (c.MarkMessagesReadResponse, error) {
	var out c.MarkMessagesReadResponse
	err := cl.call(ctx, c.OpMarkMessagesRead, req, &out, true, 0)
	return out, reposUnsupported(c.OpMarkMessagesRead, err)
}
