package daemon

import (
	"context"

	"github.com/jhh3/pyre/internal/contracts"
)

var (
	_ contracts.HeldAnswersAPI = (*LocalAPI)(nil)
	_ contracts.ProjectPageAPI = (*LocalAPI)(nil)
)

// Held answers and project pages (W-web-inbox) are forwarded to the hub
// with the caller's identity, like asks (asks_proxy.go): a session's
// update_project_page reaches the hub through pyred.

func upstreamAs[T any](l *LocalAPI, ctx context.Context, what string) (T, error) {
	var zero T
	a, err := l.d.upstream(bearerFrom(ctx))
	if err != nil {
		return zero, err
	}
	u, ok := a.(T)
	if !ok {
		return zero, contracts.Errorf(contracts.ErrUnsupported, "%s are not available through this node's hub client", what)
	}
	return u, nil
}

// HoldAnswer forwards asks.hold (the hub allows only the owner).
func (l *LocalAPI) HoldAnswer(ctx context.Context, req contracts.HoldAnswerRequest) (contracts.HeldAnswers, error) {
	u, err := upstreamAs[contracts.HeldAnswersAPI](l, ctx, "held answers")
	if err != nil {
		return contracts.HeldAnswers{}, err
	}
	return u.HoldAnswer(l.d.upstreamCtx(ctx), req)
}

// UnholdAnswer forwards asks.unhold.
func (l *LocalAPI) UnholdAnswer(ctx context.Context, req contracts.UnholdAnswerRequest) (contracts.HeldAnswers, error) {
	u, err := upstreamAs[contracts.HeldAnswersAPI](l, ctx, "held answers")
	if err != nil {
		return contracts.HeldAnswers{}, err
	}
	return u.UnholdAnswer(l.d.upstreamCtx(ctx), req)
}

// ListHeldAnswers forwards asks.held.
func (l *LocalAPI) ListHeldAnswers(ctx context.Context, req struct{}) (contracts.HeldAnswers, error) {
	u, err := upstreamAs[contracts.HeldAnswersAPI](l, ctx, "held answers")
	if err != nil {
		return contracts.HeldAnswers{}, err
	}
	return u.ListHeldAnswers(l.d.upstreamCtx(ctx), req)
}

// FlushHeldAnswers forwards asks.flush.
func (l *LocalAPI) FlushHeldAnswers(ctx context.Context, req contracts.FlushHeldAnswersRequest) (contracts.AnswerAsksResponse, error) {
	u, err := upstreamAs[contracts.HeldAnswersAPI](l, ctx, "held answers")
	if err != nil {
		return contracts.AnswerAsksResponse{}, err
	}
	return u.FlushHeldAnswers(l.d.upstreamCtx(ctx), req)
}

// SetProjectPage forwards projects.set_page.
func (l *LocalAPI) SetProjectPage(ctx context.Context, req contracts.SetProjectPageRequest) (contracts.ProjectPage, error) {
	u, err := upstreamAs[contracts.ProjectPageAPI](l, ctx, "project pages")
	if err != nil {
		return contracts.ProjectPage{}, err
	}
	return u.SetProjectPage(l.d.upstreamCtx(ctx), req)
}

// GetProjectPage forwards projects.get_page.
func (l *LocalAPI) GetProjectPage(ctx context.Context, req contracts.GetProjectPageRequest) (contracts.ProjectPage, error) {
	u, err := upstreamAs[contracts.ProjectPageAPI](l, ctx, "project pages")
	if err != nil {
		return contracts.ProjectPage{}, err
	}
	return u.GetProjectPage(l.d.upstreamCtx(ctx), req)
}
