package hub

import (
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

func TestRepoIndexReportsAndSearch(t *testing.T) {
	x := newHarness(t)
	x.connect(nodeMac, 0)
	x.connect(nodeLin, 0)
	now := x.clock.Now()
	repo := func(name, path string, age time.Duration) contracts.Repo {
		return contracts.Repo{Name: name, Path: path, DefaultBranch: "main", RemoteURL: "git@example.invalid:synth/" + name + ".git", LastActivity: now.Add(-age)}
	}
	x.report(nodeMac, contracts.ReportRepos, contracts.NodeRepoIndex{Roots: []string{"/home/synth"}, IndexedAt: now,
		Repos: []contracts.Repo{repo("work", "/home/synth/work", time.Minute), repo("worklog", "/home/synth/worklog", time.Hour)}})
	x.report(nodeLin, contracts.ReportRepos, contracts.NodeRepoIndex{Roots: []string{"/home/synth"}, IndexedAt: now,
		Repos: []contracts.Repo{repo("work", "/home/synth/work", 48*time.Hour)}})
	// A session ran in linux's checkout: that machine's entry is "used".
	sv := x.start(owner, "op-repo-start", nodeLin)

	list, err := x.h.ListRepos(as(owner), contracts.ListReposRequest{Query: "work"})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 3 || len(list.Nodes) != 2 {
		t.Fatalf("items=%+v nodes=%v", list.Items, list.Nodes)
	}
	first := list.Items[0]
	if first.NodeID != nodeLin || first.Name != "work" || first.LastUsedAt == nil {
		t.Fatalf("exact name on the recently used machine first: %+v", first)
	}
	if list.Items[2].Name != "worklog" {
		t.Fatalf("prefix match ranks below exact: %+v", list.Items)
	}
	// Node filter and limit.
	list, _ = x.h.ListRepos(as(owner), contracts.ListReposRequest{NodeID: nodeMac, Limit: 1})
	if len(list.Items) != 1 || list.Items[0].NodeID != nodeMac {
		t.Fatalf("filter: %+v", list.Items)
	}
	// A replacement snapshot replaces the node's repos.
	x.report(nodeMac, contracts.ReportRepos, contracts.NodeRepoIndex{IndexedAt: now, Repos: []contracts.Repo{repo("other", "/home/synth/other", time.Minute)}})
	list, _ = x.h.ListRepos(as(owner), contracts.ListReposRequest{NodeID: nodeMac})
	if len(list.Items) != 1 || list.Items[0].Name != "other" {
		t.Fatalf("replace: %+v", list.Items)
	}
	// A node cannot report another node's index (recorded as a reject).
	x.report(nodeMac, contracts.ReportRepos, contracts.NodeRepoIndex{NodeID: nodeLin, Repos: []contracts.Repo{repo("evil", "/x", 0)}})
	list, _ = x.h.ListRepos(as(owner), contracts.ListReposRequest{Query: "evil"})
	if len(list.Items) != 0 {
		t.Fatalf("foreign report accepted: %+v", list.Items)
	}

	// Session worktrees: none reported yet, then the node's snapshot.
	sid := sv.Session.SessionID
	sw, err := x.h.SessionWorktrees(as(owner), contracts.SessionWorktreesRequest{SessionID: sid})
	if err != nil || len(sw.Repos) != 0 {
		t.Fatalf("empty: %+v %v", sw, err)
	}
	x.report(nodeLin, contracts.ReportSessionWorktrees, contracts.SessionWorktrees{SessionID: sid, RunID: sv.Run.RunID, Cwd: "/home/synth/work",
		Repos: []contracts.RepoWorktrees{{Name: "work", Path: "/home/synth/work", Worktrees: []contracts.Worktree{
			{Path: "/home/synth/work", Branch: "main", Main: true},
			{Path: "/home/synth/work/.claude/worktrees/a", Branch: "worktree-a", CreatedInSession: true}}}}})
	sw, err = x.h.SessionWorktrees(as(owner), contracts.SessionWorktreesRequest{SessionID: sid})
	if err != nil || len(sw.Repos) != 1 || len(sw.Repos[0].Worktrees) != 2 || sw.NodeID != nodeLin {
		t.Fatalf("reported: %+v %v", sw, err)
	}
	// Another node cannot report worktrees for this session's run.
	x.report(nodeMac, contracts.ReportSessionWorktrees, contracts.SessionWorktrees{SessionID: sid, RunID: sv.Run.RunID, Repos: []contracts.RepoWorktrees{}})
	if sw, _ = x.h.SessionWorktrees(as(owner), contracts.SessionWorktreesRequest{SessionID: sid}); len(sw.Repos) != 1 {
		t.Fatalf("foreign worktree report accepted: %+v", sw)
	}
	if _, err := x.h.SessionWorktrees(as(owner), contracts.SessionWorktreesRequest{SessionID: "ses_nope"}); !contracts.IsCode(err, contracts.ErrNotFound) {
		t.Fatalf("unknown session: %v", err)
	}
	// Survives a restart (own migration namespace).
	x.restart()
	if list, _ = x.h.ListRepos(as(owner), contracts.ListReposRequest{}); len(list.Items) != 2 {
		t.Fatalf("after restart: %+v", list.Items)
	}
	// Agents may create worktrees for launches but not remove them.
	if !sessionActions[contracts.ActionWorktreeCreate] || sessionActions[contracts.ActionWorktreeRemove] {
		t.Fatal("session action allowlist")
	}
}

func TestRepoLastUsedIncludesWorktrees(t *testing.T) {
	t0 := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	dirs := map[string]time.Time{
		"/c/app/.worktrees/x":      t0.Add(3 * time.Hour),
		"/c/app-worktrees/y":       t0.Add(4 * time.Hour),
		"/c/apple":                 t0.Add(9 * time.Hour), // a different repo
		"/c/app":                   t0,
		"/c/app/.claude/worktrees": t0.Add(time.Hour),
	}
	if got := repoLastUsed(dirs, "/c/app"); !got.Equal(t0.Add(4 * time.Hour)) {
		t.Fatalf("got %v", got)
	}
}
