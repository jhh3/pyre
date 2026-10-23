package repoindex

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// NoOriginWarning is the note a fresh worktree carries when its repository
// has no origin remote, so its branch starts at the local HEAD.
const NoOriginWarning = "no origin remote; branched from local HEAD"

// FetchTimeout bounds the fetch before a fresh worktree is created. It is
// a variable so tests can shorten it.
var FetchTimeout = 60 * time.Second

// FreshStart is where a fresh worktree's new branch starts.
type FreshStart struct {
	Ref     string // origin/<default branch>, or HEAD without an origin remote
	Warning string // set when Ref is the local HEAD
}

// FreshBase fetches the default branch from origin and returns
// origin/<default> as the start point of a new branch, so a fresh worktree
// never starts from a main checkout's stale local branch. The default
// branch is origin/HEAD, else the repository's indexed default (local
// main/master), else whichever of main and master origin has. A
// repository without an origin remote starts at HEAD, with a warning. A
// failed fetch is an error naming the repository and git's message: the
// caller must not fall back to the local HEAD.
//
// The fetch uses the checkout's own remote and this machine's git
// credentials; for an https GitHub remote that the plain fetch cannot
// reach, it retries once through gh's credential helper (gh, or gh found
// on PATH when empty), the way a gh clone authenticates.
func FreshBase(ctx context.Context, repo, gh string) (FreshStart, error) {
	url, err := Git(ctx, repo, "remote", "get-url", "origin")
	if err != nil || strings.TrimSpace(url) == "" {
		return FreshStart{Ref: "HEAD", Warning: NoOriginWarning}, nil
	}
	def := ""
	if ref, err := Git(ctx, repo, "symbolic-ref", "--quiet", "refs/remotes/origin/HEAD"); err == nil {
		def = strings.TrimPrefix(strings.TrimSpace(ref), "refs/remotes/origin/")
	}
	name := filepath.Base(repo)
	if def != "" {
		err = fetchOrigin(ctx, repo, url, gh, "+refs/heads/"+def+":refs/remotes/origin/"+def)
	} else {
		// No origin/HEAD: fetch everything, then pick the default.
		err = fetchOrigin(ctx, repo, url, gh)
		if err == nil {
			var cands []string
			if common, cerr := Git(ctx, repo, "rev-parse", "--git-common-dir"); cerr == nil {
				if !filepath.IsAbs(common) {
					common = filepath.Join(repo, common)
				}
				if b := defaultBranch(common, ""); b != "" {
					cands = append(cands, b)
				}
			}
			for _, c := range append(cands, "main", "master") {
				if _, err := Git(ctx, repo, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+c); err == nil {
					def = c
					break
				}
			}
			if def == "" {
				return FreshStart{}, contracts.Errorf(contracts.ErrInvalid, "%s: cannot tell origin's default branch (no origin/HEAD, origin/main or origin/master); run `git remote set-head origin --auto` in %s", name, repo)
			}
		}
	}
	if err != nil {
		return FreshStart{}, contracts.Errorf(contracts.ErrUnavailable, "%s: git fetch origin failed, so no fresh worktree was created (it would start from the stale local HEAD): %v", name, err)
	}
	if _, err := Git(ctx, repo, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+def); err != nil {
		return FreshStart{}, contracts.Errorf(contracts.ErrInvalid, "%s: origin has no branch %s after fetching it", name, def)
	}
	return FreshStart{Ref: "origin/" + def}, nil
}

// fetchOrigin runs `git fetch origin [refspec]` within FetchTimeout, then
// once more through gh's credential helper when the remote is an https
// GitHub URL and gh is available.
func fetchOrigin(ctx context.Context, repo, url, gh string, refspec ...string) error {
	args := append([]string{"fetch", "--quiet", "--no-tags", "origin"}, refspec...)
	err := runFetch(ctx, repo, args)
	if err == nil || !strings.HasPrefix(NormalizeRemote(url), "github.com/") || !strings.HasPrefix(strings.TrimSpace(url), "https://") {
		return err
	}
	if gh == "" {
		gh, _ = exec.LookPath("gh")
	}
	if gh == "" {
		return err
	}
	helper := "!" + strings.ReplaceAll(gh, " ", `\ `) + " auth git-credential"
	if err2 := runFetch(ctx, repo, append([]string{"-c", "credential.helper=", "-c", "credential.helper=" + helper}, args...)); err2 != nil {
		return err
	}
	return nil
}

func runFetch(ctx context.Context, repo string, args []string) error {
	ctx, cancel := context.WithTimeout(ctx, FetchTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repo}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1", "LC_ALL=C")
	var errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &errb, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if len(msg) > 600 {
			msg = msg[len(msg)-600:]
		}
		if ctx.Err() != nil {
			return fmt.Errorf("no answer from origin within %s: %s", FetchTimeout, msg)
		}
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
}
