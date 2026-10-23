package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/mock"
)

// A start with no folder (a remote start onto a Windows node) runs in the
// node user's home instead of failing "working directory is required".
func TestResolveCheckoutDefaultsToHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	d := &Daemon{}
	cwd, b, err := d.resolveCheckout(context.Background(), contracts.StartRunCommand{})
	if err != nil || cwd != home || b != nil {
		t.Fatalf("no cwd: %q %+v %v", cwd, b, err)
	}
	if _, _, err := d.resolveCheckout(context.Background(), contracts.StartRunCommand{Checkout: &contracts.CheckoutRequest{Mode: contracts.CheckoutWorktree}}); err == nil {
		t.Fatal("a worktree checkout still needs its source repository")
	}
}

// The whole start path (not just resolveCheckout): an empty cwd launches in
// home. rc86 still failed on win-home because startRun checked first.
func TestStartRunNoCwdRunsInHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	n := startNode(t, shortDir(t), Config{Adapters: []contracts.RuntimeAdapter{mock.NewAdapter()}})
	r := n.start(startCmd("ses_nocwd", "run_nocwd", contracts.HarnessMock, ""))
	if r.Cwd != home {
		t.Fatalf("cwd %q, want %q (state %s: %s)", r.Cwd, home, r.State, r.StateReason)
	}
}

// The --project --repo --worktree path: a new worktree branch starts at
// origin/main (fetched), not the checkout's stale local main, and tracks
// nothing; an unreachable origin fails the launch.
func TestResolveCheckoutWorktreeStartsAtOriginMain(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git missing")
	}
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(root, ".gitconfig-none"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "Synthetic Tester")
	t.Setenv("GIT_AUTHOR_EMAIL", "synthetic@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "Synthetic Tester")
	t.Setenv("GIT_COMMITTER_EMAIL", "synthetic@example.invalid")
	g := func(dir string, args ...string) string {
		t.Helper()
		out, err := git(context.Background(), dir, args...)
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return out
	}
	seed := filepath.Join(root, "seed")
	os.MkdirAll(seed, 0o755)
	g(seed, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(seed, "a.txt"), []byte("one\n"), 0o644)
	g(seed, "add", "a.txt")
	g(seed, "commit", "-q", "-m", "init")
	origin := filepath.Join(root, "origin.git")
	g(root, "clone", "-q", "--bare", seed, origin)
	clone := filepath.Join(root, "code", "app")
	g(root, "clone", "-q", origin, clone)
	os.WriteFile(filepath.Join(seed, "a.txt"), []byte("two\n"), 0o644)
	g(seed, "commit", "-q", "-am", "two")
	g(seed, "push", "-q", origin, "main")
	tip := g(origin, "rev-parse", "main")

	d := &Daemon{}
	sc := contracts.StartRunCommand{RunID: "run_01ABCDEFGHJK", Cwd: clone, Checkout: &contracts.CheckoutRequest{Mode: contracts.CheckoutWorktree}}
	cwd, b, err := d.resolveCheckout(context.Background(), sc)
	if err != nil {
		t.Fatal(err)
	}
	if got := g(cwd, "rev-parse", "HEAD"); got != tip {
		t.Fatalf("worktree starts at %s, want origin/main %s", got, tip)
	}
	if up, err := git(context.Background(), cwd, "rev-parse", "--abbrev-ref", b.Branch+"@{upstream}"); err == nil {
		t.Fatalf("branch tracks %s; want no upstream", up)
	}

	g(clone, "remote", "set-url", "origin", filepath.Join(root, "gone.git"))
	sc.RunID, sc.Checkout.Branch = "run_01ABCDEFGHJM", "pyre/other"
	if _, _, err := d.resolveCheckout(context.Background(), sc); err == nil || !strings.Contains(err.Error(), "git fetch origin failed") {
		t.Fatalf("unreachable origin: err = %v", err)
	}
}
