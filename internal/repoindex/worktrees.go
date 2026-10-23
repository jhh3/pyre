package repoindex

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// MarkerFile is written into a worktree's git admin directory
// (.git/worktrees/<name>/) when Pyre creates it for a launch. Git ignores
// unknown files there and removes the directory with the worktree.
const MarkerFile = "pyre-created"

// Git runs git with a bounded time and a clean, non-interactive
// environment. It is a variable so tests can count calls.
var Git = func(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return strings.TrimSpace(out.String()), fmt.Errorf("git %s: %s", args[0], msg)
	}
	return strings.TrimRight(out.String(), "\n"), nil
}

// FindRepo returns the main checkout of the repository containing dir
// (walking up), following a linked worktree's .git file to its common
// directory. ok is false outside a repository.
func FindRepo(dir string) (main string, ok bool) {
	d := filepath.Clean(dir)
	for {
		st, err := os.Lstat(filepath.Join(d, ".git"))
		if err == nil {
			if st.IsDir() && hasHead(d) {
				return d, true
			}
			if adm, ok := adminDir(d); ok {
				if m, ok := mainFromAdmin(adm); ok {
					return m, true
				}
			}
			if !st.IsDir() {
				return "", false // submodule or broken link
			}
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", false
		}
		d = parent
	}
}

// hasHead reports whether d/.git is a repository: like git, a .git
// directory without HEAD is not one (seen live: a stray empty /tmp/.git
// made everything under /tmp look like one repository).
func hasHead(d string) bool {
	_, err := os.Stat(filepath.Join(d, ".git", "HEAD"))
	return err == nil
}

// adminDir reads a linked worktree's .git file ("gitdir: <admin>").
func adminDir(wt string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(wt, ".git"))
	if err != nil {
		return "", false
	}
	p, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir: ")
	if !ok {
		return "", false
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(wt, p)
	}
	return filepath.Clean(p), true
}

// mainFromAdmin: <main>/.git/worktrees/<name> -> <main>.
func mainFromAdmin(adm string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(adm, "commondir"))
	if err != nil {
		return "", false
	}
	common := strings.TrimSpace(string(b))
	if !filepath.IsAbs(common) {
		common = filepath.Join(adm, common)
	}
	common = filepath.Clean(common)
	if filepath.Base(common) != ".git" {
		return "", false // bare repository: no main checkout
	}
	return filepath.Dir(common), true
}

type wtEntry struct {
	path, head, branch string
	detached, bare     bool
}

// listWorktrees parses `git worktree list --porcelain`.
func listWorktrees(ctx context.Context, repo string) ([]wtEntry, error) {
	out, err := Git(ctx, repo, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var res []wtEntry
	var cur *wtEntry
	for _, line := range strings.Split(out, "\n") {
		k, v, _ := strings.Cut(line, " ")
		switch k {
		case "worktree":
			res = append(res, wtEntry{path: filepath.Clean(v)})
			cur = &res[len(res)-1]
		case "HEAD":
			if cur != nil && len(v) >= 9 {
				cur.head = v[:9]
			}
		case "branch":
			if cur != nil {
				cur.branch = strings.TrimPrefix(v, "refs/heads/")
			}
		case "detached":
			if cur != nil {
				cur.detached = true
			}
		case "bare":
			if cur != nil {
				cur.bare = true
			}
		}
	}
	return res, nil
}

// createdAt is when a linked worktree was added (its admin directory's
// commondir file is written once by `git worktree add`), and whether Pyre
// created it.
func createdAt(wt string) (time.Time, bool, bool) {
	adm, ok := adminDir(wt)
	if !ok {
		return time.Time{}, false, false
	}
	st, err := os.Stat(filepath.Join(adm, "commondir"))
	if err != nil {
		return time.Time{}, false, false
	}
	_, merr := os.Stat(filepath.Join(adm, MarkerFile))
	return st.ModTime(), merr == nil, true
}

// Dirty reports uncommitted or untracked changes in a worktree. An error
// counts as dirty (never remove what cannot be checked).
func Dirty(ctx context.Context, wt string) bool {
	out, err := Git(ctx, wt, "status", "--porcelain", "--untracked-files=normal")
	return err != nil || strings.TrimSpace(out) != ""
}

// Merged reports that branch is contained in the default branch (local or
// origin's).
func Merged(ctx context.Context, repo, branch, def string) bool {
	if branch == "" || def == "" || branch == def {
		return false
	}
	for _, base := range []string{"refs/heads/" + def, "refs/remotes/origin/" + def} {
		if _, err := Git(ctx, repo, "merge-base", "--is-ancestor", "refs/heads/"+branch, base); err == nil {
			return true
		}
	}
	return false
}

// Pushed reports that the branch tip is contained in the branch's own
// remote branch: its configured upstream, else origin/<branch>. A tip that
// merely appears on some other remote-tracking branch does not count.
func Pushed(ctx context.Context, repo, branch string) bool {
	up := Upstream(ctx, repo, branch)
	if up == "" {
		return false
	}
	_, err := Git(ctx, repo, "merge-base", "--is-ancestor", "refs/heads/"+branch, up)
	return err == nil
}

// Upstream returns the remote-tracking ref of branch: its configured
// upstream, else refs/remotes/origin/<branch> when it exists ("" if none).
func Upstream(ctx context.Context, repo, branch string) string {
	if branch == "" {
		return ""
	}
	if up, err := Git(ctx, repo, "rev-parse", "--symbolic-full-name", branch+"@{upstream}"); err == nil && strings.HasPrefix(up, "refs/remotes/") {
		return up
	}
	ref := "refs/remotes/origin/" + branch
	if _, err := Git(ctx, repo, "rev-parse", "--verify", "--quiet", ref); err == nil {
		return ref
	}
	return ""
}

// ---- session markers (W-lifecycle) ----

// Marker lines after the creation time: claim=<token> (written by Create)
// and session=<id> (written once by Claim, after the session started).
const (
	markerSessionPrefix = "session="
	markerClaimPrefix   = "claim="
)

// MarkerSession returns the session recorded in a worktree's pyre-created
// marker ("" when there is no marker or it names no session yet).
func MarkerSession(wt string) string {
	adm, ok := adminDir(wt)
	if !ok {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(adm, MarkerFile))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if id, ok := strings.CutPrefix(strings.TrimSpace(line), markerSessionPrefix); ok {
			return id
		}
	}
	return ""
}

