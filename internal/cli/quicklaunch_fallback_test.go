package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

// repoCapture adds a repo index and worktree.create to startCapture. Like
// the hub, a worktree.create replayed with the same operation ID returns
// the recorded result instead of creating another worktree.
type repoCapture struct {
	*startCapture
	repos     []c.RepoEntry
	created   map[string]c.WorktreeCreateResult // by operation ID
	creates   int                               // worktrees actually created
	failStart error                             // next StartSession fails with it
}

func (r *repoCapture) ListRepos(context.Context, c.ListReposRequest) (c.RepoList, error) {
	return c.RepoList{Items: r.repos}, nil
}

func (r *repoCapture) SessionWorktrees(context.Context, c.SessionWorktreesRequest) (c.SessionWorktrees, error) {
	return c.SessionWorktrees{}, nil
}

func (r *repoCapture) InvokeNodeAction(ctx context.Context, req c.NodeActionRequest) (c.NodeActionResult, error) {
	switch req.Action {
	case c.ActionWorktreeCreate:
		res, ok := r.created[req.OperationID]
		if !ok {
			var p c.WorktreeCreateParams
			_ = json.Unmarshal(req.Params, &p)
			r.creates++
			path := "/home/synth/fast/worktrees/video-editor/" + p.Branch
			res = c.WorktreeCreateResult{Cwd: path, Branch: p.Branch, ClaimToken: "synthetic-token",
				Worktrees: []c.CreatedWorktree{{Repo: p.Repos[0], Path: path, Branch: p.Branch, Head: "776e4b8de74e150970898382acbc8fb08ab18097"}}}
			r.created[req.OperationID] = res
		}
		b, _ := json.Marshal(res)
		return c.NodeActionResult{Outcome: c.OutcomeSucceeded, Result: b}, nil
	case c.ActionWorktreeClaim:
		return c.NodeActionResult{Outcome: c.OutcomeSucceeded, Result: []byte(`{"claimed":[]}`)}, nil
	}
	return r.startCapture.InvokeNodeAction(ctx, req)
}

func (r *repoCapture) StartSession(ctx context.Context, req c.StartSessionRequest) (c.StartSessionResponse, error) {
	if err := r.failStart; err != nil {
		r.failStart = nil
		return c.StartSessionResponse{}, err
	}
	return r.startCapture.StartSession(ctx, req)
}

func repoFleet(now time.Time) (*repoCapture, c.Node) {
	api, mini, _ := codexFleet(now)
	return &repoCapture{startCapture: api, created: map[string]c.WorktreeCreateResult{},
		repos: []c.RepoEntry{{Repo: c.Repo{Name: "video-editor", Path: "/home/synth/code/video-editor", RemoteURL: "https://github.com/synth/video-editor.git"},
			NodeID: mini.NodeID, NodeName: mini.Name}}}, mini
}

// The 2026-10-31 incident (iss_…): start_session
// with repo, fresh, a branch, node server, harness codex and fallback_harness
// claude, where server had no OpenAI account with room. The quick launch
// dropped fallback_harness, created the worktree, then refused the start
// (capacity_exhausted) and left it behind. It now starts Claude in the
// worktree on the named node and says so in the placement.
func TestFreshRepoStartOnNamedNodeFallsBack(t *testing.T) {
	now := time.Date(2026, 10, 31, 17, 44, 0, 0, time.UTC)
	api, mini := repoFleet(now)
	o := &Ops{API: api, Now: func() time.Time { return now }}
	var pl Placement
	_, err := o.StartSession(withPlacement(context.Background(), &pl), StartParams{Node: mini.NodeID, Harness: "codex", Model: "gpt-synthetic",
		Repo: "video-editor", Fresh: true, Branch: "provider-settings-review", Title: "review", Prompt: "review the provider settings",
		FallbackHarness: "claude", OperationID: "op_synthetic-review"})
	r := api.req
	if err != nil || r.Harness != c.HarnessClaude || r.NodeID != mini.NodeID || r.Cwd != "/home/synth/fast/worktrees/video-editor/provider-settings-review" ||
		!strings.HasPrefix(r.Title, "Codex unavailable") || strings.Contains(strings.Join(r.Args, " "), "gpt-synthetic") || !strings.HasSuffix(r.InitialPrompt, "review the provider settings") {
		t.Fatalf("fallback: %v %+v", err, r)
	}
	if api.creates != 1 || !strings.Contains(pl.Warning, "so this starts on Claude") || pl.NodeID != mini.NodeID {
		t.Fatalf("creates %d, placement %+v", api.creates, pl)
	}
}

// Without a fallback the refusal comes before any worktree is created.
func TestFreshRepoStartRefusedBeforeWorktree(t *testing.T) {
	now := time.Date(2026, 10, 31, 17, 44, 0, 0, time.UTC)
	api, mini := repoFleet(now)
	o := &Ops{API: api, Now: func() time.Time { return now }}
	_, err := o.StartSession(context.Background(), StartParams{Node: mini.NodeID, Harness: "codex", Repo: "video-editor", Fresh: true,
		Branch: "provider-settings-review", Prompt: "review", OperationID: "op_synthetic-refused"})
	if !c.IsCode(err, c.ErrCapacityExhausted) || api.creates != 0 || api.req.NodeID != "" {
		t.Fatalf("refused: %v, creates %d, sent %q", err, api.creates, api.req.NodeID)
	}
}

// A start that fails after its worktree was created names the worktree
// (path, branch, commit) and the operation ID; the retry with that ID
// reuses the worktree instead of creating another.
func TestFreshRepoStartFailureReportsWorktree(t *testing.T) {
	now := time.Date(2026, 10, 31, 17, 44, 0, 0, time.UTC)
	api, mini := repoFleet(now)
	api.failStart = c.Errorf(c.ErrUnavailable, "synthetic: the machine went away")
	o := &Ops{API: api, Now: func() time.Time { return now }}
	sp := StartParams{Node: mini.NodeID, Harness: "claude", Repo: "video-editor", Branch: "provider-settings-review", Prompt: "review", OperationID: "op_synthetic-partial"}
	_, err := o.StartSession(context.Background(), sp)
	e := c.AsError(err)
	if !c.IsCode(err, c.ErrUnavailable) || !strings.Contains(e.Message, "/home/synth/fast/worktrees/video-editor/provider-settings-review (branch provider-settings-review at 776e4b8de7)") ||
		!strings.Contains(e.Message, "a retry with operation_id op_synthetic-partial reuses it") || e.Details["operation_id"] != "op_synthetic-partial" {
		t.Fatalf("partial: %v %+v", err, e)
	}
	if _, err := o.StartSession(context.Background(), sp); err != nil || api.creates != 1 || api.req.Cwd != "/home/synth/fast/worktrees/video-editor/provider-settings-review" {
		t.Fatalf("retry: %v, creates %d, cwd %q", err, api.creates, api.req.Cwd)
	}
}
