package cli

import (
	"context"
	"slices"
	"strings"
	"testing"

	c "github.com/jhh3/pyre/internal/contracts"
)

// A Codex launch with no free OpenAI account fails before a session exists,
// or with --fallback-harness claude becomes a labelled Claude review.
func TestCodexPreflight(t *testing.T) {
	ctx := context.Background()
	waiting := &c.AccountSelection{Provider: c.ProviderOpenAI, Outcome: c.FallbackWaiting,
		Reason: `no eligible openai subscription on this node: account "code" (acct_synth-x): cooldown until 2026-10-18T03:00:00Z`}

	api := &startCapture{sel: waiting}
	o := &Ops{API: api, Self: Identity{NodeID: "node_synth-mac"}}
	_, err := o.StartSession(ctx, StartParams{Harness: "codex", Cwd: "/synthetic", Prompt: "review PR 1"})
	if !c.IsCode(err, c.ErrCapacityExhausted) || !strings.Contains(err.Error(), `account "code"`) || !strings.Contains(err.Error(), "--fallback-harness claude") {
		t.Fatalf("want a fast, explained failure, got %v", err)
	}
	if api.req.Harness != "" {
		t.Fatal("a session was started")
	}

	_, err = o.StartSession(ctx, StartParams{Harness: "codex", Cwd: "/synthetic", Prompt: "review PR 1", Title: "second opinion", FallbackHarness: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	r := api.req
	if r.Harness != c.HarnessClaude || r.Title != "Codex unavailable, Claude review: second opinion" || !slices.Contains(r.Tags, "codex-unavailable") ||
		!strings.HasPrefix(r.InitialPrompt, "[pyre] Codex unavailable, Claude review: no Codex (OpenAI) account was free") || !strings.HasSuffix(r.InitialPrompt, "review PR 1") {
		t.Fatalf("fallback request %+v", r)
	}

	// A Codex model never reaches the Claude fallback; an effort Claude
	// offers is kept, one it does not is dropped, and the prompt says so.
	api = &startCapture{sel: waiting, harnesses: synthModelHarnesses()}
	o = &Ops{API: api, Self: Identity{NodeID: "node_synth-mac"}}
	for _, tc := range []struct{ effort, want string }{{"ultra", ""}, {"high", "--pyre-effort=high"}} {
		_, err = o.StartSession(ctx, StartParams{Harness: "codex", Cwd: "/synthetic", Prompt: "review PR 1", Model: "gpt-synthetic", Effort: tc.effort, FallbackHarness: "claude"})
		if err != nil || api.req.Harness != c.HarnessClaude || strings.Join(api.req.Args, " ") != tc.want ||
			!strings.Contains(api.req.InitialPrompt, "model gpt-synthetic") || !strings.HasSuffix(api.req.InitialPrompt, "review PR 1") {
			t.Fatalf("fallback with effort %s: args %v prompt %q err %v", tc.effort, api.req.Args, api.req.InitialPrompt, err)
		}
	}
	o = &Ops{API: &startCapture{sel: waiting}, Self: Identity{NodeID: "node_synth-mac"}}

	// A free account, or a node that cannot answer: Codex as asked.
	for _, sel := range []*c.AccountSelection{{Provider: c.ProviderOpenAI, AccountID: "acct_synth-x"}, nil} {
		api := &startCapture{sel: sel}
		o := &Ops{API: api, Self: Identity{NodeID: "node_synth-mac"}}
		if _, err := o.StartSession(ctx, StartParams{Harness: "codex", Cwd: "/synthetic", FallbackHarness: "claude"}); err != nil || api.req.Harness != c.HarnessCodex {
			t.Fatalf("sel %+v: harness %s err %v", sel, api.req.Harness, err)
		}
	}
	if _, err := o.StartSession(ctx, StartParams{Harness: "codex", Cwd: "/synthetic", FallbackHarness: "gemini"}); err == nil {
		t.Fatal("unknown fallback harness accepted")
	}

	// Without --node: another connected Codex node with a free account takes
	// the launch when no --cwd ties it to the placed node; with a --cwd the
	// error names it.
	mac := c.Node{NodeID: "node_synth-mac2", Name: "mac2", Connection: c.NodeConnection{State: c.ConnConnected},
		Harnesses: []c.HarnessInfo{{Harness: c.HarnessCodex, Available: true}}}
	api = &startCapture{sel: waiting, nodes: []c.Node{mac}, sels: map[string]*c.AccountSelection{"node_synth-mac2": {AccountID: "acct_synth-y"}}}
	o = &Ops{API: api, Self: Identity{NodeID: "node_synth-mac"}}
	_, err = o.StartSession(ctx, StartParams{Harness: "codex", Cwd: "/synthetic"})
	if !c.IsCode(err, c.ErrCapacityExhausted) || !strings.Contains(err.Error(), "A free Codex account exists on mac2") {
		t.Fatalf("with --cwd: %v", err)
	}
	moved, err := o.harnessPreflightNode(ctx, StartParams{Harness: "codex"})
	if err != nil || moved != "node_synth-mac2" {
		t.Fatalf("without --cwd: node %q err %v", moved, err)
	}
}

// harnessPreflightNode runs the preflight alone and returns the node it
// settled on.
func (o *Ops) harnessPreflightNode(ctx context.Context, p StartParams) (string, error) {
	n := o.Self.NodeID
	err := o.harnessPreflight(ctx, &n, &p)
	return n, err
}
