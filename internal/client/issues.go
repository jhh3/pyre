package client

import (
	"context"

	c "github.com/jhh3/pyre/internal/contracts"
)

var _ c.IssuesAPI = (*Client)(nil)

// Issues filed by sessions. A hub that predates them answers 404,
// reported as unsupported.

// FileIssue calls issues.file.
func (cl *Client) FileIssue(ctx context.Context, req c.FileIssueRequest) (c.FileIssueResponse, error) {
	var out c.FileIssueResponse
	err := cl.mutate(ctx, c.OpIssuesFile, &req.Mutation, &req, &out)
	return out, reposUnsupported(c.OpIssuesFile, err)
}

// ListIssues calls issues.list.
func (cl *Client) ListIssues(ctx context.Context, req c.ListIssuesRequest) (c.ListIssuesResponse, error) {
	var out c.ListIssuesResponse
	err := cl.read(ctx, c.OpIssuesList, req, &out)
	return out, reposUnsupported(c.OpIssuesList, err)
}

// GetIssue calls issues.get.
func (cl *Client) GetIssue(ctx context.Context, req c.GetIssueRequest) (c.IssueDetail, error) {
	var out c.IssueDetail
	err := cl.read(ctx, c.OpIssuesGet, req, &out)
	return out, reposUnsupported(c.OpIssuesGet, err)
}

// UpdateIssue calls issues.update.
func (cl *Client) UpdateIssue(ctx context.Context, req c.UpdateIssueRequest) (c.Issue, error) {
	var out c.Issue
	err := cl.mutate(ctx, c.OpIssuesUpdate, &req.Mutation, &req, &out)
	return out, reposUnsupported(c.OpIssuesUpdate, err)
}

// GetIssueTriage calls issues.triage.get.
func (cl *Client) GetIssueTriage(ctx context.Context, req struct{}) (c.IssueTriage, error) {
	var out c.IssueTriage
	err := cl.read(ctx, c.OpIssuesTriageGet, req, &out)
	return out, reposUnsupported(c.OpIssuesTriageGet, err)
}

// SetIssueTriage calls issues.triage.set.
func (cl *Client) SetIssueTriage(ctx context.Context, req c.SetIssueTriageRequest) (c.IssueTriage, error) {
	var out c.IssueTriage
	err := cl.mutate(ctx, c.OpIssuesTriageSet, &req.Mutation, &req, &out)
	return out, reposUnsupported(c.OpIssuesTriageSet, err)
}
