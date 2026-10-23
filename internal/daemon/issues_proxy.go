package daemon

import (
	"context"

	"github.com/jhh3/pyre/internal/contracts"
)

var _ contracts.IssuesAPI = (*LocalAPI)(nil)

// Issues are forwarded to the hub with the caller's identity: the hub
// fills the reporter and decides who may update issues or set triage.

func (l *LocalAPI) issues(ctx context.Context) (contracts.IssuesAPI, error) {
	a, err := l.d.upstream(bearerFrom(ctx))
	if err != nil {
		return nil, err
	}
	u, ok := a.(contracts.IssuesAPI)
	if !ok {
		return nil, contracts.Errorf(contracts.ErrUnsupported, "issues are not available through this node's hub client")
	}
	return u, nil
}

// FileIssue forwards issues.file.
func (l *LocalAPI) FileIssue(ctx context.Context, req contracts.FileIssueRequest) (contracts.FileIssueResponse, error) {
	u, err := l.issues(ctx)
	if err != nil {
		return contracts.FileIssueResponse{}, err
	}
	return u.FileIssue(l.d.upstreamCtx(ctx), req)
}

// ListIssues forwards issues.list.
func (l *LocalAPI) ListIssues(ctx context.Context, req contracts.ListIssuesRequest) (contracts.ListIssuesResponse, error) {
	u, err := l.issues(ctx)
	if err != nil {
		return contracts.ListIssuesResponse{}, err
	}
	return u.ListIssues(l.d.upstreamCtx(ctx), req)
}

// GetIssue forwards issues.get.
func (l *LocalAPI) GetIssue(ctx context.Context, req contracts.GetIssueRequest) (contracts.IssueDetail, error) {
	u, err := l.issues(ctx)
	if err != nil {
		return contracts.IssueDetail{}, err
	}
	return u.GetIssue(l.d.upstreamCtx(ctx), req)
}

// UpdateIssue forwards issues.update.
func (l *LocalAPI) UpdateIssue(ctx context.Context, req contracts.UpdateIssueRequest) (contracts.Issue, error) {
	u, err := l.issues(ctx)
	if err != nil {
		return contracts.Issue{}, err
	}
	return u.UpdateIssue(l.d.upstreamCtx(ctx), req)
}

// GetIssueTriage forwards issues.triage.get.
func (l *LocalAPI) GetIssueTriage(ctx context.Context, req struct{}) (contracts.IssueTriage, error) {
	u, err := l.issues(ctx)
	if err != nil {
		return contracts.IssueTriage{}, err
	}
	return u.GetIssueTriage(l.d.upstreamCtx(ctx), req)
}

// SetIssueTriage forwards issues.triage.set.
func (l *LocalAPI) SetIssueTriage(ctx context.Context, req contracts.SetIssueTriageRequest) (contracts.IssueTriage, error) {
	u, err := l.issues(ctx)
	if err != nil {
		return contracts.IssueTriage{}, err
	}
	return u.SetIssueTriage(l.d.upstreamCtx(ctx), req)
}
