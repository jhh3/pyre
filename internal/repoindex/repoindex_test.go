package repoindex

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// isolate gives git and the package a synthetic home and identity.
func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, ".gitconfig-none"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "Synthetic Tester")
	t.Setenv("GIT_AUTHOR_EMAIL", "synthetic@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "Synthetic Tester")
	t.Setenv("GIT_COMMITTER_EMAIL", "synthetic@example.invalid")
	return home
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := Git(context.Background(), dir, args...)
	if err != nil {
		t.Fatalf("git %v in %s: %v", args, dir, err)
	}
	return out
}

// mkrepo creates a repository with one commit on main.
func mkrepo(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, path, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(path, "README"), []byte("synthetic\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, path, "add", "README")
	git(t, path, "commit", "-q", "-m", "init")
	return path
}

func names(idx contracts.NodeRepoIndex) []string {
	var out []string
	for _, r := range idx.Repos {
		out = append(out, r.Name)
	}
	sort.Strings(out)
	return out
}

func TestDiscoverBoundsAndSkips(t *testing.T) {
	home := isolate(t)
	code := filepath.Join(home, "code")
	alpha := mkrepo(t, filepath.Join(code, "alpha"))
	mkrepo(t, filepath.Join(code, "org", "beta"))
	mkrepo(t, filepath.Join(code, "a", "b", "c", "gamma"))      // depth 4: indexed
	mkrepo(t, filepath.Join(code, "a", "b", "c", "d", "deep"))  // depth 5: not
	mkrepo(t, filepath.Join(code, "node_modules", "dep"))       // dependency tree
	mkrepo(t, filepath.Join(code, ".hidden", "secret"))         // hidden dir
	mkrepo(t, filepath.Join(code, "alpha-worktrees", "nested")) // worktree folder sibling
	mkrepo(t, filepath.Join(alpha, "sub", "inner"))             // nested in a repo
	git(t, alpha, "worktree", "add", "-q", "-b", "feature", filepath.Join(code, "linked"), "HEAD")
	git(t, alpha, "remote", "add", "origin", "https://user:secret-token@example.invalid/owner/alpha.git")
	outside := mkrepo(t, filepath.Join(home, "vault", "notes")) // not under a root
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(code, "vault-link")); err != nil {
		t.Fatal(err)
	}
	idx, err := Discover(context.Background(), Options{Roots: []string{"~/code", "~/src", filepath.Join(home, "code")}})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(names(idx), ","); got != "alpha,beta,gamma" {
		t.Fatalf("repos = %s, want alpha,beta,gamma", got)
	}
	if len(idx.Roots) != 1 || idx.Roots[0] != code {
		t.Fatalf("roots = %v (missing roots dropped, duplicates collapsed)", idx.Roots)
	}
	for _, r := range idx.Repos {
		if r.Name != "alpha" {
			continue
		}
		if r.RemoteURL != "https://example.invalid/owner/alpha.git" {
			t.Fatalf("remote not stripped of credentials: %q", r.RemoteURL)
		}
		if r.DefaultBranch != "main" || r.Branch != "main" || r.Worktrees != 1 || r.LastActivity.IsZero() {
			t.Fatalf("alpha metadata = %+v", r)
		}
	}
	// Bounds: MaxRepos truncates.
	idx, _ = Discover(context.Background(), Options{Roots: []string{code}, MaxRepos: 1})
	if len(idx.Repos) != 1 || !idx.Truncated {
		t.Fatalf("MaxRepos=1: %d repos truncated=%v", len(idx.Repos), idx.Truncated)
	}
	// Exclude never enters a directory.
	idx, _ = Discover(context.Background(), Options{Roots: []string{code}, Exclude: []string{filepath.Join(code, "org")}})
	if got := strings.Join(names(idx), ","); got != "alpha,gamma" {
		t.Fatalf("with exclude: %s", got)
	}
}

