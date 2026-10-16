package daemon

import (
	"context"

	"github.com/jhh3/pyre/internal/contracts"
)

var _ contracts.AsksAPI = (*LocalAPI)(nil)

// Asks (W-asks) are forwarded to the hub with the caller's identity, so a
// session's pyre CLI/MCP (which talks to pyred) reaches them. There is no
// node-local fallback: the hub decides merging, deadlines and redirects.

func (l *LocalAPI) asks(ctx context.Context) (contracts.AsksAPI, error) {
	a, err := l.d.upstream(bearerFrom(ctx))
	if err != nil {
		return nil, err
	}
	u, ok := a.(contracts.AsksAPI)
	if !ok {
		return nil, contracts.Errorf(contracts.ErrUnsupported, "asks are not available through this node's hub client")
	}
	return u, nil
}

// CreateAsk forwards asks.create.
func (l *LocalAPI) CreateAsk(ctx context.Context, req contracts.CreateAskRequest) (contracts.CreateAskResponse, error) {
	u, err := l.asks(ctx)
	if err != nil {
		return contracts.CreateAskResponse{}, err
	}
	return u.CreateAsk(l.d.upstreamCtx(ctx), req)
}

// ListAsks forwards asks.list.
func (l *LocalAPI) ListAsks(ctx context.Context, req contracts.ListAsksRequest) (contracts.Page[contracts.Ask], error) {
	u, err := l.asks(ctx)
	if err != nil {
		return contracts.Page[contracts.Ask]{}, err
	}
	return u.ListAsks(l.d.upstreamCtx(ctx), req)
}

// GetAsk forwards asks.get.
func (l *LocalAPI) GetAsk(ctx context.Context, req contracts.GetRequest) (contracts.Ask, error) {
	u, err := l.asks(ctx)
	if err != nil {
		return contracts.Ask{}, err
	}
	return u.GetAsk(l.d.upstreamCtx(ctx), req)
}

// AnswerAsks forwards asks.answer (the hub allows only the owner).
func (l *LocalAPI) AnswerAsks(ctx context.Context, req contracts.AnswerAsksRequest) (contracts.AnswerAsksResponse, error) {
	u, err := l.asks(ctx)
	if err != nil {
		return contracts.AnswerAsksResponse{}, err
	}
	return u.AnswerAsks(l.d.upstreamCtx(ctx), req)
}

// WithdrawAsk forwards asks.withdraw.
func (l *LocalAPI) WithdrawAsk(ctx context.Context, req contracts.WithdrawAskRequest) (contracts.Ask, error) {
	u, err := l.asks(ctx)
	if err != nil {
		return contracts.Ask{}, err
	}
	return u.WithdrawAsk(l.d.upstreamCtx(ctx), req)
}
