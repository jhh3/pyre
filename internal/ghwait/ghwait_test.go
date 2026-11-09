package ghwait

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeGitHub serves testdata fixtures by path with ETags and answers 304
// to a matching If-None-Match, like GitHub's REST API. Never the real one.
type fakeGitHub struct {
	t      *testing.T
	mu     sync.Mutex
	routes map[string]string // path (with query) -> fixture file, or "!404" etc.
	hits   map[string]int
	full   atomic.Int64 // 200 answers
	not    atomic.Int64 // 304 answers
	srv    *httptest.Server
}

func newFakeGitHub(t *testing.T, routes map[string]string) *fakeGitHub {
	f := &fakeGitHub{t: t, routes: routes, hits: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) set(path, fixture string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[path] = fixture
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer synth-token" {
		w.WriteHeader(401)
		fmt.Fprint(w, `{"message":"Bad credentials"}`)
		return
	}
	key := strings.TrimPrefix(r.URL.RequestURI(), "/")
	f.mu.Lock()
	fx, ok := f.routes[key]
	f.hits[key]++
	f.mu.Unlock()
	w.Header().Set("X-RateLimit-Limit", "5000")
	w.Header().Set("X-RateLimit-Remaining", "4900")
	switch {
	case !ok || fx == "!404":
		w.WriteHeader(404)
		fmt.Fprint(w, `{"message":"Not Found"}`)
		return
	case fx == "!ratelimit":
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(403)
		fmt.Fprint(w, `{"message":"API rate limit exceeded for user ID 1."}`)
		return
	}
	b, err := os.ReadFile(filepath.Join("testdata", fx))
	if err != nil {
		f.t.Errorf("fixture %s: %v", fx, err)
		w.WriteHeader(500)
		return
	}
	etag := fmt.Sprintf(`W/"%x"`, sha256.Sum256(b))
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		f.not.Add(1)
		w.WriteHeader(304)
		return
	}
	f.full.Add(1)
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

const prBase = "repos/synth-org/synth-repo/"

func prRoutes(pull, checks, status, reviews, comments, lineComments string) map[string]string {
	sha := "c208475aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	return map[string]string{
		prBase + "pulls/7": pull,
		prBase + "commits/" + sha + "/check-runs?per_page=100":      checks,
		prBase + "commits/" + sha + "/status?per_page=100":          status,
		prBase + "pulls/7/reviews?per_page=100":                     reviews,
		prBase + "issues/7/comments?per_page=100":                   comments,
		prBase + "pulls/7/comments?per_page=100":                    lineComments,
		prBase + "actions/runs?head_sha=" + sha + "&per_page=100":   "runs_head_completed.json",
		prBase + "actions/runs/900":                                 "run_in_progress.json",
		prBase + "actions/runs/900/jobs?per_page=100&filter=latest": "jobs_running_onefail.json",
	}
}

func (f *fakeGitHub) client(dir string) *Client {
	return &Client{Base: f.srv.URL, Token: func(context.Context) (string, error) { return "synth-token", nil }, CacheDir: dir}
}

var (
	prT  = Target{Kind: KindPR, Owner: "synth-org", Repo: "synth-repo", Number: 7}
	runT = Target{Kind: KindRun, Owner: "synth-org", Repo: "synth-repo", RunID: 900}
	// The wait began at 21:00 UTC; fixtures before it are "old".
	waitStart = time.Date(2026, 11, 6, 21, 0, 0, 0, time.UTC)
)

