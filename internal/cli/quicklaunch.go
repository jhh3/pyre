package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/repoindex"
)

// Quick launch by repository (W-quicklaunch): `pyre new <repo>[+<repo>…]
// [@machine] [claude|codex|agy|shell] [first prompt…]`. Repository names are
// fuzzy-matched against the fleet's repo index (repos.list); the machine
// defaults to where the repository exists, preferring the one it was most
// recently used on. The default working directory is the repository's
// main checkout (the agent decides about worktrees); --fresh (implied by
// --branch and by several repositories) creates worktrees first.

// QuickParams drives a quick launch (CLI `pyre new`, MCP start_session
// with repo/repos).
type QuickParams struct {
	Repos       []string `json:"repos"`
	Path        string   `json:"path,omitempty"`  // a plain folder instead of repositories (W-qlpath)
	Mkdir       bool     `json:"mkdir,omitempty"` // create Path first (inside the machine's home only)
	Node        string   `json:"node,omitempty"`
	Harness     string   `json:"harness,omitempty"`
	Prompt      string   `json:"prompt,omitempty"`
	Title       string   `json:"title,omitempty"`
	Fresh       bool     `json:"fresh,omitempty"`
	Branch      string   `json:"branch,omitempty"`
	Account     string   `json:"account,omitempty"`
	Ambient     bool     `json:"ambient,omitempty"`
	Permissions string   `json:"permissions,omitempty"`
	Model       string   `json:"model,omitempty"`
	Effort      string   `json:"effort,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	OperationID string   `json:"operation_id,omitempty"`
	// NoClone refuses instead of cloning a repository the machine lacks
	// (W-newsession: by default it is cloned there first).
	NoClone bool `json:"no_clone,omitempty"`
	// Progress receives one-line progress notes (clone confirmations).
	Progress func(string) `json:"-"`
	// HarnessArgs: extra harness arguments (StartParams.HarnessArgs).
	HarnessArgs []string `json:"harness_args,omitempty"`
}

// QuickLaunchResult is `pyre new --json` output: the start response plus
// the repositories and any worktrees created for it.
type QuickLaunchResult struct {
	c.StartSessionResponse
	Repos     []c.RepoEntry           `json:"repos"`
	Worktrees *c.WorktreeCreateResult `json:"worktrees,omitempty"`
	Folder    string                  `json:"folder,omitempty"`    // plain-folder launch: the folder
	Created   bool                    `json:"created,omitempty"`   // the folder was created for it
	Cloned    []c.RepoCloneResult     `json:"cloned,omitempty"`    // repositories cloned onto the machine first
	Placement *Placement              `json:"placement,omitempty"` // machine chosen by load (placement.go)
}

// RepoPicker chooses among ambiguous matches (index into groups), or
// returns an error to cancel. Nil means ambiguity is an error.
type RepoPicker func(spec string, groups [][]c.RepoEntry) (int, error)

func (o *Ops) reposAPI() (c.ReposAPI, error) {
	r, ok := o.API.(c.ReposAPI)
	if !ok {
		return nil, c.Errorf(c.ErrUnsupported, "this connection does not offer the repo index (repos.list); upgrade pyre-hub and pyred")
	}
	return r, nil
}

// ListRepos searches the repo index.
func (o *Ops) ListRepos(ctx context.Context, query, node string, limit int) (c.RepoList, error) {
	r, err := o.reposAPI()
	if err != nil {
		return c.RepoList{}, err
	}
	req := c.ListReposRequest{Query: query, Limit: limit}
	if node != "" {
		if req.NodeID, err = o.NodeID(ctx, node); err != nil {
			return c.RepoList{}, err
		}
	}
	return r.ListRepos(ctx, req)
}

// RescanResult is one machine's repos.reindex outcome.
type RescanResult struct {
	NodeID   string `json:"node_id"`
	NodeName string `json:"node_name"`
	Repos    int    `json:"repos"`
	// Pending: the machine rescanned, but the hub had not listed the new
	// index within the wait (it arrives through the node's report outbox).
	Pending bool   `json:"pending,omitempty"`
	Error   string `json:"error,omitempty"`
}

// rescanWait bounds how long RescanRepos waits for the hub to list a new index.
var rescanWait = 10 * time.Second

// RescanRepos runs repos.reindex now on one machine (node) or on every
// connected machine, then waits (bounded) until the hub lists each new
// index, so a freshly cloned repository shows up at once. A machine that
// fails is reported in its result; the error is returned only when none
// rescanned.
func (o *Ops) RescanRepos(ctx context.Context, node string) ([]RescanResult, error) {
	if _, err := o.reposAPI(); err != nil {
		return nil, err
	}
	var nodes []c.Node
	if node != "" {
		n, err := o.ResolveNode(ctx, node)
		if err != nil {
			return nil, err
		}
		nodes = []c.Node{n}
	} else {
		page, err := o.ListNodes(ctx, false)
		if err != nil {
			return nil, err
		}
		for _, n := range page.Items {
			if n.Connection.State == c.ConnConnected {
				nodes = append(nodes, n)
			}
		}
		if len(nodes) == 0 {
			return nil, c.Errorf(c.ErrUnavailable, "no connected machines to rescan (see `pyre nodes`)")
		}
	}
	out := make([]RescanResult, len(nodes))
	idx := make([]c.NodeRepoIndex, len(nodes))
	var wg sync.WaitGroup
	for i, n := range nodes {
		out[i] = RescanResult{NodeID: n.NodeID, NodeName: n.Name}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := o.NodeAction(ctx, n.NodeID, c.ActionRepoReindex, struct{}{}, &idx[i], ""); err != nil {
				out[i].Error = err.Error()
				return
			}
			out[i].Repos = len(idx[i].Repos)
		}()
	}
	wg.Wait()
	var firstErr error
	ok := 0
	for i := range out {
		if out[i].Error == "" {
			ok++
		} else if firstErr == nil {
			firstErr = c.Errorf(c.ErrUnavailable, "rescan on %s: %s", orDash(out[i].NodeName), out[i].Error)
		}
	}
	if ok == 0 {
		return out, firstErr
	}
	// Wait until the hub lists each new index: reported at (or after) the
	// rescan, or already holding every repository it found (an unchanged
	// index is not re-sent).
	deadline := time.Now().Add(rescanWait)
	for i := range out {
		if out[i].Error != "" {
			continue
		}
		for {
			list, err := o.ListRepos(ctx, "", out[i].NodeID, 500)
			if err == nil && hubHasIndex(list, out[i].NodeID, idx[i]) {
				break
			}
			if time.Now().After(deadline) {
				out[i].Pending = true
				break
			}
			select {
			case <-ctx.Done():
				return out, ctx.Err()
			case <-time.After(300 * time.Millisecond):
			}
		}
	}
	return out, nil
}

func hubHasIndex(list c.RepoList, nodeID string, idx c.NodeRepoIndex) bool {
	if at, ok := list.Nodes[nodeID]; ok && !idx.IndexedAt.IsZero() && !at.Before(idx.IndexedAt.Truncate(time.Second)) {
		return true
	}
	have := map[string]bool{}
	for _, e := range list.Items {
		if e.NodeID == nodeID {
			have[e.Path] = true
		}
	}
	for _, r := range idx.Repos {
		if !have[r.Path] {
			return false
		}
	}
	return true
}

func describeGroup(g []c.RepoEntry) string {
	var nodes []string
	for _, e := range g {
		nodes = append(nodes, e.NodeName)
	}
	s := g[0].Name + " (" + strings.Join(nodes, ", ") + ")"
	if g[0].RemoteURL != "" {
		s += " " + shortRemote(g[0].RemoteURL)
	} else {
		s += " " + g[0].Path
	}
	return s
}

func shortRemote(u string) string {
	k := strings.TrimPrefix(repoindex.RepoKey(c.Repo{RemoteURL: u}), "remote:")
	return k
}

func ambiguousError(spec string, groups [][]c.RepoEntry) error {
	var lines []string
	for i, g := range groups {
		if i == 8 {
			lines = append(lines, fmt.Sprintf("  … and %d more", len(groups)-8))
			break
		}
		lines = append(lines, "  "+describeGroup(g))
	}
	return usagef("%q matches several repositories; be more specific:\n%s", spec, strings.Join(lines, "\n"))
}

// QuickLaunch resolves repositories and a machine, creates fresh
// worktrees when asked, and starts the session.
func (o *Ops) QuickLaunch(ctx context.Context, p QuickParams, pick RepoPicker) (res QuickLaunchResult, err error) {
	var pl Placement
	ctx = withPlacement(ctx, &pl)
	defer func() {
		if pl.NodeID != "" {
			res.Placement = &pl
		}
	}()
	if p.Path != "" {
		return o.folderLaunch(ctx, p)
	}
	if len(p.Repos) == 0 {
		return res, usagef("name a repository (see `pyre repos`)")
	}
	if p.Harness == "" {
		p.Harness = string(c.HarnessClaude)
	}
	if !validHarness(p.Harness) {
		return res, usagef("harness must be claude, codex or shell (got %q)", p.Harness)
	}
	ra, err := o.reposAPI()
	if err != nil {
		return res, err
	}
	filterNode := ""
	if p.Node != "" {
		if filterNode, err = o.NodeID(ctx, p.Node); err != nil {
			return res, err
		}
	}
	nodes, err := listAll(ctx, func(ctx context.Context, cur string) (c.Page[c.Node], error) {
		return o.API.ListNodes(ctx, c.ListNodesRequest{PageRequest: c.PageRequest{Cursor: cur}})
	})
	if err != nil {
		return res, err
	}
	connected := map[string]bool{}
	names := map[string]string{}
	for _, n := range nodes.Items {
		connected[n.NodeID] = n.Connection.State == c.ConnConnected
		names[n.NodeID] = n.Name
	}
	// Each spec is a repository some machine has indexed, else one from
	// the global catalog (GitHub), else an owner/name or clone URL.
	type choice struct {
		group []c.RepoEntry
		cat   *c.CatalogRepo
	}
	var choices []choice
	var catalog []c.CatalogRepo
	catLoaded := false
	loadCatalog := func() {
		if catLoaded {
			return
		}
		catLoaded = true
		if ca, ok := o.API.(c.RepoCatalogAPI); ok {
			if cat, err := ca.RepoCatalog(ctx, c.RepoCatalogRequest{Limit: 2000}); err == nil {
				catalog = cat.Items
			}
		}
	}
	for _, spec := range p.Repos {
		spec = strings.TrimSpace(spec)
		if spec == "" {
			return res, usagef("empty repository name in %q", strings.Join(p.Repos, "+"))
		}
		list, err := ra.ListRepos(ctx, c.ListReposRequest{Query: spec, NodeID: filterNode, Limit: 100})
		if err != nil {
			return res, err
		}
		groups := repoindex.Contenders(repoindex.Group(list.Items))
		if len(groups) > 0 && filterNode != "" {
			// On a named machine, an exact repository elsewhere beats a
			// loose match here (it is cloned there).
			exact := false
			for _, g := range groups {
				exact = exact || strings.EqualFold(g[0].Name, spec) || strings.EqualFold(repoindex.FullNameOf(g[0].RemoteURL), spec)
			}
			if !exact {
				loadCatalog()
				if item, _, ok := repoindex.FindCatalog(spec, catalog); ok {
					choices = append(choices, choice{cat: &item})
					continue
				}
			}
		}
		if len(groups) == 0 {
			loadCatalog()
			item, many, ok := repoindex.FindCatalog(spec, catalog)
			switch {
			case ok:
				choices = append(choices, choice{cat: &item})
				continue
			case len(many) > 1:
				var lines []string
				for _, m := range many {
					lines = append(lines, "  "+orDash(m.FullName)+" "+m.RemoteURL)
				}
				return res, usagef("%q matches several repositories; name it as owner/name:\n%s", spec, strings.Join(lines, "\n"))
			case repoindex.IsGitHubSpec(spec) || strings.Contains(spec, "://") || strings.HasPrefix(spec, "git@"):
				remote := repoindex.GitHubRemote(spec)
				name := path.Base(strings.TrimSuffix(repoindex.NormalizeRemote(remote), "/"))
				choices = append(choices, choice{cat: &c.CatalogRepo{Key: repoindex.CatalogKey(c.Repo{RemoteURL: remote}), Name: name, FullName: repoindex.FullNameOf(remote), RemoteURL: remote, Present: []c.RepoEntry{}}})
				continue
			}
			where := "any machine"
			if filterNode != "" {
				where = names[filterNode]
			}
			return res, c.Errorf(c.ErrNotFound, "no repository matches %q on %s or in your GitHub repositories; `pyre repos` lists the index (nodes scan ~/code, ~/src, ~/Documents/code and ~/projects every 10 minutes; pyred --repo-roots changes that), and owner/name clones any GitHub repository", spec, where)
		}
		i, ok := repoindex.PickGroup(spec, groups)
		if !ok {
			if pick == nil {
				return res, ambiguousError(spec, groups)
			}
			if i, err = pick(spec, groups); err != nil {
				return res, err
			}
		}
		choices = append(choices, choice{group: groups[i]})
	}
	// The machine: the named one; else one holding every repository; else
	// the one holding most of them (the rest are cloned); else this one.
	nodeID := filterNode
	if nodeID == "" {
		var chosen [][]c.RepoEntry
		all := true
		for _, ch := range choices {
			if ch.group == nil {
				all = false
				continue
			}
			chosen = append(chosen, ch.group)
		}
		if all {
			id, err := repoindex.ChooseMachine(chosen, connected, "")
			if err == nil {
				nodeID = id
				// Several machines hold them all: the least loaded one
				// (placement.go); recency only breaks ties.
				if pl, perr := o.placeAmong(ctx, nodes.Items, p.Harness, p.Account, p.Ambient, false, repoindex.Machines(chosen, connected), p.Model != "" || p.Effort != ""); perr == nil {
					nodeID = pl.NodeID
					notePlacement(ctx, pl)
				}
			} else if p.NoClone {
				_, err := chooseNode(chosen, connected, "")
				return res, err
			}
		}
		if nodeID == "" {
			// Most repositories present; ties go to the better machine
			// (connected, used most recently) of the first repository.
			best := 0
			count := map[string]int{}
			var order []string
			for _, g := range chosen {
				for _, id := range repoindex.Machines([][]c.RepoEntry{g}, connected) {
					if connected[id] {
						if count[id] == 0 {
							order = append(order, id)
						}
						count[id]++
					}
				}
			}
			for _, id := range order {
				if count[id] > best {
					nodeID, best = id, count[id]
				}
			}
		}
		if nodeID == "" {
			id, err := o.NodeID(ctx, "")
			if err != nil {
				return res, usagef("say which machine gets the clone: pyre new %s @<machine> (%v)", strings.Join(p.Repos, "+"), err)
			}
			nodeID = id
		}
	}
	opID := p.OperationID
	if opID == "" {
		opID = c.NewID(c.PrefixOperation)
	}
	var paths []string
	for i, ch := range choices {
		if e, ok := repoindex.EntryOn(ch.group, nodeID); ok {
			res.Repos = append(res.Repos, e)
			paths = append(paths, e.Path)
			continue
		}
		var remote, label string
		var elsewhere []string
		switch {
		case ch.group != nil:
			remote, label = ch.group[0].RemoteURL, ch.group[0].Name
			for _, e := range ch.group {
				elsewhere = append(elsewhere, e.NodeName)
			}
		default:
			remote, label = ch.cat.RemoteURL, orDash(ch.cat.FullName)
			if ch.cat.FullName == "" {
				label = ch.cat.Name
			}
			for _, e := range ch.cat.Present {
				elsewhere = append(elsewhere, e.NodeName)
			}
		}
		if remote == "" {
			return res, usagef("%s has no remote, so it cannot be cloned onto %s (it is on %s)", label, orDash(names[nodeID]), strings.Join(elsewhere, ", "))
		}
		if p.NoClone {
			return res, usagef("%s is not on %s; drop --no-clone to clone it there", label, orDash(names[nodeID]))
		}
		if !connected[nodeID] {
			return res, c.Errorf(c.ErrUnavailable, "%s is not connected; cannot clone %s there", orDash(names[nodeID]), label)
		}
		if p.Progress != nil {
			p.Progress(repoindex.DescribeClone(remote, orDash(names[nodeID])) + "…")
		}
		cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		cl, err := repoindex.CloneVia(cctx, o.API, nodeID, c.RepoCloneParams{Remote: remote}, fmt.Sprintf("%s.clone.%d", opID, i))
		cancel()
		if err != nil {
			return res, err
		}
		if p.Progress != nil {
			if cl.Cloned {
				p.Progress("cloned into " + cl.Path)
			} else {
				p.Progress("already there: " + cl.Path)
			}
		}
		res.Cloned = append(res.Cloned, cl)
		res.Repos = append(res.Repos, c.RepoEntry{Repo: cl.Repo, NodeID: nodeID, NodeName: names[nodeID]})
		paths = append(paths, cl.Path)
	}
	fresh := p.Fresh || p.Branch != "" || len(paths) > 1
	cwd := paths[0]
	if fresh {
		slug := p.Title
		if slug == "" {
			slug = p.Prompt
		}
		wctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		wt, err := repoindex.CreateVia(wctx, o.API, nodeID, c.WorktreeCreateParams{Repos: paths, Branch: p.Branch, Slug: slug}, opID+".worktrees")
		cancel()
		if err != nil {
			return res, err
		}
		for _, w := range wt.Warnings {
			if p.Progress != nil {
				p.Progress("warning: " + w)
			}
		}
		res.Worktrees = &wt
		cwd = wt.Cwd
	}
	start, err := o.StartSession(ctx, StartParams{Node: nodeID, Harness: p.Harness, Cwd: cwd, Prompt: p.Prompt, Title: p.Title,
		Account: p.Account, Ambient: p.Ambient, Permissions: p.Permissions, Model: p.Model, Effort: p.Effort, Tags: p.Tags, OperationID: opID, HarnessArgs: p.HarnessArgs})
	res.StartSessionResponse = start
	if res.Worktrees != nil {
		res.Warnings = append(res.Warnings, res.Worktrees.Warnings...) // start_session callers see them too
	}
	if err == nil && res.Worktrees != nil {
		o.claimWorktrees(ctx, nodeID, *res.Worktrees, start.Session.Session.SessionID, opID+".claim")
	}
	return res, err
}

// claimWorktrees names the session started in fresh worktrees in their
// markers, so its automatic cleanup may remove them (W-lifecycle). Best
// effort: an unclaimed worktree is simply never removed automatically.
func (o *Ops) claimWorktrees(ctx context.Context, nodeID string, wt c.WorktreeCreateResult, sessionID, opID string) {
	if wt.ClaimToken == "" || sessionID == "" || len(wt.Worktrees) == 0 {
		return
	}
	paths := make([]string, 0, len(wt.Worktrees))
	for _, w := range wt.Worktrees {
		paths = append(paths, w.Path)
	}
	var res c.WorktreeClaimResult
	_, _ = o.NodeAction(ctx, nodeID, c.ActionWorktreeClaim, c.WorktreeClaimParams{SessionID: sessionID, Token: wt.ClaimToken, Paths: paths}, &res, opID)
}

// folderLaunch starts a session in a plain folder (`pyre new ~/scratch`):
// on the named machine (default: this one, or the only connected one),
// created first with Mkdir, otherwise required to exist.
func (o *Ops) folderLaunch(ctx context.Context, p QuickParams) (QuickLaunchResult, error) {
	var res QuickLaunchResult
	if p.Harness == "" {
		p.Harness = string(c.HarnessClaude)
	}
	if !validHarness(p.Harness) {
		return res, usagef("harness must be claude, codex or shell (got %q)", p.Harness)
	}
	cwd, _ := os.Getwd()
	home, _ := os.UserHomeDir()
	folder, _ := repoindex.NormPath(p.Path, cwd, home)
	nodeID, err := o.NodeID(ctx, p.Node)
	if err != nil {
		if p.Node == "" {
			return res, usagef("say which machine: pyre new %s @<machine> (%v)", p.Path, err)
		}
		return res, err
	}
	opID := p.OperationID
	if opID == "" {
		opID = c.NewID(c.PrefixOperation)
	}
	if p.Mkdir {
		var mk c.FsMkdirResult
		if _, err := o.NodeAction(ctx, nodeID, c.ActionFsMkdir, c.FsMkdirParams{Path: folder}, &mk, opID+".mkdir"); err != nil {
			if c.IsCode(err, c.ErrNotFound) && strings.Contains(err.Error(), "unknown action") {
				return res, c.Errorf(c.ErrUnsupported, "this machine's pyred cannot create folders yet; upgrade it or create %s there", folder)
			}
			return res, err
		}
		res.Created = mk.Created
		if mk.Path != "" {
			folder = mk.Path
		}
	} else {
		var dl c.DirListResult
		if _, err := o.NodeAction(ctx, nodeID, c.ActionDirList, c.DirListParams{Path: folder}, &dl, ""); err != nil {
			if c.IsCode(err, c.ErrInvalid) {
				return res, usagef("%s is not a folder on that machine (%v); --mkdir creates it", folder, err)
			}
			return res, err
		}
		if dl.Path != "" {
			folder = dl.Path
		}
	}
	res.Folder = folder
	start, err := o.StartSession(ctx, StartParams{Node: nodeID, Harness: p.Harness, Cwd: folder, Prompt: p.Prompt, Title: p.Title,
		Account: p.Account, Ambient: p.Ambient, Permissions: p.Permissions, Model: p.Model, Effort: p.Effort, Tags: p.Tags, OperationID: opID, HarnessArgs: p.HarnessArgs})
	res.StartSessionResponse = start
	return res, err
}

// isFolderArg reports whether the first `pyre new` word is a folder: a
// path, or "home" when no repository has that name.
func (o *Ops) isFolderArg(ctx context.Context, a string) bool {
	if repoindex.IsPathSpec(a) {
		return true
	}
	if !strings.EqualFold(a, "home") {
		return false
	}
	list, err := o.ListRepos(ctx, a, "", 50)
	if err != nil {
		return true
	}
	for _, e := range list.Items {
		if strings.EqualFold(e.Name, a) {
			return false
		}
	}
	return true
}

// chooseNode picks the machine holding every chosen repository.
func chooseNode(chosen [][]c.RepoEntry, connected map[string]bool, want string) (string, error) {
	id, err := repoindex.ChooseMachine(chosen, connected, want)
	switch {
	case errors.Is(err, repoindex.ErrNoCommonMachine):
		var where []string
		for _, g := range chosen {
			where = append(where, describeGroup(g))
		}
		return "", usagef("no single machine has all of these repositories: %s", strings.Join(where, "; "))
	case err != nil:
		return "", usagef("%v", err)
	}
	return id, nil
}

// parseNewArgs splits `<repo>[+<repo>…] [@machine] [harness] [prompt…]`.
func parseNewArgs(args []string) (repos []string, node, harness, prompt string) {
	return repoindex.ParseLine(args)
}

func (a *App) addQuickLaunchCommands(root *cobra.Command) {
	var p QuickParams
	cmd := &cobra.Command{
		Use:   "new <repo>[+<repo>…] | <path>  [@machine] [claude|codex|agy|shell] [first prompt…]",
		Short: "Start a session in a repository by name (fuzzy), or in any folder",
		Long: `Start a session in a repository found by name. Names are fuzzy-matched
against the repositories every node indexes (pyre repos lists them); an
ambiguous name asks you to choose (or lists the choices when not on a
terminal). The machine defaults to where the repository exists: the least
loaded of those machines (CPU, memory, swap, live runs; recency breaks ties),
named on stderr with the reason; @machine picks one. Claude is the
default harness; the rest of the line is the first prompt.

The session starts in the repository's main checkout: the agent decides
whether to make its own worktree (Claude Code makes them under
.claude/worktrees). --fresh starts it in a new worktree instead, on a branch
named from the prompt or title (pyre/fix-share-link-redirect, deduplicated),
in the repository's worktree folder (<repo>/.worktrees/, or an existing
<repo>-worktrees/ next to it). --branch names that branch (implies --fresh).
The new branch starts at origin's default branch, fetched first (never the
main checkout's possibly stale local branch), and tracks nothing: push it
with git push -u origin <branch>. A failed fetch stops the launch; a
repository without an origin remote branches from its HEAD, with a warning.

