package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

// projectCapture adds a project with a shared checkout of video-editor to
// repoCapture.
type projectCapture struct {
	*repoCapture
	proj c.Project
}

func (p *projectCapture) GetProject(context.Context, c.GetRequest) (c.Project, error) {
	return p.proj, nil
}

func projectFleet(now time.Time) (*projectCapture, c.Node) {
	api, mini := repoFleet(now)
	return &projectCapture{repoCapture: api, proj: c.Project{ProjectID: "prj_01SYNTHETICPROJECT00000000", Name: "synthetic-v2-port",
		Repos:     []c.RepoBinding{{RepoID: "video-editor", Name: "video-editor", URL: "https://github.com/synth/video-editor.git"}},
		Checkouts: []c.CheckoutBinding{{NodeID: mini.NodeID, Path: "/home/synth/code/video-editor", RepoID: "video-editor", Mode: c.CheckoutShared, Branch: "main"}}}}, mini
}

// The 2026-10-31 incident (iss_…): start_session
// with project, repo video-editor, fresh and a unique branch started in the
// project's shared checkout on main, because fresh and branch were read only
// without a project. An explicit fresh or branch now wins over the shared
// checkout mode: a fresh worktree at <root>/<repo>/<branch>, made from the
// shared checkout, recorded as the run's worktree checkout.
func TestProjectFreshStartWinsOverSharedCheckout(t *testing.T) {
	now := time.Date(2026, 10, 31, 23, 5, 0, 0, time.UTC)
	for _, sp := range []StartParams{
		{Fresh: true, Branch: "merge-importer-codex"},
		{Branch: "merge-importer-codex"},
		{Fresh: true, Worktree: "merge-importer-codex"},
	} {
		api, mini := projectFleet(now)
		o := &Ops{API: api, Now: func() time.Time { return now }}
		sp.Node, sp.Harness, sp.Project, sp.Repo, sp.Prompt, sp.OperationID = mini.NodeID, "claude", api.proj.ProjectID, "video-editor", "port the importer", "op_synthetic-fresh-project"
		res, err := o.StartSession(context.Background(), sp)
		want := "/home/synth/fast/worktrees/video-editor/merge-importer-codex"
		r := api.req
		if err != nil || r.Cwd != want || r.Checkout == nil || r.Checkout.Mode != c.CheckoutWorktree || r.Checkout.Path != want ||
			r.Checkout.Branch != "merge-importer-codex" || r.Checkout.RepoID != "video-editor" || r.ProjectID == nil || *r.ProjectID != api.proj.ProjectID {
			t.Fatalf("%+v: %v %+v checkout %+v", sp, err, r, r.Checkout)
		}
		if api.creates != 1 || res.Cwd != want || res.Branch != "merge-importer-codex" {
			t.Fatalf("%+v: creates %d, result cwd %q branch %q", sp, api.creates, res.Cwd, res.Branch)
		}
		if got := api.created["op_synthetic-fresh-project.worktrees"]; len(got.Worktrees) != 1 || got.Worktrees[0].Repo != "/home/synth/code/video-editor" {
			t.Fatalf("worktree source: %+v", got)
		}
	}
}

// Without fresh or branch the shared checkout stays the default.
func TestProjectStartWithoutFreshUsesSharedCheckout(t *testing.T) {
	now := time.Date(2026, 10, 31, 23, 5, 0, 0, time.UTC)
	api, mini := projectFleet(now)
	o := &Ops{API: api, Now: func() time.Time { return now }}
	_, err := o.StartSession(context.Background(), StartParams{Node: mini.NodeID, Harness: "claude", Project: api.proj.ProjectID, Repo: "video-editor", Prompt: "look"})
	if r := api.req; err != nil || api.creates != 0 || r.Checkout == nil || r.Checkout.Mode != c.CheckoutShared || r.Cwd != "/home/synth/code/video-editor" {
		t.Fatalf("shared: %v creates %d %+v", err, api.creates, api.req.Checkout)
	}
}

// A project start that fails after its fresh worktree exists names it; the
// retry with the same operation ID reuses it.
func TestProjectFreshStartFailureReportsWorktree(t *testing.T) {
	now := time.Date(2026, 10, 31, 23, 5, 0, 0, time.UTC)
	api, mini := projectFleet(now)
	api.failStart = c.Errorf(c.ErrUnavailable, "synthetic: the machine went away")
	o := &Ops{API: api, Now: func() time.Time { return now }}
	sp := StartParams{Node: mini.NodeID, Harness: "claude", Project: api.proj.ProjectID, Repo: "video-editor", Branch: "merge-importer-codex", Prompt: "port", OperationID: "op_synthetic-project-partial"}
	_, err := o.StartSession(context.Background(), sp)
	if e := c.AsError(err); !c.IsCode(err, c.ErrUnavailable) || !strings.Contains(e.Message, "a retry with operation_id op_synthetic-project-partial reuses it") {
		t.Fatalf("partial: %v", err)
	}
	if _, err := o.StartSession(context.Background(), sp); err != nil || api.creates != 1 || api.req.Cwd != "/home/synth/fast/worktrees/video-editor/merge-importer-codex" {
		t.Fatalf("retry: %v, creates %d, cwd %q", err, api.creates, api.req.Cwd)
	}
}

// fresh or branch never silently land in a shared directory: without a
// repository to branch they are refused, and so are disagreeing branches.
func TestFreshWithoutRepoIsRefused(t *testing.T) {
	now := time.Date(2026, 10, 31, 23, 5, 0, 0, time.UTC)
	api, mini := projectFleet(now)
	o := &Ops{API: api, Now: func() time.Time { return now }}
	for _, sp := range []StartParams{
		{Project: api.proj.ProjectID, Cwd: "/home/synth/code/video-editor", Fresh: true},
		{Cwd: "/home/synth/code/video-editor", Branch: "x"},
		{Project: api.proj.ProjectID, Repo: "video-editor", Branch: "a", Worktree: "b"},
	} {
		sp.Node, sp.Harness = mini.NodeID, "claude"
		var ue *usageError
		if _, err := o.StartSession(context.Background(), sp); !errors.As(err, &ue) || api.creates != 0 || api.req.NodeID != "" {
			t.Fatalf("%+v: %v creates %d", sp, err, api.creates)
		}
	}
}
