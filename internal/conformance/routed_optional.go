package conformance

import (
	"context"

	c "github.com/jhh3/pyre/internal/contracts"
)

// Optional (non-API) operations on the routed target: forwarded when the
// picked implementation offers them, ErrUnsupported otherwise.

var (
	_ c.HeldAnswersAPI = routed{}
	_ c.ProjectPageAPI = routed{}
)

func pickAs[T any](rt routed, ctx context.Context, op string) (T, error) {
	t, ok := rt.pick(ctx).(T)
	if !ok {
		var zero T
		return zero, c.Errorf(c.ErrUnsupported, "%s is not offered by this target", op)
	}
	return t, nil
}

func (rt routed) HoldAnswer(ctx context.Context, req c.HoldAnswerRequest) (c.HeldAnswers, error) {
	t, err := pickAs[c.HeldAnswersAPI](rt, ctx, c.OpAsksHold)
	if err != nil {
		return c.HeldAnswers{}, err
	}
	return t.HoldAnswer(ctx, req)
}

func (rt routed) UnholdAnswer(ctx context.Context, req c.UnholdAnswerRequest) (c.HeldAnswers, error) {
	t, err := pickAs[c.HeldAnswersAPI](rt, ctx, c.OpAsksUnhold)
	if err != nil {
		return c.HeldAnswers{}, err
	}
	return t.UnholdAnswer(ctx, req)
}

func (rt routed) ListHeldAnswers(ctx context.Context, req struct{}) (c.HeldAnswers, error) {
	t, err := pickAs[c.HeldAnswersAPI](rt, ctx, c.OpAsksHeld)
	if err != nil {
		return c.HeldAnswers{}, err
	}
	return t.ListHeldAnswers(ctx, req)
}

func (rt routed) FlushHeldAnswers(ctx context.Context, req c.FlushHeldAnswersRequest) (c.AnswerAsksResponse, error) {
	t, err := pickAs[c.HeldAnswersAPI](rt, ctx, c.OpAsksFlush)
	if err != nil {
		return c.AnswerAsksResponse{}, err
	}
	return t.FlushHeldAnswers(ctx, req)
}

func (rt routed) SetProjectPage(ctx context.Context, req c.SetProjectPageRequest) (c.ProjectPage, error) {
	t, err := pickAs[c.ProjectPageAPI](rt, ctx, c.OpProjectsSetPage)
	if err != nil {
		return c.ProjectPage{}, err
	}
	return t.SetProjectPage(ctx, req)
}

func (rt routed) GetProjectPage(ctx context.Context, req c.GetProjectPageRequest) (c.ProjectPage, error) {
	t, err := pickAs[c.ProjectPageAPI](rt, ctx, c.OpProjectsGetPage)
	if err != nil {
		return c.ProjectPage{}, err
	}
	return t.GetProjectPage(ctx, req)
}