// Claim records sessionID in the markers of the worktrees at paths that
// Create made with token (its ClaimToken), once: a marker that names a
// session already, carries another token or none, is left alone. A marker
// that never names a session is treated as the owner's and is never removed
// automatically. It returns the worktrees claimed.
func Claim(paths []string, sessionID, token string) []string {
	if sessionID == "" || token == "" {
		return nil
	}
	var out []string
	for _, wt := range paths {
		adm, ok := adminDir(filepath.Clean(wt))
		if !ok {
			continue
		}
		mp := filepath.Join(adm, MarkerFile)
		b, err := os.ReadFile(mp)
		if err != nil || strings.Contains(string(b), markerSessionPrefix) || !hasLine(string(b), markerClaimPrefix+token) {
			continue
		}
		f, err := os.OpenFile(mp, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			continue
		}
		_, werr := f.WriteString(markerSessionPrefix + sessionID + "\n")
		if f.Close() == nil && werr == nil {
			out = append(out, wt)
		}
	}
	return out
}

func hasLine(s, want string) bool {
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) == want {
			return true
		}
	}
	return false
}

// ScanInput describes a session for Scan.
type ScanInput struct {
	SessionID string
	RunID     string
	Cwd       string
	// Since is when the session started (node clock); worktrees added
	// after it count as created during the session.
	Since time.Time
	// Check runs the (slower) dirty/merged/pushed checks for worktrees
	// created during the session.
	Check bool
}

