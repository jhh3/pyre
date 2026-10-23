package repoindex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// LaggingClone makes a bare origin, a clone of it, and then moves origin's
// main two commits ahead, so the clone's local main is stale (the
// devbox video-editor case). It returns the clone and origin's tip.
func laggingClone(t *testing.T, root string) (clone, tip string) {
	t.Helper()
	seed := mkrepo(t, filepath.Join(root, "seed"))
	origin := filepath.Join(root, "origin.git")
	git(t, root, "clone", "-q", "--bare", seed, origin)
	clone = filepath.Join(root, "code", "app")
	git(t, root, "clone", "-q", origin, clone)
	for i, msg := range []string{"two", "three"} {
		if err := os.WriteFile(filepath.Join(seed, "README"), []byte(strings.Repeat("x", i+2)+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, seed, "commit", "-q", "-am", msg)
	}
	git(t, seed, "push", "-q", origin, "main")
	tip = git(t, origin, "rev-parse", "main")
	if git(t, clone, "rev-parse", "main") == tip {
		t.Fatal("setup: the clone should lag origin")
	}
	return clone, tip
}

func TestCreateFreshBranchStartsAtOriginMain(t *testing.T) {
	root := isolate(t)
	clone, tip := laggingClone(t, root)
	before := git(t, clone, "rev-parse", "main")
	res, err := Create(context.Background(), contracts.WorktreeCreateParams{Repos: []string{clone}, Slug: "new ui"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("warnings: %v", res.Warnings)
	}
	wt := res.Worktrees[0]
	if got := git(t, wt.Path, "rev-parse", "HEAD"); got != tip {
		t.Fatalf("fresh branch starts at %s, want origin/main %s", got, tip)
	}
	if up, err := Git(context.Background(), wt.Path, "rev-parse", "--abbrev-ref", wt.Branch+"@{upstream}"); err == nil {
		t.Fatalf("fresh branch tracks %s; want no upstream", up)
	}
	if got := git(t, clone, "rev-parse", "main"); got != before {
		t.Fatal("the main checkout's local main moved")
	}
}

func TestCreateFetchFailureFailsLoudly(t *testing.T) {
	root := isolate(t)
	clone, _ := laggingClone(t, root)
	git(t, clone, "remote", "set-url", "origin", filepath.Join(root, "gone.git"))
	_, err := Create(context.Background(), contracts.WorktreeCreateParams{Repos: []string{clone}, Slug: "x"}, time.Now())
	if err == nil || !strings.Contains(err.Error(), "app: git fetch origin failed") {
		t.Fatalf("err = %v", err)
	}
	if branchExists(context.Background(), clone, "pyre/x") {
		t.Fatal("a branch was created despite the failed fetch")
	}
	if _, err := os.Stat(filepath.Join(clone, ".worktrees", "x")); err == nil {
		t.Fatal("a worktree was created despite the failed fetch")
	}
}

func TestCreateNoOriginKeepsHeadWithWarning(t *testing.T) {
	root := isolate(t)
	r := mkrepo(t, filepath.Join(root, "code", "local"))
	head := git(t, r, "rev-parse", "HEAD")
	res, err := Create(context.Background(), contracts.WorktreeCreateParams{Repos: []string{r}, Slug: "y"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) != 1 || res.Warnings[0] != NoOriginWarning {
		t.Fatalf("warnings: %v", res.Warnings)
	}
	if got := git(t, res.Worktrees[0].Path, "rev-parse", "HEAD"); got != head {
		t.Fatalf("start %s, want HEAD %s", got, head)
	}
}

// Without origin/HEAD the default comes from the local main/master.
func TestFreshBaseWithoutOriginHead(t *testing.T) {
	root := isolate(t)
	clone, tip := laggingClone(t, root)
	git(t, clone, "remote", "set-head", "origin", "--delete")
	st, err := FreshBase(context.Background(), clone, "")
	if err != nil || st.Ref != "origin/main" || git(t, clone, "rev-parse", "origin/main") != tip {
		t.Fatalf("start = %+v, %v", st, err)
	}
}
