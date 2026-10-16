package repoindex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// The owner's GitHub repositories through the gh CLI's own authentication
// (W-newsession). Pyre never reads or stores a GitHub token: it runs `gh
// api` on a node where gh is logged in.

// GHRunner runs `gh <args>` and returns stdout.
type GHRunner func(ctx context.Context, args ...string) ([]byte, error)

// ExecGH runs the gh binary at path, bounded, non-interactive.
func ExecGH(path string) GHRunner {
	return func(ctx context.Context, args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, path, args...)
		cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1", "NO_COLOR=1", "GH_PAGER=", "LC_ALL=C")
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			msg := strings.TrimSpace(errb.String())
			if len(msg) > 300 {
				msg = msg[:300]
			}
			if msg == "" {
				msg = err.Error()
			}
			return out.Bytes(), fmt.Errorf("gh %s: %s", args[0], msg)
		}
		return out.Bytes(), nil
	}
}

// maxGitHubRepos bounds one snapshot.
const maxGitHubRepos = 3000

// FetchGitHub lists the authenticated user's repositories (own,
// collaborator, organization) and their recent contribution signals:
// commit search by author and the user's recent push / pull request
// events. A failing signal query is skipped; failing to identify the
// user (gh not logged in) is an error.
func FetchGitHub(ctx context.Context, gh GHRunner, now time.Time) (contracts.GitHubRepoIndex, error) {
	idx := contracts.GitHubRepoIndex{Repos: []contracts.GitHubRepo{}}
	login, err := gh(ctx, "api", "user", "--jq", ".login")
	if err != nil {
		return idx, contracts.Errorf(contracts.ErrUnavailable, "gh is not logged in here (%v)", err)
	}
	idx.Login = strings.TrimSpace(string(login))
	if idx.Login == "" {
		return idx, contracts.Errorf(contracts.ErrUnavailable, "gh did not report a login")
	}
	raw, err := gh(ctx, "api", "--paginate", "user/repos?per_page=100&sort=pushed&affiliation=owner,collaborator,organization_member",
		"--jq", `.[] | {full_name, clone_url, ssh_url, description, default_branch, private, fork, archived, pushed_at}`)
	if err != nil && len(raw) == 0 {
		return idx, contracts.Errorf(contracts.ErrUnavailable, "list GitHub repositories: %v", err)
	}
	at := map[string]int{}
	for _, line := range jsonLines(raw) {
		var r contracts.GitHubRepo
		if json.Unmarshal(line, &r) != nil || r.FullName == "" {
			continue
		}
		if len(idx.Repos) >= maxGitHubRepos {
			idx.Truncated = true
			break
		}
		if _, dup := at[strings.ToLower(r.FullName)]; dup {
			continue
		}
		at[strings.ToLower(r.FullName)] = len(idx.Repos)
		idx.Repos = append(idx.Repos, r)
	}
	// Contribution signals: the newest per repository.
	contrib := map[string]time.Time{}
	note := func(repo string, t time.Time) {
		k := strings.ToLower(repo)
		if repo == "" || t.IsZero() || t.After(now.Add(time.Hour)) {
			return
		}
		if t.After(contrib[k]) {
			contrib[k] = t
		}
		if _, ok := at[k]; !ok && len(idx.Repos) < maxGitHubRepos {
			at[k] = len(idx.Repos)
			idx.Repos = append(idx.Repos, contracts.GitHubRepo{FullName: repo, CloneURL: "https://github.com/" + repo + ".git",
				SSHURL: "git@github.com:" + repo + ".git"})
		}
	}
	type sig struct {
		Repo string    `json:"repo"`
		At   time.Time `json:"at"`
	}
	q := url.QueryEscape("author:" + idx.Login)
	if out, err := gh(ctx, "api", "search/commits?q="+q+"&sort=author-date&order=desc&per_page=100",
		"--jq", `.items[] | {repo: .repository.full_name, at: .commit.author.date}`); err == nil {
		for _, line := range jsonLines(out) {
			var s sig
			if json.Unmarshal(line, &s) == nil {
				note(s.Repo, s.At)
			}
		}
	}
	if out, err := gh(ctx, "api", "users/"+url.PathEscape(idx.Login)+"/events?per_page=100",
		"--jq", `.[] | select(.type=="PushEvent" or .type=="PullRequestEvent" or .type=="PullRequestReviewEvent" or .type=="CreateEvent") | {repo: .repo.name, at: .created_at}`); err == nil {
		for _, line := range jsonLines(out) {
			var s sig
			if json.Unmarshal(line, &s) == nil {
				note(s.Repo, s.At)
			}
		}
	}
	for i := range idx.Repos {
		if t, ok := contrib[strings.ToLower(idx.Repos[i].FullName)]; ok {
			t := t.UTC()
			idx.Repos[i].ContributedAt = &t
		}
	}
	sort.SliceStable(idx.Repos, func(i, j int) bool { return idx.Repos[i].FullName < idx.Repos[j].FullName })
	idx.FetchedAt = now.UTC()
	return idx, nil
}

// jsonLines splits gh --jq output (one compact JSON value per line).
func jsonLines(b []byte) [][]byte {
	var out [][]byte
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	for sc.Scan() {
		if l := bytes.TrimSpace(sc.Bytes()); len(l) > 0 && l[0] == '{' {
			out = append(out, append([]byte(nil), l...))
		}
	}
	return out
}
