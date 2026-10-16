package tuitest

import (
	"context"

	"github.com/jhh3/pyre/internal/contracts"
)

// The test wrappers pass the repo index operations through to the wrapped
// API when it offers them (contracts.ReposAPI is not part of contracts.API).

func reposVia(api contracts.API) (contracts.ReposAPI, error) {
	if r, ok := api.(contracts.ReposAPI); ok {
		return r, nil
	}
	return nil, contracts.Errorf(contracts.ErrUnsupported, "repo index not offered")
}

func (e *Embeddable) ListRepos(ctx context.Context, req contracts.ListReposRequest) (contracts.RepoList, error) {
	r, err := reposVia(e.API)
	if err != nil {
		return contracts.RepoList{}, err
	}
	return r.ListRepos(ctx, req)
}

func (e *Embeddable) SessionWorktrees(ctx context.Context, req contracts.SessionWorktreesRequest) (contracts.SessionWorktrees, error) {
	r, err := reposVia(e.API)
	if err != nil {
		return contracts.SessionWorktrees{}, err
	}
	return r.SessionWorktrees(ctx, req)
}

func (r *Recorder) ListRepos(ctx context.Context, req contracts.ListReposRequest) (contracts.RepoList, error) {
	a, err := reposVia(r.API)
	if err != nil {
		return contracts.RepoList{}, err
	}
	return a.ListRepos(ctx, req)
}

func (r *Recorder) SessionWorktrees(ctx context.Context, req contracts.SessionWorktreesRequest) (contracts.SessionWorktrees, error) {
	a, err := reposVia(r.API)
	if err != nil {
		return contracts.SessionWorktrees{}, err
	}
	return a.SessionWorktrees(ctx, req)
}

// InvokeNodeAction records the worktree actions only (other actions, such
// as dir.list, are reads and stay unrecorded).
func (r *Recorder) InvokeNodeAction(ctx context.Context, req contracts.NodeActionRequest) (contracts.NodeActionResult, error) {
	switch req.Action {
	case contracts.ActionWorktreeCreate, contracts.ActionWorktreeRemove, contracts.ActionFsMkdir, contracts.ActionRepoClone:
		r.rec("InvokeNodeAction:"+req.Action, req.OperationID, req)
	}
	return r.API.InvokeNodeAction(ctx, req)
}

func (e *Embeddable) RepoCatalog(ctx context.Context, req contracts.RepoCatalogRequest) (contracts.RepoCatalog, error) {
	if r, ok := e.API.(contracts.RepoCatalogAPI); ok {
		return r.RepoCatalog(ctx, req)
	}
	return contracts.RepoCatalog{}, contracts.Errorf(contracts.ErrUnsupported, "repo catalog not offered")
}

func (r *Recorder) RepoCatalog(ctx context.Context, req contracts.RepoCatalogRequest) (contracts.RepoCatalog, error) {
	if a, ok := r.API.(contracts.RepoCatalogAPI); ok {
		return a.RepoCatalog(ctx, req)
	}
	return contracts.RepoCatalog{}, contracts.Errorf(contracts.ErrUnsupported, "repo catalog not offered")
}
