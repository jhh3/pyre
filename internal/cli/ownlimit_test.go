package cli

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

// agyNodes: synth-server (idle, agy at its quota until reset) and synth-mini
// (busier, agy with room).
func agyNodes(reset time.Time) (limited, free c.Node) {
	limited = synthNode("node_synth-server", 1, 30000, 32000, 0, 0, 0, c.HarnessAgy, c.HarnessClaude)
	limited.Harnesses[0].Limit = &c.LimitEvent{Kind: "usage_limit", Detail: "Individual quota reached.", ResetAt: &reset}
	free = synthNode("node_synth-mini", 60, 8000, 32000, 0, 0, 6, c.HarnessAgy, c.HarnessClaude)
	return limited, free
}

// Placement skips a node whose agy login is at its quota until the reset
// (decision I72), and only for agy.
func TestPlacementSkipsAgyLimitedNode(t *testing.T) {
	now := time.Date(2026, 10, 25, 3, 41, 0, 0, time.UTC)
	limited, free := agyNodes(now.Add(13 * time.Hour))
	api := &startCapture{nodes: []c.Node{limited, free}}
	o := &Ops{API: api, Now: func() time.Time { return now }}
	ctx := context.Background()
	pl, err := o.placeAmong(ctx, api.nodes, "agy", "", false, false, nil, false, "", false, false, false)
	if err != nil || pl.NodeID != free.NodeID || !strings.Contains(pl.Reason, "no usable account on synth-server: no google account with room for agy (at its limit: the own agy login until Sun 9:41 AM PDT (in 13h))") {
		t.Fatalf("agy placement: %+v %v", pl, err)
	}
	// Claude on the same machine is not affected: the idle node wins.
	if pl, err := o.placeAmong(ctx, api.nodes, "claude", "", false, false, nil, false, "", false, false, false); err != nil || pl.NodeID != limited.NodeID {
		t.Fatalf("claude placement: %+v %v", pl, err)
	}
	// After the reset the limited node is back (and wins on load).
	o.Now = func() time.Time { return now.Add(13*time.Hour + time.Second) }
	if pl, err := o.placeAmong(ctx, api.nodes, "agy", "", false, false, nil, false, "", false, false, false); err != nil || pl.NodeID != limited.NodeID {
		t.Fatalf("after the reset: %+v %v", pl, err)
	}
}

// With every agy node at its quota, a start without --node is refused with
// each node's reset and what to do instead; --force takes the least loaded
// one with a warning.
func TestPlacementAllAgyLimitedRefuses(t *testing.T) {
	now := time.Date(2026, 10, 25, 3, 41, 0, 0, time.UTC)
	limited, other := agyNodes(now.Add(13 * time.Hour))
	other.Harnesses[0].Limit = &c.LimitEvent{Kind: "usage_limit", ResetAt: ptrTime(now.Add(2 * time.Hour))}
	api := &startCapture{nodes: []c.Node{limited, other}}
	o := &Ops{API: api, Now: func() time.Time { return now }}
	ctx := context.Background()
	_, err := o.StartSession(ctx, StartParams{Harness: "agy", Prompt: "synthetic task"})
	t.Log(err)
	if !c.IsCode(err, c.ErrCapacityExhausted) {
		t.Fatalf("all limited: %v", err)
	}
	for _, want := range []string{"synth-server has no google account with room for agy (at its limit: the own agy login until Sun 9:41 AM PDT (in 13h))",
		"synth-mini has no google account with room for agy (at its limit: the own agy login until 10:41 PM PDT (in 2h))",
		"so no session was started", "try another --harness, or wait for a reset (--force starts it anyway)"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("missing %q in %v", want, err)
		}
	}
	if api.req.NodeID != "" {
		t.Fatalf("a start was sent: %+v", api.req)
	}
	var pl Placement
	if _, err := o.StartSession(withPlacement(ctx, &pl), StartParams{Harness: "agy", Prompt: "synthetic task", Force: true}); err != nil {
		t.Fatalf("force: %v", err)
	}
	if api.req.NodeID != limited.NodeID || !strings.Contains(pl.Warning, "synth-server has no google account with room for agy") || !strings.Contains(pl.Warning, "--force") {
		t.Fatalf("force placement: %+v %+v", api.req.NodeID, pl)
	}
}

