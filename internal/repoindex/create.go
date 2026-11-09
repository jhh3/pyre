package repoindex

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/jhh3/pyre/internal/contracts"
)

// workspaceTag marks the instruction files Pyre writes into a multi-repo
// folder, so cleanup only ever removes files it wrote and nobody edited.
const workspaceTag = "<!-- pyre-workspace -->"

var workspaceFiles = map[string]bool{"CLAUDE.md": true, "AGENTS.md": true}

var fillerWords = map[string]bool{"a": true, "an": true, "the": true, "please": true, "can": true, "could": true, "you": true, "we": true, "i": true, "let's": true, "lets": true}

// Slug makes a short branch/folder name from a prompt or title:
// "Fix the share link redirect" -> "fix-share-link-redirect".
func Slug(text string) string {
	var words []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			w := cur.String()
			cur.Reset()
			if !fillerWords[w] {
				words = append(words, w)
			}
		}
	}
	for _, r := range strings.ToLower(text) {
		switch {
		case r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			cur.WriteRune(r)
		case r == '\'':
		default:
			flush()
		}
		if len(words) >= 5 {
			break
		}
	}
	flush()
	if len(words) > 5 {
		words = words[:5]
	}
	s := strings.Join(words, "-")
	for len(s) > 40 {
		i := strings.LastIndexByte(s, '-')
		if i <= 0 {
			s = s[:40]
			break
		}
		s = s[:i]
	}
	return s
}

// DefaultWorktreeRoot is where fresh worktrees go on a node without a
// configured root (pyred --worktree-root): ~/worktrees.
func DefaultWorktreeRoot() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "worktrees")
}

// worktreeOnBranch is the worktree of repo that has branch checked out, or "".
func worktreeOnBranch(ctx context.Context, repo, branch string) string {
	out, err := Git(ctx, repo, "worktree", "list", "--porcelain")
	if err != nil {
		return ""
	}
	path := ""
	for _, l := range strings.Split(out, "\n") {
		if p, ok := strings.CutPrefix(l, "worktree "); ok {
			path = p
		} else if l == "branch refs/heads/"+branch {
			return path
		}
	}
	return ""
}

func branchExists(ctx context.Context, repo, branch string) bool {
	_, err := Git(ctx, repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

// ValidBranch reports whether b is a branch name Pyre accepts.
func ValidBranch(b string) bool { return validBranch(b) }

// validBranch rejects names git or a shell could misread.
func validBranch(b string) bool {
	if b == "" || strings.HasPrefix(b, "-") || strings.HasPrefix(b, "/") || strings.HasSuffix(b, "/") || strings.Contains(b, "..") || strings.HasSuffix(b, ".lock") {
		return false
	}
	for _, r := range b {
		if !(r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r)) || r == '-' || r == '_' || r == '.' || r == '/') {
			return false
		}
	}
	return true
}

// CreateOptions are the node's settings for Create.
type CreateOptions struct {
	GH string // gh binary for the fetch's credential fallback (FreshBase); empty: found on PATH
	// Root is the node's worktree root (pyred --worktree-root): a single
	// repository's fresh worktree goes to <Root>/<repo>/<branch or slug>.
	// Empty: DefaultWorktreeRoot.
	Root string
}

// Create makes fresh worktrees for a launch with default options.
func Create(ctx context.Context, p contracts.WorktreeCreateParams, now time.Time) (contracts.WorktreeCreateResult, error) {
	return CreateWith(ctx, p, now, CreateOptions{})
}

