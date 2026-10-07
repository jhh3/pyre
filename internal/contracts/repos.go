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
