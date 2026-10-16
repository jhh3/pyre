package daemon

import (
	"context"

	"github.com/jhh3/pyre/internal/contracts"
)

var _ contracts.MessageReadAPI = (*LocalAPI)(nil)

// MarkMessagesRead forwards messages.mark_read to the hub with the caller's
// identity (W-delivery: a session's own read resolves its pending paste).
func (l *LocalAPI) MarkMessagesRead(ctx context.Context, req contracts.MarkMessagesReadRequest) (contracts.MarkMessagesReadResponse, error) {
	a, err := l.d.upstream(bearerFrom(ctx))
	if err != nil {
		return contracts.MarkMessagesReadResponse{}, err
	}
	u, ok := a.(contracts.MessageReadAPI)
	if !ok {
		return contracts.MarkMessagesReadResponse{}, contracts.Errorf(contracts.ErrUnsupported, "messages.mark_read is not available through this node's hub client")
	}
	return u.MarkMessagesRead(l.d.upstreamCtx(ctx), req)
}
