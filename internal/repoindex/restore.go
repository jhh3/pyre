package repoindex

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Restore adds back a worktree automatic cleanup removed (rm, a "removed"
// WorktreeRemoval that recorded its Repo and Head), at the same path, for a
// resume of its session. `git worktree remove` keeps the branch unless it
// was merged or pushed, so the worktree goes back on its branch when the
// branch still exists, else on a new branch of that name at the recorded
// commit (which is merged or on the remote: git keeps it). Its marker names
// sessionID again, so the next cleanup treats it like the original.
// It fails, saying why, when the path is taken or neither the branch nor
// the commit is left.
func Restore(ctx context.Context, rm contracts.WorktreeRemoval, sessionID string) error {
	if rm.Repo == "" || rm.Path == "" || (rm.Branch == "" && rm.Head == "") {
		return errors.New("cleanup recorded no repository, branch or commit for it")
	}
	if _, err := os.Lstat(rm.Path); !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s exists again", rm.Path)
	}
	if _, err := os.Stat(filepath.Join(rm.Repo, ".git")); err != nil {
		return fmt.Errorf("its repository %s is gone", rm.Repo)
	}
	_, _ = Git(ctx, rm.Repo, "worktree", "prune") // a stale entry for the path blocks the add
	if err := os.MkdirAll(filepath.Dir(rm.Path), 0o755); err != nil {
		return err
	}
	haveBranch := rm.Branch != "" && refExists(ctx, rm.Repo, "refs/heads/"+rm.Branch)
	haveHead := rm.Head != "" && refExists(ctx, rm.Repo, rm.Head+"^{commit}")
	var err error
	switch {
	case haveBranch:
		_, err = Git(ctx, rm.Repo, "worktree", "add", rm.Path, rm.Branch)
	case haveHead && rm.Branch != "":
		_, err = Git(ctx, rm.Repo, "worktree", "add", "--no-track", "-b", rm.Branch, rm.Path, rm.Head)
		if err == nil && refExists(ctx, rm.Repo, "refs/remotes/origin/"+rm.Branch) {
			_, _ = Git(ctx, rm.Repo, "branch", "--set-upstream-to=origin/"+rm.Branch, rm.Branch)
		}
	case haveHead:
		_, err = Git(ctx, rm.Repo, "worktree", "add", "--detach", rm.Path, rm.Head)
	case rm.Branch != "":
		return fmt.Errorf("branch %s and commit %s are both gone from %s", rm.Branch, shortHead(rm.Head), rm.Repo)
	default:
		return fmt.Errorf("commit %s is gone from %s", shortHead(rm.Head), rm.Repo)
	}
	if err != nil {
		return err
	}
	if adm, ok := adminDir(rm.Path); ok {
		_ = os.WriteFile(filepath.Join(adm, MarkerFile), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"+markerSessionPrefix+sessionID+"\n"), 0o644)
	}
	return nil
}

func refExists(ctx context.Context, repo, ref string) bool {
	_, err := Git(ctx, repo, "rev-parse", "--verify", "--quiet", ref)
	return err == nil
}

func shortHead(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	if h == "" {
		return "(unknown)"
	}
	return h
}

// RestoreFor picks the removal in rms that a resume in cwd needs: the one
// whose path is cwd or contains it, or whose parent folder is cwd (a
// multi-repo session folder cleanup also removed). Paths compare
// lexically, then with the symlinks of their nearest existing parent
// resolved (macOS /var -> /private/var).
func RestoreFor(rms []contracts.WorktreeRemoval, cwd string) []contracts.WorktreeRemoval {
	cwd = existingCanon(cwd)
	var out []contracts.WorktreeRemoval
	for _, rm := range rms {
		p := existingCanon(rm.Path)
		if p == cwd || strings.HasPrefix(cwd, p+string(filepath.Separator)) || filepath.Dir(p) == cwd {
			out = append(out, rm)
		}
	}
	return out
}

// existingCanon resolves the symlinks of p's nearest existing ancestor and
// keeps the missing rest as is.
func existingCanon(p string) string {
	p = filepath.Clean(p)
	rest := ""
	for d := p; ; d = filepath.Dir(d) {
		if r, err := filepath.EvalSymlinks(d); err == nil {
			return filepath.Join(r, rest)
		}
		if filepath.Dir(d) == d {
			return p
		}
		rest = filepath.Join(filepath.Base(d), rest)
	}
}
