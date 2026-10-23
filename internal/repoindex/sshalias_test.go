package repoindex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// fakeSSH replaces ssh -G with a synthetic config and counts lookups.
func fakeSSH(t *testing.T, hosts map[string]sshHost) map[string]int {
	t.Helper()
	calls := map[string]int{}
	prev := sshResolve
	sshResolve = func(_ context.Context, host string) (sshHost, error) {
		calls[host]++
		if h, ok := hosts[host]; ok {
			return h, nil
		}
		if host == "broken" {
			return sshHost{}, errors.New("synthetic ssh failure")
		}
		return sshHost{HostName: host, User: "synthetic", Port: "22"}, nil // no alias
	}
	resetSSHCache()
	t.Cleanup(func() { sshResolve = prev; resetSSHCache() })
	return calls
}

func resetSSHCache() {
	sshCache.mu.Lock()
	sshCache.m = nil
	sshCache.mu.Unlock()
}

func TestCanonicalRemoteResolvesSSHAlias(t *testing.T) {
	calls := fakeSSH(t, map[string]sshHost{
		"github": {HostName: "github.com", User: "git", Port: "22"},
		"gitbox": {HostName: "git.example.invalid", User: "git", Port: "2222"},
	})
	for in, want := range map[string]string{
		"github:acme/video-editor":          "git@github.com:acme/video-editor",
		"github:/example/repo.git":          "git@github.com:/example/repo.git",
		"me@github:acme/atlas.git":          "me@github.com:acme/atlas.git",
		"ssh://github/acme/atlas.git":       "ssh://git@github.com/acme/atlas.git",
		"gitbox:team/tool.git":              "ssh://git@git.example.invalid:2222/team/tool.git",
		"git@github.com:acme/atlas.git":     "git@github.com:acme/atlas.git", // a real host: never resolved
		"https://github.com/acme/atlas.git": "https://github.com/acme/atlas.git",
		"file:///srv/git/atlas.git":         "file:///srv/git/atlas.git",
		"nas:repos/atlas.git":               "nas:repos/atlas.git", // not an alias
		"broken:acme/atlas.git":             "broken:acme/atlas.git",
		"/srv/git/atlas.git":                "/srv/git/atlas.git",
		"ssh://github:2200/acme/atlas.git":  "ssh://github:2200/acme/atlas.git", // explicit port: left alone
		"./relative/path:with-colon/x":      "./relative/path:with-colon/x",
		"localhost:acme/atlas.git":          "localhost:acme/atlas.git",
		"git@github.com:acme/video-editor/": "git@github.com:acme/video-editor/",
	} {
		if got := CanonicalRemote(in); got != want {
			t.Errorf("CanonicalRemote(%q) = %q, want %q", in, got, want)
		}
	}
	// One lookup per distinct host, failures included.
	CanonicalRemote("github:other/repo")
	CanonicalRemote("broken:x/y")
	for h, n := range calls {
		if n != 1 {
			t.Errorf("ssh -G %s ran %d times, want 1", h, n)
		}
	}
	if calls["github.com"] != 0 || calls["localhost"] != 0 {
		t.Errorf("resolved a real host name: %v", calls)
	}
}

func TestReadRepoRecordsResolvedRemote(t *testing.T) {
	fakeSSH(t, map[string]sshHost{"github": {HostName: "github.com", User: "git", Port: "22"}})
	isolate(t)
	dir := mkrepo(t, filepath.Join(t.TempDir(), "video-editor"))
	git(t, dir, "remote", "add", "origin", "github:acme/video-editor")
	r, ok := ReadRepo(dir)
	if !ok || r.RemoteURL != "git@github.com:acme/video-editor" {
		t.Fatalf("ReadRepo remote = %q (%v)", r.RemoteURL, ok)
	}
	// The checkout's own config is untouched.
	if got := git(t, dir, "config", "remote.origin.url"); got != "github:acme/video-editor" {
		t.Fatalf("checkout config changed: %q", got)
	}
}

