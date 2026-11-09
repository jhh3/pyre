package ghwait

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/ghguard"
)

// NoChecksGrace is how long checks-done waits for a head with no checks
// at all before it reports "no checks" (CI may not have started yet).
const NoChecksGrace = 3 * time.Minute

// WaitCtx is what a wait knows beyond the snapshot.
type WaitCtx struct {
	Start    time.Time // when the wait was created
	Now      time.Time
	BaseHead string // the PR head when the wait started (head-changed)
	// After (--after): failures that finished before it are being rerun,
	// so checks-done/checks-fail treat them as pending.
	After time.Time
}

// Result is a condition's evaluation.
type Result struct {
	Met   bool
	Text  string // the message body once met
	State string // one line: the current state (logs, timeout message)
	// Settle, when set, is what must look the same on a second poll at
	// least SettleDelay later before a met result is reported (checks-done
	// and checks-fail: GitHub can list an old attempt's result for a while
	// after a rerun was accepted).
	Settle string
}

// CheckSummary counts a head's check runs and commit statuses.
type CheckSummary struct {
	Total, Passed, Failed, Pending, Skipped int
	Failures                                []Failure
	PendingNames                            []string
}

// Failure is a failed check with where to read it.
type Failure struct {
	Name, Conclusion, URL, LogCmd string
	Attempt                       int // the Actions run attempt, when known
}

func failedConclusion(c string) bool {
	switch c {
	case "failure", "timed_out", "cancelled", "action_required", "startup_failure", "stale":
		return true
	}
	return false
}

// Checks summarizes s's head: check runs (latest per name) and statuses.
func Checks(s *PRSnapshot) CheckSummary { return ChecksAfter(s, time.Time{}) }

// ChecksAfter is Checks, rerun-aware. A check run of an Actions run whose
// latest attempt is still queued or running counts as pending when it
// finished before that attempt started (GitHub still lists the old
// attempt's result until the rerun replaces it), and such a run with no
// pending check yet counts as one pending check itself. With after set,
// a failure that finished before it is pending too (it is being rerun).
func ChecksAfter(s *PRSnapshot, after time.Time) CheckSummary {
	var c CheckSummary
	repo := strings.TrimPrefix(strings.TrimSuffix(s.Pull.HTMLURL, fmt.Sprintf("/pull/%d", s.Pull.Number)), "https://github.com/")
	runs := map[int64]Run{}
	for _, r := range s.Runs {
		runs[r.ID] = r
	}
	pendingIn := map[int64]bool{}
	for _, r := range s.CheckRuns {
		c.Total++
		run, inRun := runs[r.RunID()]
		done := r.Status == "completed"
		stale := done && inRun && run.Status != "completed" && (r.CompletedAt == nil || r.CompletedAt.Before(run.StartedAt))
		rerun := done && failedConclusion(r.Conclusion) && !after.IsZero() && r.CompletedAt != nil && r.CompletedAt.Before(after)
		switch {
		case !done || stale || rerun:
			c.Pending++
			name := r.Name
			if inRun && run.Attempt > 1 {
				name += fmt.Sprintf(" (attempt %d)", run.Attempt)
			}
			c.PendingNames = append(c.PendingNames, name)
			if inRun {
				pendingIn[run.ID] = true
			}
		case failedConclusion(r.Conclusion):
			c.Failed++
			f := Failure{Name: r.Name, Conclusion: r.Conclusion, URL: firstNonEmpty(r.HTMLURL, r.DetailsURL)}
			if inRun {
				f.Attempt = run.Attempt
			}
			if r.App.Slug == "github-actions" && repo != "" {
				f.LogCmd = fmt.Sprintf("gh run view --job %d --log-failed -R %s", r.ID, repo)
			}
			c.Failures = append(c.Failures, f)
		case r.Conclusion == "skipped" || r.Conclusion == "neutral":
			c.Skipped++
		default:
			c.Passed++
		}
	}
	for _, run := range s.Runs {
		if run.Status != "completed" && !pendingIn[run.ID] {
			c.Total++
			c.Pending++
			c.PendingNames = append(c.PendingNames, fmt.Sprintf("%s (attempt %d, %s)", firstNonEmpty(run.Name, fmt.Sprint(run.ID)), max(run.Attempt, 1), run.Status))
		}
	}
	for _, st := range s.Statuses {
		c.Total++
		failed := st.State == "failure" || st.State == "error"
		if failed && !after.IsZero() && !st.UpdatedAt.IsZero() && st.UpdatedAt.Before(after) {
			st.State = "pending" // being rerun
		}
		switch st.State {
		case "pending":
			c.Pending++
			c.PendingNames = append(c.PendingNames, st.Context)
		case "failure", "error":
			c.Failed++
			c.Failures = append(c.Failures, Failure{Name: st.Context, Conclusion: st.State, URL: st.TargetURL})
		default:
			c.Passed++
		}
	}
	sort.SliceStable(c.Failures, func(i, j int) bool { return c.Failures[i].Name < c.Failures[j].Name })
	return c
}

