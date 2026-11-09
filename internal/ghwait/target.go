// Package ghwait is `pyre wait gh-pr` / `pyre wait gh-run`: a GitHub-aware
// wait that evaluates a condition (checks done, merged, a review, ...) on a
// pull request or an Actions run and produces one pre-summarized message.
//
// It polls GitHub's REST API with conditional requests (If-None-Match), so
// an unchanged answer (304) does not count against the account's rate
// limit. Waits on the same target on one machine share one poller
// (Poller): the first waiter whose snapshot is stale fetches under the
// target's file lock and every other waiter reads the snapshot it wrote.
// The CLI side (internal/cli/wait_gh.go) runs a wait as a monitor session,
// so it survives pyred restarts and reboots like any monitor.
package ghwait

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Kind is what a wait watches.
type Kind string

const (
	KindPR  Kind = "gh-pr"
	KindRun Kind = "gh-run"
)

// Target is a pull request (Number) or an Actions run (RunID).
type Target struct {
	Kind   Kind   `json:"kind"`
	Owner  string `json:"owner"`
	Repo   string `json:"repo"`
	Number int    `json:"number,omitempty"`
	RunID  int64  `json:"run_id,omitempty"`
}

// Repo is "owner/repo".
func (t Target) FullRepo() string { return t.Owner + "/" + t.Repo }

// String is "owner/repo#N" or "owner/repo run ID".
func (t Target) String() string {
	if t.Kind == KindRun {
		return fmt.Sprintf("%s run %d", t.FullRepo(), t.RunID)
	}
	return fmt.Sprintf("%s#%d", t.FullRepo(), t.Number)
}

// Key identifies the target on disk (snapshot and lock file names).
func (t Target) Key() string {
	k := strings.ToLower(t.Owner + "_" + t.Repo)
	if t.Kind == KindRun {
		return fmt.Sprintf("run_%s_%d", k, t.RunID)
	}
	return fmt.Sprintf("pr_%s_%d", k, t.Number)
}

// URL is the target's page on github.com.
func (t Target) URL() string {
	if t.Kind == KindRun {
		return fmt.Sprintf("https://github.com/%s/actions/runs/%d", t.FullRepo(), t.RunID)
	}
	return fmt.Sprintf("https://github.com/%s/pull/%d", t.FullRepo(), t.Number)
}

var (
	nameRe  = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	shortPR = regexp.MustCompile(`^([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)#([0-9]+)$`)
)

// ParsePR accepts https://github.com/o/r/pull/N[/...] or o/r#N.
func ParsePR(s string) (Target, error) {
	s = strings.TrimSpace(s)
	if m := shortPR.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[3])
		return prTarget(m[1], m[2], n)
	}
	parts, ok := githubPath(s)
	if ok && len(parts) >= 4 && (parts[2] == "pull" || parts[2] == "pulls") {
		n, err := strconv.Atoi(parts[3])
		if err == nil {
			return prTarget(parts[0], parts[1], n)
		}
	}
	return Target{}, fmt.Errorf("not a pull request: %q (want https://github.com/OWNER/REPO/pull/N or OWNER/REPO#N)", s)
}

func prTarget(o, r string, n int) (Target, error) {
	if !nameRe.MatchString(o) || !nameRe.MatchString(r) || n <= 0 {
		return Target{}, fmt.Errorf("not a pull request: %s/%s#%d", o, r, n)
	}
	return Target{Kind: KindPR, Owner: o, Repo: r, Number: n}, nil
}

// ParseRun accepts https://github.com/o/r/actions/runs/ID[/...] or the two
// arguments "o/r" "ID" (also "o/r/ID" in one).
func ParseRun(args []string) (Target, error) {
	bad := fmt.Errorf("not an Actions run: %q (want https://github.com/OWNER/REPO/actions/runs/ID or OWNER/REPO RUN-ID)", strings.Join(args, " "))
	var o, r, id string
	switch len(args) {
	case 1:
		if parts, ok := githubPath(args[0]); ok {
			if len(parts) >= 5 && parts[2] == "actions" && parts[3] == "runs" {
				o, r, id = parts[0], parts[1], parts[4]
			}
		} else if p := strings.Split(strings.TrimSpace(args[0]), "/"); len(p) == 3 {
			o, r, id = p[0], p[1], p[2]
		}
	case 2:
		p := strings.Split(strings.TrimSpace(args[0]), "/")
		if len(p) == 2 {
			o, r, id = p[0], p[1], strings.TrimSpace(args[1])
		}
	}
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil || n <= 0 || !nameRe.MatchString(o) || !nameRe.MatchString(r) {
		return Target{}, bad
	}
	return Target{Kind: KindRun, Owner: o, Repo: r, RunID: n}, nil
}

// githubPath splits a github.com URL's path.
func githubPath(s string) ([]string, bool) {
	if !strings.Contains(s, "://") {
		if !strings.HasPrefix(s, "github.com/") {
			return nil, false
		}
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || !strings.EqualFold(strings.TrimPrefix(u.Host, "www."), "github.com") {
		return nil, false
	}
	var parts []string
	for _, p := range strings.Split(u.Path, "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return parts, true
}

// Cond is a parsed --until condition.
type Cond struct {
	Name  string `json:"name"`            // checks-done, checks-fail, merged, closed, review-from, comment, head-changed; done, failed
	Login string `json:"login,omitempty"` // review-from:<login>, comment:<login>
}

func (c Cond) String() string {
	if c.Login != "" {
		return c.Name + ":" + c.Login
	}
	return c.Name
}

// PRConds and RunConds are the accepted --until values.
var (
	PRConds  = []string{"checks-done", "checks-fail", "merged", "closed", "review-from:<login>", "comment[:<login>]", "head-changed"}
	RunConds = []string{"done", "failed"}
)

// ParseCond parses --until for kind k.
func ParseCond(k Kind, s string) (Cond, error) {
	s = strings.TrimSpace(s)
	name, login, _ := strings.Cut(s, ":")
	name = strings.ToLower(name)
	login = strings.TrimPrefix(strings.TrimSpace(login), "@")
	c := Cond{Name: name, Login: login}
	if k == KindRun {
		switch name {
		case "done", "failed":
			if login == "" {
				return c, nil
			}
		}
		return Cond{}, fmt.Errorf("--until for gh-run is one of: %s (got %q)", strings.Join(RunConds, ", "), s)
	}
	switch name {
	case "checks-done", "checks-fail", "merged", "closed", "head-changed":
		if login == "" {
			return c, nil
		}
	case "review-from":
		if login != "" {
			return c, nil
		}
	case "comment":
		return c, nil
	}
	return Cond{}, fmt.Errorf("--until for gh-pr is one of: %s (got %q)", strings.Join(PRConds, ", "), s)
}

// sameLogin compares GitHub logins ignoring case and a "[bot]" suffix, so
// review-from:gemini-code-assist matches gemini-code-assist[bot].
func sameLogin(a, want string) bool {
	norm := func(s string) string { return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), "[bot]") }
	return norm(a) == norm(want)
}
