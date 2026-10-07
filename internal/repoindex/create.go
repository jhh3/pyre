package repoindex

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// WorktreeDir is where a repository's linked worktrees go: an existing
// <repo>/.worktrees, else an existing <repo>-worktrees sibling, else
// <repo>/.worktrees (the convention on the owner's machines).
func WorktreeDir(repo string) string {
	in := filepath.Join(repo, ".worktrees")
	if st, err := os.Stat(in); err == nil && st.IsDir() {
		return in
	}
	sib := filepath.Join(filepath.Dir(repo), filepath.Base(repo)+"-worktrees")
	if st, err := os.Stat(sib); err == nil && st.IsDir() {
		return sib
	}
	return in
}

// ensureIgnored keeps a new in-repo .worktrees folder out of `git status`
// through the repository's local exclude file (never a committed file).
func ensureIgnored(ctx context.Context, repo, dir string) {
	if filepath.Dir(dir) != filepath.Clean(repo) {
		return
	}
	if _, err := Git(ctx, repo, "check-ignore", "-q", filepath.Base(dir)+"/"); err == nil {
		return
	}
	p := filepath.Join(repo, ".git", "info", "exclude")
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString("\n# pyre: linked worktrees\n/" + filepath.Base(dir) + "/\n")
}

func branchExists(ctx context.Context, repo, branch string) bool {
	_, err := Git(ctx, repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

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

// Create makes fresh worktrees for a launch. It never touches a main
// checkout's working tree or index (`git worktree add -b` from HEAD), and
// rolls back what it created if a later repository fails.
func Create(ctx context.Context, p contracts.WorktreeCreateParams, now time.Time) (contracts.WorktreeCreateResult, error) {
	var res contracts.WorktreeCreateResult
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
	slug := Slug(p.Slug)
	if slug == "" {
		slug = Slug(strings.TrimPrefix(p.Branch, "pyre/"))
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
		return filepath.Join(WorktreeDir(repos[i]), strings.ReplaceAll(slug, "/", "-")+sfx)
	}
	branch, sfx := "", ""
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
		if !multi {
			ensureIgnored(ctx, r, filepath.Dir(path))
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			rollback()
			return contracts.WorktreeCreateResult{}, err
		}
		if _, err := Git(ctx, r, "worktree", "add", "-b", branch, path, "HEAD"); err != nil {
			rollback()
			return contracts.WorktreeCreateResult{}, contracts.Errorf(contracts.ErrInvalid, "%s: %v", filepath.Base(r), err)
		}
		if adm, ok := adminDir(path); ok {
			_ = os.WriteFile(filepath.Join(adm, MarkerFile), []byte(now.UTC().Format(time.RFC3339)+"\n"), 0o644)
		}
		res.Worktrees = append(res.Worktrees, contracts.CreatedWorktree{Repo: r, Path: path, Branch: branch})
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
