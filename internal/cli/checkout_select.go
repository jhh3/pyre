package cli

import (
	"context"
	"path"
	"strings"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/repoindex"
)

// launchCheckout turns `--project P --repo R [--worktree B] [--cwd D]` into a
// checkout request plus the working directory sent with the launch.
//
// The project's recorded checkout bindings (`pyre project update --checkout`)
// say where repo R lives on each node. Without --worktree the session runs in
// that directory (shared mode: other sessions may use it too). With
// --worktree the binding is the source repository the isolated worktree is
// created from. An explicit --cwd always wins: it either selects one of
// several bindings or names an unrecorded directory to use as the shared
// checkout. Fresh or branch go to freshProjectCheckout instead.
func (o *Ops) launchCheckout(ctx context.Context, projectID *string, nodeID string, p StartParams) (*c.CheckoutRequest, string, error) {
	cwd := p.Cwd
	if p.Worktree == "" && p.Repo == "" {
		return nil, cwd, nil
	}
	mode := c.CheckoutShared
	if p.Worktree != "" {
		mode = c.CheckoutWorktree
	}
	co := &c.CheckoutRequest{RepoID: p.Repo, Mode: mode, Branch: p.Worktree}
	if p.Repo != "" {
		if projectID == nil {
			return nil, "", usagef("--repo names a repository of a project; pass --project too (or use --cwd alone for a plain directory)")
		}
		proj, b, found, err := o.projectRepoBinding(ctx, *projectID, nodeID, p.Repo, cwd)
		if err != nil {
			return nil, "", err
		}
		switch {
		case found:
			if cwd == "" {
				cwd = b.Path
			}
			if mode != c.CheckoutWorktree {
				co.Path = b.Path
				if b.Mode == c.CheckoutPlain {
					co.Mode = c.CheckoutPlain
				}
				if co.Branch == "" {
					co.Branch = b.Branch
				}
			}
		case cwd == "":
			return nil, "", usagef("project %s has no checkout of repo %s on that node; record one with `pyre project update %s --checkout <node>:/abs/path:%s:shared`, or pass --cwd",
				proj.Name, p.Repo, proj.Name, p.Repo)
		}
	}
	if mode != c.CheckoutWorktree && co.Path == "" {
		if cwd == "" {
			return nil, "", usagef("a shared checkout needs a directory: pass --cwd (or --project with --repo)")
		}
		co.Path = cwd
	}
	if mode == c.CheckoutWorktree && cwd == "" {
		return nil, "", usagef("--worktree needs the source repository: pass --cwd (or --project with --repo and a recorded checkout)")
	}
	return co, cwd, nil
}

// projectRepoBinding checks that repo is one of the project's repositories
// and picks its recorded checkout on node (pickBinding).
func (o *Ops) projectRepoBinding(ctx context.Context, projectID, nodeID, repo, cwd string) (c.Project, c.CheckoutBinding, bool, error) {
	proj, err := o.API.GetProject(ctx, c.GetRequest{ID: projectID})
	if err != nil {
		return proj, c.CheckoutBinding{}, false, err
	}
	known := false
	var repoIDs []string
	for _, r := range proj.Repos {
		repoIDs = append(repoIDs, r.RepoID)
		known = known || r.RepoID == repo
	}
	if !known {
		return proj, c.CheckoutBinding{}, false, usagef("project %s has no repo %q (repos: %s); add it with `pyre project update %s --repo %s=<url>`",
			proj.Name, repo, orDash(strings.Join(repoIDs, ", ")), proj.Name, repo)
	}
	b, found, err := pickBinding(proj, nodeID, repo, cwd)
	return proj, b, found, err
}

// freshProjectCheckout is `--project P --repo R` with fresh or branch: an
// explicit fresh or branch wins over the project's checkout mode, shared
// included (iss_…); the recorded checkout is only
// the source. The worktree is a quick launch's (worktree.create): at <node's
// worktree root>/<repo>/<branch>, on exactly that branch or a conflict
// naming it, and a retry with the same operation ID gets the recorded one
// back. The session's checkout record names the worktree, which the node
// reuses as it is.
func (o *Ops) freshProjectCheckout(ctx context.Context, projectID *string, nodeID string, p StartParams, opID string) (*c.CheckoutRequest, c.WorktreeCreateResult, error) {
	var wt c.WorktreeCreateResult
	branch := p.Branch
	if p.Worktree != "" && branch != "" && p.Worktree != branch {
		return nil, wt, usagef("branch %q and worktree %q disagree; give one", branch, p.Worktree)
	}
	if branch == "" {
		branch = p.Worktree
	}
	if p.Repo == "" || projectID == nil {
		return nil, wt, usagef("fresh and branch need the repository: pass --repo with --project (or, without a project, a repository name)")
	}
	proj, b, found, err := o.projectRepoBinding(ctx, *projectID, nodeID, p.Repo, p.Cwd)
	if err != nil {
		return nil, wt, err
	}
	src := p.Cwd
	if found {
		src = b.Path
	}
	if src == "" {
		return nil, wt, usagef("project %s has no checkout of repo %s on that node to make the fresh worktree from; record one with `pyre project update %s --checkout <node>:/abs/path:%s:shared`, or pass --cwd",
			proj.Name, p.Repo, proj.Name, p.Repo)
	}
	slug := p.Title
	if slug == "" {
		slug = p.Prompt
	}
	wctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	if wt, err = repoindex.CreateVia(wctx, o.API, nodeID, c.WorktreeCreateParams{Repos: []string{src}, Branch: branch, Slug: slug}, opID+".worktrees"); err != nil {
		return nil, wt, err
	}
	return &c.CheckoutRequest{RepoID: p.Repo, Mode: c.CheckoutWorktree, Branch: wt.Branch, Path: wt.Cwd}, wt, nil
}

