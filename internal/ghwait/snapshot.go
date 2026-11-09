package ghwait

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"time"
)

// User is a GitHub user reference.
type User struct {
	Login string `json:"login"`
}

// Pull is the part of GET /repos/{o}/{r}/pulls/{n} a wait reads.
type Pull struct {
	Number   int        `json:"number"`
	Title    string     `json:"title"`
	HTMLURL  string     `json:"html_url"`
	State    string     `json:"state"` // open, closed
	Merged   bool       `json:"merged"`
	MergedAt *time.Time `json:"merged_at"`
	ClosedAt *time.Time `json:"closed_at"`
	MergedBy *User      `json:"merged_by"`
	MergeSHA string     `json:"merge_commit_sha"`
	User     User       `json:"user"`
	Head     struct {
		SHA string `json:"sha"`
		Ref string `json:"ref"`
	} `json:"head"`
}

// CheckRun is one check run (GitHub Actions jobs are check runs whose ID
// is the job ID).
type CheckRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`     // queued, in_progress, completed, waiting, requested, pending
	Conclusion string `json:"conclusion"` // success, failure, neutral, cancelled, skipped, timed_out, action_required, stale, startup_failure
	HTMLURL    string `json:"html_url"`
	DetailsURL string `json:"details_url"`
	App        struct {
		Slug string `json:"slug"`
	} `json:"app"`
	Output struct {
		Title string `json:"title"`
	} `json:"output"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

var actionsJobURL = regexp.MustCompile(`/actions/runs/([0-9]+)/job/[0-9]+`)

// RunID is the Actions run a GitHub Actions check run (job) belongs to,
// from its link; 0 for other apps.
func (r CheckRun) RunID() int64 {
	if r.App.Slug != "github-actions" {
		return 0
	}
	for _, u := range []string{r.HTMLURL, r.DetailsURL} {
		if m := actionsJobURL.FindStringSubmatch(u); m != nil {
			id, _ := strconv.ParseInt(m[1], 10, 64)
			return id
		}
	}
	return 0
}

// Status is one commit status (the older statuses API).
type Status struct {
	Context     string    `json:"context"`
	State       string    `json:"state"` // error, failure, pending, success
	TargetURL   string    `json:"target_url"`
	Description string    `json:"description"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Review is one pull request review.
type Review struct {
	ID          int64     `json:"id"`
	User        User      `json:"user"`
	State       string    `json:"state"` // APPROVED, CHANGES_REQUESTED, COMMENTED, DISMISSED, PENDING
	Body        string    `json:"body"`
	SubmittedAt time.Time `json:"submitted_at"`
	CommitID    string    `json:"commit_id"`
	HTMLURL     string    `json:"html_url"`
}

// Comment is an issue comment or a review (line) comment.
type Comment struct {
	ID        int64     `json:"id"`
	User      User      `json:"user"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
	HTMLURL   string    `json:"html_url"`
	Path      string    `json:"path,omitempty"` // review comments only
}

// PRSnapshot is everything a PR wait evaluates, fetched in one poll.
type PRSnapshot struct {
	Pull           Pull       `json:"pull"`
	CheckRuns      []CheckRun `json:"check_runs,omitempty"`
	Statuses       []Status   `json:"statuses,omitempty"`
	Reviews        []Review   `json:"reviews,omitempty"`
	Comments       []Comment  `json:"comments,omitempty"`        // issue comments
	ReviewComments []Comment  `json:"review_comments,omitempty"` // line comments
	// Runs are the Actions runs the head's check runs belong to, at their
	// latest attempt (a rerun makes the old attempt's results stale).
	Runs []Run `json:"runs,omitempty"`
}

// Run is the part of GET /repos/{o}/{r}/actions/runs/{id} a wait reads.
type Run struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	Title      string    `json:"display_title"`
	Status     string    `json:"status"` // queued, in_progress, completed, waiting, requested, pending
	Conclusion string    `json:"conclusion"`
	HTMLURL    string    `json:"html_url"`
	HeadSHA    string    `json:"head_sha"`
	HeadBranch string    `json:"head_branch"`
	Event      string    `json:"event"`
	Attempt    int       `json:"run_attempt"`
	UpdatedAt  time.Time `json:"updated_at"`
	// StartedAt is when the latest attempt started.
	StartedAt time.Time `json:"run_started_at"`
}

// Job is one job of a run.
type Job struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	HTMLURL    string `json:"html_url"`
	Steps      []struct {
		Name       string `json:"name"`
		Conclusion string `json:"conclusion"`
	} `json:"steps"`
}

// RunSnapshot is everything a run wait evaluates.
type RunSnapshot struct {
	Run  Run   `json:"run"`
	Jobs []Job `json:"jobs,omitempty"`
}

// Snapshot is one poll of a target.
type Snapshot struct {
	Target  Target       `json:"target"`
	PR      *PRSnapshot  `json:"pr,omitempty"`
	Run     *RunSnapshot `json:"run,omitempty"`
	Fetched time.Time    `json:"fetched"`
}

const maxPages = 10

