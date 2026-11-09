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

// A worktree automatic cleanup removed comes back at the same path for a
// resume: on its kept branch, or on a new branch of that name at the
// recorded commit when cleanup deleted the merged/pushed branch, or
// detached; its marker names the session again
// (iss_…).
func TestRestoreRemovedWorktree(t *testing.T) {
	repo, wts := autoFixture(t)
	ctx := context.Background()
	res := CleanSession(ctx, ScanInput{SessionID: synthSession, Cwd: repo, Since: time.Now().Add(-time.Hour)}, busyOnly(wts))
	removed := map[string]contracts.WorktreeRemoval{}
	for _, r := range res {
		if r.Outcome == "removed" {
			if r.Repo == "" || len(r.Head) != 40 || r.SessionID != synthSession {
				t.Fatalf("removal records too little to restore: %+v", r)
			}
			removed[filepath.Base(r.Path)] = r
		}
	}
	for name, branch := range map[string]string{"merged": "merged", "pushed": "pushed", "gone": "gone", "detached-safe": ""} {
		rm, ok := removed[name]
		if !ok {
			t.Fatalf("%s was not removed: %v", name, outcomes(res))
		}
		if _, err := os.Stat(rm.Path); err == nil {
			t.Fatalf("%s still exists", name)
		}
		if err := Restore(ctx, rm, synthSession); err != nil {
			t.Fatalf("restore %s: %v", name, err)
		}
		if head := git(t, rm.Path, "rev-parse", "HEAD"); head != rm.Head {
			t.Errorf("%s: HEAD %s, want %s", name, head, rm.Head)
		}
		want := branch
		if want == "" {
			want = "HEAD"
		}
		if br := git(t, rm.Path, "rev-parse", "--abbrev-ref", "HEAD"); br != want {
			t.Errorf("%s: on %s, want %s", name, br, want)
		}
		if got := MarkerSession(rm.Path); got != synthSession {
			t.Errorf("%s: marker session %q", name, got)
		}
	}
	if up := git(t, removed["pushed"].Path, "rev-parse", "--abbrev-ref", "pushed@{upstream}"); up != "origin/pushed" {
		t.Errorf("recreated pushed branch tracks %q", up)
	}
	// The path is taken now: never overwritten.
	if err := Restore(ctx, removed["merged"], synthSession); err == nil || !strings.Contains(err.Error(), "exists again") {
		t.Fatalf("restore over an existing folder: %v", err)
	}
	// Neither branch nor commit left.
	lost := contracts.WorktreeRemoval{Repo: repo, Path: filepath.Join(t.TempDir(), "lost"), Branch: "never-was", Head: strings.Repeat("ab", 20), Outcome: "removed"}
	if err := Restore(ctx, lost, synthSession); err == nil || !strings.Contains(err.Error(), "branch never-was and commit abababababab are both gone") {
		t.Fatalf("restore of a lost worktree: %v", err)
	}
}

func TestRestoreFor(t *testing.T) {
	base := t.TempDir()
	rms := []contracts.WorktreeRemoval{{Path: filepath.Join(base, "ws", "a")}, {Path: filepath.Join(base, "ws", "b")}, {Path: filepath.Join(base, "other")}}
	names := func(got []contracts.WorktreeRemoval) string {
		var s []string
		for _, r := range got {
			s = append(s, filepath.Base(r.Path))
		}
		return strings.Join(s, ",")
	}
	for cwd, want := range map[string]string{
		filepath.Join(base, "ws", "a"):        "a",
		filepath.Join(base, "ws", "a", "sub"): "a",
		filepath.Join(base, "ws"):             "a,b",
		filepath.Join(base, "elsewhere"):      "",
		base:                                  "other",
	} {
		if got := names(RestoreFor(rms, cwd)); got != want {
			t.Errorf("%s: %q, want %q", cwd, got, want)
		}
	}
}
