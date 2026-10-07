package mock

import (
	"context"
	"path"
	"strings"
	"sync"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/repoindex"
)

// Repo index in the mock: a synthetic repository set per node (derived
// from the node's platform), "last used" from the mock's runs, and
// synthetic worktree actions that record what a launch created so the
// session worktree view and archive cleanup have something to show.

var _ c.ReposAPI = (*Backend)(nil)

func init() {
	actionsAllowedForSessions[c.ActionWorktreeCreate] = true
	actionsAllowedForSessions[c.ActionWorktreeScan] = true
}

var (
	mockRepoMu sync.Mutex
	mockWT     = map[*Backend]map[string]c.SessionWorktrees{} // by session; or by cwd before the session exists
)

// synthRepoNames per node name; unknown nodes get a default set.
var synthRepoNames = map[string][]string{
	"demo-bigbox": {"atlas", "atlas-web", "tilekit", "infra", "dotfiles"},
	"demo-studio": {"atlas", "atlas-web", "notes-app", "scratch"},
	"demo-laptop": {"atlas", "firmware"},
	"demo-pi":     {"firmware"},
	"synth-mac":   {"atlas", "notes-app", "tundra", "api"},
	"synth-linux": {"atlas", "tundra-old", "api", "web"},
}

func synthRepos(n *c.Node, now time.Time) []c.Repo {
	names := synthRepoNames[n.Name]
	if names == nil {
		names = []string{"atlas", "notes-app"}
	}
	base := "/home/demo/src"
	if n.Platform == "darwin" {
		base = "/Users/demo/code"
	}
	var out []c.Repo
	for i, name := range names {
		r := c.Repo{Name: name, Path: path.Join(base, name), DefaultBranch: "main", Branch: "main",
			RemoteURL: "git@example.invalid:demo/" + name + ".git", LastActivity: now.Add(-time.Duration(i+1) * 7 * time.Hour)}
		if name == "scratch" {
			r.RemoteURL = ""
		}
		out = append(out, r)
	}
	return out
}