// CreateWith makes fresh worktrees for a launch. A single repository's
// worktree goes to <root>/<repo>/<name>: name is the requested branch
// ("/" read as "-"), which is created with exactly that name or the launch
// fails (ErrConflict) when it already exists locally, on origin or as the
// folder; without a branch, name is the slug of the title or prompt and the
// branch pyre/<slug>, deduplicated with -2, -3…. Each new branch starts at
// origin's default branch, fetched first (FreshBase; HEAD, with a warning,
// only without an origin remote), and tracks nothing, so a plain `git
// push` never targets main (`git push -u origin <branch>` sets the
// upstream). It never touches a main checkout's working tree, index or
// local branches (`git worktree add --no-track -b`), and rolls back what
// it created if a later repository fails.
func CreateWith(ctx context.Context, p contracts.WorktreeCreateParams, now time.Time, opts CreateOptions) (contracts.WorktreeCreateResult, error) {
	var res contracts.WorktreeCreateResult
	token := contracts.NewID("claim") // names the session started here (Claim)
	res.ClaimToken = token
	if len(p.Repos) == 0 {
		return res, contracts.Errorf(contracts.ErrInvalid, "worktree.create needs at least one repository")
	}
	if len(p.Repos) > 8 {
		return res, contracts.Errorf(contracts.ErrInvalid, "at most 8 repositories per session")
	}
	repos := make([]string, len(p.Repos))
	for i, r := range p.Repos {
		r = filepath.Clean(ExpandHome(r))
		main, ok := FindRepo(r)
		if !filepath.IsAbs(r) || !ok || !samePath(main, r) {
			return res, contracts.Errorf(contracts.ErrInvalid, "%s is not the main checkout of a git repository on this machine", r)
		}
		repos[i] = main
	}
	exact := p.Branch != "" // the caller named the branch: that name or nothing
	slug := Slug(p.Slug)
	if exact {
		slug = strings.ReplaceAll(strings.TrimPrefix(p.Branch, "pyre/"), "/", "-")
	}
	if slug == "" && len(repos) > 1 {
		var names []string
		for _, r := range repos {
			names = append(names, filepath.Base(r))
		}
		slug = Slug(strings.Join(names, " "))
	}
	if slug == "" {
		slug = "session-" + now.Format("0102-1504")
	}
	base := p.Branch
	if base == "" {
		base = "pyre/" + slug
	}
	if !validBranch(base) {
		return res, contracts.Errorf(contracts.ErrInvalid, "invalid branch name %q", base)
	}
	multi := len(repos) > 1
	root := opts.Root
	if root == "" {
		root = DefaultWorktreeRoot()
	}
	root = filepath.Clean(ExpandHome(root))
	var folder string
	if multi {
		folder = p.Dir
		if folder == "" {
			home, _ := os.UserHomeDir()
			folder = filepath.Join(home, "pyre-work", slug)
		}
		folder = filepath.Clean(ExpandHome(folder))
	}
	// Pick one branch name (and folder) free in every repository.
	pathFor := func(i int, sfx string) string {
		if multi {
			name := filepath.Base(repos[i])
			for j := 0; j < i; j++ {
				if filepath.Base(repos[j]) == name {
					name = fmt.Sprintf("%s-%d", name, i+1)
					break
				}
			}
			return filepath.Join(folder+sfx, name)
		}
		return filepath.Join(root, filepath.Base(repos[i]), strings.ReplaceAll(slug, "/", "-")+sfx)
	}
	branch, sfx := "", ""
	if exact {
		for i, r := range repos {
			if branchExists(ctx, r, base) {
				where := ""
				if wt := worktreeOnBranch(ctx, r, base); wt != "" {
					where = " (checked out in " + wt + ")"
				}
				return res, contracts.Errorf(contracts.ErrConflict, "branch %s already exists in %s%s; name a new branch, or start in the existing worktree with cwd", base, r, where)
			}
			if _, err := os.Lstat(pathFor(i, "")); err == nil {
				return res, contracts.Errorf(contracts.ErrConflict, "%s already exists; name another branch, or start there with cwd", pathFor(i, ""))
			}
		}
		branch = base
	}
	for n := 1; n <= 50 && branch == ""; n++ {
		s := ""
		if n > 1 {
			s = fmt.Sprintf("-%d", n)
		}
		free := true
		for i, r := range repos {
			if branchExists(ctx, r, base+s) {
				free = false
				break
			}
			if _, err := os.Lstat(pathFor(i, s)); err == nil {
				free = false
				break
			}
		}
		if free {
			branch, sfx = base+s, s
		}
	}
	if branch == "" {
		return res, contracts.Errorf(contracts.ErrConflict, "no free branch name near %s", base)
	}
	// Fetch every repository (concurrently, each bounded) before creating
	// anything: a failed fetch fails the launch with nothing to roll back.
	starts := make([]FreshStart, len(repos))
	errs := make([]error, len(repos))
	var wg sync.WaitGroup
	for i, r := range repos {
		wg.Add(1)
		go func() {
			defer wg.Done()
			starts[i], errs[i] = FreshBase(ctx, r, opts.GH)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return res, err
		}
	}
	if exact {
		// After the fetch: a branch of that name on origin would make this
		// one diverge from it (a push is then refused or clobbers it).
		for _, r := range repos {
			if OriginHasBranch(ctx, r, opts.GH, branch) {
				return res, contracts.Errorf(contracts.ErrConflict, "branch %s already exists on origin of %s; name a new branch (a fresh worktree never starts from an existing branch)", branch, r)
			}
		}
	}
	for i, st := range starts {
		if st.Warning != "" {
			w := st.Warning
			if multi {
				w = filepath.Base(repos[i]) + ": " + w
			}
			res.Warnings = append(res.Warnings, w)
		}
	}
	if multi {
		folder += sfx
		if err := os.MkdirAll(folder, 0o755); err != nil {
			return res, err
		}
	}
	res.Branch = branch
	rollback := func() {
		for _, w := range res.Worktrees {
			_, _ = Git(ctx, w.Repo, "worktree", "remove", "--force", w.Path)
			_, _ = Git(ctx, w.Repo, "branch", "-D", w.Branch)
		}
		if multi {
			_ = os.Remove(folder)
		}
	}
	for i, r := range repos {
		path := pathFor(i, sfx)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			rollback()
			return contracts.WorktreeCreateResult{}, err
		}
		if _, err := Git(ctx, r, "worktree", "add", "--no-track", "-b", branch, path, starts[i].Ref); err != nil {
			rollback()
			return contracts.WorktreeCreateResult{}, contracts.Errorf(contracts.ErrInvalid, "%s: %v", filepath.Base(r), err)
		}
		if adm, ok := adminDir(path); ok {
			_ = os.WriteFile(filepath.Join(adm, MarkerFile), []byte(now.UTC().Format(time.RFC3339)+"\n"+markerClaimPrefix+token+"\n"), 0o644)
		}
		head, _ := Git(ctx, path, "rev-parse", "HEAD")
		res.Worktrees = append(res.Worktrees, contracts.CreatedWorktree{Repo: r, Path: path, Branch: branch, Head: strings.TrimSpace(head)})
	}
	res.Cwd = res.Worktrees[0].Path
	if multi {
		res.Cwd = folder
		writeWorkspaceNotes(folder, res)
	}
	return res, nil
}

// writeWorkspaceNotes tells the agent (Claude reads CLAUDE.md, Codex reads
// AGENTS.md from its working directory) which repositories the folder holds.
func writeWorkspaceNotes(folder string, res contracts.WorktreeCreateResult) {
	var b strings.Builder
	b.WriteString(workspaceTag + "\n# Pyre multi-repo workspace\n\n")
	b.WriteString("This folder holds one git worktree per repository, all on branch `" + res.Branch + "`:\n\n")
	for _, w := range res.Worktrees {
		fmt.Fprintf(&b, "- `%s/` — worktree of %s\n", filepath.Base(w.Path), w.Repo)
	}
	b.WriteString("\nCommit in each repository separately. Each repository's own CLAUDE.md/AGENTS.md still applies inside it.\n")
	for name := range workspaceFiles {
		_ = os.WriteFile(filepath.Join(folder, name), []byte(b.String()), 0o644)
	}
}
