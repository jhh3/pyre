package repoindex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Automatic worktree cleanup (W-lifecycle). Unlike Remove (an owner's
// explicit choice, which removes the folder of an unmerged branch and keeps
// the branch), automatic cleanup removes a worktree only when Pyre created
// it for a session, nothing can be lost and nobody may be using it:
//
//   - its pyre-created marker names the session (ClaimMarkers records the
//     session when its run starts): never the owner's own worktrees, never
//     one matched only by its creation time;
//   - never the main checkout, a bare or locked worktree, or one with the
//     default branch (or main/master) checked out;
//   - no uncommitted or untracked changes, and no ignored files other than
//     rebuildable ones (node_modules, .venv, target, ...): a gitignored
//     .env would go with the folder;
//   - its branch is merged into the default branch or contained in its own
//     upstream (a detached HEAD must be on the default branch or a remote
//     branch);
//   - InUse finds no live session or process working in it (asked right
//     before the removal).
//
// The branch of a removed worktree is deleted when merged, or when the
// remote still has it (git ls-remote); otherwise it is kept.

// InUse says why a worktree may be in use ("" = it is not). created is when
// the worktree was added (zero when unknown) and repo its main checkout.
type InUse func(w contracts.Worktree, repo string, created time.Time) string

// Kept outcomes of automatic cleanup (WorktreeRemoval.Outcome).
const (
	OutcomeKeptUnmerged = "kept_unmerged"
	OutcomeKeptInUse    = "kept_in_use"
	OutcomeKeptRecent   = "kept_recent"
)

// rebuildable are ignored paths a worktree may hold and still be removed
// (dependency and build output folders, recreated by a build).
var rebuildable = map[string]bool{
	"node_modules": true, ".venv": true, "venv": true, "target": true, "__pycache__": true, "dist": true,
	"build": true, ".next": true, ".turbo": true, ".pytest_cache": true, ".mypy_cache": true, ".gradle": true,
}

// CleanSession removes the worktrees whose marker names in.SessionID under
// the automatic rules. Other worktrees are not reported.
func CleanSession(ctx context.Context, in ScanInput, inUse InUse) []contracts.WorktreeRemoval {
	scan, err := Scan(ctx, in)
	if err != nil {
		return []contracts.WorktreeRemoval{{Path: in.Cwd, Outcome: "failed", Detail: err.Error()}}
	}
	var out []contracts.WorktreeRemoval
	for _, r := range scan.Repos {
		for _, w := range r.Worktrees {
			if w.Main || in.SessionID == "" || MarkerSession(w.Path) != in.SessionID {
				continue
			}
			w.Dirty = Dirty(ctx, w.Path)
			w.Merged = Merged(ctx, r.Path, w.Branch, r.DefaultBranch)
			w.Pushed = Pushed(ctx, r.Path, w.Branch)
			created, _, _ := createdAt(w.Path)
			res := autoRemove(ctx, r, w, created, inUse)
			if res.Outcome == "removed" && in.Cwd != "" && canon(filepath.Dir(w.Path)) == canon(in.Cwd) {
				_ = removeIfOnlyPyreFiles(in.Cwd)
			}
			out = append(out, res)
		}
	}
	return out
}

// SweepRepo removes the orphan worktrees of one repository: worktrees Pyre
// created for a session that keep(session) does not hold on to, whose git
// metadata did not change for idle, under the automatic rules. Others are
// skipped silently. It also prunes the admin data of worktrees whose
// folder is gone.
func SweepRepo(ctx context.Context, repo string, idle time.Duration, now time.Time, keep func(sessionID string) bool, inUse InUse, dryRun bool) []contracts.WorktreeRemoval {
	rw := contracts.RepoWorktrees{Name: filepath.Base(repo), Path: repo}
	rw.DefaultBranch = defaultBranch(filepath.Join(repo, ".git"), "")
	if !dryRun {
		_, _ = Git(ctx, repo, "worktree", "prune")
	}
	ents, err := listWorktreesFull(ctx, repo)
	if err != nil {
		return []contracts.WorktreeRemoval{{Path: repo, Outcome: "failed", Detail: err.Error()}}
	}
	var out []contracts.WorktreeRemoval
	for _, e := range ents {
		if e.bare || e.locked || samePath(e.path, repo) {
			continue
		}
		sid := MarkerSession(e.path)
		if sid == "" || (keep != nil && keep(sid)) {
			continue // not Pyre's for a session, or its session still runs
		}
		if t, ok := lastTouched(e.path); !ok || now.Sub(t) < idle {
			continue // unknown or recently used: not an orphan (yet)
		}
		w := contracts.Worktree{Path: e.path, Branch: e.branch, Head: e.head, PyreCreated: true}
		w.Dirty = Dirty(ctx, e.path)
		w.Merged = Merged(ctx, repo, e.branch, rw.DefaultBranch)
		w.Pushed = Pushed(ctx, repo, e.branch)
		created, _, _ := createdAt(e.path)
		if dryRun {
			res := contracts.WorktreeRemoval{Path: w.Path, Branch: w.Branch}
			res.Outcome, res.Detail = autoVerdict(ctx, rw, w, created, inUse)
			if res.Outcome == "" {
				res.Outcome = "would_remove"
			}
			out = append(out, res)
			continue
		}
		out = append(out, autoRemove(ctx, rw, w, created, inUse))
	}
	return out
}

// autoVerdict returns the kept outcome and detail, or "" when the worktree
// may be removed.
func autoVerdict(ctx context.Context, repo contracts.RepoWorktrees, w contracts.Worktree, created time.Time, inUse InUse) (string, string) {
	if adm, ok := adminDir(w.Path); ok {
		if _, err := os.Stat(filepath.Join(adm, "locked")); err == nil {
			return OutcomeKeptInUse, "locked (git worktree lock)"
		}
	}
	if w.Branch != "" && (w.Branch == repo.DefaultBranch || w.Branch == "main" || w.Branch == "master") {
		return OutcomeKeptInUse, "has the default branch " + w.Branch + " checked out"
	}
	if w.Dirty {
		return "kept_dirty", "has uncommitted or untracked changes"
	}
	if f := keptIgnored(ctx, w.Path); f != "" {
		return "kept_dirty", "has ignored files that would be lost (" + f + ")"
	}
	switch {
	case w.Branch == "" && !headSafe(ctx, repo.Path, w.Path, repo.DefaultBranch):
		return OutcomeKeptUnmerged, "detached HEAD " + w.Head + " is on no remote or default branch"
	case w.Branch != "" && !w.Merged && !w.Pushed:
		return OutcomeKeptUnmerged, "branch " + w.Branch + " is neither merged nor pushed"
	}
	if inUse != nil {
		if why := inUse(w, repo.Path, created); why != "" {
			return OutcomeKeptInUse, why
		}
	}
	return "", ""
}

// keptIgnored names an ignored path in wt that is not a rebuildable
// folder ("" when every ignored path is one).
func keptIgnored(ctx context.Context, wt string) string {
	out, err := Git(ctx, wt, "status", "--porcelain", "--ignored", "--untracked-files=normal")
	if err != nil {
		return "status failed"
	}
	for _, line := range strings.Split(out, "\n") {
		p, ok := strings.CutPrefix(line, "!! ")
		if !ok {
			continue
		}
		p = strings.Trim(p, `"`)
		// Only a whole ignored folder git collapsed ("node_modules/") whose
		// own name is rebuildable passes; a single ignored file inside a
		// tracked build/ or dist/ folder (deploy/build/prod.env) does not.
		dir, isDir := strings.CutSuffix(p, "/")
		if !isDir || !rebuildable[filepath.Base(dir)] {
			return p
		}
	}
	return ""
}

func autoRemove(ctx context.Context, repo contracts.RepoWorktrees, w contracts.Worktree, created time.Time, inUse InUse) contracts.WorktreeRemoval {
	out := contracts.WorktreeRemoval{Path: w.Path, Branch: w.Branch}
	if o, d := autoVerdict(ctx, repo, w, created, inUse); o != "" {
		out.Outcome, out.Detail = o, d
		return out
	}
	if _, err := Git(ctx, repo.Path, "worktree", "remove", w.Path); err != nil {
		out.Outcome, out.Detail = "failed", err.Error()
		return out
	}
	out.Outcome = "removed"
	switch {
	case w.Branch == "":
		out.BranchOutcome = "kept"
		return out
	case !w.Merged && !remoteHas(ctx, repo.Path, w.Branch):
		out.BranchOutcome, out.Detail = "kept", "the remote did not confirm branch "+w.Branch+"; kept"
		return out
	}
	if _, err := Git(ctx, repo.Path, "branch", "-D", w.Branch); err != nil {
		out.BranchOutcome, out.Detail = "kept", err.Error()
	} else {
		out.BranchOutcome = "deleted"
	}
	return out
}

// remoteHas asks the branch's remote (git ls-remote) whether its branch
// still contains the local tip, before the local branch is deleted.
func remoteHas(ctx context.Context, repo, branch string) bool {
	rest, ok := strings.CutPrefix(Upstream(ctx, repo, branch), "refs/remotes/")
	if !ok {
		return false
	}
	remote, rbranch, ok := strings.Cut(rest, "/")
	if !ok {
		return false
	}
	out, err := Git(ctx, repo, "ls-remote", remote, "refs/heads/"+rbranch)
	if err != nil {
		return false
	}
	sha, _, _ := strings.Cut(strings.TrimSpace(out), "\t")
	if sha == "" {
		return false
	}
	_, err = Git(ctx, repo, "merge-base", "--is-ancestor", "refs/heads/"+branch, sha)
	return err == nil
}

// headSafe: a detached worktree's commit is on the default branch or on a
// remote-tracking branch.
func headSafe(ctx context.Context, repo, wt, def string) bool {
	head, err := Git(ctx, wt, "rev-parse", "HEAD")
	if err != nil || head == "" {
		return false
	}
	if def != "" {
		for _, base := range []string{"refs/heads/" + def, "refs/remotes/origin/" + def} {
			if _, err := Git(ctx, repo, "merge-base", "--is-ancestor", head, base); err == nil {
				return true
			}
		}
	}
	out, err := Git(ctx, repo, "for-each-ref", "--contains", head, "--format=%(refname)", "refs/remotes")
	return err == nil && strings.TrimSpace(out) != ""
}

// lastTouched is the newest change to a linked worktree's git metadata
// (HEAD moves, index writes, commits): its admin directory's HEAD, index
// and logs/HEAD. Reading never updates these (GIT_OPTIONAL_LOCKS=0).
func lastTouched(wt string) (time.Time, bool) {
	adm, ok := adminDir(wt)
	if !ok {
		return time.Time{}, false
	}
	var last time.Time
	found := false
	for _, f := range []string{"HEAD", "index", filepath.Join("logs", "HEAD"), "ORIG_HEAD"} {
		if st, err := os.Stat(filepath.Join(adm, f)); err == nil {
			found = true
			if st.ModTime().After(last) {
				last = st.ModTime()
			}
		}
	}
	return last, found
}

type wtEntryFull struct {
	wtEntry
	locked bool
}

// listWorktreesFull is listWorktrees plus the locked flag.
func listWorktreesFull(ctx context.Context, repo string) ([]wtEntryFull, error) {
	out, err := Git(ctx, repo, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var res []wtEntryFull
	for _, line := range strings.Split(out, "\n") {
		k, v, _ := strings.Cut(line, " ")
		if k == "worktree" {
			res = append(res, wtEntryFull{wtEntry: wtEntry{path: filepath.Clean(v)}})
			continue
		}
		if len(res) == 0 {
			continue
		}
		cur := &res[len(res)-1]
		switch k {
		case "HEAD":
			if len(v) >= 9 {
				cur.head = v[:9]
			}
		case "branch":
			cur.branch = strings.TrimPrefix(v, "refs/heads/")
		case "detached":
			cur.detached = true
		case "bare":
			cur.bare = true
		case "locked":
			cur.locked = true
		}
	}
	return res, nil
}

// Inside reports whether p is base or below it (symlinks resolved).
func Inside(p, base string) bool { return insideOrEqual(p, base) }