func fetchSnap(t *testing.T, f *fakeGitHub, tg Target) Snapshot {
	t.Helper()
	p := &Poller{Dir: t.TempDir(), Client: f.client(t.TempDir())}
	s, _, err := p.Poll(context.Background(), tg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func mustCond(t *testing.T, k Kind, s string) Cond {
	t.Helper()
	c, err := ParseCond(k, s)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func evalFixture(t *testing.T, routes map[string]string, tg Target, cond string, w WaitCtx) Result {
	t.Helper()
	f := newFakeGitHub(t, routes)
	if w.Start.IsZero() {
		w.Start = waitStart
	}
	if w.Now.IsZero() {
		w.Now = waitStart.Add(time.Hour)
	}
	return Eval(mustCond(t, tg.Kind, cond), fetchSnap(t, f, tg), w)
}

func wantMet(t *testing.T, r Result, met bool, contains ...string) {
	t.Helper()
	if r.Met != met {
		t.Fatalf("met=%v, want %v; state %q\n%s", r.Met, met, r.State, r.Text)
	}
	for _, s := range contains {
		if !strings.Contains(r.Text, s) {
			t.Errorf("message lacks %q:\n%s", s, r.Text)
		}
	}
}

func TestEvalChecks(t *testing.T) {
	base := func(checks, status string) map[string]string {
		return prRoutes("pull_open.json", checks, status, "reviews.json", "issue_comments.json", "review_comments.json")
	}
	// Running checks: neither condition is met yet.
	wantMet(t, evalFixture(t, base("checks_running.json", "status_empty.json"), prT, "checks-done", WaitCtx{}), false)
	wantMet(t, evalFixture(t, base("checks_running.json", "status_empty.json"), prT, "checks-fail", WaitCtx{}), false)
	// A pending commit status keeps checks-done waiting even with all check runs done.
	wantMet(t, evalFixture(t, base("checks_green.json", "status_pending.json"), prT, "checks-done", WaitCtx{}), false)

	// One failed while another still runs: checks-fail fires (fail fast), checks-done does not.
	r := evalFixture(t, base("checks_onefail_running.json", "status_empty.json"), prT, "checks-fail", WaitCtx{})
	wantMet(t, r, true, "PR #7", "a check failed on c208475", "Run Tests shard 3/8 (failure): https://github.com/synth-org/synth-repo/actions/runs/900/job/102",
		"gh run view --job 102 --log-failed -R synth-org/synth-repo", "still running: web", "https://github.com/synth-org/synth-repo/pull/7")
	wantMet(t, evalFixture(t, base("checks_onefail_running.json", "status_empty.json"), prT, "checks-done", WaitCtx{}), false)

	// All done: counts, every failure with its link, check runs and statuses both.
	r = evalFixture(t, base("checks_done.json", "status_failure.json"), prT, "checks-done", WaitCtx{})
	wantMet(t, r, true, "checks done on c208475, 3 failed", "1 passed, 3 failed, 1 skipped", "web (timed_out)",
		"ci/synth-deploy (failure): https://ci.synth.example/build/1", "Run Tests shard 3/8 (failure)")
	// Green: neutral counts as skipped, not failed; checks-fail reports done too.
	r = evalFixture(t, base("checks_green.json", "status_empty.json"), prT, "checks-fail", WaitCtx{})
	wantMet(t, r, true, "all passed", "1 passed, 1 skipped")
	if strings.Contains(r.Text, "failed:") {
		t.Fatalf("green run lists failures:\n%s", r.Text)
	}

	// No checks at all: wait out the grace (CI may not have started), then report it.
	wantMet(t, evalFixture(t, base("checks_none.json", "status_empty.json"), prT, "checks-done", WaitCtx{Now: waitStart.Add(time.Minute)}), false)
	wantMet(t, evalFixture(t, base("checks_none.json", "status_empty.json"), prT, "checks-done", WaitCtx{Now: waitStart.Add(NoChecksGrace)}), true, "no checks reported")
}

func TestEvalMergedClosedHead(t *testing.T) {
	routes := func(pull string) map[string]string {
		return prRoutes(pull, "checks_green.json", "status_empty.json", "reviews.json", "issue_comments.json", "review_comments.json")
	}
	wantMet(t, evalFixture(t, routes("pull_open.json"), prT, "merged", WaitCtx{}), false)
	wantMet(t, evalFixture(t, routes("pull_merged.json"), prT, "merged", WaitCtx{}), true, "merged by synth-owner at 11:25 AM PT Nov 6", "merge commit 4eb2ecd")
	wantMet(t, evalFixture(t, routes("pull_merged.json"), prT, "closed", WaitCtx{}), true, "merged by synth-owner")
	// Closed without merging ends a merged wait too (it never will merge).
	wantMet(t, evalFixture(t, routes("pull_closed.json"), prT, "merged", WaitCtx{}), true, "closed without merging at 12:00 PM PT Nov 6")
	wantMet(t, evalFixture(t, routes("pull_closed.json"), prT, "closed", WaitCtx{}), true, "closed without merging")

	wantMet(t, evalFixture(t, routes("pull_open.json"), prT, "head-changed", WaitCtx{BaseHead: "c208475aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}), false)
	wantMet(t, evalFixture(t, routes("pull_open.json"), prT, "head-changed", WaitCtx{BaseHead: "1111111bbbb"}), true, "head changed from 1111111 to c208475")
}

func TestEvalReviewsAndComments(t *testing.T) {
	routes := prRoutes("pull_open.json", "checks_green.json", "status_empty.json", "reviews.json", "issue_comments.json", "review_comments.json")
	// A review on the current head (submitted after the wait began), with the
	// reviewer, state, link, line comments and an excerpt; PENDING drafts never count.
	r := evalFixture(t, routes, prT, "review-from:synth-reviewer", WaitCtx{})
	wantMet(t, r, true, "review from synth-reviewer (changes requested) on c208475", "pullrequestreview-2", "1 line comment(s)", "> Please rename the widget.")
	// gemini-code-assist matches gemini-code-assist[bot], but its only review
	// is on an old head from before the wait: not met.
	wantMet(t, evalFixture(t, routes, prT, "review-from:gemini-code-assist", WaitCtx{}), false)
	// Started before it was submitted, the same review counts.
	wantMet(t, evalFixture(t, routes, prT, "review-from:gemini-code-assist", WaitCtx{Start: waitStart.Add(-4 * time.Hour)}), true,
		"review from gemini-code-assist[bot] (commented)", "> ## Summary of Changes")
	// A bot that comments instead of reviewing (a guideline review).
	wantMet(t, evalFixture(t, routes, prT, "review-from:claude[bot]", WaitCtx{}), true, "comment from claude[bot]", "> Guideline review: looks good.")
	wantMet(t, evalFixture(t, routes, prT, "review-from:@synth-nobody", WaitCtx{}), false)

	// comment: the first new comment of any kind; old ones do not count.
	wantMet(t, evalFixture(t, routes, prT, "comment", WaitCtx{}), true, "new comment from claude[bot]", "2 more new comment(s)")
	wantMet(t, evalFixture(t, routes, prT, "comment:synth-reviewer", WaitCtx{}), true, "new line comment on widget.go from synth-reviewer", "> nit: typo")
	wantMet(t, evalFixture(t, routes, prT, "comment:synth-author", WaitCtx{}), false)
}

func TestEvalRun(t *testing.T) {
	routes := prRoutes("pull_open.json", "checks_green.json", "status_empty.json", "reviews.json", "issue_comments.json", "review_comments.json")
	wantMet(t, evalFixture(t, routes, runT, "done", WaitCtx{}), false)
	wantMet(t, evalFixture(t, routes, runT, "failed", WaitCtx{}), true, "a job failed while the run is still in_progress", "Run Tests shard 3/8 (failure, step \"Run tests\")",
		"gh run view --job 102 --log-failed -R synth-org/synth-repo")
	routes[prBase+"actions/runs/900"] = "run_failed.json"
	routes[prBase+"actions/runs/900/jobs?per_page=100&filter=latest"] = "jobs_onefail.json"
	wantMet(t, evalFixture(t, routes, runT, "done", WaitCtx{}), true, `Run "synthetic: add widget" 900`, "completed: failure", "https://github.com/synth-org/synth-repo/actions/runs/900", "failed jobs:")
}

func TestParse(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/acme/widgets/pull/172":       "acme/widgets#172",
		"https://github.com/acme/widgets/pull/172/files": "acme/widgets#172",
		"github.com/o/r/pull/3":                               "o/r#3",
		"o/r.go#12":                                           "o/r.go#12",
	} {
		got, err := ParsePR(in)
		if err != nil || got.String() != want {
			t.Errorf("ParsePR(%q) = %v, %v; want %s", in, got, err, want)
		}
	}
	for _, bad := range []string{"#12", "o/r", "https://gitlab.com/o/r/pull/1", "o/r#0", "https://github.com/o/r/issues/1"} {
		if _, err := ParsePR(bad); err == nil {
			t.Errorf("ParsePR(%q) accepted", bad)
		}
	}
	for in, want := range map[string]string{
		"https://github.com/o/r/actions/runs/41266069739": "o/r run 41266069739",
		"https://github.com/o/r/actions/runs/5/job/6":     "o/r run 5",
		"o/r 77": "o/r run 77",
		"o/r/77": "o/r run 77",
	} {
		got, err := ParseRun(strings.Fields(in))
		if err != nil || got.String() != want {
			t.Errorf("ParseRun(%q) = %v, %v; want %s", in, got, err, want)
		}
	}
	for _, s := range []string{"checks-done", "review-from:gemini-code-assist[bot]", "comment", "comment:x", "merged"} {
		if _, err := ParseCond(KindPR, s); err != nil {
			t.Errorf("ParseCond(%q): %v", s, err)
		}
	}
	for _, s := range []string{"review-from", "merged:x", "done", "checks"} {
		if _, err := ParseCond(KindPR, s); err == nil {
			t.Errorf("ParseCond(pr, %q) accepted", s)
		}
	}
	if _, err := ParseCond(KindRun, "checks-done"); err == nil {
		t.Error("run accepted checks-done")
	}
}

// Conditional requests: the second fetch of unchanged data sends
// If-None-Match and gets 304s (free against the rate limit), with the same
// snapshot; a changed answer is fetched in full.
func TestConditionalRequests(t *testing.T) {
	f := newFakeGitHub(t, prRoutes("pull_open.json", "checks_running.json", "status_empty.json", "reviews.json", "issue_comments.json", "review_comments.json"))
	cl := f.client(t.TempDir())
	a, err := FetchPR(context.Background(), cl, prT)
	if err != nil {
		t.Fatal(err)
	}
	if f.full.Load() != 7 || f.not.Load() != 0 { // 6 PR answers + the head's Actions runs
		t.Fatalf("first fetch: %d full, %d not modified", f.full.Load(), f.not.Load())
	}
	b, err := FetchPR(context.Background(), cl, prT)
	if err != nil {
		t.Fatal(err)
	}
	if f.full.Load() != 7 || f.not.Load() != 7 || cl.NotModified != 7 {
		t.Fatalf("second fetch: %d full, %d not modified", f.full.Load(), f.not.Load())
	}
	if len(b.CheckRuns) != len(a.CheckRuns) || b.Pull.Head.SHA != a.Pull.Head.SHA || len(b.Reviews) != 3 {
		t.Fatalf("304 answers lost data: %+v", b)
	}
	f.set(prBase+"commits/c208475aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/check-runs?per_page=100", "checks_done.json")
	c, err := FetchPR(context.Background(), cl, prT)
	if err != nil {
		t.Fatal(err)
	}
	if f.full.Load() != 8 || Checks(c).Pending != 0 {
		t.Fatalf("changed checks: %d full, pending %d", f.full.Load(), Checks(c).Pending)
	}
	if left, limit := cl.RateLeft(); left != 4900 || limit != 5000 {
		t.Fatalf("rate %d/%d", left, limit)
	}
	// Errors: bad auth and not found need a person; a rate limit is transient.
	bad := &Client{Base: f.srv.URL, Token: func(context.Context) (string, error) { return "wrong", nil }}
	if _, err := bad.Get(context.Background(), prBase+"pulls/7"); !Permanent(err) {
		t.Fatalf("401 permanent: %v", err)
	}
	if _, err := cl.Get(context.Background(), prBase+"pulls/8"); !Permanent(err) {
		t.Fatalf("404 permanent: %v", err)
	}
	f.set(prBase+"pulls/7", "!ratelimit")
	var he *HTTPError
	if _, err := cl.Get(context.Background(), prBase+"pulls/7"); Permanent(err) || !errors.As(err, &he) || !he.RateLimited {
		t.Fatalf("rate limit transient: %v", err)
	}
}

// Shared poller: many waiters on one target, in parallel and in two
// Poller values (two processes), cost one fetch per interval; the
// interval adapts to the state.
func TestPollerSharesOneFetch(t *testing.T) {
	dir := t.TempDir()
	var fetches atomic.Int64
	now := waitStart
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	state := atomic.Value{}
	state.Store("running")
	fetch := func(ctx context.Context, tg Target) (Snapshot, error) {
		fetches.Add(1)
		time.Sleep(20 * time.Millisecond) // waiters pile up on the lock meanwhile
		s := &PRSnapshot{}
		s.Pull.State = "open"
		switch state.Load() {
		case "running":
			s.CheckRuns = []CheckRun{{Name: "ci", Status: "in_progress"}}
		case "idle":
			s.CheckRuns = []CheckRun{{Name: "ci", Status: "completed", Conclusion: "success"}}
		case "merged":
			s.Pull.State, s.Pull.Merged = "closed", true
		}
		return Snapshot{Target: tg, PR: s}, nil
	}
	pollers := []*Poller{{Dir: dir, Now: clock, Fetch: fetch}, {Dir: dir, Now: clock, Fetch: fetch}}
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := pollers[i%2].Poll(context.Background(), prT); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := fetches.Load(); n != 1 {
		t.Fatalf("20 waiters fetched %d times, want 1", n)
	}
	_, info, _ := pollers[0].Poll(context.Background(), prT)
	if info.Fetched || info.Next.Sub(now) != IntervalActive {
		t.Fatalf("running checks: next in %s (fetched %v)", info.Next.Sub(now), info.Fetched)
	}
	// Due again: one more fetch. After 10 minutes running it slows to 60 s.
	advance(IntervalActive)
	if _, info, _ = pollers[1].Poll(context.Background(), prT); !info.Fetched || fetches.Load() != 2 {
		t.Fatalf("due poll: %+v, fetches %d", info, fetches.Load())
	}
	advance(activeFastFor)
	if _, info, _ = pollers[1].Poll(context.Background(), prT); info.Next.Sub(clock()) != IntervalActiveLate {
		t.Fatalf("long-running checks: next in %s", info.Next.Sub(clock()))
	}
	state.Store("idle")
	advance(IntervalActiveLate)
	if _, info, _ = pollers[0].Poll(context.Background(), prT); info.Next.Sub(clock()) != IntervalIdle {
		t.Fatalf("idle PR: next in %s", info.Next.Sub(clock()))
	}
	state.Store("merged")
	advance(IntervalIdle)
	if _, info, _ = pollers[0].Poll(context.Background(), prT); info.Next.Sub(clock()) != IntervalTerminal {
		t.Fatalf("merged PR: next in %s", info.Next.Sub(clock()))
	}
	// A different target has its own snapshot.
	other := prT
	other.Number = 8
	if _, info, _ = pollers[0].Poll(context.Background(), other); !info.Fetched {
		t.Fatal("another PR reused PR 7's snapshot")
	}
	// An error is shared too until it is retried, and says whether it needs a person.
	failing := &Poller{Dir: dir, Now: clock, Fetch: func(context.Context, Target) (Snapshot, error) {
		fetches.Add(1)
		return Snapshot{}, &HTTPError{Status: 404, URL: "synth", Message: "Not Found"}
	}}
	third := prT
	third.Number = 9
	before := fetches.Load()
	for range 3 {
		_, _, err := failing.Poll(context.Background(), third)
		var pe *PollError
		if !errors.As(err, &pe) || !pe.Permanent {
			t.Fatalf("shared error: %v", err)
		}
	}
	if fetches.Load()-before != 1 {
		t.Fatalf("error refetched %d times", fetches.Load()-before)
	}
}

// The wait loop: one message when met; a relaunch (after a restart or
// reboot) keeps the original start, so the timeout counts from it, and a
// wait that already ended ends again without a second message.
func TestWaiterRestartAndTimeout(t *testing.T) {
	dir := t.TempDir()
	var saved WaitState
	var savedOK bool
	load := func() (WaitState, bool) { return saved, savedOK }
	save := func(st WaitState) { saved, savedOK = st, true }
	now := waitStart
	running := Snapshot{Target: prT, PR: &PRSnapshot{Pull: Pull{State: "open", Number: 7}, CheckRuns: []CheckRun{{Name: "ci", Status: "in_progress"}}}}
	snap := running
	poller := &Poller{Dir: dir, Now: func() time.Time { return now }, Fetch: func(context.Context, Target) (Snapshot, error) { return snap, nil }}
	var sent []string
	newWaiter := func(ctx context.Context, cancelAfter int) *Waiter {
		sleeps := 0
		return &Waiter{Target: prT, Cond: Cond{Name: "checks-done"}, Timeout: time.Hour, Poller: poller, Now: func() time.Time { return now },
			Load: load, Save: save,
			Sleep: func(ctx context.Context, d time.Duration) error {
				sleeps++
				if cancelAfter > 0 && sleeps >= cancelAfter {
					return context.Canceled
				}
				now = now.Add(d)
				return nil
			},
			Deliver: func(_ context.Context, text string) error { sent = append(sent, text); return nil }}
	}
	// First run: dies (a reboot) after a few polls 30 minutes in.
	w := newWaiter(context.Background(), 60)
	if _, err := w.Run(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("first run: %v", err)
	}
	if len(sent) != 0 || !saved.Start.Equal(waitStart) {
		t.Fatalf("before the restart: sent %v, state %+v", sent, saved)
	}
	// Relaunched: still running; the timeout counts from the original start.
	out, err := newWaiter(context.Background(), 0).Run(context.Background())
	if err != nil || out.Kind != "timeout" || len(sent) != 1 || !strings.Contains(sent[0], "timed out after 1h0m0s") || now.Sub(waitStart) > time.Hour+time.Minute {
		t.Fatalf("relaunch: %+v %v sent %q after %s", out, err, sent, now.Sub(waitStart))
	}
	// Resumed again by hand: it already ended, nothing more is sent.
	if out, err := newWaiter(context.Background(), 0).Run(context.Background()); err != nil || out.Kind != "timeout" || len(sent) != 1 {
		t.Fatalf("ended wait relaunched: %+v %v, sent %d", out, err, len(sent))
	}

	// A met condition delivers once; a failed delivery is retried, not lost.
	saved, savedOK = WaitState{}, false
	snap = Snapshot{Target: prT, PR: &PRSnapshot{Pull: Pull{State: "open", Number: 7}, CheckRuns: []CheckRun{{Name: "ci", Status: "completed", Conclusion: "success"}}}}
	sent = nil
	fails := 1
	w = newWaiter(context.Background(), 0)
	w.Deliver = func(_ context.Context, text string) error {
		if fails > 0 {
			fails--
			return errors.New("synthetic hub outage")
		}
		sent = append(sent, text)
		return nil
	}
	now = now.Add(IntervalTerminal) // past the snapshot's next
	out, err = w.Run(context.Background())
	if err != nil || out.Kind != "met" || len(sent) != 1 || !strings.Contains(sent[0], "[pyre wait gh-pr synth-org/synth-repo#7 --until checks-done] condition met") {
		t.Fatalf("met: %+v %v %q", out, err, sent)
	}

	// A permanent error ends the wait at once with an error message.
	saved, savedOK = WaitState{}, false
	sent = nil
	errPoller := &Poller{Dir: t.TempDir(), Now: func() time.Time { return now }, Fetch: func(context.Context, Target) (Snapshot, error) {
		return Snapshot{}, &HTTPError{Status: 404, URL: "https://api.github.com/repos/synth-org/synth-repo/pulls/7", Message: "Not Found"}
	}}
	w = newWaiter(context.Background(), 0)
	w.Poller = errPoller
	if out, err := w.Run(context.Background()); err != nil || out.Kind != "error" || len(sent) != 1 || !strings.Contains(sent[0], "GitHub 404") {
		t.Fatalf("permanent error: %+v %v %q", out, err, sent)
	}
	// A transient one is retried until ErrorGrace, then reported.
	saved, savedOK = WaitState{}, false
	sent = nil
	errPoller = &Poller{Dir: t.TempDir(), Now: errPoller.Now}
	errPoller.Fetch = func(context.Context, Target) (Snapshot, error) {
		return Snapshot{}, &HTTPError{Status: 403, RateLimited: true, URL: "synth", Message: "API rate limit exceeded"}
	}
	w = newWaiter(context.Background(), 0)
	w.Poller, w.ErrorGrace = errPoller, 5*time.Minute
	start := now
	if out, err := w.Run(context.Background()); err != nil || out.Kind != "error" || len(sent) != 1 || !strings.Contains(sent[0], "GitHub kept failing for") || now.Sub(start) < 5*time.Minute {
		t.Fatalf("transient error: %+v %v %q after %s", out, err, sent, now.Sub(start))
	}
}

// A rerun accepted while GitHub still lists the old attempt's failed
// check run (Nov 6: a wait registered right after `gh run rerun --failed`
// fired on the old failure): the checks are pending until attempt 2 ends.
func TestEvalChecksRerun(t *testing.T) {
	routes := func(checks, runs string) map[string]string {
		r := prRoutes("pull_open.json", checks, "status_empty.json", "reviews.json", "issue_comments.json", "review_comments.json")
		r[prBase+"actions/runs?head_sha=c208475aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa&per_page=100"] = runs
		return r
	}
	for _, cond := range []string{"checks-done", "checks-fail"} {
		r := evalFixture(t, routes("checks_rerun_stale.json", "runs_head_rerun_queued.json"), prT, cond, WaitCtx{})
		wantMet(t, r, false)
		if !strings.Contains(r.State, "2 running") {
			t.Fatalf("%s: stale results not pending: %s", cond, r.State)
		}
	}
	// Attempt 2 done: reported with its attempt number.
	r := evalFixture(t, routes("checks_rerun_done.json", "runs_head_rerun_done.json"), prT, "checks-done", WaitCtx{})
	wantMet(t, r, true, "checks done on c208475, 1 failed", "reruns: CI attempt 2", "Run Tests shard 3/8 (failure, attempt 2): https://github.com/synth-org/synth-repo/actions/runs/901/job/105")
	// A fresh failure of the running attempt still fails fast.
	wantMet(t, evalFixture(t, routes("checks_rerun_done.json", "runs_head_rerun_queued.json"), prT, "checks-fail", WaitCtx{}), true, "attempt 2")

	// Before GitHub shows attempt 2 at all, only --after (or the settle
	// confirmation, TestWaiterSettles) holds the old failure back.
	old := routes("checks_rerun_stale.json", "runs_head_attempt1_done.json")
	wantMet(t, evalFixture(t, old, prT, "checks-fail", WaitCtx{}), true, "Run Tests shard 3/8 (failure)")
	after := time.Date(2026, 11, 6, 23, 10, 0, 0, time.UTC)
	wantMet(t, evalFixture(t, old, prT, "checks-fail", WaitCtx{After: after}), false)
	wantMet(t, evalFixture(t, old, prT, "checks-done", WaitCtx{After: after}), false)
	// The rerun's new failure is past the barrier and counts.
	wantMet(t, evalFixture(t, routes("checks_rerun_done.json", "runs_head_rerun_done.json"), prT, "checks-done", WaitCtx{After: after}), true, "1 failed")

	// No access to the runs (404) still evaluates the checks by themselves.
	noActions := routes("checks_done.json", "!404")
	noActions[prBase+"actions/runs/900"] = "!404"
	wantMet(t, evalFixture(t, noActions, prT, "checks-done", WaitCtx{}), true, "2 failed")
}

// checks-done is reported only after two polls at least Settle apart see
// the same state; the second is a fresh fetch, not the shared snapshot.
func TestWaiterSettles(t *testing.T) {
	now := waitStart
	ci := func(runStatus string, attempt int, conclusion string, completed time.Time) Snapshot {
		started := waitStart.Add(-time.Hour)
		if attempt > 1 {
			started = waitStart.Add(time.Duration(attempt) * time.Second)
		}
		return Snapshot{Target: prT, PR: &PRSnapshot{Pull: Pull{State: "open", Number: 7},
			CheckRuns: []CheckRun{{ID: 102, Name: "tests", Status: "completed", Conclusion: conclusion, CompletedAt: &completed,
				HTMLURL: "https://github.com/synth-org/synth-repo/actions/runs/901/job/102", App: struct {
					Slug string `json:"slug"`
				}{"github-actions"}}},
			Runs: []Run{{ID: 901, Name: "CI", Status: runStatus, Attempt: attempt, StartedAt: started}}}}
	}
	oldFail := ci("completed", 1, "failure", waitStart.Add(-time.Minute))
	// GitHub's view after the rerun was accepted: first poll still shows
	// attempt 1's failure, the confirmation poll shows attempt 2 queued,
	// then attempt 2 passes.
	seq := []Snapshot{oldFail, ci("queued", 2, "failure", waitStart.Add(-time.Minute)), ci("completed", 2, "success", waitStart.Add(5*time.Minute))}
	var fetchedAt []time.Time
	poller := &Poller{Dir: t.TempDir(), Now: func() time.Time { return now }, Fetch: func(context.Context, Target) (Snapshot, error) {
		fetchedAt = append(fetchedAt, now)
		i := min(len(fetchedAt)-1, len(seq)-1)
		return seq[i], nil
	}}
	var sent []string
	w := &Waiter{Target: prT, Cond: Cond{Name: "checks-done"}, Timeout: time.Hour, Poller: poller, Now: func() time.Time { return now },
		Sleep:   func(_ context.Context, d time.Duration) error { now = now.Add(d); return nil },
		Deliver: func(_ context.Context, text string) error { sent = append(sent, text); return nil }}
	out, err := w.Run(context.Background())
	if err != nil || out.Kind != "met" || len(sent) != 1 {
		t.Fatalf("rerun wait: %+v %v %q", out, err, sent)
	}
	if strings.Contains(sent[0], "failed") || !strings.Contains(sent[0], "all passed") || !strings.Contains(sent[0], "reruns: CI attempt 2") {
		t.Fatalf("reported the old attempt, or no attempt number:\n%s", sent[0])
	}
	if d := fetchedAt[1].Sub(fetchedAt[0]); d < SettleDelay || d > SettleDelay+5*time.Second {
		t.Fatalf("confirmation poll %s after the first, want ~%s", d, SettleDelay)
	}

	// A state that holds is reported on the second look, ~20 s in, even
	// though the shared snapshot is not due for minutes.
	now, fetchedAt, sent = waitStart, nil, nil
	seq = []Snapshot{ci("completed", 1, "success", waitStart.Add(-time.Minute))}
	poller.Dir = t.TempDir()
	out, err = w.Run(context.Background())
	if err != nil || out.Kind != "met" || len(sent) != 1 || len(fetchedAt) != 2 || fetchedAt[1].Sub(fetchedAt[0]) != SettleDelay {
		t.Fatalf("settled wait: %+v %v sent %d, fetches at %v", out, err, len(sent), fetchedAt)
	}

	// A shared snapshot fetched a minute before this waiter registered
	// (another wait on the PR) still gets a confirmation fetch a full
	// settle after this waiter first saw it.
	poller.Dir, fetchedAt, sent = t.TempDir(), nil, nil
	now = waitStart.Add(-time.Minute)
	seq = []Snapshot{oldFail}
	if _, _, err := poller.Poll(context.Background(), prT); err != nil {
		t.Fatal(err)
	}
	now = waitStart
	seq = []Snapshot{ci("queued", 2, "failure", waitStart.Add(-time.Minute)), ci("completed", 2, "success", waitStart.Add(5*time.Minute))}
	fetchedAt = nil
	out, err = w.Run(context.Background())
	if err != nil || out.Kind != "met" || len(sent) != 1 || !strings.Contains(sent[0], "all passed") || fetchedAt[0].Sub(waitStart) < SettleDelay {
		t.Fatalf("cached old failure: %+v %v %q, fetches at %v", out, err, sent, fetchedAt)
	}

	// A failing confirmation fetch keeps the error backoff (no 1 s retries).
	poller.Dir, fetchedAt, sent = t.TempDir(), nil, nil
	now = waitStart
	calls := 0
	poller.Fetch = func(context.Context, Target) (Snapshot, error) {
		calls++
		fetchedAt = append(fetchedAt, now)
		if calls == 1 || calls > 3 {
			return oldFail, nil
		}
		return Snapshot{}, &HTTPError{Status: 502, URL: "synth", Message: "Bad Gateway"}
	}
	out, err = w.Run(context.Background())
	if err != nil || out.Kind != "met" || calls != 4 {
		t.Fatalf("error during confirmation: %+v %v, %d fetches at %v", out, err, calls, fetchedAt)
	}
	if d := fetchedAt[2].Sub(fetchedAt[1]); d < IntervalError {
		t.Fatalf("retried %s after an error, want the %s backoff", d, IntervalError)
	}
}
