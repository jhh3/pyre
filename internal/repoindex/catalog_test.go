package repoindex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

func TestNormalizeRemote(t *testing.T) {
	for _, u := range []string{"git@github.com:Acme/Atlas.git", "https://github.com/acme/atlas", "https://github.com/acme/atlas.git/",
		"ssh://git@github.com/acme/atlas.git", "https://user:secret@github.com/acme/atlas.git"} {
		if got := NormalizeRemote(u); got != "github.com/acme/atlas" {
			t.Errorf("NormalizeRemote(%q) = %q", u, got)
		}
	}
	if FullNameOf("git@github.com:acme/atlas.git") != "acme/atlas" || FullNameOf("") != "" {
		t.Error("FullNameOf")
	}
	for s, want := range map[string]bool{"acme/atlas": true, "atlas": false, "~/x/y": false, "git@github.com:a/b": false, "a/b/c": false} {
		if IsGitHubSpec(s) != want {
			t.Errorf("IsGitHubSpec(%q) != %v", s, want)
		}
	}
}

func TestCatalogMergeDedupeRank(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) *time.Time { t := now.Add(-d); return &t }
	entries := []contracts.RepoEntry{
		{Repo: contracts.Repo{Name: "atlas", Path: "/home/synth/code/atlas", RemoteURL: "git@github.com:acme/atlas.git", LastActivity: now.Add(-time.Hour)}, NodeID: "n1", NodeName: "server", LastUsedAt: ago(2 * time.Hour)},
		{Repo: contracts.Repo{Name: "atlas", Path: "/Users/synth/code/atlas", RemoteURL: "https://github.com/acme/atlas", LastActivity: now.Add(-3 * time.Hour)}, NodeID: "n2", NodeName: "laptop"},
		{Repo: contracts.Repo{Name: "tool", Path: "/home/synth/code/tool", RemoteURL: "https://example.invalid/synth/tool.git", LastActivity: now.Add(-5 * time.Hour)}, NodeID: "n1", NodeName: "server"},
		{Repo: contracts.Repo{Name: "scratch", Path: "/home/synth/code/scratch", LastActivity: now.Add(-50 * time.Hour)}, NodeID: "n1", NodeName: "server"},
	}
	gh := []contracts.GitHubRepoIndex{{Login: "synth-owner", Repos: []contracts.GitHubRepo{
		{FullName: "acme/atlas", CloneURL: "https://github.com/acme/atlas.git", ContributedAt: ago(30 * time.Hour)},
		{FullName: "acme/orbit", CloneURL: "https://github.com/acme/orbit.git", ContributedAt: ago(4 * time.Hour), Description: "synthetic"},
		{FullName: "acme/lumen", CloneURL: "https://github.com/acme/lumen.git", PushedAt: now.Add(-40 * 24 * time.Hour)},
		{FullName: "acme/stale", CloneURL: "https://github.com/acme/stale.git", ContributedAt: ago(400 * 24 * time.Hour)},
		{FullName: "acme/old", CloneURL: "https://github.com/acme/old.git", Archived: true},
	}}}
	items := Catalog(entries, gh, "", now)
	var names []string
	for _, c := range items {
		names = append(names, c.Name)
	}
	// used > contributed > local (by activity) > the rest; archived and
	// not checked out: left out; atlas (ssh + https, two machines) is one.
	if got := strings.Join(names, ","); got != "atlas,orbit,tool,scratch,lumen,stale" {
		t.Fatalf("order: %s", got)
	}
	a := items[0]
	if len(a.Present) != 2 || !a.GitHub || a.FullName != "acme/atlas" || a.Rank != contracts.CatalogRankUsed || a.ContributedAt == nil {
		t.Fatalf("atlas: %+v", a)
	}
	if items[1].Rank != contracts.CatalogRankContributed || len(items[1].Present) != 0 || items[1].RemoteURL != "https://github.com/acme/orbit.git" {
		t.Fatalf("orbit: %+v", items[1])
	}
	if items[5].Rank != contracts.CatalogRankOther {
		t.Fatalf("an old contribution lifted stale: %+v", items[5])
	}
	// A query: better matches first; owner/name exactly.
	if q := Catalog(entries, gh, "acme/orbit", now); len(q) == 0 || q[0].Name != "orbit" {
		t.Fatalf("owner/name query: %+v", q)
	}
	if q := RankCatalog(items, "lum"); len(q) != 1 || q[0].Name != "lumen" {
		t.Fatalf("query: %+v", q)
	}
	if c, _, ok := FindCatalog("git@github.com:acme/orbit.git", items); !ok || c.Name != "orbit" {
		t.Fatalf("FindCatalog by remote: %+v %v", c, ok)
	}
	if c, _, ok := FindCatalog("tool", items); !ok || c.Name != "tool" {
		t.Fatalf("FindCatalog by name: %+v %v", c, ok)
	}
}

