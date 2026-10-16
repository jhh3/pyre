package client

import (
	"context"

	c "github.com/jhh3/pyre/internal/contracts"
)

var _ c.AsksAPI = (*Client)(nil)

// Asks (W-asks). A hub that predates them answers 404, reported as
// unsupported; a 404 naming an ask stays not found.

// CreateAsk calls asks.create.
func (cl *Client) CreateAsk(ctx context.Context, req c.CreateAskRequest) (c.CreateAskResponse, error) {
	var out c.CreateAskResponse
	err := cl.mutate(ctx, c.OpAsksCreate, &req.Mutation, &req, &out)
	return out, reposUnsupported(c.OpAsksCreate, err)
}

// ListAsks calls asks.list.
func (cl *Client) ListAsks(ctx context.Context, req c.ListAsksRequest) (c.Page[c.Ask], error) {
	var out c.Page[c.Ask]
	err := cl.read(ctx, c.OpAsksList, req, &out)
	return out, reposUnsupported(c.OpAsksList, err)
}

// GetAsk calls asks.get.
func (cl *Client) GetAsk(ctx context.Context, req c.GetRequest) (c.Ask, error) {
	var out c.Ask
	err := cl.read(ctx, c.OpAsksGet, req, &out)
	return out, reposUnsupported(c.OpAsksGet, err)
}

// AnswerAsks calls asks.answer (owner only; per-item results).
func (cl *Client) AnswerAsks(ctx context.Context, req c.AnswerAsksRequest) (c.AnswerAsksResponse, error) {
	var out c.AnswerAsksResponse
	err := cl.mutate(ctx, c.OpAsksAnswer, &req.Mutation, &req, &out)
	return out, reposUnsupported(c.OpAsksAnswer, err)
}

// WithdrawAsk calls asks.withdraw.
func (cl *Client) WithdrawAsk(ctx context.Context, req c.WithdrawAskRequest) (c.Ask, error) {
	var out c.Ask
	err := cl.mutate(ctx, c.OpAsksWithdraw, &req.Mutation, &req, &out)
	return out, reposUnsupported(c.OpAsksWithdraw, err)
}
