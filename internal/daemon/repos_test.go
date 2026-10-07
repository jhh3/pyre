package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/repoindex"
	"github.com/jhh3/pyre/internal/runtime/mock"
)

func gitT(t *testing.T, dir string, args ...string) {
	t.Helper()
	if _, err := repoindex.Git(context.Background(), dir, args...); err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
}

// synthHome isolates git and the node's home for repo tests.
func synthHome(t *testing.T) string {
	t.Helper()
	home := shortDir(t)
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, ".gitconfig-none"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "Synthetic Tester")
	}
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "synthetic@example.invalid")
	}
	return home
}

func mkRepo(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, path, "init", "-q", "-b", "main")
	_ = os.WriteFile(filepath.Join(path, "README"), []byte("synthetic\n"), 0o644)
	gitT(t, path, "add", "README")
	gitT(t, path, "commit", "-q", "-m", "init")
}

func TestRepoIndexLoopReportsAndWorktreeActions(t *testing.T) {
	home := synthHome(t)
	code := filepath.Join(home, "code")
	mkRepo(t, filepath.Join(code, "alpha"))
	mkRepo(t, filepath.Join(code, "beta"))
	ad := &countingAdapter{RuntimeAdapter: mock.NewAdapter()}
	n := startNode(t, shortDir(t), Config{Adapters: []contracts.RuntimeAdapter{ad},
		RepoIndex: RepoIndexConfig{Enabled: true, Roots: []string{code}, StartDelay: -1, Interval: time.Hour, WorktreeInterval: time.Hour}})

	// The background pass reports the index once.
	waitFor(t, "repo report", func() bool { return len(n.rep.Reports(contracts.ReportRepos)) == 1 })
	var idx contracts.NodeRepoIndex
	if err := json.Unmarshal(n.rep.Reports(contracts.ReportRepos)[0].Payload, &idx); err != nil {
		t.Fatal(err)
	}
	if idx.NodeID != "node_test" || len(idx.Repos) != 2 || idx.Roots[0] != code {
		t.Fatalf("index: %+v", idx)
	}
	// An unchanged rescan is not reported again; a new repo is.
	if _, err := n.d.reindex(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(n.rep.Reports(contracts.ReportRepos)); got != 1 {
		t.Fatalf("unchanged rescan reported (%d reports)", got)
	}
	mkRepo(t, filepath.Join(code, "gamma"))
	if _, err := n.d.reindex(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(n.rep.Reports(contracts.ReportRepos)); got != 2 {
		t.Fatalf("changed rescan not reported (%d reports)", got)
	}
	// The local API answers from the node's own index while the hub is away.
	local := n.d.localRepos(contracts.ListReposRequest{Query: "gam"})
	if len(local.Items) != 1 || local.Items[0].Name != "gamma" {
		t.Fatalf("local repos: %+v", local.Items)
	}

	// worktree.create (multi-repo) then a session in that folder.
	raw, _ := json.Marshal(contracts.WorktreeCreateParams{Repos: []string{filepath.Join(code, "alpha"), filepath.Join(code, "beta")}, Slug: "share links"})
	v, err := n.d.actionWorktreeCreate(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	wt := v.(contracts.WorktreeCreateResult)
	if wt.Cwd != filepath.Join(home, "pyre-work", "share-links") || len(wt.Worktrees) != 2 {
		t.Fatalf("create: %+v", wt)
	}
	n.start(startCmd("ses_wt", "run_wt", contracts.HarnessMock, wt.Cwd))
	// The agent makes its own worktree in alpha during the session.
	agentWT := filepath.Join(code, "alpha", ".claude", "worktrees", "agent-x")
	gitT(t, filepath.Join(code, "alpha"), "worktree", "add", "-q", "-b", "agent-x", agentWT, "HEAD")

	n.d.reportWorktrees(context.Background())
	reps := n.rep.Reports(contracts.ReportSessionWorktrees)
	if len(reps) != 1 {
		t.Fatalf("worktree reports: %d", len(reps))
	}
	var sw contracts.SessionWorktrees
	_ = json.Unmarshal(reps[0].Payload, &sw)
	if sw.SessionID != "ses_wt" || len(sw.Repos) != 2 {
		t.Fatalf("session worktrees: %+v", sw)
	}
	created := 0
	for _, r := range sw.Repos {
		for _, w := range r.Worktrees {
			if w.CreatedInSession {
				created++
			}
		}
	}
	if created != 3 { // two pyre-created + the agent's own
		t.Fatalf("created in session = %d: %+v", created, sw)
	}
	n.d.reportWorktrees(context.Background())
	if len(n.rep.Reports(contracts.ReportSessionWorktrees)) != 1 {
		t.Fatal("unchanged worktrees reported again")
	}

	// Removal is refused while the session runs.
	rmRaw, _ := json.Marshal(contracts.WorktreeRemoveParams{SessionID: "ses_wt", Paths: []string{agentWT}})
	if _, err := n.d.actionWorktreeRemove(context.Background(), rmRaw); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("remove while live: %v", err)
	}
	res := n.do(command(contracts.CmdStop, contracts.RunCommand{SessionID: "ses_wt", RunID: "run_wt"}))
	if res.Error != nil {
		t.Fatalf("stop: %v", res.Error)
	}
	waitFor(t, "run ended", func() bool {
		n.d.PollNow(context.Background(), "run_wt")
		return n.run("run_wt").Run.State.Terminal()
	})
	scanRaw, _ := json.Marshal(contracts.WorktreeScanParams{SessionID: "ses_wt"})
	if v, err = n.d.actionWorktreeScan(context.Background(), scanRaw); err != nil || len(v.(contracts.SessionWorktrees).Repos) != 2 {
		t.Fatalf("scan after end: %+v %v", v, err)
	}
	rmRaw, _ = json.Marshal(contracts.WorktreeRemoveParams{SessionID: "ses_wt", Paths: []string{agentWT, filepath.Join(code, "alpha")}})
	v, err = n.d.actionWorktreeRemove(context.Background(), rmRaw)
	if err != nil {
		t.Fatal(err)
	}
	out := v.(contracts.WorktreeRemoveResult).Results
	if len(out) != 2 || out[0].Outcome != "removed" || out[0].BranchOutcome != "deleted" || out[1].Outcome != "kept_main" {
		t.Fatalf("remove: %+v", out)
	}
	if _, err := os.Stat(filepath.Join(code, "alpha", "README")); err != nil {
		t.Fatal("main checkout touched")
	}
}

func TestRepoIndexDisabled(t *testing.T) {
	home := synthHome(t)
	mkRepo(t, filepath.Join(home, "code", "alpha"))
	n := startNode(t, shortDir(t), Config{RepoIndex: RepoIndexConfig{Enabled: false, Roots: []string{filepath.Join(home, "code")}, StartDelay: -1}})
	time.Sleep(100 * time.Millisecond)
	if got := len(n.rep.Reports(contracts.ReportRepos)); got != 0 {
		t.Fatalf("disabled index reported %d times", got)
	}
}