func TestCloneDestLayout(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "code")
	flat := contracts.NodeRepoIndex{Roots: []string{root}, Repos: []contracts.Repo{
		{Path: filepath.Join(root, "a"), RemoteURL: "git@github.com:acme/a.git"},
		{Path: filepath.Join(root, "b"), RemoteURL: "git@github.com:other/b.git"},
	}}
	if d, _ := CloneDest(flat, nil, "https://github.com/acme/atlas.git"); d != filepath.Join(root, "atlas") {
		t.Fatalf("flat: %s", d)
	}
	byOwner := contracts.NodeRepoIndex{Roots: []string{filepath.Join(home, "src"), root}, Repos: []contracts.Repo{
		{Path: filepath.Join(root, "acme", "a"), RemoteURL: "git@github.com:acme/a.git"},
		{Path: filepath.Join(root, "other", "b"), RemoteURL: "https://github.com/other/b"},
		{Path: filepath.Join(home, "src", "c"), RemoteURL: "https://github.com/x/c"},
	}}
	if d, _ := CloneDest(byOwner, nil, "acme/atlas"); d != filepath.Join(root, "acme", "atlas") {
		t.Fatalf("by owner (the root with most repositories): %s", d)
	}
	if _, err := CloneDest(flat, nil, "https://github.com/"); err == nil {
		t.Fatal("a remote without a name was accepted")
	}
}

// synthGit isolates git (and the home) for clone tests.
func synthGit(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, ".gitconfig-none"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "Synthetic Tester")
	t.Setenv("GIT_COMMITTER_NAME", "Synthetic Tester")
	t.Setenv("GIT_AUTHOR_EMAIL", "synthetic@example.invalid")
	t.Setenv("GIT_COMMITTER_EMAIL", "synthetic@example.invalid")
	return home
}

func gitOK(t *testing.T, dir string, args ...string) {
	t.Helper()
	if _, err := Git(context.Background(), dir, args...); err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
}

