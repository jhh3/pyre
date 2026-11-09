package ghguard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Main runs the shim: args are gh's arguments (without argv0). It returns
// an exit code only when it does not exec gh (Windows always returns gh's).
func Main(args []string) int {
	self, _ := os.Executable()
	real, err := FindReal(os.Getenv("PATH"), os.Getenv(EnvShimDir), self)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pyre gh shim: %v\n", err)
		return 127
	}
	args = RewriteArgs(args)
	if os.Getenv(EnvGuard) != "off" && !Exempt(args) {
		if code, stop := guard(args); stop {
			return code
		}
	}
	return execReal(real, args)
}

// guard asks pyred for the call. pyred unreachable or not a Pyre session:
// the call goes ahead (the guard never breaks gh).
func guard(args []string) (int, bool) {
	sock, tok := os.Getenv("PYRE_SOCKET"), os.Getenv("PYRE_SESSION_TOKEN")
	if sock == "" || tok == "" {
		return 0, false
	}
	resp, err := ask(sock, tok, CallRequest{RunID: os.Getenv("PYRE_RUN_ID"), Command: Command(args)})
	if err != nil {
		return 0, false
	}
	if resp.Warn != "" {
		fmt.Fprintln(os.Stderr, resp.Warn)
	}
	if resp.Refuse {
		fmt.Fprintln(os.Stderr, resp.Message)
		return 1, true
	}
	if resp.WaitMS > 0 {
		if resp.Message != "" {
			fmt.Fprintln(os.Stderr, resp.Message)
		}
		time.Sleep(time.Duration(resp.WaitMS) * time.Millisecond)
	}
	return 0, false
}

func ask(sock, tok string, req CallRequest) (CallResponse, error) {
	hc := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		}}}
	b, _ := json.Marshal(req)
	hr, err := http.NewRequest(http.MethodPost, "http://pyred"+PathCall, bytes.NewReader(b))
	if err != nil {
		return CallResponse{}, err
	}
	hr.Header.Set("Authorization", "Bearer "+tok)
	hr.Header.Set("Content-Type", "application/json")
	r, err := hc.Do(hr)
	if err != nil {
		return CallResponse{}, err
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		return CallResponse{}, fmt.Errorf("pyred: HTTP %d", r.StatusCode)
	}
	var out CallResponse
	err = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&out)
	return out, err
}

// FindReal returns the first gh on path that is not the shim: not in
// shimDir and not the same file as self (the pyre binary).
func FindReal(path, shimDir, self string) (string, error) {
	var selfSt os.FileInfo
	if self != "" {
		selfSt, _ = os.Stat(self)
	}
	names := []string{"gh"}
	if runtime.GOOS == "windows" {
		names = []string{"gh.exe", "gh.cmd", "gh.bat"}
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" || (shimDir != "" && samePath(dir, shimDir)) {
			continue
		}
		for _, n := range names {
			p := filepath.Join(dir, n)
			st, err := os.Stat(p)
			if err != nil || st.IsDir() || (runtime.GOOS != "windows" && st.Mode()&0o111 == 0) {
				continue
			}
			if selfSt != nil && os.SameFile(st, selfSt) {
				continue
			}
			return p, nil
		}
	}
	return "", fmt.Errorf("no gh found on PATH besides Pyre's shim (install GitHub CLI, or set %s=off)", EnvGuard)
}

func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// RateLimit parses `gh api rate_limit` output.
func RateLimit(b []byte, now time.Time) (contracts.GitHubBudget, error) {
	type bucket struct {
		Limit     int   `json:"limit"`
		Remaining int   `json:"remaining"`
		Reset     int64 `json:"reset"`
	}
	var v struct {
		Resources map[string]bucket `json:"resources"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return contracts.GitHubBudget{}, err
	}
	core, ok := v.Resources["core"]
	if !ok {
		return contracts.GitHubBudget{}, fmt.Errorf("rate_limit: no core bucket")
	}
	conv := func(x bucket) contracts.GitHubLimit {
		l := contracts.GitHubLimit{Limit: x.Limit, Remaining: x.Remaining}
		if x.Reset > 0 {
			l.Reset = time.Unix(x.Reset, 0).UTC()
		}
		return l
	}
	return contracts.GitHubBudget{Core: conv(core), GraphQL: conv(v.Resources["graphql"]), ObservedAt: now}, nil
}

var pacific = func() *time.Location {
	if l, err := time.LoadLocation("America/Los_Angeles"); err == nil {
		return l
	}
	return time.Local
}()

// Clock formats t as the owner reads times ("3:01 PM PT").
func Clock(t time.Time) string { return t.In(pacific).Format("3:04 PM") + " PT" }

// LimitText is one bucket as "812/5000 left (resets 3:01 PM PT)".
func LimitText(l contracts.GitHubLimit) string {
	s := fmt.Sprintf("%d/%d left", l.Remaining, l.Limit)
	if !l.Reset.IsZero() {
		s += " (resets " + Clock(l.Reset) + ")"
	}
	return s
}

// BudgetText is a sampled budget in one line: "core 812/5000 left (resets
// 3:01 PM PT), graphql 4990/5000 left (...)".
func BudgetText(b contracts.GitHubBudget) string {
	s := "core " + LimitText(b.Core)
	if b.GraphQL.Limit > 0 {
		s += ", graphql " + LimitText(b.GraphQL)
	}
	return s
}

// The shim's stderr messages. sid names the session.
func WaitMessage(sid string, d Decision, perMinute int) string {
	return fmt.Sprintf("pyre: gh call delayed %.0fs: session %s made %d gh calls in the last minute (budget %d/min). "+
		"The GitHub API limit is shared by every agent on every machine: poll GitHub at most once a minute, "+
		"and wait on CI, PRs and reviews with `pyre wait gh-pr <url> --until checks-done` instead of watch loops.", d.Wait.Seconds(), sid, d.LastMinute, perMinute)
}

func RefuseMessage(sid string, d Decision, perMinute int) string {
	return fmt.Sprintf("pyre: gh call refused: session %s made %d gh calls in the last minute, well over its budget of %d/min. "+
		"The GitHub API limit is shared by every agent on every machine: poll GitHub at most once a minute "+
		"(to wait on CI, a PR or a review: `pyre wait gh-pr <url> --until checks-done`), and retry this call in a few seconds.", sid, d.LastMinute, perMinute)
}

func WarnMessage(b contracts.GitHubBudget) string {
	return "pyre: GitHub API budget is low: " + BudgetText(b) +
		". Every agent on every machine shares it: poll GitHub at most once a minute and skip calls you can avoid."
}

// Take asks pyred for one call from this session's budget, as the shim
// does before each gh call (pyre wait's GitHub poller uses it): it sleeps
// a throttle wait and returns an error when the call is refused. Outside a
// Pyre session, or with pyred unreachable, the call goes ahead.
func Take(ctx context.Context, command string) error {
	sock, tok := os.Getenv("PYRE_SOCKET"), os.Getenv("PYRE_SESSION_TOKEN")
	if sock == "" || tok == "" || os.Getenv(EnvGuard) == "off" {
		return nil
	}
	resp, err := ask(sock, tok, CallRequest{RunID: os.Getenv("PYRE_RUN_ID"), Command: command})
	if err != nil {
		return nil
	}
	if resp.Refuse {
		return fmt.Errorf("pyre gh budget: %s", resp.Message)
	}
	if resp.WaitMS > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(resp.WaitMS) * time.Millisecond):
		}
	}
	return nil
}
