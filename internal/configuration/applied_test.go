package configuration

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

// launchMCP returns the MCP server names of a new Claude launch's overlay.
func launchMCP(t *testing.T, e *env) (contracts.EnvironmentGeneration, []string) {
	t.Helper()
	gen, lo, err := e.m.Resolve(context.Background(), Scope{Harness: contracts.HarnessClaude})
	if err != nil {
		t.Fatal(err)
	}
	p := flagValue(lo.ExtraArgs, "--mcp-config")
	if p == "" {
		return gen, nil
	}
	var doc struct {
		MCPServers map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(read(t, p)), &doc); err != nil {
		t.Fatal(err)
	}
	var names []string
	for n := range doc.MCPServers {
		names = append(names, n)
	}
	return gen, names
}

func has(names []string, n string) bool {
	for _, x := range names {
		if x == n {
			return true
		}
	}
	return false
}

func overlayProfile(mcp string) Profile {
	return Profile{Name: "default", MCPServers: []MCPServerSpec{{Name: mcp, Harness: contracts.HarnessClaude, Command: "/opt/synth/" + mcp, Args: []string{"--stdio"}}}}
}

func (e *env) setDesired(p Profile) ProfileSetResult {
	e.t.Helper()
	res, err := e.m.SetProfile(context.Background(), ProfileSetParams{Profile: p})
	if err != nil {
		e.t.Fatal(err)
	}
	return res
}

func (e *env) apply(plan contracts.ChangePlan) contracts.EnvironmentGeneration {
	e.t.Helper()
	gen, err := e.m.Apply(context.Background(), contracts.EnvApplyParams{PlanID: plan.PlanID, ResultDigest: plan.ResultDigest})
	if err != nil {
		e.t.Fatal(err)
	}
	return gen
}

// A15: a desired profile set with env.profile.set is pending: new launches
// keep resolving the active profile until the reviewed plan is applied, and
// a rollback makes the earlier profile active again for new launches.
func TestLaunchFollowsAppliedProfile(t *testing.T) {
	e := newEnv(t)
	r1 := e.setDesired(overlayProfile("synth-alpha"))
	if _, names := launchMCP(t, e); has(names, "synth-alpha") {
		t.Fatalf("pending desired profile reached a launch before apply: %v", names)
	}
	gen1 := e.apply(r1.Plan)
	if _, names := launchMCP(t, e); !has(names, "synth-alpha") {
		t.Fatalf("after apply 1: %v", names)
	}

	r2 := e.setDesired(overlayProfile("synth-beta"))
	if _, names := launchMCP(t, e); !has(names, "synth-alpha") || has(names, "synth-beta") {
		t.Fatalf("pending desired profile reached a launch: %v", names)
	}
	found := false
	for _, ch := range r2.Plan.Changes {
		if strings.Contains(ch.Path+ch.Diff, "synth-beta") {
			found = true
		}
	}
	if !found {
		t.Fatalf("plan does not show the desired change: %+v", r2.Plan.Changes)
	}
	g, _ := e.m.GetProfile(context.Background(), ProfileGetParams{})
	if !g.Pending || g.DesiredDigest != r2.ProfileDigest || g.AppliedGeneration != gen1.GenerationID {
		t.Fatalf("get while pending: %+v", g)
	}
	gen2 := e.apply(r2.Plan)
	if _, names := launchMCP(t, e); !has(names, "synth-beta") || has(names, "synth-alpha") {
		t.Fatalf("after apply 2: %v", names)
	}
	if g, _ := e.m.GetProfile(context.Background(), ProfileGetParams{}); g.Pending || g.ProfileDigest != r2.ProfileDigest {
		t.Fatalf("get after apply: %+v", g)
	}

	if _, err := e.m.Rollback(context.Background(), contracts.EnvRollbackParams{GenerationID: gen1.GenerationID}); err != nil {
		t.Fatal(err)
	}
	if _, names := launchMCP(t, e); !has(names, "synth-alpha") || has(names, "synth-beta") {
		t.Fatalf("after rollback to %s (from %s): %v", gen1.GenerationID, gen2.GenerationID, names)
	}
	if g, _ := e.m.GetProfile(context.Background(), ProfileGetParams{}); g.Pending || g.ProfileDigest != r1.ProfileDigest || g.AppliedGeneration != gen1.GenerationID {
		t.Fatalf("get after rollback: %+v", g)
	}
	again, err := e.m.Plan(context.Background(), contracts.EnvPlanParams{Scope: "global"})
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Changes) != 0 {
		t.Fatalf("plan after rollback is not a no-op: %+v", again.Changes)
	}
}