// Scan finds the repositories a session touches (its cwd's repository, or
// the repositories of worktrees directly inside a multi-repo folder) and
// lists their worktrees.
func Scan(ctx context.Context, in ScanInput) (contracts.SessionWorktrees, error) {
	res := contracts.SessionWorktrees{SessionID: in.SessionID, RunID: in.RunID, Cwd: in.Cwd, Repos: []contracts.RepoWorktrees{}}
	if in.Cwd == "" {
		return res, nil
	}
	var repos []string
	if r, ok := FindRepo(in.Cwd); ok {
		repos = append(repos, r)
	} else if ents, err := os.ReadDir(in.Cwd); err == nil {
		for _, e := range ents {
			if !e.IsDir() || len(repos) >= 16 {
				continue
			}
			if r, ok := FindRepo(filepath.Join(in.Cwd, e.Name())); ok && !contains(repos, r) && strings.HasPrefix(filepath.Join(in.Cwd, e.Name()), in.Cwd) {
				if _, isLinked := adminDir(filepath.Join(in.Cwd, e.Name())); isLinked {
					repos = append(repos, r)
				}
			}
		}
	}
	for _, repo := range repos {
		rw, err := scanRepo(ctx, repo, in)
		if err != nil {
			return res, err
		}
		res.Repos = append(res.Repos, rw)
	}
	return res, nil
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func scanRepo(ctx context.Context, repo string, in ScanInput) (contracts.RepoWorktrees, error) {
	rw := contracts.RepoWorktrees{Name: filepath.Base(repo), Path: repo, Worktrees: []contracts.Worktree{}}
	rw.DefaultBranch = defaultBranch(filepath.Join(repo, ".git"), "")
	ents, err := listWorktrees(ctx, repo)
	if err != nil {
		return rw, err
	}
	for _, e := range ents {
		if e.bare {
			continue
		}
		w := contracts.Worktree{Path: e.path, Branch: e.branch, Head: e.head, Main: samePath(e.path, repo)}
		if !w.Main {
			at, pyre, ok := createdAt(e.path)
			if ok {
				w.PyreCreated = pyre && insideOrEqual(e.path, in.Cwd)
				w.CreatedInSession = w.PyreCreated || (!in.Since.IsZero() && !at.Before(in.Since))
			}
		}
		if w.CreatedInSession && in.Check {
			w.Dirty = Dirty(ctx, e.path)
			w.Merged = Merged(ctx, repo, e.branch, rw.DefaultBranch)
			w.Pushed = Pushed(ctx, repo, e.branch)
		}
		rw.Worktrees = append(rw.Worktrees, w)
	}
	return rw, nil
}

func samePath(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, e1 := filepath.EvalSymlinks(a)
	rb, e2 := filepath.EvalSymlinks(b)
	return e1 == nil && e2 == nil && ra == rb
}

// canon resolves symlinks (macOS /var -> /private/var) so paths from git
// and from callers compare equal.
func canon(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// insideOrEqual: p is base or directly/indirectly below it.
func insideOrEqual(p, base string) bool {
	if base == "" {
		return false
	}
	p, base = canon(p), canon(base)
	return p == base || strings.HasPrefix(p, base+string(filepath.Separator))
}

// Remove removes the named worktrees of a session after re-checking every
// rule: the path must be a linked worktree (never a main checkout) that
// the fresh scan finds created during the session, with no uncommitted or
// untracked changes. The branch is deleted only when merged into the
// default branch or pushed; otherwise it is kept and the result says so.
// A multi-repo folder left empty is removed (rmdir only).
func Remove(ctx context.Context, in ScanInput, paths []string) contracts.WorktreeRemoveResult {
	in.Check = true
	res := contracts.WorktreeRemoveResult{Results: []contracts.WorktreeRemoval{}}
	scan, err := Scan(ctx, in)
	byPath := map[string]struct {
		w    contracts.Worktree
		repo contracts.RepoWorktrees
	}{}
	if err == nil {
		for _, r := range scan.Repos {
			for _, w := range r.Worktrees {
				byPath[canon(w.Path)] = struct {
					w    contracts.Worktree
					repo contracts.RepoWorktrees
				}{w, r}
			}
		}
	}
	for _, p := range paths {
		p = filepath.Clean(p)
		out := contracts.WorktreeRemoval{Path: p}
		e, ok := byPath[canon(p)]
		switch {
		case err != nil:
			out.Outcome, out.Detail = "failed", err.Error()
		case !ok:
			if _, serr := os.Stat(p); errors.Is(serr, os.ErrNotExist) {
				out.Outcome = "missing"
			} else {
				out.Outcome, out.Detail = "kept_not_session", "not a worktree of a repository this session touched"
			}
		case e.w.Main:
			out.Outcome, out.Detail = "kept_main", "the main checkout is never removed"
		case !e.w.CreatedInSession:
			out.Outcome, out.Detail = "kept_not_session", "existed before the session started"
		case e.w.Dirty:
			out.Branch, out.Outcome, out.Detail = e.w.Branch, "kept_dirty", "has uncommitted or untracked changes"
		default:
			out.Branch = e.w.Branch
			if _, gerr := Git(ctx, e.repo.Path, "worktree", "remove", p); gerr != nil {
				out.Outcome, out.Detail = "failed", gerr.Error()
				break
			}
			out.Outcome = "removed"
			switch {
			case e.w.Branch == "":
				out.BranchOutcome = "kept"
			case e.w.Merged || e.w.Pushed:
				if _, gerr := Git(ctx, e.repo.Path, "branch", "-D", e.w.Branch); gerr != nil {
					out.BranchOutcome, out.Detail = "kept", gerr.Error()
				} else {
					out.BranchOutcome = "deleted"
				}
			default:
				out.BranchOutcome = "kept_unmerged"
				out.Detail = "branch " + e.w.Branch + " is neither merged nor pushed; kept"
			}
			if in.Cwd != "" && canon(filepath.Dir(p)) == canon(in.Cwd) {
				_ = removeIfOnlyPyreFiles(in.Cwd)
			}
		}
		res.Results = append(res.Results, out)
	}
	return res
}

// removeIfOnlyPyreFiles removes a multi-repo folder that holds nothing but
// the instruction files Pyre wrote into it.
func removeIfOnlyPyreFiles(dir string) error {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if !workspaceFiles[e.Name()] {
			return nil
		}
	}
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil || !bytes.Contains(b, []byte(workspaceTag)) {
			return nil // edited or not ours: keep the folder
		}
	}
	for _, e := range ents {
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
	return os.Remove(dir)
}