// ListRepos ranks the synthetic repos of every non-revoked node.
func (b *Backend) ListRepos(ctx context.Context, req c.ListReposRequest) (c.RepoList, error) {
	if _, err := b.principal(ctx); err != nil {
		return c.RepoList{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	out := c.RepoList{Items: []c.RepoEntry{}, Nodes: map[string]time.Time{}}
	var all []c.RepoEntry
	for _, n := range b.nodes {
		if req.NodeID != "" && n.NodeID != req.NodeID || n.Connection.State == c.ConnRevoked {
			continue
		}
		out.Nodes[n.NodeID] = now.Add(-3 * time.Minute)
		for _, r := range synthRepos(n, now) {
			e := c.RepoEntry{Repo: r, NodeID: n.NodeID, NodeName: n.Name}
			for _, run := range b.runs {
				if run.NodeID == n.NodeID && (run.Cwd == r.Path || strings.HasPrefix(run.Cwd, r.Path+"/") || synthCwdRepo(run.Cwd) == r.Name) {
					t := run.StartedAt
					if e.LastUsedAt == nil || t.After(*e.LastUsedAt) {
						e.LastUsedAt = &t
					}
				}
			}
			all = append(all, e)
		}
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
	return out, nil
}

// synthCwdRepo maps the demo fleet's /home/demo/src/<repo> cwds.
func synthCwdRepo(cwd string) string {
	if rest, ok := strings.CutPrefix(cwd, "/home/demo/src/"); ok {
		name, _, _ := strings.Cut(rest, "/")
		return name
	}
	return ""
}

// SessionWorktrees returns what a mock launch created, or a synthetic
// snapshot for a session working in a synthetic repo.
func (b *Backend) SessionWorktrees(ctx context.Context, req c.SessionWorktreesRequest) (c.SessionWorktrees, error) {
	if _, err := b.principal(ctx); err != nil {
		return c.SessionWorktrees{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessionWorktreesLocked(req.SessionID)
}

func (b *Backend) sessionWorktreesLocked(sid string) (c.SessionWorktrees, error) {
	s, ok := b.sessions[sid]
	if !ok {
		return c.SessionWorktrees{}, notFound(c.KindSession, sid)
	}
	out := c.SessionWorktrees{SessionID: sid, Repos: []c.RepoWorktrees{}, ReportedAt: b.now()}
	run := b.runs[s.CurrentRunID]
	if run == nil {
		return out, nil
	}
	out.RunID, out.NodeID, out.Cwd = run.RunID, run.NodeID, run.Cwd
	mockRepoMu.Lock()
	defer mockRepoMu.Unlock()
	m := mockWT[b]
	if sw, ok := m[sid]; ok {
		return sw, nil
	}
	if sw, ok := m["cwd:"+run.Cwd]; ok {
		sw.SessionID, sw.RunID, sw.NodeID = sid, run.RunID, run.NodeID
		m[sid] = sw
		delete(m, "cwd:"+run.Cwd)
		return sw, nil
	}
	if name := synthCwdRepo(run.Cwd); name != "" {
		repo := "/home/demo/src/" + name
		out.Repos = append(out.Repos, c.RepoWorktrees{Name: name, Path: repo, DefaultBranch: "main", Worktrees: []c.Worktree{
			{Path: repo, Branch: "main", Head: "4f1c2ab90", Main: true},
			{Path: repo + "/.claude/worktrees/tile-cache", Branch: "worktree-tile-cache", Head: "9ad03e117", CreatedInSession: true, Pushed: true},
			{Path: repo + "/.claude/worktrees/bench-fix", Branch: "worktree-bench-fix", Head: "c0ffee123", CreatedInSession: true, Dirty: true},
		}})
	}
	return out, nil
}

// repoAction runs the worktree actions; ok is false for other actions.
func (b *Backend) repoAction(n *c.Node, action string, params c.RawJSON) (any, bool, error) {
	switch action {
	case c.ActionWorktreeCreate:
		var p c.WorktreeCreateParams
		if err := decodeParams(params, &p); err != nil {
			return nil, true, err
		}
		if len(p.Repos) == 0 {
			return nil, true, c.Errorf(c.ErrInvalid, "worktree.create needs at least one repository")
		}
		slug := repoindex.Slug(p.Slug)
		if slug == "" && len(p.Repos) > 1 {
			var names []string
			for _, r := range p.Repos {
				names = append(names, path.Base(r))
			}
			slug = repoindex.Slug(strings.Join(names, " "))
		}
		if slug == "" {
			slug = "session"
		}
		branch := p.Branch
		if branch == "" {
			branch = "pyre/" + slug
		}
		res := c.WorktreeCreateResult{Branch: branch}
		sw := c.SessionWorktrees{NodeID: n.NodeID, Repos: []c.RepoWorktrees{}, ReportedAt: b.now()}
		home := "/home/demo"
		if n.Platform == "darwin" {
			home = "/Users/demo"
		}
		for _, r := range p.Repos {
			wt := path.Join(r, ".worktrees", slug)
			if len(p.Repos) > 1 {
				wt = path.Join(home, "pyre-work", slug, path.Base(r))
			}
			res.Worktrees = append(res.Worktrees, c.CreatedWorktree{Repo: r, Path: wt, Branch: branch})
			sw.Repos = append(sw.Repos, c.RepoWorktrees{Name: path.Base(r), Path: r, DefaultBranch: "main", Worktrees: []c.Worktree{
				{Path: r, Branch: "main", Main: true}, {Path: wt, Branch: branch, CreatedInSession: true, PyreCreated: true, Merged: true}}})
		}
		res.Cwd = res.Worktrees[0].Path
		if len(p.Repos) > 1 {
			res.Cwd = path.Join(home, "pyre-work", slug)
		}
		sw.Cwd = res.Cwd
		mockRepoMu.Lock()
		if mockWT[b] == nil {
			mockWT[b] = map[string]c.SessionWorktrees{}
		}
		mockWT[b]["cwd:"+res.Cwd] = sw
		mockRepoMu.Unlock()
		return res, true, nil
	case c.ActionWorktreeScan:
		var p c.WorktreeScanParams
		if err := decodeParams(params, &p); err != nil {
			return nil, true, err
		}
		sw, err := b.sessionWorktreesLocked(p.SessionID)
		return sw, true, err
	case c.ActionWorktreeRemove:
		var p c.WorktreeRemoveParams
		if err := decodeParams(params, &p); err != nil {
			return nil, true, err
		}
		sw, err := b.sessionWorktreesLocked(p.SessionID)
		if err != nil {
			return nil, true, err
		}
		res := c.WorktreeRemoveResult{Results: []c.WorktreeRemoval{}}
		for _, want := range p.Paths {
			out := c.WorktreeRemoval{Path: want, Outcome: "missing"}
			for ri, r := range sw.Repos {
				for wi, w := range r.Worktrees {
					if w.Path != want {
						continue
					}
					out.Branch = w.Branch
					switch {
					case w.Main:
						out.Outcome = "kept_main"
					case !w.CreatedInSession:
						out.Outcome = "kept_not_session"
					case w.Dirty:
						out.Outcome = "kept_dirty"
					default:
						out.Outcome, out.BranchOutcome = "removed", "deleted"
						if !w.Merged && !w.Pushed {
							out.BranchOutcome = "kept_unmerged"
						}
						sw.Repos[ri].Worktrees = append(r.Worktrees[:wi:wi], r.Worktrees[wi+1:]...)
					}
				}
			}
			res.Results = append(res.Results, out)
		}
		mockRepoMu.Lock()
		if mockWT[b] == nil {
			mockWT[b] = map[string]c.SessionWorktrees{}
		}
		mockWT[b][p.SessionID] = sw
		mockRepoMu.Unlock()
		return res, true, nil
	case c.ActionRepoReindex:
		return c.NodeRepoIndex{NodeID: n.NodeID, Roots: []string{"~/code"}, Repos: synthRepos(n, b.now()), IndexedAt: b.now()}, true, nil
	}
	return nil, false, nil
}
