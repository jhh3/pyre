package contracts

import (
	"context"
	"time"
)

// Repo index and session worktrees (additive, W-quicklaunch).
//
// Each node discovers git repositories under configured roots in the
// background and reports their metadata (never file contents) as one
// replaceable snapshot (ReportRepos). The hub answers repos.list with a
// fuzzy, recent-first search across nodes, so `pyre new <repo>` and the
// quick-launch bar start a session without browsing directories.
//
// A node also reports the git worktrees of the repositories each live
// session touches (ReportSessionWorktrees): its working directory's
// repository and any worktree created under it during the session, the
// agent's own .claude/worktrees/* included. Archive offers to remove only
// the worktrees created during the session; the node re-checks every rule
// at removal time (never the main checkout, never uncommitted changes,
// never deleting an unmerged and unpushed branch).
//
// Hubs that predate these operations answer 404 (clients report
// unsupported). They are not part of API: callers type-assert ReposAPI.
const (
	// OpListRepos searches the fleet's repo index (ListReposRequest -> RepoList).
	OpListRepos = "repos.list"
	// OpSessionWorktrees returns a session's last reported worktrees
	// (SessionWorktreesRequest -> SessionWorktrees).
	OpSessionWorktrees = "sessions.worktrees"
)

// Node report kinds (additive; an older hub records them as rejects).
const (
	ReportRepos            ReportKind = "repos"             // NodeRepoIndex (replaceable per node)
	ReportSessionWorktrees ReportKind = "session_worktrees" // SessionWorktrees (replaceable per session)
)

// Node actions (additive).
const (
	// ActionWorktreeCreate creates fresh worktrees for a launch
	// (WorktreeCreateParams -> WorktreeCreateResult).
	ActionWorktreeCreate = "worktree.create"
	// ActionWorktreeScan reports a session's worktrees now
	// (WorktreeScanParams -> SessionWorktrees).
	ActionWorktreeScan = "worktree.scan"
	// ActionWorktreeRemove removes worktrees a session created, re-checking
	// every safety rule on the node (WorktreeRemoveParams -> WorktreeRemoveResult).
	ActionWorktreeRemove = "worktree.remove"
	// ActionRepoReindex rescans the node's repo roots now ({} -> NodeRepoIndex).
	ActionRepoReindex = "repos.reindex"
)

// Repo is one discovered repository's metadata on a node.
type Repo struct {
	Name          string    `json:"name"`                     // directory name
	Path          string    `json:"path"`                     // main checkout (absolute)
	DefaultBranch string    `json:"default_branch,omitempty"` // origin/HEAD, else main/master, else current
	Branch        string    `json:"branch,omitempty"`         // main checkout's current branch
	RemoteURL     string    `json:"remote_url,omitempty"`     // origin (credentials stripped)
	LastActivity  time.Time `json:"last_activity"`            // newest of HEAD/index/refs mtimes
	Worktrees     int       `json:"worktrees,omitempty"`      // linked worktrees
}

// NodeRepoIndex is a node's full repo snapshot (ReportRepos payload).
type NodeRepoIndex struct {
	NodeID    string    `json:"node_id,omitempty"`
	Roots     []string  `json:"roots"`
	Repos     []Repo    `json:"repos"`
	IndexedAt time.Time `json:"indexed_at"`
	Truncated bool      `json:"truncated,omitempty"` // a bound was hit (dirs or repos)
}

type ListReposRequest struct {
	Query  string `json:"query,omitempty"` // fuzzy over name, remote and path
	NodeID string `json:"node_id,omitempty"`
	Limit  int    `json:"limit,omitempty"` // default 50, max 500
}

// RepoEntry is a repo on one node, with fleet context.
type RepoEntry struct {
	Repo
	NodeID     string     `json:"node_id"`
	NodeName   string     `json:"node_name"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"` // latest session run in it (or its worktrees) on this node
	Score      int        `json:"score,omitempty"`        // match quality for Query (higher is better)
}

type RepoList struct {
	Items []RepoEntry `json:"items"`
	// Nodes maps node ID to when its index was last reported.
	Nodes map[string]time.Time `json:"nodes"`
}

type SessionWorktreesRequest struct {
	SessionID string `json:"session_id"`
}

// Worktree is one git worktree of a repository a session touched.
type Worktree struct {
	Path   string `json:"path"`
	Branch string `json:"branch,omitempty"` // empty when detached
	Head   string `json:"head,omitempty"`   // short commit
	Main   bool   `json:"main,omitempty"`   // the repository's main checkout
	// CreatedInSession: created after the session started (agent-made), or
	// created by Pyre for this session's launch.
	CreatedInSession bool `json:"created_in_session,omitempty"`
	PyreCreated      bool `json:"pyre_created,omitempty"`
	// Dirty: uncommitted or untracked changes (checked for session-created
	// worktrees only).
	Dirty bool `json:"dirty,omitempty"`
	// Merged: the branch is contained in the default branch; Pushed: its
	// commits are all on a remote branch.
	Merged bool `json:"merged,omitempty"`
	Pushed bool `json:"pushed,omitempty"`
}

