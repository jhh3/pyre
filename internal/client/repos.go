package client

import (
	"context"
	"errors"

	c "github.com/jhh3/pyre/internal/contracts"
)

var _ c.ReposAPI = (*Client)(nil)

// reposUnsupported maps a 404 for the operation itself (an older hub) to
// unsupported; a 404 naming a session stays not found.
func reposUnsupported(op string, err error) error {
	var e *c.Error
	if c.IsCode(err, c.ErrNotFound) && !(errors.As(err, &e) && e.Ref != nil) {
		return c.Errorf(c.ErrUnsupported, "this hub does not offer %s; upgrade pyre-hub", op)
	}
	return err
}

// ListRepos calls repos.list.
func (cl *Client) ListRepos(ctx context.Context, req c.ListReposRequest) (c.RepoList, error) {
	var out c.RepoList
	err := cl.read(ctx, c.OpListRepos, req, &out)
	return out, reposUnsupported(c.OpListRepos, err)
}

// SessionWorktrees calls sessions.worktrees.
func (cl *Client) SessionWorktrees(ctx context.Context, req c.SessionWorktreesRequest) (c.SessionWorktrees, error) {
	var out c.SessionWorktrees
	err := cl.read(ctx, c.OpSessionWorktrees, req, &out)
	return out, reposUnsupported(c.OpSessionWorktrees, err)
}
