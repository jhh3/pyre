package client

import (
	"context"

	c "github.com/jhh3/pyre/internal/contracts"
)

var (
	_ c.HeldAnswersAPI = (*Client)(nil)
	_ c.ProjectPageAPI = (*Client)(nil)
)

// Held answers and project pages (W-web-inbox). A hub that predates them
// answers 404, reported as unsupported.

// HoldAnswer calls asks.hold (owner only).
func (cl *Client) HoldAnswer(ctx context.Context, req c.HoldAnswerRequest) (c.HeldAnswers, error) {
	var out c.HeldAnswers
	err := cl.mutate(ctx, c.OpAsksHold, &req.Mutation, &req, &out)
	return out, reposUnsupported(c.OpAsksHold, err)
}

// UnholdAnswer calls asks.unhold (owner only).
func (cl *Client) UnholdAnswer(ctx context.Context, req c.UnholdAnswerRequest) (c.HeldAnswers, error) {
	var out c.HeldAnswers
	err := cl.mutate(ctx, c.OpAsksUnhold, &req.Mutation, &req, &out)
	return out, reposUnsupported(c.OpAsksUnhold, err)
}

// ListHeldAnswers calls asks.held (owner only).
func (cl *Client) ListHeldAnswers(ctx context.Context, req struct{}) (c.HeldAnswers, error) {
	var out c.HeldAnswers
	err := cl.read(ctx, c.OpAsksHeld, req, &out)
	return out, reposUnsupported(c.OpAsksHeld, err)
}

// FlushHeldAnswers calls asks.flush (owner only).
func (cl *Client) FlushHeldAnswers(ctx context.Context, req c.FlushHeldAnswersRequest) (c.AnswerAsksResponse, error) {
	var out c.AnswerAsksResponse
	err := cl.mutate(ctx, c.OpAsksFlush, &req.Mutation, &req, &out)
	return out, reposUnsupported(c.OpAsksFlush, err)
}

// SetProjectPage calls projects.set_page.
func (cl *Client) SetProjectPage(ctx context.Context, req c.SetProjectPageRequest) (c.ProjectPage, error) {
	var out c.ProjectPage
	err := cl.mutate(ctx, c.OpProjectsSetPage, &req.Mutation, &req, &out)
	return out, reposUnsupported(c.OpProjectsSetPage, err)
}

// GetProjectPage calls projects.get_page.
func (cl *Client) GetProjectPage(ctx context.Context, req c.GetProjectPageRequest) (c.ProjectPage, error) {
	var out c.ProjectPage
	err := cl.read(ctx, c.OpProjectsGetPage, req, &out)
	return out, reposUnsupported(c.OpProjectsGetPage, err)
}