// settleKey is the state a settle confirmation compares.
func (c CheckSummary) settleKey(s *PRSnapshot) string {
	k := fmt.Sprintf("%s %s %s", s.Pull.Head.SHA, c.counts(), reruns(s))
	for _, f := range c.Failures {
		k += fmt.Sprintf("|%s %s %d", f.Name, f.Conclusion, f.Attempt)
	}
	return k
}

// reruns names the Actions runs on the head that are past attempt 1.
func reruns(s *PRSnapshot) string {
	var p []string
	for _, r := range s.Runs {
		if r.Attempt > 1 {
			p = append(p, fmt.Sprintf("%s attempt %d", firstNonEmpty(r.Name, fmt.Sprint(r.ID)), r.Attempt))
		}
	}
	if len(p) == 0 {
		return ""
	}
	sort.Strings(p)
	return "reruns: " + strings.Join(p, ", ") + "\n"
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// counts renders "38 passed, 2 failed, 1 skipped, 3 running".
func (c CheckSummary) counts() string {
	var p []string
	add := func(n int, w string) {
		if n > 0 {
			p = append(p, fmt.Sprintf("%d %s", n, w))
		}
	}
	add(c.Passed, "passed")
	add(c.Failed, "failed")
	add(c.Skipped, "skipped")
	add(c.Pending, "running")
	if len(p) == 0 {
		return "no checks"
	}
	return strings.Join(p, ", ")
}

func (c CheckSummary) failureLines() string {
	var b strings.Builder
	for i, f := range c.Failures {
		if i == 15 {
			fmt.Fprintf(&b, "- … and %d more\n", len(c.Failures)-i)
			break
		}
		att := ""
		if f.Attempt > 1 {
			att = fmt.Sprintf(", attempt %d", f.Attempt)
		}
		fmt.Fprintf(&b, "- %s (%s%s): %s\n", f.Name, f.Conclusion, att, orNone(f.URL))
		if f.LogCmd != "" {
			fmt.Fprintf(&b, "  log: `%s`\n", f.LogCmd)
		}
	}
	return b.String()
}

func orNone(s string) string {
	if s == "" {
		return "(no link)"
	}
	return s
}

func prName(s *PRSnapshot) string {
	return fmt.Sprintf("PR #%d \"%s\"", s.Pull.Number, clip(s.Pull.Title, 80))
}

// prState is the PR's one-line state.
func prState(s *PRSnapshot) string {
	st := s.Pull.State
	if s.Pull.Merged {
		st = "merged"
	}
	return fmt.Sprintf("%s, head %s, checks: %s", st, short(s.Pull.Head.SHA), Checks(s).counts())
}

// Eval evaluates cond on snap.
func Eval(cond Cond, snap Snapshot, w WaitCtx) Result {
	if snap.Run != nil {
		return evalRun(cond, snap.Run, snap.Target)
	}
	if snap.PR == nil {
		return Result{State: "no data yet"}
	}
	s := snap.PR
	head := fmt.Sprintf("%s\n", s.Pull.HTMLURL)
	res := Result{State: prState(s)}
	switch cond.Name {
	case "checks-done", "checks-fail":
		c := ChecksAfter(s, w.After)
		if cond.Name == "checks-fail" && c.Failed > 0 {
			res.Met, res.Settle = true, c.settleKey(s)
			res.Text = fmt.Sprintf("%s: a check failed on %s (%s).\n%s", prName(s), short(s.Pull.Head.SHA), c.counts(), head) + reruns(s) + "failed:\n" + c.failureLines()
			if c.Pending > 0 {
				res.Text += fmt.Sprintf("still running: %s\n", clip(strings.Join(c.PendingNames, ", "), 300))
			}
			return res
		}
		if c.Pending > 0 {
			return res
		}
		if c.Total == 0 {
			if s.Pull.State != "closed" && w.Now.Sub(w.Start) < NoChecksGrace {
				return res
			}
			res.Met, res.Settle = true, "no checks "+s.Pull.Head.SHA
			res.Text = fmt.Sprintf("%s: no checks reported on head %s (waited %s).\n%s", prName(s), short(s.Pull.Head.SHA), w.Now.Sub(w.Start).Round(time.Second), head)
			return res
		}
		res.Met, res.Settle = true, c.settleKey(s)
		verdict := "all passed"
		if c.Failed > 0 {
			verdict = fmt.Sprintf("%d failed", c.Failed)
		}
		res.Text = fmt.Sprintf("%s: checks done on %s, %s (%s).\n%s", prName(s), short(s.Pull.Head.SHA), verdict, c.counts(), head) + reruns(s)
		if c.Failed > 0 {
			res.Text += "failed:\n" + c.failureLines()
		}
		return res
	case "merged", "closed":
		if s.Pull.State != "closed" && !s.Pull.Merged {
			return res
		}
		res.Met = true
		switch {
		case s.Pull.Merged:
			by, at := "", ""
			if s.Pull.MergedBy != nil {
				by = " by " + s.Pull.MergedBy.Login
			}
			if s.Pull.MergedAt != nil {
				at = " at " + ghguard.Clock(*s.Pull.MergedAt) + " " + s.Pull.MergedAt.In(pt()).Format("Jan 2")
			}
			res.Text = fmt.Sprintf("%s merged%s%s (merge commit %s).\n%s", prName(s), by, at, short(s.Pull.MergeSHA), head)
		default:
			at := ""
			if s.Pull.ClosedAt != nil {
				at = " at " + ghguard.Clock(*s.Pull.ClosedAt) + " " + s.Pull.ClosedAt.In(pt()).Format("Jan 2")
			}
			res.Text = fmt.Sprintf("%s was closed without merging%s.\n%s", prName(s), at, head)
		}
		return res
	case "head-changed":
		if w.BaseHead == "" || s.Pull.Head.SHA == w.BaseHead {
			if s.Pull.State == "closed" {
				res.Met = true
				res.Text = fmt.Sprintf("%s was %s before its head changed (still %s).\n%s", prName(s), map[bool]string{true: "merged", false: "closed"}[s.Pull.Merged], short(s.Pull.Head.SHA), head)
			}
			return res
		}
		res.Met = true
		res.Text = fmt.Sprintf("%s: head changed from %s to %s.\n%s", prName(s), short(w.BaseHead), short(s.Pull.Head.SHA), head)
		return res
	case "review-from":
		return evalReview(cond, s, w, res)
	case "comment":
		return evalComment(cond, s, w, res)
	}
	res.State = "unknown condition " + cond.String()
	return res
}

// evalReview: a review from cond.Login on the current head or submitted
// since the wait began, or an issue comment by it since then (bots such as
// a guideline reviewer post comments, not reviews).
func evalReview(cond Cond, s *PRSnapshot, w WaitCtx, res Result) Result {
	var best *Review
	for i := range s.Reviews {
		r := &s.Reviews[i]
		if r.State == "PENDING" || !sameLogin(r.User.Login, cond.Login) {
			continue
		}
		if r.CommitID != s.Pull.Head.SHA && r.SubmittedAt.Before(w.Start) {
			continue
		}
		if best == nil || r.SubmittedAt.After(best.SubmittedAt) {
			best = r
		}
	}
	lines := 0
	for _, c := range s.ReviewComments {
		if sameLogin(c.User.Login, cond.Login) && !c.CreatedAt.Before(w.Start) {
			lines++
		}
	}
	if best != nil {
		res.Met = true
		res.Text = fmt.Sprintf("%s: review from %s (%s) on %s at %s.\n%s\n", prName(s), best.User.Login, strings.ToLower(strings.ReplaceAll(best.State, "_", " ")),
			short(best.CommitID), ghguard.Clock(best.SubmittedAt), orNone(best.HTMLURL))
		if lines > 0 {
			res.Text += fmt.Sprintf("%d line comment(s) from %s since the wait began.\n", lines, best.User.Login)
		}
		res.Text += excerpt(best.Body)
		return res
	}
	for _, c := range newest(s.Comments, w.Start, cond.Login) {
		res.Met = true
		res.Text = fmt.Sprintf("%s: comment from %s at %s.\n%s\n", prName(s), c.User.Login, ghguard.Clock(c.CreatedAt), orNone(c.HTMLURL)) + excerpt(c.Body)
		return res
	}
	return res
}

// evalComment: any new comment (issue comment, line comment or a review
// with a body) since the wait began, from cond.Login when set.
func evalComment(cond Cond, s *PRSnapshot, w WaitCtx, res Result) Result {
	type item struct {
		at         time.Time
		who, url   string
		body, kind string
	}
	var all []item
	for _, c := range s.Comments {
		all = append(all, item{c.CreatedAt, c.User.Login, c.HTMLURL, c.Body, "comment"})
	}
	for _, c := range s.ReviewComments {
		k := "line comment"
		if c.Path != "" {
			k += " on " + c.Path
		}
		all = append(all, item{c.CreatedAt, c.User.Login, c.HTMLURL, c.Body, k})
	}
	for _, r := range s.Reviews {
		if r.State != "PENDING" && strings.TrimSpace(r.Body) != "" {
			all = append(all, item{r.SubmittedAt, r.User.Login, r.HTMLURL, r.Body, "review (" + strings.ToLower(r.State) + ")"})
		}
	}
	var hits []item
	for _, it := range all {
		if it.at.Before(w.Start) || (cond.Login != "" && !sameLogin(it.who, cond.Login)) {
			continue
		}
		hits = append(hits, it)
	}
	if len(hits) == 0 {
		return res
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].at.Before(hits[j].at) })
	first := hits[0]
	res.Met = true
	res.Text = fmt.Sprintf("%s: new %s from %s at %s.\n%s\n", prName(s), first.kind, first.who, ghguard.Clock(first.at), orNone(first.url)) + excerpt(first.body)
	if len(hits) > 1 {
		res.Text += fmt.Sprintf("%d more new comment(s) since the wait began.\n", len(hits)-1)
	}
	return res
}