// pickBinding chooses the project's checkout of repo on node. With cwd set it
// must be that binding's path (no match is not an error: cwd is then an
// unrecorded directory). Otherwise shared beats plain beats worktree, and
// several bindings of the best mode are ambiguous.
func pickBinding(p c.Project, nodeID, repo, cwd string) (c.CheckoutBinding, bool, error) {
	var cands []c.CheckoutBinding
	for _, b := range p.Checkouts {
		if b.NodeID == nodeID && b.RepoID == repo {
			cands = append(cands, b)
		}
	}
	if cwd != "" {
		for _, b := range cands {
			if path.Clean(b.Path) == path.Clean(cwd) {
				return b, true, nil
			}
		}
		return c.CheckoutBinding{}, false, nil
	}
	for _, m := range []c.CheckoutMode{c.CheckoutShared, c.CheckoutPlain, c.CheckoutWorktree} {
		var hits []c.CheckoutBinding
		for _, b := range cands {
			if b.Mode == m {
				hits = append(hits, b)
			}
		}
		switch len(hits) {
		case 0:
			continue
		case 1:
			return hits[0], true, nil
		}
		var paths []string
		for _, b := range hits {
			paths = append(paths, b.Path)
		}
		return c.CheckoutBinding{}, false, usagef("project %s has %d %s checkouts of %s on that node (%s); choose one with --cwd",
			p.Name, len(hits), m, repo, strings.Join(paths, ", "))
	}
	return c.CheckoutBinding{}, false, nil
}

// SharedCheckoutPeers lists other live sessions whose current run works in
// dir on node: the people to tell what you will change before you change it.
func (o *Ops) SharedCheckoutPeers(ctx context.Context, nodeID, dir, exclude string) ([]c.SessionView, error) {
	page, err := listAll(ctx, func(ctx context.Context, cur string) (c.Page[c.SessionView], error) {
		return o.API.ListSessions(ctx, c.ListSessionsRequest{PageRequest: c.PageRequest{Cursor: cur}, NodeID: nodeID})
	})
	if err != nil {
		return nil, err
	}
	dir = path.Clean(dir)
	var out []c.SessionView
	for _, s := range page.Items {
		r := s.Run
		if s.Session.SessionID == exclude || r == nil || r.State.Terminal() {
			continue
		}
		where := r.Cwd
		if r.Checkout != nil && r.Checkout.Path != "" {
			if r.Checkout.Mode == c.CheckoutWorktree {
				continue // isolated by construction
			}
			where = r.Checkout.Path
		}
		if where != "" && path.Clean(where) == dir {
			out = append(out, s)
		}
	}
	return out, nil
}

// warnSharedCheckout tells the launcher (on stderr, never in --json output)
// which other live sessions already work in the new session's directory.
// Sharing is allowed; this only makes it visible. Worktree launches are
// isolated and skipped. Lookup failures are silent: the launch succeeded.
func (a *App) warnSharedCheckout(ctx context.Context, ops *Ops, res c.StartSessionResponse, worktree bool) {
	r := res.Session.Run
	if r == nil {
		return
	}
	dir := r.Cwd
	if r.Checkout != nil {
		if r.Checkout.Mode == c.CheckoutWorktree {
			return
		}
		if r.Checkout.Path != "" {
			dir = r.Checkout.Path
		}
	}
	if dir == "" || worktree {
		return
	}
	peers, err := ops.SharedCheckoutPeers(ctx, r.NodeID, dir, res.Session.Session.SessionID)
	if err != nil || len(peers) == 0 {
		return
	}
	a.warnf("shared checkout %s on %s is also used by:", dir, orDash(res.Session.NodeName))
	for _, s := range peers {
		a.warnf("  %s %q (%s, %s)", s.Session.SessionID, s.Session.Title, s.Session.Harness, s.Run.State)
	}
	a.warnf("tell them what you will change (pyre send <session> \"...\"), or use --worktree for an isolated checkout")
}
