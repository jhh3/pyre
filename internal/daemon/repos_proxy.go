package daemon

import (
	"context"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/repoindex"
)

// The node-local API forwards repos.list and sessions.worktrees to the
// hub. While the hub is unreachable, repos.list answers from this node's
// own last index, so `pyre new <repo>` still finds local repositories.
var _ contracts.ReposAPI = (*LocalAPI)(nil)

func (l *LocalAPI) reposAPI(ctx context.Context) (contracts.ReposAPI, error) {
	a, err := l.d.upstream(bearerFrom(ctx))
	if err != nil {
		return nil, err
	}
	r, ok := a.(contracts.ReposAPI)
	if !ok {
		return nil, contracts.Errorf(contracts.ErrUnsupported, "the repo index is not available through this node's hub client")
	}
	return r, nil
}

// ListRepos forwards repos.list (local index when the hub is down).
func (l *LocalAPI) ListRepos(ctx context.Context, req contracts.ListReposRequest) (contracts.RepoList, error) {
	r, err := l.reposAPI(ctx)
	if err == nil {
		res, err := r.ListRepos(l.d.upstreamCtx(ctx), req)
		if err == nil || !hubDown(err) {
			return res, err
		}
	} else if !hubDown(err) && !contracts.IsCode(err, contracts.ErrUnavailable) {
		return contracts.RepoList{}, err
	}
	return l.d.localRepos(req), nil
}

func (d *Daemon) localRepos(req contracts.ListReposRequest) contracts.RepoList {
	rs := d.repoState()
	rs.mu.Lock()
	idx := rs.last
	rs.mu.Unlock()
	out := contracts.RepoList{Items: []contracts.RepoEntry{}, Nodes: map[string]time.Time{}}
	if req.NodeID != "" && req.NodeID != d.nodeID {
		return out
	}
	if !idx.IndexedAt.IsZero() {
		out.Nodes[d.nodeID] = idx.IndexedAt
	}
	name := d.cfg.NodeName
	if name == "" {
		name = d.nodeID
	}
	var all []contracts.RepoEntry
	for _, r := range idx.Repos {
		all = append(all, contracts.RepoEntry{Repo: r, NodeID: d.nodeID, NodeName: name})
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 50
	}
	ranked := repoindex.Rank(req.Query, all)
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}
	out.Items = append(out.Items, ranked...)
	return out
}

// SessionWorktrees forwards sessions.worktrees.
func (l *LocalAPI) SessionWorktrees(ctx context.Context, req contracts.SessionWorktreesRequest) (contracts.SessionWorktrees, error) {
	r, err := l.reposAPI(ctx)
	if err != nil {
		return contracts.SessionWorktrees{}, err
	}
	return r.SessionWorktrees(l.d.upstreamCtx(ctx), req)
}
