package daemon

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/repoindex"
)

// Global repo catalog support on the node (W-newsession): the hourly
// GitHub snapshot through the node's own gh login, and clone-on-demand
// into the node's repo root (the index is updated at once).

func (c GitHubCatalogConfig) ghPath() string {
	if !c.Enabled {
		return ""
	}
	if c.GH != "" {
		return c.GH
	}
	p, err := exec.LookPath("gh")
	if err != nil {
		return ""
	}
	return p
}

// fetchGitHub fetches the snapshot and reports it when it changed (or
// every 6 hours).
func (d *Daemon) fetchGitHub(ctx context.Context, gh string) (contracts.GitHubRepoIndex, error) {
	rs := d.repoState()
	rs.ghMu.Lock()
	defer rs.ghMu.Unlock()
	idx, err := repoindex.FetchGitHub(ctx, repoindex.ExecGH(gh), d.now())
	if err != nil {
		return idx, err
	}
	idx.NodeID = d.nodeID
	body := idx
	body.FetchedAt = time.Time{}
	b, _ := json.Marshal(body)
	h := hashToken(string(b))
	rs.mu.Lock()
	send := h != rs.ghHash || d.now().Sub(rs.ghSent) > 6*time.Hour
	rs.mu.Unlock()
	if send {
		if _, err := d.rep.Enqueue(ctx, contracts.ReportGitHubRepos, idx); err != nil {
			return idx, err
		}
		rs.mu.Lock()
		rs.ghHash, rs.ghSent = h, d.now()
		rs.mu.Unlock()
	}
	return idx, nil
}

func (d *Daemon) githubLoop(ctx context.Context) {
	cfg := d.cfg.RepoIndex.withDefaults()
	gh := cfg.GitHub.ghPath()
	if !cfg.Enabled || gh == "" {
		return
	}
	every := cfg.GitHub.Interval
	if every <= 0 {
		every = time.Hour
	}
	wait := cfg.StartDelay + 15*time.Second
	if cfg.StartDelay < 0 {
		wait = 0
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	failed := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if _, err := d.fetchGitHub(ctx, gh); err != nil && ctx.Err() == nil && !failed {
			d.noteErr("github catalog: %v", err) // once; retried every interval
			failed = true
		} else if err == nil {
			failed = false
		}
		t.Reset(every)
	}
}

func (d *Daemon) actionGitHubRefresh(ctx context.Context, _ json.RawMessage) (any, error) {
	gh := d.cfg.RepoIndex.GitHub.ghPath()
	if gh == "" {
		return nil, contracts.Errorf(contracts.ErrUnsupported, "the GitHub catalog is off on this node (no gh CLI, or not enabled)")
	}
	return d.fetchGitHub(ctx, gh)
}

// actionRepoClone clones a repository into the node's repo root and adds
// it to the index at once.
func (d *Daemon) actionRepoClone(ctx context.Context, raw json.RawMessage) (any, error) {
	p, err := decodeAction[contracts.RepoCloneParams](contracts.ActionRepoClone, raw)
	if err != nil {
		return nil, err
	}
	rs := d.repoState()
	rs.mu.Lock()
	idx := rs.last
	rs.mu.Unlock()
	opts := d.repoOptions()
	if idx.IndexedAt.IsZero() {
		// No pass yet: read the layout now (bounded, metadata only).
		if fresh, err := repoindex.Discover(ctx, opts); err == nil {
			idx = fresh
		}
	}
	roots := opts.Roots
	if len(roots) == 0 {
		home, _ := os.UserHomeDir()
		roots = repoindex.DefaultRoots(home)
	}
	gh := ""
	if found, err := exec.LookPath("gh"); err == nil {
		gh = found
	}
	if c := d.cfg.RepoIndex.GitHub.GH; c != "" {
		gh = c
	}
	res, err := repoindex.Clone(ctx, p, repoindex.CloneOptions{Index: idx, Roots: roots, GH: gh})
	if err != nil {
		return nil, err
	}
	d.cfg.Logger.Info("repository cloned", "path", res.Path, "cloned", res.Cloned)
	d.addToIndex(ctx, res.Repo)
	return res, nil
}

// addToIndex records a new checkout in the last index and reports it now
// (when the index is enabled), so the hub lists it without a rescan.
func (d *Daemon) addToIndex(ctx context.Context, r contracts.Repo) {
	if r.Path == "" || !d.cfg.RepoIndex.Enabled {
		return
	}
	rs := d.repoState()
	rs.mu.Lock()
	idx := rs.last
	for _, e := range idx.Repos {
		if filepath.Clean(e.Path) == filepath.Clean(r.Path) {
			rs.mu.Unlock()
			return
		}
	}
	idx.Repos = append(append([]contracts.Repo{}, idx.Repos...), r)
	idx.NodeID = d.nodeID
	idx.IndexedAt = d.now().UTC()
	if idx.Roots == nil {
		idx.Roots = []string{}
	}
	rs.last = idx
	rs.lastHash = "" // the next pass reports again
	rs.mu.Unlock()
	if _, err := d.rep.Enqueue(ctx, contracts.ReportRepos, idx); err == nil {
		rs.mu.Lock()
		rs.lastSent = d.now()
		rs.mu.Unlock()
	}
}

// ---- node-local API ----

var _ contracts.RepoCatalogAPI = (*LocalAPI)(nil)

// RepoCatalog forwards repos.catalog; while the hub is unreachable (or
// predates the catalog) it answers from this node's own index.
func (l *LocalAPI) RepoCatalog(ctx context.Context, req contracts.RepoCatalogRequest) (contracts.RepoCatalog, error) {
	a, err := l.d.upstream(bearerFrom(ctx))
	if err == nil {
		if r, ok := a.(contracts.RepoCatalogAPI); ok {
			res, err := r.RepoCatalog(l.d.upstreamCtx(ctx), req)
			if err == nil || (!hubDown(err) && !contracts.IsCode(err, contracts.ErrUnsupported)) {
				return res, err
			}
		}
	} else if !hubDown(err) && !contracts.IsCode(err, contracts.ErrUnavailable) {
		return contracts.RepoCatalog{}, err
	}
	// An older hub: its repos.list (every machine); the hub away: this node.
	list, err := l.ListRepos(ctx, contracts.ListReposRequest{Limit: 500})
	if err != nil {
		list = l.d.localRepos(contracts.ListReposRequest{Limit: 500})
	}
	items := repoindex.Catalog(list.Items, nil, req.Query, l.d.now())
	return contracts.RepoCatalog{Items: items, Nodes: list.Nodes}, nil
}
