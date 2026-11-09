package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/mock"
)

// cwdAdapter is the mock harness reporting where its conversation worked
// lately (like Claude's transcript cwd) and recording its launches.
type cwdAdapter struct {
	contracts.RuntimeAdapter
	mu   sync.Mutex
	cwds []string
	last contracts.ResolvedLaunch
}

func (c *cwdAdapter) RecentCwds(contracts.Run) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cwds
}

func (c *cwdAdapter) Launch(ctx context.Context, l contracts.ResolvedLaunch) (contracts.RunHandle, error) {
	c.mu.Lock()
	c.last = l
	c.mu.Unlock()
	return c.RuntimeAdapter.Launch(ctx, l)
}

// A session that removed its launch worktree and went on working in
// another one resumes in the newest directory its conversation used that
// still exists, and is told so; the resume never fails only because the
// old directory is gone (iss_…).
func TestResumeFallsBackWhenCwdIsGone(t *testing.T) {
	synthHome(t)
	old := tempRoots // the test's dirs are under $TMPDIR: not a reboot-emptied scratch dir here
	tempRoots = nil
	t.Cleanup(func() { tempRoots = old })
	ad := &cwdAdapter{RuntimeAdapter: mock.NewAdapter()}
	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{ad}})
	base := t.TempDir()
	launch := filepath.Join(base, "sync-fixture-parity")
	later := filepath.Join(base, "replication-test-split")
	for _, d := range []string{launch, later} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	n.start(startCmd("ses_c", "run_c1", contracts.HarnessMock, launch))
	if res := n.do(command(contracts.CmdStop, contracts.RunCommand{SessionID: "ses_c", RunID: "run_c1"})); res.Error != nil {
		t.Fatalf("stop = %+v", res.Error)
	}
	if err := os.RemoveAll(launch); err != nil { // merged and cleaned up
		t.Fatal(err)
	}
	// Newest first; a directory that is gone too is passed over.
	ad.cwds = []string{filepath.Join(base, "also-gone"), later, launch}

	prev := n.run("run_c1").Run
	rc := contracts.ResumeCommand{StartRunCommand: startCmd("ses_c", "run_c2", contracts.HarnessMock, launch)}
	rc.PreviousRun = &prev
	if res := n.do(command(contracts.CmdResume, rc)); res.Error != nil {
		t.Fatalf("resume = %+v", res.Error)
	}
	n.d.WaitLaunches()
	r2 := n.run("run_c2")
	if r2.Phase != phaseLive || r2.Run.Cwd != later || r2.Run.ContinuationKind != contracts.ContinuationNativeResume {
		t.Fatalf("resumed = %+v %s", r2.Run, r2.Phase)
	}
	ad.mu.Lock()
	l := ad.last
	ad.mu.Unlock()
	if l.Cwd != later || !strings.Contains(l.InitialPrompt, "resumed in "+later) || !strings.Contains(l.InitialPrompt, launch+" no longer exists") {
		t.Fatalf("launch cwd %q prompt %q", l.Cwd, l.InitialPrompt)
	}
}

// Without a known later directory the resume runs in the repository's main
// checkout (the old directory was inside it), else in the home directory.
func TestResumeCwdFallbackOrder(t *testing.T) {
	home := synthHome(t)
	n := startNode(t, t.TempDir(), Config{})
	ctx := context.Background()
	repo := filepath.Join(home, "code", "svc")
	mkRepo(t, repo)
	wt := filepath.Join(repo, ".claude", "worktrees", "feature")
	gitT(t, repo, "worktree", "add", "-q", "-b", "feature", wt, "main")
	gitT(t, repo, "worktree", "remove", wt)

	ad := &cwdAdapter{RuntimeAdapter: mock.NewAdapter()}
	sc := contracts.StartRunCommand{Session: contracts.Session{SessionID: "ses_o"}, Cwd: wt, PreviousRun: &contracts.Run{}}
	if note := n.d.resumeCwdFallback(ctx, ad, &sc); sc.Cwd != repo || !strings.Contains(note, "main checkout") {
		t.Fatalf("cwd %q note %q", sc.Cwd, note)
	}
	sc.Cwd = filepath.Join(home, "scratch", "gone", "deeper")
	if note := n.d.resumeCwdFallback(ctx, ad, &sc); sc.Cwd != home || !strings.Contains(note, "home directory") {
		t.Fatalf("cwd %q note %q", sc.Cwd, note)
	}
	// An existing directory, or a checkout launch, is left alone.
	sc.Cwd = repo
	if note := n.d.resumeCwdFallback(ctx, ad, &sc); note != "" || sc.Cwd != repo {
		t.Fatalf("existing cwd changed: %q %q", sc.Cwd, note)
	}
	sc.Cwd, sc.Checkout = wt, &contracts.CheckoutRequest{Mode: contracts.CheckoutWorktree}
	if note := n.d.resumeCwdFallback(ctx, ad, &sc); note != "" || sc.Cwd != wt {
		t.Fatalf("checkout launch changed: %q %q", sc.Cwd, note)
	}
}