// bareRemote makes a synthetic bare repository with one commit.
func bareRemote(t *testing.T, base, name string) string {
	t.Helper()
	work := filepath.Join(base, "work-"+name)
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	gitOK(t, work, "init", "-q", "-b", "main")
	_ = os.WriteFile(filepath.Join(work, "README"), []byte("synthetic\n"), 0o644)
	gitOK(t, work, "add", "README")
	gitOK(t, work, "commit", "-q", "-m", "init")
	bare := filepath.Join(base, "remotes", "synth-owner", name+".git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	gitOK(t, base, "clone", "-q", "--bare", work, bare)
	return "file://" + bare
}

func TestCloneLocalBareRemote(t *testing.T) {
	home := synthGit(t)
	remote := bareRemote(t, t.TempDir(), "widget")
	root := filepath.Join(home, "code")
	ctx := context.Background()
	o := CloneOptions{Roots: []string{root}}

	res, err := Clone(ctx, contracts.RepoCloneParams{Remote: remote}, o)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "widget")
	if res.Path != want || !res.Cloned || res.Repo.Name != "widget" || NormalizeRemote(res.Repo.RemoteURL) != NormalizeRemote(remote) {
		t.Fatalf("clone: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(want, "README")); err != nil {
		t.Fatal("clone has no files")
	}
	// Again: the same repository is reused, nothing changes.
	again, err := Clone(ctx, contracts.RepoCloneParams{Remote: remote}, o)
	if err != nil || again.Cloned || again.Path != want {
		t.Fatalf("second clone: %+v %v", again, err)
	}
	// A different repository at the destination is refused, untouched.
	other := bareRemote(t, t.TempDir(), "widget")
	_, err = Clone(ctx, contracts.RepoCloneParams{Remote: other}, o)
	if !contracts.IsCode(err, contracts.ErrConflict) || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("different remote: %v", err)
	}
	// A plain directory is refused too.
	plain := filepath.Join(root, "gadget")
	_ = os.MkdirAll(plain, 0o755)
	_ = os.WriteFile(filepath.Join(plain, "keep.txt"), []byte("synthetic"), 0o644)
	_, err = Clone(ctx, contracts.RepoCloneParams{Remote: bareRemote(t, t.TempDir(), "gadget")}, o)
	if !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("plain dir: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(plain, "keep.txt")); string(b) != "synthetic" {
		t.Fatal("existing directory was modified")
	}
	// Outside home, and local paths, are refused.
	if _, err := Clone(ctx, contracts.RepoCloneParams{Remote: remote, Dest: "/tmp/elsewhere"}, o); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("dest outside home: %v", err)
	}
	if _, err := Clone(ctx, contracts.RepoCloneParams{Remote: "/some/local/path"}, o); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("local path: %v", err)
	}
	// A failing clone leaves nothing behind.
	_, err = Clone(ctx, contracts.RepoCloneParams{Remote: "file://" + filepath.Join(home, "missing", "nope.git")}, o)
	if err == nil {
		t.Fatal("missing remote cloned")
	}
	ents, _ := os.ReadDir(root)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".pyre-clone-") || e.Name() == "nope" {
			t.Fatalf("left behind: %s", e.Name())
		}
	}
}

func TestFetchGitHubWithFakeGH(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	var calls []string
	gh := func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		switch {
		case args[1] == "user":
			return []byte("synth-owner\n"), nil
		case strings.HasPrefix(args[2], "user/repos"):
			return []byte(`{"full_name":"acme/atlas","clone_url":"https://github.com/acme/atlas.git","ssh_url":"git@github.com:acme/atlas.git","pushed_at":"2026-10-06T10:00:00Z"}
{"full_name":"acme/lumen","clone_url":"https://github.com/acme/lumen.git","private":true,"pushed_at":"2026-08-01T10:00:00Z"}
`), nil
		case strings.HasPrefix(args[1], "search/commits"):
			return []byte(`{"repo":"acme/atlas","at":"2026-10-06T09:00:00Z"}
{"repo":"upstream/lib","at":"2026-10-05T09:00:00Z"}
`), nil
		case strings.Contains(args[1], "/events"):
			return nil, errors.New("synthetic events failure") // skipped
		}
		return nil, errors.New("unexpected")
	}
	idx, err := FetchGitHub(context.Background(), gh, now)
	if err != nil {
		t.Fatal(err)
	}
	if idx.Login != "synth-owner" || len(idx.Repos) != 3 || !idx.FetchedAt.Equal(now) {
		t.Fatalf("index: %+v", idx)
	}
	by := map[string]contracts.GitHubRepo{}
	for _, r := range idx.Repos {
		by[r.FullName] = r
	}
	if by["acme/atlas"].ContributedAt == nil || by["acme/lumen"].ContributedAt != nil || !by["acme/lumen"].Private {
		t.Fatalf("signals: %+v", idx.Repos)
	}
	if lib := by["upstream/lib"]; lib.CloneURL != "https://github.com/upstream/lib.git" || lib.ContributedAt == nil {
		t.Fatalf("contributed-to repository not added: %+v", lib)
	}
	if !strings.Contains(calls[2], "author%3Asynth-owner") {
		t.Fatalf("search query: %v", calls)
	}
	// Not logged in: an error.
	if _, err := FetchGitHub(context.Background(), func(context.Context, ...string) ([]byte, error) { return nil, errors.New("not logged in") }, now); err == nil {
		t.Fatal("no login accepted")
	}
}