// --node on a node whose agy login is at its quota is refused unless
// --force (MCP force), which starts it there with a warning; the refusal
// ends at the reset.
func TestNamedAgyLimitedNodeRefused(t *testing.T) {
	now := time.Date(2026, 10, 25, 3, 41, 0, 0, time.UTC)
	reset := now.Add(13 * time.Hour)
	limited, free := agyNodes(reset)
	api := &startCapture{nodes: []c.Node{limited, free}}
	o := &Ops{API: api, Now: func() time.Time { return now }}
	ctx := context.Background()
	_, err := o.StartSession(ctx, StartParams{Node: limited.NodeID, Harness: "agy", Cwd: "/synthetic", Prompt: "synthetic task"})
	if !c.IsCode(err, c.ErrCapacityExhausted) || !strings.Contains(err.Error(), "synth-server has no google account with room for agy (at its limit: the own agy login until Sun 9:41 AM PDT (in 13h))") ||
		!strings.Contains(err.Error(), "one has room on synth-mini") ||
		!strings.Contains(err.Error(), "--force") || api.req.NodeID != "" {
		t.Fatalf("named limited node: %v (sent %q)", err, api.req.NodeID)
	}
	// Claude there is fine.
	if _, err := o.StartSession(ctx, StartParams{Node: limited.NodeID, Harness: "claude", Cwd: "/synthetic"}); err != nil || api.req.NodeID != limited.NodeID {
		t.Fatalf("claude on the node: %v", err)
	}
	api.req = c.StartSessionRequest{}
	var pl Placement
	if _, err := o.StartSession(withPlacement(ctx, &pl), StartParams{Node: limited.NodeID, Harness: "agy", Cwd: "/synthetic", Force: true}); err != nil ||
		api.req.NodeID != limited.NodeID || !strings.Contains(pl.Warning, "starting there anyway (--force)") {
		t.Fatalf("force: %v %+v", err, pl)
	}
	api.req = c.StartSessionRequest{}
	o.Now = func() time.Time { return reset.Add(time.Second) }
	if _, err := o.StartSession(ctx, StartParams{Node: limited.NodeID, Harness: "agy", Cwd: "/synthetic"}); err != nil || api.req.NodeID != limited.NodeID {
		t.Fatalf("after the reset: %v", err)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

// codexFleet is the 2026-10-26 incident (iss_…):
// synth-laptop (idle) has only its own OpenAI login, registered as
// synth-own and at its weekly limit; synth-server (busier) holds synth-code,
// which has room. Their dry runs answer as the nodes did: no eligible
// account on synth-laptop (the exhausted own login is skipped), synth-code
// on synth-server.
func codexFleet(now time.Time) (*startCapture, c.Node, c.Node) {
	mini := synthNode("node_synth-laptop", 1, 30000, 32000, 0, 0, 0, c.HarnessCodex, c.HarnessClaude)
	mini.Harnesses[0].Login = &c.HarnessLogin{Email: "synth-owner@example.invalid"}
	server := synthNode("node_synth-server", 60, 8000, 32000, 0, 0, 6, c.HarnessCodex, c.HarnessClaude)
	weekly := now.Add(6*24*time.Hour + 7*time.Hour)
	own := c.AccountView{Account: c.Account{AccountID: "acct_synth-own", Provider: c.ProviderOpenAI, Label: "synth-own", RotaRef: "synth-owner@example.invalid"},
		ByNode: []c.AccountObservation{{AccountID: "acct_synth-own", Provider: c.ProviderOpenAI, ReportingNode: mini.NodeID, ObservedAt: now.Add(-time.Minute),
			Eligibility: c.EligibleExhausted, ResetAt: &weekly, Windows: []c.UsageWindow{{Name: "seven_day", UsedPercent: 100, ResetAt: &weekly}}}}}
	code := c.AccountView{Account: c.Account{AccountID: "acct_synth-code", Provider: c.ProviderOpenAI, Label: "synth-code"},
		Grants: []c.DeviceGrant{{GrantID: "grant_synth-code", AccountID: "acct_synth-code", NodeID: server.NodeID, Harness: c.HarnessCodex, Kind: c.CredCodexHomeFile, State: c.GrantValid}},
		ByNode: []c.AccountObservation{{AccountID: "acct_synth-code", Provider: c.ProviderOpenAI, ReportingNode: server.NodeID, ObservedAt: now.Add(-time.Minute),
			Eligibility: c.EligibleYes, Windows: []c.UsageWindow{{Name: "seven_day", UsedPercent: 13, ResetAt: &weekly}}}}}
	api := &startCapture{nodes: []c.Node{mini, server}, accounts: []c.AccountView{own, code}, sels: map[string]*c.AccountSelection{
		mini.NodeID: {Provider: c.ProviderOpenAI, Outcome: c.FallbackIntervention, Reason: "no eligible openai account"},
		server.NodeID:  {Provider: c.ProviderOpenAI, AccountID: "acct_synth-code"}}}
	return api, mini, server
}

// Placement skips a machine whose only OpenAI login (its own, at its weekly
// limit) has no room, and says so; once the node's dry run finds an
// account there it is trusted again (decision I74).
func TestPlacementSkipsCodexOwnLoginAtLimit(t *testing.T) {
	now := time.Date(2026, 10, 26, 23, 23, 0, 0, time.UTC)
	api, mini, server := codexFleet(now)
	o := &Ops{API: api, Now: func() time.Time { return now }}
	ctx := context.Background()
	pl, err := o.placeAmong(ctx, api.nodes, "codex", "", false, false, nil, false, "", false, false, false)
	if err != nil || pl.NodeID != server.NodeID ||
		!strings.Contains(pl.Reason, "no usable account on synth-laptop: no openai account with room for codex (at its limit: synth-own (own login) until Nov 1 10:23 PM PST (in 6d 7h))") {
		t.Fatalf("codex placement: %+v %v", pl, err)
	}
	// Claude there is not affected: the idle machine wins (the fake's dry
	// runs answer for OpenAI only, so none answers here).
	sels := api.sels
	api.sels = map[string]*c.AccountSelection{mini.NodeID: nil, server.NodeID: nil}
	if pl, err := o.placeAmong(ctx, api.nodes, "claude", "", false, false, nil, false, "", false, false, false); err != nil || pl.NodeID != mini.NodeID {
		t.Fatalf("claude placement: %+v %v", pl, err)
	}
	api.sels = sels
	api.sels[mini.NodeID] = &c.AccountSelection{Provider: c.ProviderOpenAI, AccountID: "acct_synth-own"}
	if pl, err := o.placeAmong(ctx, api.nodes, "codex", "", false, false, nil, false, "", false, false, false); err != nil || pl.NodeID != mini.NodeID {
		t.Fatalf("dry run found room: %+v %v", pl, err)
	}
}

// With no OpenAI login with room anywhere, a start without --node is
// refused with every machine's resets; --fallback-harness claude still
// places it (the preflight then runs it on Claude) and --force takes a
// machine at its limit with a warning.
func TestPlacementAllCodexAtLimitRefuses(t *testing.T) {
	now := time.Date(2026, 10, 26, 23, 23, 0, 0, time.UTC)
	api, _, server := codexFleet(now)
	five := now.Add(2 * time.Hour)
	api.accounts[1].ByNode[0].Eligibility, api.accounts[1].ByNode[0].ResetAt = c.EligibleExhausted, &five
	api.sels[server.NodeID] = &c.AccountSelection{Provider: c.ProviderOpenAI, Outcome: c.FallbackWaiting, Reason: "synth-code at its limit"}
	o := &Ops{API: api, Now: func() time.Time { return now }}
	ctx := context.Background()
	_, err := o.StartSession(ctx, StartParams{Harness: "codex", Prompt: "synthetic task"})
	if !c.IsCode(err, c.ErrCapacityExhausted) || api.req.NodeID != "" {
		t.Fatalf("all at limit: %v (sent %q)", err, api.req.NodeID)
	}
	for _, want := range []string{"synth-laptop has no openai account with room for codex (at its limit: synth-own (own login) until Nov 1 10:23 PM PST (in 6d 7h))",
		"synth-server has no openai account with room for codex (at its limit: synth-code until 6:23 PM PDT (in 2h))",
		"so no session was started; try another --harness, or wait for a reset (--force starts it anyway)"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("missing %q in %v", want, err)
		}
	}
	if _, err := o.StartSession(ctx, StartParams{Harness: "codex", Prompt: "synthetic review", FallbackHarness: "claude"}); err != nil || api.req.Harness != c.HarnessClaude {
		t.Fatalf("fallback harness: %v %+v", err, api.req)
	}
	api.req = c.StartSessionRequest{}
	var pl Placement
	if _, err := o.placeAmong(withPlacement(ctx, &pl), api.nodes, "codex", "", false, false, nil, false, "", false, true, false); err != nil {
		t.Fatalf("force: %v", err)
	}
}

// --node on a machine with no OpenAI login with room is refused, naming the
// login at its limit, its reset and the machine with room; --force starts
// it there with a warning.
func TestNamedCodexNodeAtLimitRefused(t *testing.T) {
	now := time.Date(2026, 10, 26, 23, 23, 0, 0, time.UTC)
	api, mini, _ := codexFleet(now)
	o := &Ops{API: api, Now: func() time.Time { return now }}
	ctx := context.Background()
	_, err := o.StartSession(ctx, StartParams{Node: mini.NodeID, Harness: "codex", Cwd: "/synthetic", Prompt: "synthetic task"})
	if !c.IsCode(err, c.ErrCapacityExhausted) || api.req.NodeID != "" {
		t.Fatalf("named node: %v (sent %q)", err, api.req.NodeID)
	}
	for _, want := range []string{"synth-laptop has no openai account with room for codex (at its limit: synth-own (own login) until Nov 1 10:23 PM PST (in 6d 7h))",
		"one has room on synth-server (name it with --node, or leave out --node)", "--force starts it anyway"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("missing %q in %v", want, err)
		}
	}
	// --ambient asks for the own login only: refused the same way.
	if _, err := o.StartSession(ctx, StartParams{Node: mini.NodeID, Harness: "codex", Cwd: "/synthetic", Ambient: true}); !c.IsCode(err, c.ErrCapacityExhausted) {
		t.Fatalf("ambient: %v", err)
	}
	var pl Placement
	api.sels[mini.NodeID] = nil // the preflight cannot ask: the launch proceeds
	if _, err := o.StartSession(withPlacement(ctx, &pl), StartParams{Node: mini.NodeID, Harness: "codex", Cwd: "/synthetic", Force: true}); err != nil ||
		api.req.NodeID != mini.NodeID || !strings.Contains(pl.Warning, "starting there anyway (--force)") {
		t.Fatalf("force: %v %+v", err, pl)
	}
}