// Without any applied generation (profiles placed by hand, never applied),
// launches keep resolving the profile store.
func TestLaunchWithoutAppliedGenerationUsesStore(t *testing.T) {
	e := newEnv(t)
	e.saveGlobal(overlayProfile("synth-gamma"))
	if _, names := launchMCP(t, e); !has(names, "synth-gamma") {
		t.Fatalf("store profile not used: %v", names)
	}
}

// env.profile.set validates and stores the desired profile and returns the
// plan for it; env.profile.get returns it.
func TestProfileSetGetHandlers(t *testing.T) {
	e := newEnv(t)
	h := e.m.Handlers()
	ctx := context.Background()
	raw, _ := json.Marshal(ProfileSetParams{Profile: overlayProfile("synth-delta")})
	out, err := h[contracts.ActionEnvProfileSet](ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	res := out.(ProfileSetResult)
	if res.Plan.PlanID == "" || res.Plan.Scope != "global" || !strings.HasPrefix(string(res.ProfileDigest), "sha256:") {
		t.Fatalf("set result: %+v", res)
	}
	if _, names := launchMCP(t, e); has(names, "synth-delta") {
		t.Fatalf("pending profile reached a launch: %v", names)
	}
	got, err := h[contracts.ActionEnvProfileGet](ctx, []byte(`{"name":"default"}`))
	if err != nil {
		t.Fatal(err)
	}
	gr := got.(ProfileGetResult)
	if gr.Present || !gr.Pending || gr.Desired == nil || len(gr.Desired.MCPServers) != 1 || gr.DesiredDigest != res.ProfileDigest {
		t.Fatalf("get: %+v", gr)
	}
	// Credential-like material is refused.
	bad, _ := json.Marshal(ProfileSetParams{Profile: Profile{Name: "default", MCPServers: []MCPServerSpec{{Name: "x", Command: "x", Env: map[string]string{"API_TOKEN": "synthetic-token-value"}}}}})
	if _, err := h[contracts.ActionEnvProfileSet](ctx, bad); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("secret profile accepted: %v", err)
	}
	// Unknown fields are refused (strict decoding).
	if _, err := h[contracts.ActionEnvProfileSet](ctx, []byte(`{"profile":{"name":"default"},"bogus":1}`)); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("unknown field accepted: %v", err)
	}
	// A project profile is planned in the project scope.
	raw, _ = json.Marshal(ProfileSetParams{ProjectID: synthProject, Profile: overlayProfile("synth-proj")})
	out, err = h[contracts.ActionEnvProfileSet](ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	if s := out.(ProfileSetResult).Plan.Scope; s != "project:"+synthProject {
		t.Fatalf("project scope: %s", s)
	}
	// Missing profile: get returns present=false.
	got, err = h[contracts.ActionEnvProfileGet](ctx, []byte(`{"name":"nope"}`))
	if err != nil || got.(ProfileGetResult).Present || got.(ProfileGetResult).Pending {
		t.Fatalf("missing profile: %+v %v", got, err)
	}
}

// A node whose launcher cannot apply a Claude --settings overlay (or Codex -c
// overlays) refuses such a plan at review time instead of failing every later
// launch; overlay-only profiles still apply.
func TestUnsupportedOverlaysAreBlockingConflicts(t *testing.T) {
	e := newEnv(t)
	e.m.opt.ClaudeSettingsUnsupported = "synthetic: no claude --settings overlay on this node"
	e.m.opt.CodexOverlayUnsupported = "synthetic: no codex -c overlay on this node"
	p := overlayProfile("synth-eps")
	p.Settings = map[string]any{"claude.cleanupPeriodDays": 45, "codex.model_reasoning_effort": "high"}
	res := e.setDesired(p)
	var claude, codex bool
	for _, c := range res.Plan.Conflicts {
		if c.Kind == "conflict" && c.Path == "claude --settings overlay" && strings.Contains(c.Detail, "synthetic: no claude") {
			claude = true
		}
		if c.Kind == "conflict" && c.Path == "codex -c overlay" && strings.Contains(c.Detail, "synthetic: no codex") {
			codex = true
		}
	}
	if !claude || !codex {
		t.Fatalf("conflicts: %+v", res.Plan.Conflicts)
	}
	if _, err := e.m.Apply(context.Background(), contracts.EnvApplyParams{PlanID: res.Plan.PlanID, ResultDigest: res.Plan.ResultDigest}); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("apply of an unlaunchable plan: %v", err)
	}
	ok := e.setDesired(overlayProfile("synth-eps"))
	if len(ok.Plan.Conflicts) != 0 {
		t.Fatalf("overlay-only profile conflicts: %+v", ok.Plan.Conflicts)
	}
	e.apply(ok.Plan)
}
