// Package repoindex discovers git repositories on a node, reads their
// metadata (never file contents), tracks the worktrees a session touches,
// and creates and removes worktrees for quick launch under strict safety
// rules. Discovery reads .git directly (no git subprocess per repository)
// and is bounded in depth, directories visited and repositories recorded.
package repoindex

import (
	"bufio"
	"context"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Options bound a discovery pass.
type Options struct {
	// Roots are scanned (absolute or ~-relative). Default: DefaultRoots.
	Roots []string
	// MaxDepth is the deepest directory level examined below a root
	// (default 4: ~/code/a/b/c/repo).
	MaxDepth int
	// MaxDirs bounds directories read per pass (default 20000).
	MaxDirs int
	// MaxRepos bounds repositories recorded (default 2000).
	MaxRepos int
	// Exclude lists absolute directories never entered (Pyre state, the
	// multi-repo work folder).
	Exclude []string
	// PauseEvery/Pause yield between batches of directory reads so a pass
	// stays a low-priority background task (default 200 dirs / 2ms).
	PauseEvery int
	Pause      time.Duration
	// Now stamps the index (default time.Now).
	Now func() time.Time
}

// DefaultRoots are the conventional code directories under home.
func DefaultRoots(home string) []string {
	return []string{filepath.Join(home, "code"), filepath.Join(home, "src"), filepath.Join(home, "Documents", "code"), filepath.Join(home, "projects")}
}

// skipNames are directory names never entered: dependency trees, VCS
// internals and worktree folders (those hold linked worktrees of a repo
// already indexed, Claude Code's own .claude/worktrees included).
var skipNames = map[string]bool{
	"node_modules": true, ".git": true, ".worktrees": true, ".claude": true,
}

func (o *Options) defaults() {
	home, _ := os.UserHomeDir()
	if len(o.Roots) == 0 {
		o.Roots = DefaultRoots(home)
	}
	if o.MaxDepth <= 0 {
		o.MaxDepth = 4
	}
	if o.MaxDirs <= 0 {
		o.MaxDirs = 20000
	}
	if o.MaxRepos <= 0 {
		o.MaxRepos = 2000
	}
	if o.PauseEvery <= 0 {
		o.PauseEvery = 200
	}
	if o.Pause == 0 {
		o.Pause = 2 * time.Millisecond
	}
	if o.Now == nil {
		o.Now = time.Now
	}
}

// ExpandHome turns ~ and ~/x into absolute paths.
func ExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// Discover scans the roots and returns the node's repo index. Only roots
// that exist are listed. Symlinked directories below a root are not
// followed (no loops, no escaping the roots).
func Discover(ctx context.Context, o Options) (contracts.NodeRepoIndex, error) {
	o.defaults()
	idx := contracts.NodeRepoIndex{Roots: []string{}, Repos: []contracts.Repo{}}
	exclude := map[string]bool{}
	for _, e := range o.Exclude {
		exclude[filepath.Clean(ExpandHome(e))] = true
	}
	seen := map[string]bool{}
	dirs := 0
	type item struct {
		path  string
		depth int
	}
	for _, r := range o.Roots {
		root := filepath.Clean(ExpandHome(r))
		if !filepath.IsAbs(root) {
			continue
		}
		real, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		if st, err := os.Stat(real); err != nil || !st.IsDir() {
			continue
		}
		if seen[real] {
			continue // the same directory reached through another root
		}
		seen[real] = true
		idx.Roots = append(idx.Roots, root)
		stack := []item{{root, 0}}
		for len(stack) > 0 {
			if err := ctx.Err(); err != nil {
				return idx, err
			}
			it := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if dirs >= o.MaxDirs || len(idx.Repos) >= o.MaxRepos {
				idx.Truncated = true
				break
			}
			dirs++
			if dirs%o.PauseEvery == 0 && o.Pause > 0 {
				select {
				case <-ctx.Done():
					return idx, ctx.Err()
				case <-time.After(o.Pause):
				}
			}
			ents, err := os.ReadDir(it.path)
			if err != nil {
				continue
			}
			isRepo, linked := false, false
			for _, e := range ents {
				if e.Name() == ".git" {
					if e.IsDir() {
						isRepo = true
					} else {
						linked = true // linked worktree or submodule
					}
				}
			}
			if linked {
				continue
			}
			if isRepo {
				if rp, ok := readRepo(it.path); ok {
					idx.Repos = append(idx.Repos, rp)
				}
				continue // nested repositories are not indexed
			}
			if it.depth >= o.MaxDepth {
				continue
			}
			for i := len(ents) - 1; i >= 0; i-- {
				e := ents[i]
				name := e.Name()
				if !e.IsDir() || e.Type()&os.ModeSymlink != 0 || skipNames[name] || strings.HasPrefix(name, ".") || strings.HasSuffix(name, "-worktrees") {
					continue
				}
				p := filepath.Join(it.path, name)
				if exclude[p] {
					continue
				}
				stack = append(stack, item{p, it.depth + 1})
			}
		}
	}
	sort.Slice(idx.Repos, func(i, j int) bool {
		if !idx.Repos[i].LastActivity.Equal(idx.Repos[j].LastActivity) {
			return idx.Repos[i].LastActivity.After(idx.Repos[j].LastActivity)
		}
		return idx.Repos[i].Path < idx.Repos[j].Path
	})
	idx.IndexedAt = o.Now().UTC()
	return idx, nil
}

// readRepo reads a main checkout's metadata from its .git directory.
func readRepo(path string) (contracts.Repo, bool) {
	gd := filepath.Join(path, ".git")
	head, err := os.ReadFile(filepath.Join(gd, "HEAD"))
	if err != nil {
		return contracts.Repo{}, false
	}
	r := contracts.Repo{Name: filepath.Base(path), Path: path}
	if ref, ok := strings.CutPrefix(strings.TrimSpace(string(head)), "ref: refs/heads/"); ok {
		r.Branch = ref
	}
	r.RemoteURL = remoteURL(filepath.Join(gd, "config"))
	r.DefaultBranch = defaultBranch(gd, r.Branch)
	for _, f := range []string{"HEAD", "index", filepath.Join("logs", "HEAD")} {
		if st, err := os.Stat(filepath.Join(gd, f)); err == nil && st.ModTime().After(r.LastActivity) {
			r.LastActivity = st.ModTime().UTC()
		}
	}
	if ents, err := os.ReadDir(filepath.Join(gd, "worktrees")); err == nil {
		for _, e := range ents {
			if e.IsDir() {
				r.Worktrees++
			}
		}
	}
	return r, true
}

// remoteURL returns origin's URL (or the first remote's) with any
// credentials removed.
func remoteURL(configPath string) string {
	f, err := os.Open(configPath)
	if err != nil {
		return ""
	}
	defer f.Close()
	var section, first, origin string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			section = line
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) != "url" || !strings.HasPrefix(section, `[remote "`) {
			continue
		}
		v = strings.TrimSpace(v)
		if section == `[remote "origin"]` {
			origin = v
		} else if first == "" {
			first = v
		}
	}
	if origin == "" {
		origin = first
	}
	return StripCredentials(origin)
}

// StripCredentials removes user:password from URL-form remotes.
func StripCredentials(u string) string {
	if !strings.Contains(u, "://") {
		return u
	}
	pu, err := url.Parse(u)
	if err != nil || pu.User == nil {
		return u
	}
	pu.User = nil
	return pu.String()
}

func defaultBranch(gd, current string) string {
	if b, err := os.ReadFile(filepath.Join(gd, "refs", "remotes", "origin", "HEAD")); err == nil {
		if ref, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "ref: refs/remotes/origin/"); ok {
			return ref
		}
	}
	packed, _ := os.ReadFile(filepath.Join(gd, "packed-refs"))
	for _, b := range []string{"main", "master"} {
		if _, err := os.Stat(filepath.Join(gd, "refs", "heads", b)); err == nil {
			return b
		}
		if strings.Contains(string(packed), " refs/heads/"+b+"\n") {
			return b
		}
	}
	return current
}