// The 2026-10-27 incident (iss_…): a Codex review
// named a machine whose only OpenAI login is its own, at its weekly limit,
// with --fallback-harness claude. The node's dry run picks that own login
// (no Pyre OpenAI account there, so no grant), which used to pass the
// preflight: the run started as Codex and stopped at once on the limit.
// It now starts on Claude, says so in the placement warning, and carries
// the task; a Codex launch that goes ahead keeps the fallback tag.
func TestNamedCodexOwnLoginAtLimitFallsBack(t *testing.T) {
	now := time.Date(2026, 10, 26, 23, 23, 0, 0, time.UTC)
	api, mini, server := codexFleet(now)
	api.sels[mini.NodeID] = &c.AccountSelection{Provider: c.ProviderOpenAI, AccountID: "acct_synth-own",
		Reason: "credential_kind=ambient: your own login on this node (this node has no Pyre openai accounts); never used for automatic fallback"}
	o := &Ops{API: api, Now: func() time.Time { return now }}
	ctx := context.Background()
	var pl Placement
	_, err := o.StartSession(withPlacement(ctx, &pl), StartParams{Node: mini.NodeID, Harness: "codex", Model: "gpt-synthetic", Cwd: "/synthetic",
		Title: "review #1", Prompt: "review PR 1", FallbackHarness: "claude"})
	r := api.req
	if err != nil || r.Harness != c.HarnessClaude || r.NodeID != mini.NodeID || r.Title != "Codex unavailable, Claude review: review #1" ||
		!slices.Contains(r.Tags, c.TagCodexUnavailable) || slices.Contains(r.Tags, c.TagFallbackClaude) || strings.Contains(strings.Join(r.Args, " "), "gpt-synthetic") ||
		!strings.Contains(r.InitialPrompt, "synth-own (own login) until Nov 1") || !strings.HasSuffix(r.InitialPrompt, "review PR 1") {
		t.Fatalf("fallback: %v %+v", err, r)
	}
	if !strings.Contains(pl.Warning, "no Codex (OpenAI) account has room on synth-laptop") || !strings.Contains(pl.Warning, "so this starts on Claude") {
		t.Fatalf("placement warning: %+v", pl)
	}
	// Without the fallback the named machine is refused as before.
	api.req = c.StartSessionRequest{}
	if _, err := o.StartSession(ctx, StartParams{Node: mini.NodeID, Harness: "codex", Cwd: "/synthetic", Prompt: "review PR 1"}); !c.IsCode(err, c.ErrCapacityExhausted) || api.req.NodeID != "" {
		t.Fatalf("no fallback: %v", err)
	}
	// Placed: the own-login node is not "free" for the cross-node search
	// either; server's account takes it, as Codex, tagged for a runtime fallback.
	if _, err := o.StartSession(ctx, StartParams{Node: server.NodeID, Harness: "codex", Cwd: "/synthetic", Prompt: "review PR 1", FallbackHarness: "claude"}); err != nil ||
		api.req.Harness != c.HarnessCodex || !slices.Contains(api.req.Tags, c.TagFallbackClaude) {
		t.Fatalf("codex with room: %v %+v", err, api.req)
	}
	o.Self = Identity{NodeID: mini.NodeID}
	if moved, err := o.harnessPreflightNode(ctx, StartParams{Harness: "codex"}); err != nil || moved != server.NodeID {
		t.Fatalf("search: moved to %q, err %v", moved, err)
	}
}