func TestDefaultBranchFromOriginHead(t *testing.T) {
	home := isolate(t)
	r := mkrepo(t, filepath.Join(home, "code", "r"))
	git(t, r, "checkout", "-q", "-b", "topic")
	_ = os.MkdirAll(filepath.Join(r, ".git", "refs", "remotes", "origin"), 0o755)
	_ = os.WriteFile(filepath.Join(r, ".git", "refs", "remotes", "origin", "HEAD"), []byte("ref: refs/remotes/origin/trunk\n"), 0o644)
	rp, ok := readRepo(r)
	if !ok || rp.DefaultBranch != "trunk" || rp.Branch != "topic" {
		t.Fatalf("got %+v", rp)
	}
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"Fix the share link redirect":                        "fix-share-link-redirect",
		"please can you migrate auth to Postgres, then test": "migrate-auth-to-postgres-then",
		"  ":                      "",
		"Don't break   prod!!":    "dont-break-prod",
		"ünïcode only":            "n-code-only",
		strings.Repeat("abc", 30): strings.Repeat("abc", 30)[:40],
	} {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRankFuzzyRecentFirst(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	used := now.Add(-time.Hour)
	es := []contracts.RepoEntry{
		{Repo: contracts.Repo{Name: "voyager-web", Path: "/c/voyager-web", RemoteURL: "git@example.invalid:acme/voyager-web.git", LastActivity: now.Add(-48 * time.Hour)}, NodeName: "server"},
		{Repo: contracts.Repo{Name: "tundra", Path: "/c/tundra", RemoteURL: "git@example.invalid:acme/tundra.git", LastActivity: now.Add(-72 * time.Hour)}, NodeName: "server", LastUsedAt: &used},
		{Repo: contracts.Repo{Name: "tundra", Path: "/u/tundra", RemoteURL: "https://example.invalid/acme/tundra", LastActivity: now.Add(-1 * time.Hour)}, NodeName: "mac"},
		{Repo: contracts.Repo{Name: "tundra-old", Path: "/c/tundra-old", LastActivity: now}, NodeName: "server"},
		{Repo: contracts.Repo{Name: "pyre", Path: "/c/pyre", RemoteURL: "https://example.invalid/jhh3/pyre", LastActivity: now.Add(-2 * time.Hour)}, NodeName: "mac"},
	}
	got := Rank("tundra", es)
	if len(got) != 3 || got[0].NodeName != "server" || got[0].Name != "tundra" || got[2].Name != "tundra-old" {
		t.Fatalf("rank tundra: %+v", got)
	}
	g := Group(got)
	if len(g) != 2 || len(g[0]) != 2 || g[0][0].NodeName != "server" {
		t.Fatalf("group: %+v", g)
	}
	if r := Rank("vw", es); len(r) != 1 || r[0].Name != "voyager-web" {
		t.Fatalf("initials: %+v", r)
	}
	if r := Rank("jhh3", es); len(r) != 1 || r[0].Name != "pyre" {
		t.Fatalf("remote match: %+v", r)
	}
	if r := Rank("zzz", es); len(r) != 0 {
		t.Fatalf("no match: %+v", r)
	}
	all := Rank("", es)
	if len(all) != 5 || all[0].NodeName != "server" || all[0].Name != "tundra" {
		t.Fatalf("empty query recent-first (used before active): %+v", all[0])
	}
}

func TestCreateSingleDedupAndIgnore(t *testing.T) {
	home := isolate(t)
	r := mkrepo(t, filepath.Join(home, "code", "app"))
	now := time.Now()
	res, err := Create(context.Background(), contracts.WorktreeCreateParams{Repos: []string{r}, Slug: "Fix the share link redirect"}, now)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(r, ".worktrees", "fix-share-link-redirect")
	if res.Cwd != want || res.Branch != "pyre/fix-share-link-redirect" {
		t.Fatalf("got %+v", res)
	}
	if out := git(t, r, "status", "--porcelain"); out != "" {
		t.Fatalf("main checkout shows changes after create: %q", out)
	}
	if _, pyre, ok := createdAt(want); !ok || !pyre {
		t.Fatal("marker missing")
	}
	res2, err := Create(context.Background(), contracts.WorktreeCreateParams{Repos: []string{r}, Slug: "Fix the share link redirect"}, now)
	if err != nil || res2.Branch != "pyre/fix-share-link-redirect-2" || filepath.Base(res2.Cwd) != "fix-share-link-redirect-2" {
		t.Fatalf("dedup: %+v %v", res2, err)
	}
	// An existing <repo>-worktrees sibling is the repo's convention.
	r2 := mkrepo(t, filepath.Join(home, "code", "lib"))
	_ = os.MkdirAll(filepath.Join(home, "code", "lib-worktrees"), 0o755)
	res3, err := Create(context.Background(), contracts.WorktreeCreateParams{Repos: []string{r2}, Branch: "pyre/x"}, now)
	if err != nil || res3.Cwd != filepath.Join(home, "code", "lib-worktrees", "x") {
		t.Fatalf("sibling convention: %+v %v", res3, err)
	}
	if _, err := Create(context.Background(), contracts.WorktreeCreateParams{Repos: []string{filepath.Join(r, "sub")}}, now); err == nil {
		t.Fatal("non-repo path accepted")
	}
	if _, err := Create(context.Background(), contracts.WorktreeCreateParams{Repos: []string{r}, Branch: "--force"}, now); err == nil {
		t.Fatal("bad branch accepted")
	}
}

func TestCreateMultiRepoFolder(t *testing.T) {
	home := isolate(t)
	a := mkrepo(t, filepath.Join(home, "code", "api"))
	b := mkrepo(t, filepath.Join(home, "code", "web"))
	res, err := Create(context.Background(), contracts.WorktreeCreateParams{Repos: []string{a, b}, Slug: "share links"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	folder := filepath.Join(home, "pyre-work", "share-links")
	if res.Cwd != folder || len(res.Worktrees) != 2 || res.Branch != "pyre/share-links" {
		t.Fatalf("got %+v", res)
	}
	note, err := os.ReadFile(filepath.Join(folder, "CLAUDE.md"))
	if err != nil || !strings.Contains(string(note), "`api/`") || !strings.Contains(string(note), "`web/`") {
		t.Fatalf("workspace note: %q %v", note, err)
	}
	if git(t, filepath.Join(folder, "web"), "rev-parse", "--abbrev-ref", "HEAD") != "pyre/share-links" {
		t.Fatal("web not on the shared branch")
	}
	// Scan from the folder finds both repositories, both worktrees created
	// for this session.
	sw, err := Scan(context.Background(), ScanInput{SessionID: "ses_synth", Cwd: folder, Since: time.Now().Add(time.Hour), Check: true})
	if err != nil || len(sw.Repos) != 2 {
		t.Fatalf("scan: %+v %v", sw, err)
	}
	for _, r := range sw.Repos {
		created := 0
		for _, w := range r.Worktrees {
			if w.CreatedInSession {
				created++
				if !w.PyreCreated || w.Dirty || !w.Merged { // no new commits: merged
					t.Fatalf("worktree flags: %+v", w)
				}
			}
		}
		if created != 1 {
			t.Fatalf("%s: %d created", r.Name, created)
		}
	}
	// Removing both leaves no folder behind (only our notes were in it).
	rm := Remove(context.Background(), ScanInput{SessionID: "ses_synth", Cwd: folder}, []string{filepath.Join(folder, "api"), filepath.Join(folder, "web")})
	for _, x := range rm.Results {
		if x.Outcome != "removed" || x.BranchOutcome != "deleted" {
			// fresh branches are merged (same commit as main): deleted
			t.Fatalf("remove: %+v", x)
		}
	}
	if _, err := os.Stat(folder); !os.IsNotExist(err) {
		t.Fatalf("folder kept: %v", err)
	}
}

func TestRemoveSafetyRules(t *testing.T) {
	home := isolate(t)
	r := mkrepo(t, filepath.Join(home, "code", "svc"))
	before := filepath.Join(r, ".worktrees", "older")
	git(t, r, "worktree", "add", "-q", "-b", "older", before, "HEAD")
	since := time.Now().Add(time.Second)
	// make "older" clearly predate the session
	adm, _ := adminDir(before)
	old := since.Add(-time.Hour)
	_ = os.Chtimes(filepath.Join(adm, "commondir"), old, old)
	time.Sleep(10 * time.Millisecond)
	agent := filepath.Join(r, ".claude", "worktrees", "agent-a")
	dirty := filepath.Join(r, ".claude", "worktrees", "agent-b")
	merged := filepath.Join(r, ".claude", "worktrees", "agent-c")
	for _, p := range []struct{ path, br string }{{agent, "agent-a"}, {dirty, "agent-b"}, {merged, "agent-c"}} {
		git(t, r, "worktree", "add", "-q", "-b", p.br, p.path, "HEAD")
		a, _ := adminDir(p.path)
		future := since.Add(time.Minute)
		_ = os.Chtimes(filepath.Join(a, "commondir"), future, future)
	}
	// agent-a: an unpushed, unmerged commit; agent-b: uncommitted change;
	// agent-c: no new commits (merged).
	_ = os.WriteFile(filepath.Join(agent, "new.txt"), []byte("x\n"), 0o644)
	git(t, agent, "add", "new.txt")
	git(t, agent, "commit", "-q", "-m", "work")
	_ = os.WriteFile(filepath.Join(dirty, "README"), []byte("changed\n"), 0o644)
	in := ScanInput{SessionID: "ses_synth", Cwd: r, Since: since}
	sw, err := Scan(context.Background(), ScanInput{SessionID: in.SessionID, Cwd: r, Since: since, Check: true})
	if err != nil {
		t.Fatal(err)
	}
	flags := map[string]contracts.Worktree{}
	for _, w := range sw.Repos[0].Worktrees {
		flags[filepath.Base(w.Path)] = w
	}
	if !flags["svc"].Main || flags["older"].CreatedInSession || !flags["agent-a"].CreatedInSession || !flags["agent-b"].Dirty || !flags["agent-c"].Merged || flags["agent-a"].Merged || flags["agent-a"].Pushed {
		t.Fatalf("flags: %+v", flags)
	}
	rm := Remove(context.Background(), in, []string{r, before, agent, dirty, merged, filepath.Join(r, "nope")})
	got := map[string]string{}
	for _, x := range rm.Results {
		got[filepath.Base(x.Path)] = x.Outcome + "/" + x.BranchOutcome
	}
	want := map[string]string{"svc": "kept_main/", "older": "kept_not_session/", "agent-a": "removed/kept_unmerged",
		"agent-b": "kept_dirty/", "agent-c": "removed/deleted", "nope": "missing/"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %s, want %s", k, got[k], v)
		}
	}
	if !branchExists(context.Background(), r, "agent-a") || branchExists(context.Background(), r, "agent-c") {
		t.Fatal("branch handling wrong")
	}
	if _, err := os.Stat(dirty); err != nil {
		t.Fatal("dirty worktree removed")
	}
	if _, err := os.Stat(filepath.Join(r, "README")); err != nil {
		t.Fatal("main checkout touched")
	}
}

func TestFindRepoFromLinkedWorktree(t *testing.T) {
	home := isolate(t)
	r := mkrepo(t, filepath.Join(home, "code", "x"))
	wt := filepath.Join(r, ".worktrees", "w")
	git(t, r, "worktree", "add", "-q", "-b", "w", wt, "HEAD")
	_ = os.MkdirAll(filepath.Join(wt, "deep", "er"), 0o755)
	if m, ok := FindRepo(filepath.Join(wt, "deep", "er")); !ok || !samePath(m, r) {
		t.Fatalf("FindRepo = %q %v", m, ok)
	}
	if _, ok := FindRepo(home); ok {
		t.Fatal("home is not a repo")
	}
}
