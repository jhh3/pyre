package client

import (
	"context"
	"errors"

	c "github.com/jhh3/pyre/internal/contracts"
)

var _ c.InboxWaitAPI = (*Client)(nil)

// InboxWait calls inbox.wait (W-localwait). Only pyred's local socket
// serves it; a hub or an older pyred answers 404, reported as
// ErrUnsupported so the caller falls back to a messages.list long-poll.
func (cl *Client) InboxWait(ctx context.Context, req c.InboxWaitRequest) (c.InboxWaitResponse, error) {
	var out c.InboxWaitResponse
	err := cl.call(ctx, c.OpInboxWait, req, &out, true, longPoll(req.WaitSeconds))
	var e *c.Error
	if c.IsCode(err, c.ErrNotFound) && !(errors.As(err, &e) && e.Ref != nil) {
		return out, c.Errorf(c.ErrUnsupported, "this endpoint does not offer %s (not a current pyred socket)", c.OpInboxWait)
	}
	return out, err
}
