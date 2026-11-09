package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/repoindex"
)

// A worktree session cleanup removed is recorded under the session and
// added back when a resume needs it as its working directory; other
// missing folders are left alone, and an unrecoverable one says why and
// what to do (iss_…).
func TestResumeRestoresRemovedWorktree(t *testing.T) {
	home := synthHome(t)
	n := startNode(t, t.TempDir(), Config{})
	ctx := context.Background()
	sid := "ses_synth-restore"
	repo := filepath.Join(home, "code", "svc")
	mkRepo(t, repo)
	wt := filepath.Join(home, "wt", "svc", "review")
	gitT(t, repo, "worktree", "add", "-q", "-b", "review", wt, "main")
	_ = os.WriteFile(filepath.Join(wt, "notes.txt"), []byte("synthetic\n"), 0o644)
	gitT(t, wt, "add", "notes.txt")
	gitT(t, wt, "commit", "-q", "-m", "review notes")
	head, _ := repoindex.Git(ctx, wt, "rev-parse", "HEAD")
	gitT(t, repo, "worktree", "remove", wt) // as cleanup does; the branch stays

	n.d.recordRemovedWorktrees(ctx, sid, []contracts.WorktreeRemoval{
		{Path: wt, Branch: "review", Repo: repo, Head: head, Outcome: "removed", BranchOutcome: "kept"},
		{Path: filepath.Join(home, "wt", "svc", "kept"), Branch: "kept", Repo: repo, Outcome: "kept_dirty"},
	})
	if rms := n.d.removedWorktrees(ctx, sid); len(rms) != 1 {
		t.Fatalf("recorded %+v", rms)
	}
	// Another missing folder: not Pyre's removal, nothing happens.
	other := filepath.Join(home, "code", "gone")
	if err := n.d.restoreRemovedWorktrees(ctx, sid, other); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(other); err == nil {
		t.Fatal("an unrecorded folder was created")
	}
	// Another session's resume does not get it.
	if err := n.d.restoreRemovedWorktrees(ctx, "ses_synth-other", wt); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wt); err == nil {
		t.Fatal("restored for the wrong session")
	}
	if err := n.d.restoreRemovedWorktrees(ctx, sid, wt); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got, _ := repoindex.Git(ctx, wt, "rev-parse", "HEAD"); got != head {
		t.Fatalf("HEAD %s, want %s", got, head)
	}
	if _, err := os.Stat(filepath.Join(wt, "notes.txt")); err != nil {
		t.Fatal("restored worktree lacks the session's commit")
	}
	if repoindex.MarkerSession(wt) != sid {
		t.Fatal("restored worktree not marked for the session")
	}
	if rms := n.d.removedWorktrees(ctx, sid); len(rms) != 0 {
		t.Fatalf("record kept after restore: %+v", rms)
	}

	// Branch and commit gone: the error names the branch to start fresh on.
	gone := filepath.Join(home, "wt", "svc", "lost")
	n.d.recordRemovedWorktrees(ctx, sid, []contracts.WorktreeRemoval{{Path: gone, Branch: "lost", Repo: repo, Head: strings.Repeat("cd", 20), Outcome: "removed"}})
	err := n.d.restoreRemovedWorktrees(ctx, sid, gone)
	if err == nil || !strings.Contains(err.Error(), "removed when this session was archived") || !strings.Contains(err.Error(), "Start a fresh session on branch lost") {
		t.Fatalf("unrecoverable restore: %v", err)
	}
}