// FetchPR polls a pull request: the PR, its head's check runs and commit
// statuses, reviews, issue comments and review comments.
func FetchPR(ctx context.Context, c *Client, t Target) (*PRSnapshot, error) {
	repo := "repos/" + t.FullRepo()
	r, err := c.Get(ctx, fmt.Sprintf("%s/pulls/%d", repo, t.Number))
	if err != nil {
		return nil, err
	}
	s := &PRSnapshot{}
	if err := json.Unmarshal(r.Body, &s.Pull); err != nil {
		return nil, fmt.Errorf("decode pull: %w", err)
	}
	if sha := s.Pull.Head.SHA; sha != "" {
		err := c.GetAll(ctx, fmt.Sprintf("%s/commits/%s/check-runs?per_page=100", repo, sha), maxPages, func(b []byte) error {
			var p struct {
				CheckRuns []CheckRun `json:"check_runs"`
			}
			err := json.Unmarshal(b, &p)
			s.CheckRuns = append(s.CheckRuns, p.CheckRuns...)
			return err
		})
		if err != nil {
			return nil, err
		}
		err = c.GetAll(ctx, fmt.Sprintf("%s/commits/%s/status?per_page=100", repo, sha), maxPages, func(b []byte) error {
			var p struct {
				Statuses []Status `json:"statuses"`
			}
			err := json.Unmarshal(b, &p)
			s.Statuses = append(s.Statuses, p.Statuses...)
			return err
		})
		if err != nil {
			return nil, err
		}
		if s.Runs, err = fetchRunsFor(ctx, c, repo, sha, s.CheckRuns); err != nil {
			return nil, err
		}
	}
	list := func(path string, add func([]byte) error) error {
		return c.GetAll(ctx, fmt.Sprintf("%s/%s?per_page=100", repo, path), maxPages, add)
	}
	if err := list(fmt.Sprintf("pulls/%d/reviews", t.Number), func(b []byte) error {
		var p []Review
		err := json.Unmarshal(b, &p)
		s.Reviews = append(s.Reviews, p...)
		return err
	}); err != nil {
		return nil, err
	}
	if err := list(fmt.Sprintf("issues/%d/comments", t.Number), func(b []byte) error {
		var p []Comment
		err := json.Unmarshal(b, &p)
		s.Comments = append(s.Comments, p...)
		return err
	}); err != nil {
		return nil, err
	}
	if err := list(fmt.Sprintf("pulls/%d/comments", t.Number), func(b []byte) error {
		var p []Comment
		err := json.Unmarshal(b, &p)
		s.ReviewComments = append(s.ReviewComments, p...)
		return err
	}); err != nil {
		return nil, err
	}
	return s, nil
}

// maxRunFetches bounds the per-run lookups for runs the head_sha list
// did not return.
const maxRunFetches = 10

// fetchRunsFor returns the Actions runs (latest attempt) that the check
// runs belong to: one listing by head SHA, then a direct lookup of any run
// it missed (a rerun of an older run, pagination).
func fetchRunsFor(ctx context.Context, c *Client, repo, sha string, crs []CheckRun) ([]Run, error) {
	want := map[int64]bool{}
	for _, r := range crs {
		if id := r.RunID(); id != 0 {
			want[id] = true
		}
	}
	if len(want) == 0 {
		return nil, nil
	}
	var out []Run
	err := c.GetAll(ctx, fmt.Sprintf("%s/actions/runs?head_sha=%s&per_page=100", repo, sha), 3, func(b []byte) error {
		var p struct {
			Runs []Run `json:"workflow_runs"`
		}
		err := json.Unmarshal(b, &p)
		for _, r := range p.Runs {
			if want[r.ID] {
				out = append(out, r)
				delete(want, r.ID)
			}
		}
		return err
	})
	if Permanent(err) {
		return nil, nil // no Actions access with this token: judge the checks by themselves
	}
	if err != nil {
		return nil, err
	}
	missing := make([]int64, 0, len(want))
	for id := range want {
		missing = append(missing, id)
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i] < missing[j] })
	for i, id := range missing {
		if i == maxRunFetches {
			break
		}
		r, err := c.Get(ctx, fmt.Sprintf("%s/actions/runs/%d", repo, id))
		if err != nil {
			if Permanent(err) {
				continue // a run we cannot read: judge its checks by themselves
			}
			return nil, err
		}
		var run Run
		if err := json.Unmarshal(r.Body, &run); err != nil {
			return nil, fmt.Errorf("decode run: %w", err)
		}
		out = append(out, run)
	}
	return out, nil
}

// FetchRun polls an Actions run and its latest attempt's jobs.
func FetchRun(ctx context.Context, c *Client, t Target) (*RunSnapshot, error) {
	repo := "repos/" + t.FullRepo()
	r, err := c.Get(ctx, fmt.Sprintf("%s/actions/runs/%d", repo, t.RunID))
	if err != nil {
		return nil, err
	}
	s := &RunSnapshot{}
	if err := json.Unmarshal(r.Body, &s.Run); err != nil {
		return nil, fmt.Errorf("decode run: %w", err)
	}
	err = c.GetAll(ctx, fmt.Sprintf("%s/actions/runs/%d/jobs?per_page=100&filter=latest", repo, t.RunID), maxPages, func(b []byte) error {
		var p struct {
			Jobs []Job `json:"jobs"`
		}
		err := json.Unmarshal(b, &p)
		s.Jobs = append(s.Jobs, p.Jobs...)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s, nil
}