Any repository works on any machine: owner/name (one of your GitHub
repositories, or any GitHub repository) or a repository another machine has
is cloned onto the chosen machine first, into its repo root laid out like the
repositories already there (~/code/<name>, or ~/code/<owner>/<name>), with
that machine's own git credentials; a line on stderr says so. An existing
directory is never overwritten. --no-clone refuses instead.

Several repositories (a+b) always get fresh worktrees on one branch, side by
side in ~/pyre-work/<slug>/, and the agent starts in that folder; a
CLAUDE.md/AGENTS.md there lists the repositories.

A path instead of a name (starting with /, ~, ./ or ../; ~ or "home" alone
is the home folder) starts in that plain folder, no repository or project
needed, on @machine (default: this machine, or the only connected one).
Relative paths resolve against the current directory. The folder must exist
unless --mkdir creates it (inside the machine's home only).

Examples:
  pyre new pyre
  pyre new studio @server "fix the share link redirect"
  pyre new acme/atlas @laptop          # cloned onto laptop first if it lacks it
  pyre new studio --fresh fix the share link redirect
  pyre new api+web codex "rename the share endpoint in both"
  pyre new ~ "what is using my disk space?"
  pyre new ~/scratch/vid @server --mkdir "convert these clips"`,
		Args: cobra.ArbitraryArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			var repos []string
			var node, harness, prompt string
			folder := ""
			if len(args) > 0 && ops.isFolderArg(cmd.Context(), args[0]) {
				folder = args[0]
				_, node, harness, prompt = parseNewArgs(append([]string{"_"}, args[1:]...))
			} else {
				repos, node, harness, prompt = parseNewArgs(args)
			}
			if p.Mkdir && folder == "" {
				return usagef("--mkdir needs a path (pyre new ~/new-folder --mkdir)")
			}
			if len(repos) == 0 && folder == "" {
				if !a.IsTerminal() || a.jsonOut {
					return usagef("name a repository: pyre new <repo> [@machine] [claude|codex|agy|shell] [prompt…] (pyre repos lists them)")
				}
				spec, err := a.promptRepo(cmd.Context(), ops)
				if err != nil {
					return err
				}
				repos = []string{spec}
			}
			q := p
			q.Repos, q.Path, q.OperationID = repos, folder, a.opID
			q.Progress = func(line string) { a.warnf("%s", line) } // stderr: the confirmation line, also with --json
			if node != "" {
				q.Node = node
			}
			if harness != "" {
				q.Harness = harness
			}
			if prompt != "" {
				if q.Prompt != "" {
					return usagef("give the prompt either as words or with --prompt, not both")
				}
				q.Prompt = prompt
			}
			if q.Prompt, err = a.readArg(q.Prompt); err != nil {
				return err
			}
			var pick RepoPicker
			if a.IsTerminal() && !a.jsonOut {
				pick = a.pickRepoInline
			}
			res, err := ops.QuickLaunch(cmd.Context(), q, pick)
			if err != nil {
				return err
			}
			a.notePlaced(res.Placement)
			if res.Worktrees == nil && res.Folder == "" {
				a.warnSharedRepo(cmd.Context(), ops, res.StartSessionResponse)
			}
			return a.emit(res, func(w io.Writer) { printQuickLaunch(w, res) })
		}),
	}
	f := cmd.Flags()
	f.BoolVar(&p.NoClone, "no-clone", false, "refuse instead of cloning a repository the machine does not have yet")
	f.BoolVar(&p.Mkdir, "mkdir", false, "with a path: create the folder first (inside the machine's home)")
	f.BoolVar(&p.Fresh, "fresh", false, "start in a new worktree on a new branch (default: the main checkout; the agent decides about worktrees)")
	f.StringVar(&p.Branch, "branch", "", "branch for the fresh worktree (implies --fresh; default pyre/<slug of the prompt or title>)")
	f.StringVar(&p.Account, "account", "", "explicit account: ID, label or rota name, or a unique prefix")
	f.BoolVar(&p.Ambient, "ambient", false, "use the machine's existing login (no Pyre account assignment)")
	f.StringVar(&p.Permissions, "permissions", "", "ask or bypass (default: the machine's pyred --permissions)")
	f.StringVar(&p.Model, "model", "", "model (default: the harness's own; choices: pyre models)")
	f.StringVar(&p.Effort, "effort", "", "reasoning effort: low, medium, high, xhigh, max (default: the harness's own)")
	f.StringVar(&p.Title, "title", "", "session title (default: automatic)")
	f.StringVar(&p.Prompt, "prompt", "", "first prompt (- reads stdin), instead of trailing words")
	f.StringSliceVar(&p.Tags, "tag", nil, "tags")
	root.AddCommand(cmd)

	var node string
	var limit int
	var rescan bool
	repos := &cobra.Command{
		Use:   "repos [query] [--node N] [--rescan]",
		Short: "List or search the repositories the machines have indexed",
		Long: `List the git repositories each machine found under its repo roots
(~/code, ~/src, ~/Documents/code and ~/projects by default; pyred
--repo-roots changes them), most recently used first. A query fuzzy-matches
names, remotes and paths. Only metadata is indexed, never file contents.

Machines rescan every 10 minutes; --rescan rescans now (every connected
machine, or --node N) and lists after the hub has the new index, so a
repository you just cloned shows up at once.`,
		Args: cobra.MaximumNArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			q := ""
			if len(args) == 1 {
				q = args[0]
			}
			if rescan {
				res, err := ops.RescanRepos(cmd.Context(), node)
				for _, r := range res {
					switch {
					case r.Error != "":
						a.warnf("rescan on %s failed: %s", orDash(r.NodeName), r.Error)
					case r.Pending:
						a.warnf("rescanned %s (%d %s); the hub has not listed the new index yet, try again shortly", orDash(r.NodeName), r.Repos, pluralWord(r.Repos, "repository", "repositories"))
					default:
						a.warnf("rescanned %s: %d %s", orDash(r.NodeName), r.Repos, pluralWord(r.Repos, "repository", "repositories"))
					}
				}
				if err != nil {
					return err
				}
			}
			list, err := ops.ListRepos(cmd.Context(), q, node, limit)
			if err != nil {
				return err
			}
			return a.emit(list, func(w io.Writer) { printRepos(w, list) })
		}),
	}
	repos.Flags().StringVar(&node, "node", "", "only this machine")
	repos.Flags().IntVar(&limit, "limit", 50, "most results (max 500)")
	repos.Flags().BoolVar(&rescan, "rescan", false, "rescan the repo roots now (all connected machines, or --node), then list")
	root.AddCommand(repos)
}

func printRepos(w io.Writer, list c.RepoList) {
	if len(list.Items) == 0 {
		fmt.Fprintln(w, "no repositories indexed yet (machines scan their repo roots every 10 minutes)")
		return
	}
	var rows [][]string
	for _, e := range list.Items {
		used := "-"
		if e.LastUsedAt != nil {
			used = ago(*e.LastUsedAt)
		}
		branch := e.Branch
		if e.DefaultBranch != "" && e.DefaultBranch != e.Branch {
			branch += " (default " + e.DefaultBranch + ")"
		}
		rows = append(rows, []string{e.Name, e.NodeName, e.Path, orDash(branch), used, ago(e.LastActivity)})
	}
	table(w, "REPO\tMACHINE\tPATH\tBRANCH\tLAST SESSION\tLAST GIT ACTIVITY", rows)
}

func printQuickLaunch(w io.Writer, res QuickLaunchResult) {
	s := res.Session
	var names []string
	for _, r := range res.Repos {
		names = append(names, r.Name)
	}
	where := strings.Join(names, "+")
	if res.Folder != "" {
		where = res.Folder
		if res.Created {
			where += " (new folder)"
		}
	}
	fmt.Fprintf(w, "started %s in %s on %s (%s)", s.Session.SessionID, where, orDash(s.NodeName), s.Session.Harness)
	if s.Session.Title != "" {
		fmt.Fprintf(w, " %q", s.Session.Title)
	}
	fmt.Fprintln(w)
	for _, cl := range res.Cloned {
		if cl.Cloned {
			fmt.Fprintf(w, "cloned %s into %s\n", cl.Remote, cl.Path)
		}
	}
	if res.Worktrees != nil {
		fmt.Fprintf(w, "fresh worktree on branch %s: %s\n", res.Worktrees.Branch, res.Worktrees.Cwd)
	} else if len(res.Repos) == 1 {
		fmt.Fprintf(w, "in the main checkout %s (the agent decides about worktrees; --fresh starts in a new one)\n", res.Repos[0].Path)
	}
	if s.Run != nil && s.Run.AccountID != "" {
		fmt.Fprintf(w, "account %s\n", s.Run.AccountID)
	}
	fmt.Fprintf(w, "next: pyre attach %s | pyre read %s --tail 40 | pyre send %s \"...\"\n", s.Session.SessionID, s.Session.SessionID, s.Session.SessionID)
}

// warnSharedRepo is the shared-checkout warning for a session started in a
// repository's main checkout.
func (a *App) warnSharedRepo(ctx context.Context, ops *Ops, res c.StartSessionResponse) {
	r := res.Session.Run
	if r == nil || r.Cwd == "" {
		return
	}
	peers, err := ops.SharedCheckoutPeers(ctx, r.NodeID, r.Cwd, res.Session.Session.SessionID)
	if err != nil || len(peers) == 0 {
		return
	}
	a.warnf("shared checkout %s on %s is also used by:", r.Cwd, orDash(res.Session.NodeName))
	for _, s := range peers {
		a.warnf("  %s %q (%s, %s)", s.Session.SessionID, s.Session.Title, s.Session.Harness, s.Run.State)
	}
	a.warnf("tell them what you will change (pyre send <session> \"...\"), or start with --fresh for an isolated worktree")
}

// pickRepoInline asks on the terminal which match was meant.
func (a *App) pickRepoInline(spec string, groups [][]c.RepoEntry) (int, error) {
	n := min(len(groups), 9)
	fmt.Fprintf(a.Stderr, "%q matches several repositories:\n", spec)
	for i := 0; i < n; i++ {
		fmt.Fprintf(a.Stderr, "  %d  %s\n", i+1, describeGroup(groups[i]))
	}
	fmt.Fprintf(a.Stderr, "choose 1-%d (enter = 1, q cancels): ", n)
	line, _ := bufio.NewReader(a.Stdin).ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return 0, nil
	}
	if line == "q" {
		return 0, usagef("cancelled")
	}
	i, err := strconv.Atoi(line)
	if err != nil || i < 1 || i > n {
		return 0, usagef("no choice %q", line)
	}
	return i - 1, nil
}

// promptRepo lists recent repositories and reads a name or number.
func (a *App) promptRepo(ctx context.Context, ops *Ops) (string, error) {
	list, err := ops.ListRepos(ctx, "", "", 9)
	if err != nil {
		return "", err
	}
	groups := repoindex.Group(list.Items)
	if len(groups) > 0 {
		fmt.Fprintln(a.Stderr, "recent repositories:")
		for i, g := range groups {
			fmt.Fprintf(a.Stderr, "  %d  %s\n", i+1, describeGroup(g))
		}
	}
	fmt.Fprint(a.Stderr, "repository (name or number): ")
	line, _ := bufio.NewReader(a.Stdin).ReadString('\n')
	line = strings.TrimSpace(line)
	if i, err := strconv.Atoi(line); err == nil && i >= 1 && i <= len(groups) {
		return groups[i-1][0].Name, nil
	}
	if line == "" {
		return "", usagef("no repository given")
	}
	return line, nil
}

// startFromRepos is start_session/pyre start with repo or repos and no
// project: a quick launch without an interactive picker (ambiguity is an
// error listing the choices).
func (o *Ops) startFromRepos(ctx context.Context, p StartParams) (c.StartSessionResponse, error) {
	repos := p.Repos
	if p.Repo != "" {
		repos = append([]string{p.Repo}, repos...)
	}
	branch := p.Branch
	if branch == "" {
		branch = p.Worktree
	}
	res, err := o.QuickLaunch(ctx, QuickParams{Repos: repos, Node: p.Node, Harness: p.Harness, Prompt: p.Prompt, Title: p.Title,
		Fresh: p.Fresh, Branch: branch, Account: p.Account, Ambient: p.Ambient, Permissions: p.Permissions, Model: p.Model, Effort: p.Effort, Tags: p.Tags,
		OperationID: p.OperationID, HarnessArgs: p.HarnessArgs}, nil)
	return res.StartSessionResponse, err
}