// newest returns the comments by login (any when "") since start, newest first.
func newest(cs []Comment, start time.Time, login string) []Comment {
	var out []Comment
	for _, c := range cs {
		if !c.CreatedAt.Before(start) && (login == "" || sameLogin(c.User.Login, login)) {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

func evalRun(cond Cond, s *RunSnapshot, t Target) Result {
	r := s.Run
	name := fmt.Sprintf("Run %q %d (%s, %s on %s %s)", clip(firstNonEmpty(r.Title, r.Name), 80), r.ID, t.FullRepo(), r.Event, r.HeadBranch, short(r.HeadSHA))
	res := Result{State: fmt.Sprintf("%s %s", r.Status, r.Conclusion)}
	var failed []Job
	pending := 0
	for _, j := range s.Jobs {
		if j.Status != "completed" {
			pending++
		} else if failedConclusion(j.Conclusion) {
			failed = append(failed, j)
		}
	}
	jobLines := func() string {
		var b strings.Builder
		for i, j := range failed {
			if i == 15 {
				fmt.Fprintf(&b, "- … and %d more\n", len(failed)-i)
				break
			}
			step := ""
			for _, st := range j.Steps {
				if failedConclusion(st.Conclusion) {
					step = fmt.Sprintf(", step %q", st.Name)
					break
				}
			}
			fmt.Fprintf(&b, "- %s (%s%s): %s\n  log: `gh run view --job %d --log-failed -R %s`\n", j.Name, j.Conclusion, step, orNone(j.HTMLURL), j.ID, t.FullRepo())
		}
		return b.String()
	}
	url := firstNonEmpty(r.HTMLURL, t.URL()) + "\n"
	if r.Status == "completed" {
		res.Met = true
		res.Text = fmt.Sprintf("%s completed: %s.\n%s", name, firstNonEmpty(r.Conclusion, "no conclusion"), url)
		if len(failed) > 0 {
			res.Text += "failed jobs:\n" + jobLines()
		}
		return res
	}
	if cond.Name == "failed" && len(failed) > 0 {
		res.Met = true
		res.Text = fmt.Sprintf("%s: a job failed while the run is still %s (%d job(s) running).\n%s", name, r.Status, pending, url) + "failed jobs:\n" + jobLines()
	}
	return res
}

// excerpt quotes the start of a comment body.
func excerpt(body string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return ""
	}
	body = clip(body, 600)
	var b strings.Builder
	for _, l := range strings.Split(body, "\n") {
		b.WriteString("> " + l + "\n")
	}
	return b.String()
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}

func pt() *time.Location {
	if l, err := time.LoadLocation("America/Los_Angeles"); err == nil {
		return l
	}
	return time.Local
}