type RepoWorktrees struct {
	Name          string     `json:"name"`
	Path          string     `json:"path"` // main checkout
	DefaultBranch string     `json:"default_branch,omitempty"`
	Worktrees     []Worktree `json:"worktrees"`
}

// SessionWorktrees is a session's worktree snapshot (ReportSessionWorktrees
// payload and sessions.worktrees result).
type SessionWorktrees struct {
	SessionID  string          `json:"session_id"`
	RunID      string          `json:"run_id,omitempty"`
	NodeID     string          `json:"node_id,omitempty"`
	Cwd        string          `json:"cwd,omitempty"`
	Repos      []RepoWorktrees `json:"repos"`
	ReportedAt time.Time       `json:"reported_at"`
}

// WorktreeCreateParams: one repo = a worktree in the repo's worktree
// directory (<repo>/.worktrees/<slug>, or an existing <repo>-worktrees/
// sibling); several = Dir (default ~/pyre-work/<slug>) holding one worktree
// per repo, all on the same branch. The branch is deduplicated per repo
// (pyre/x, pyre/x-2, ...) and starts at each repo's current HEAD.
type WorktreeCreateParams struct {
	Repos  []string `json:"repos"`            // main checkout paths on the node
	Branch string   `json:"branch,omitempty"` // default pyre/<slug>
	Slug   string   `json:"slug,omitempty"`   // folder name hint (from the prompt or title)
	Dir    string   `json:"dir,omitempty"`    // multi-repo folder override
}

type CreatedWorktree struct {
	Repo   string `json:"repo"` // main checkout
	Path   string `json:"path"`
	Branch string `json:"branch"`
}

type WorktreeCreateResult struct {
	Cwd       string            `json:"cwd"` // where the session should start
	Branch    string            `json:"branch"`
	Worktrees []CreatedWorktree `json:"worktrees"`
}

type WorktreeScanParams struct {
	SessionID string `json:"session_id"`
}

// WorktreeRemoveParams names the worktrees to remove; the node removes only
// those it finds created during the session, clean, and not a main checkout.
type WorktreeRemoveParams struct {
	SessionID string   `json:"session_id"`
	Paths     []string `json:"paths"`
}

type WorktreeRemoval struct {
	Path   string `json:"path"`
	Branch string `json:"branch,omitempty"`
	// Outcome: removed | kept_dirty | kept_main | kept_not_session | missing | failed
	Outcome string `json:"outcome"`
	// Branch handling: deleted | kept_unmerged | kept (detached/none) | ""
	BranchOutcome string `json:"branch_outcome,omitempty"`
	Detail        string `json:"detail,omitempty"`
}

type WorktreeRemoveResult struct {
	Results []WorktreeRemoval `json:"results"`
}

// ReposAPI is implemented by hubs, the mock backend, the client and the
// node-local API.
type ReposAPI interface {
	ListRepos(ctx context.Context, req ListReposRequest) (RepoList, error)
	SessionWorktrees(ctx context.Context, req SessionWorktreesRequest) (SessionWorktrees, error)
}

// ---- Global repo catalog and clone-on-demand (additive, W-newsession) ----
//
// The catalog merges every node's repo index with the owner's GitHub
// repositories into one list, deduplicated by normalized remote (ssh and
// https forms of one repository are one entry), ranked: used in Pyre
// recently > contributed to on GitHub recently > present on a machine >
// the rest. GitHub data comes from the gh CLI's own authentication on a
// node (Pyre stores no GitHub token): a node with an authenticated gh
// refreshes hourly and reports a replaceable GitHubRepoIndex; the hub keeps
// the latest per node. A machine that lacks a chosen repository clones it
// first (repo.clone) into its primary repo root, laid out like the
// repositories already there; an existing directory is never overwritten.
const (
	// OpRepoCatalog lists the merged catalog (RepoCatalogRequest -> RepoCatalog).
	OpRepoCatalog = "repos.catalog"
	// ReportGitHubRepos carries a node's GitHubRepoIndex (replaceable per node).
	ReportGitHubRepos ReportKind = "github_repos"
	// ActionRepoClone clones a repository onto the node (RepoCloneParams ->
	// RepoCloneResult); asynchronous on the node, idempotent per destination.
	ActionRepoClone = "repo.clone"
	// ActionGitHubRefresh refetches the owner's GitHub repositories through
	// the node's gh CLI now and reports them ({} -> GitHubRepoIndex).
	ActionGitHubRefresh = "repos.github"
)