func TestNormalizeUnresolvedGitHubAlias(t *testing.T) {
	for _, u := range []string{"github:acme/video-editor", "git@github:acme/video-editor.git",
		"github:/acme/video-editor.git", "git@github.com:/acme/video-editor.git"} {
		if got := NormalizeRemote(u); got != "github.com/acme/video-editor" {
			t.Errorf("NormalizeRemote(%q) = %q", u, got)
		}
		if got := FullNameOf(u); got != "acme/video-editor" {
			t.Errorf("FullNameOf(%q) = %q", u, got)
		}
	}
	// An older index's alias remote and GitHub's own entry are one catalog row.
	items := Catalog([]contracts.RepoEntry{{NodeID: "node_s", Repo: contracts.Repo{Name: "video-editor", Path: "/home/synthetic/code/video-editor",
		RemoteURL: "github:acme/video-editor"}}},
		[]contracts.GitHubRepoIndex{{Repos: []contracts.GitHubRepo{{FullName: "acme/video-editor", CloneURL: "https://github.com/acme/video-editor.git"}}}},
		"", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	if len(items) != 1 || !items[0].GitHub || len(items[0].Present) != 1 || items[0].FullName != "acme/video-editor" {
		t.Fatalf("catalog did not dedupe: %+v", items)
	}
}

func TestCloneCommandGitHubURL(t *testing.T) {
	sshIdx := contracts.NodeRepoIndex{Repos: []contracts.Repo{{RemoteURL: "git@github.com:a/b.git"}, {RemoteURL: "git@github.com:a/c.git"}}}
	httpsIdx := contracts.NodeRepoIndex{Repos: []contracts.Repo{{RemoteURL: "https://github.com/a/b.git"}}}
	const dir = "/home/synthetic/code/.pyre-clone-x"
	cases := []struct {
		name   string
		o      CloneOptions
		remote string
		want   []string
	}{
		{"alias, no gh, https node", CloneOptions{Index: httpsIdx}, "github:acme/video-editor",
			[]string{"git", "clone", "--quiet", "--", "https://github.com/acme/video-editor.git", dir}},
		{"alias, empty index", CloneOptions{}, "github:acme/video-editor",
			[]string{"git", "clone", "--quiet", "--", "https://github.com/acme/video-editor.git", dir}},
		{"scp remote, no ssh preference", CloneOptions{}, "git@github.com:acme/atlas.git",
			[]string{"git", "clone", "--quiet", "--", "https://github.com/acme/atlas.git", dir}},
		{"node prefers ssh", CloneOptions{Index: sshIdx}, "github:acme/video-editor",
			[]string{"git", "clone", "--quiet", "--", "git@github.com:acme/video-editor.git", dir}},
		{"gh installed", CloneOptions{Index: httpsIdx, GH: "/usr/bin/gh"}, "github:acme/video-editor",
			[]string{"/usr/bin/gh", "repo", "clone", "acme/video-editor", dir, "--", "--quiet"}},
		{"other host as given", CloneOptions{}, "git@git.example.invalid:team/tool.git",
			[]string{"git", "clone", "--quiet", "--", "git@git.example.invalid:team/tool.git", dir}},
	}
	for _, c := range cases {
		name, args := cloneCommand(c.o, c.remote, dir)
		got := append([]string{name}, args...)
		if len(got) != len(c.want) {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: %q, want %q", c.name, got, c.want)
				break
			}
		}
	}
	// The destination of an alias remote is still named after the repository.
	home := isolate(t)
	root := filepath.Join(home, "code")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if d, err := CloneDest(contracts.NodeRepoIndex{}, []string{root}, "github:acme/video-editor"); err != nil || d != filepath.Join(root, "video-editor") {
		t.Fatalf("CloneDest = %q, %v", d, err)
	}
}
