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

// Standing grants (W-grants).

var _ c.StandingGrantsAPI = routed{}

func (rt routed) CreateGrant(ctx context.Context, req c.CreateGrantRequest) (c.StandingGrant, error) {
	t, err := pickAs[c.StandingGrantsAPI](rt, ctx, c.OpGrantsCreate)
	if err != nil {
		return c.StandingGrant{}, err
	}
	return t.CreateGrant(ctx, req)
}

func (rt routed) ListGrants(ctx context.Context, req c.ListGrantsRequest) (c.ListGrantsResponse, error) {
	t, err := pickAs[c.StandingGrantsAPI](rt, ctx, c.OpGrantsList)
	if err != nil {
		return c.ListGrantsResponse{}, err
	}
	return t.ListGrants(ctx, req)
}

func (rt routed) RevokeGrant(ctx context.Context, req c.RevokeGrantRequest) (c.StandingGrant, error) {
	t, err := pickAs[c.StandingGrantsAPI](rt, ctx, c.OpGrantsRevoke)
	if err != nil {
		return c.StandingGrant{}, err
	}
	return t.RevokeGrant(ctx, req)
}

// Issues filed by sessions.
var _ c.IssuesAPI = routed{}

func (rt routed) FileIssue(ctx context.Context, req c.FileIssueRequest) (c.FileIssueResponse, error) {
	t, err := pickAs[c.IssuesAPI](rt, ctx, c.OpIssuesFile)
	if err != nil {
		return c.FileIssueResponse{}, err
	}
	return t.FileIssue(ctx, req)
}

func (rt routed) ListIssues(ctx context.Context, req c.ListIssuesRequest) (c.ListIssuesResponse, error) {
	t, err := pickAs[c.IssuesAPI](rt, ctx, c.OpIssuesList)
	if err != nil {
		return c.ListIssuesResponse{}, err
	}
	return t.ListIssues(ctx, req)
}

func (rt routed) GetIssue(ctx context.Context, req c.GetIssueRequest) (c.IssueDetail, error) {
	t, err := pickAs[c.IssuesAPI](rt, ctx, c.OpIssuesGet)
	if err != nil {
		return c.IssueDetail{}, err
	}
	return t.GetIssue(ctx, req)
}

func (rt routed) UpdateIssue(ctx context.Context, req c.UpdateIssueRequest) (c.Issue, error) {
	t, err := pickAs[c.IssuesAPI](rt, ctx, c.OpIssuesUpdate)
	if err != nil {
		return c.Issue{}, err
	}
	return t.UpdateIssue(ctx, req)
}

func (rt routed) GetIssueTriage(ctx context.Context, req struct{}) (c.IssueTriage, error) {
	t, err := pickAs[c.IssuesAPI](rt, ctx, c.OpIssuesTriageGet)
	if err != nil {
		return c.IssueTriage{}, err
	}
	return t.GetIssueTriage(ctx, req)
}

func (rt routed) SetIssueTriage(ctx context.Context, req c.SetIssueTriageRequest) (c.IssueTriage, error) {
	t, err := pickAs[c.IssuesAPI](rt, ctx, c.OpIssuesTriageSet)
	if err != nil {
		return c.IssueTriage{}, err
	}
	return t.SetIssueTriage(ctx, req)
}