// GitHubRepo is one repository GitHub lists for the owner (their own,
// collaborator and organization repositories, plus repositories they
// recently committed or opened pull requests to).
type GitHubRepo struct {
	FullName      string    `json:"full_name"`         // owner/name
	CloneURL      string    `json:"clone_url"`         // https
	SSHURL        string    `json:"ssh_url,omitempty"` // git@github.com:owner/name.git
	Description   string    `json:"description,omitempty"`
	DefaultBranch string    `json:"default_branch,omitempty"`
	Private       bool      `json:"private,omitempty"`
	Fork          bool      `json:"fork,omitempty"`
	Archived      bool      `json:"archived,omitempty"`
	PushedAt      time.Time `json:"pushed_at,omitempty"`
	// ContributedAt is the owner's latest commit, push or pull request seen
	// in GitHub's commit search and the owner's recent events.
	ContributedAt *time.Time `json:"contributed_at,omitempty"`
}

// GitHubRepoIndex is a node's GitHub snapshot (ReportGitHubRepos payload).
type GitHubRepoIndex struct {
	NodeID    string       `json:"node_id,omitempty"`
	Login     string       `json:"login"`
	Repos     []GitHubRepo `json:"repos"`
	FetchedAt time.Time    `json:"fetched_at"`
	Truncated bool         `json:"truncated,omitempty"`
}

type RepoCatalogRequest struct {
	Query string `json:"query,omitempty"` // fuzzy over name, owner/name, remote and path
	Limit int    `json:"limit,omitempty"` // default 200, max 2000
}

// Catalog ranks (CatalogRepo.Rank).
const (
	CatalogRankOther       = 0
	CatalogRankLocal       = 1 // checked out on a machine
	CatalogRankContributed = 2 // the owner contributed on GitHub recently
	CatalogRankUsed        = 3 // a Pyre session ran in it
)

// CatalogRepo is one repository across the fleet and GitHub.
type CatalogRepo struct {
	// Key is the normalized remote ("remote:github.com/owner/name"), or
	// "name:<dir>" for a repository without a remote (RepoKey).
	Key         string `json:"key"`
	Name        string `json:"name"`                // directory/repository name
	FullName    string `json:"full_name,omitempty"` // owner/name when known
	RemoteURL   string `json:"remote_url,omitempty"`
	Description string `json:"description,omitempty"`
	Private     bool   `json:"private,omitempty"`
	Archived    bool   `json:"archived,omitempty"`
	// Present lists the checkouts on machines, best first.
	Present       []RepoEntry `json:"present"`
	GitHub        bool        `json:"github,omitempty"` // listed by GitHub for the owner
	LastUsedAt    *time.Time  `json:"last_used_at,omitempty"`
	ContributedAt *time.Time  `json:"contributed_at,omitempty"`
	PushedAt      *time.Time  `json:"pushed_at,omitempty"`
	LastActivity  time.Time   `json:"last_activity,omitempty"` // newest local git activity
	Rank          int         `json:"rank"`
	Score         int         `json:"score,omitempty"`
}

// GitHubSource says where the catalog's GitHub part came from.
type GitHubSource struct {
	Login     string    `json:"login"`
	NodeID    string    `json:"node_id"`
	FetchedAt time.Time `json:"fetched_at"`
}

type RepoCatalog struct {
	Items []CatalogRepo `json:"items"`
	// Nodes maps node ID to when its repo index was last reported.
	Nodes  map[string]time.Time `json:"nodes"`
	GitHub *GitHubSource        `json:"github,omitempty"` // nil: no node has reported GitHub repositories
}

// RepoCloneParams: Remote is a clone URL (ssh or https) or GitHub
// "owner/name". Dest defaults to the node's primary repo root, laid out
// like the repositories already there (<root>/<owner>/<name> or
// <root>/<name>); an explicit Dest must be inside the node's home.
type RepoCloneParams struct {
	Remote string `json:"remote"`
	Dest   string `json:"dest,omitempty"`
}

type RepoCloneResult struct {
	Path   string `json:"path"`
	Remote string `json:"remote"`
	// Cloned is false when Path already held a checkout of the same
	// repository (nothing was changed).
	Cloned bool `json:"cloned"`
	Repo   Repo `json:"repo"`
}

// RepoCatalogAPI is implemented by hubs, the mock backend, the client and
// the node-local API (older hubs answer 404: unsupported).
type RepoCatalogAPI interface {
	RepoCatalog(ctx context.Context, req RepoCatalogRequest) (RepoCatalog, error)
}
