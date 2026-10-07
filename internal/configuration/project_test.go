package configuration

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

// A15 project scope: a project profile overrides the node (global) profile.
// A same-name MCP server in the project profile replaces the global one for
// the project's runs (registered once, no conflict), a project setting wins
// over the global value, and runs outside the project keep the global one.
func TestProjectProfileOverridesGlobal(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	global := Profile{Name: "default",
		Settings:   map[string]any{"claude.env.SYNTH_SCOPE": "global", "codex.model_reasoning_effort": "high"},
		MCPServers: []MCPServerSpec{{Name: "synth-scope", Command: "/opt/synth/global-mcp", Args: []string{"--stdio"}}}}
	project := Profile{Name: "default",
		Settings:   map[string]any{"claude.env.SYNTH_SCOPE": "project", "codex.model_reasoning_effort": "low"},
		MCPServers: []MCPServerSpec{{Name: "synth-scope", Command: "/opt/synth/project-mcp", Args: []string{"--stdio"}}}}
	if _, err := e.m.SetProfile(ctx, ProfileSetParams{Profile: global}); err != nil {
		t.Fatal(err)
	}
	e.planApply("global")
	res, err := e.m.SetProfile(ctx, ProfileSetParams{ProjectID: synthProject, Profile: project})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range res.Plan.Conflicts {
		if c.Kind == "conflict" {
			t.Fatalf("project override reported as conflict: %+v", c)
		}
	}
	e.apply(res.Plan)

	gen, lo, err := e.m.Resolve(ctx, Scope{Harness: contracts.HarnessClaude, ProjectID: synthProject})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		MCPServers map[string]struct {
			Command string `json:"command"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(read(t, flagValue(lo.ExtraArgs, "--mcp-config"))), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.MCPServers["synth-scope"].Command != "/opt/synth/project-mcp" {
		t.Fatalf("project MCP did not override: %+v", doc.MCPServers)
	}
	if s := read(t, flagValue(lo.ExtraArgs, "--settings")); !strings.Contains(s, `"SYNTH_SCOPE": "project"`) {
		t.Fatalf("project setting did not override: %s", s)
	}
	n := 0
	for _, r := range gen.MCPServers {
		if r.Name == "synth-scope" && r.Harness == contracts.HarnessClaude && r.Managed {
			n++
			if r.Layer != contracts.LayerManagedProject {
				t.Fatalf("winning layer = %s", r.Layer)
			}
		}
	}
	if n != 1 {
		t.Fatalf("synth-scope listed %d times: %+v", n, gen.MCPServers)
	}
	for _, v := range gen.Values {
		if v.Key == "claude.env.SYNTH_SCOPE" && (v.Layer != contracts.LayerManagedProject || len(v.Overrode) == 0 || v.Overrode[len(v.Overrode)-1] != contracts.LayerManagedGlobal) {
			t.Fatalf("provenance = %+v", v)
		}
	}
	// Codex: the project's values only, each once.
	_, clo, err := e.m.Resolve(ctx, Scope{Harness: contracts.HarnessCodex, ProjectID: synthProject})
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(clo.ExtraArgs, " ")
	if strings.Count(args, "mcp_servers.synth-scope.command") != 1 || !strings.Contains(args, "project-mcp") || strings.Contains(args, "global-mcp") || !strings.Contains(args, `model_reasoning_effort="low"`) {
		t.Fatalf("codex args = %s", args)
	}
	// Outside the project: global.
	_, glo, err := e.m.Resolve(ctx, Scope{Harness: contracts.HarnessClaude})
	if err != nil {
		t.Fatal(err)
	}
	if s := read(t, flagValue(glo.ExtraArgs, "--mcp-config")); !strings.Contains(s, "global-mcp") || strings.Contains(s, "project-mcp") {
		t.Fatalf("global run got %s", s)
	}
}

// Two servers of the same name in one managed layer remain a conflict.
func TestSameLayerDuplicateMCPIsConflict(t *testing.T) {
	e := newEnv(t)
	p := Profile{Name: "default", MCPServers: []MCPServerSpec{
		{Name: "synth-dup", Harness: contracts.HarnessClaude, Command: "/opt/synth/a"},
		{Name: "synth-dup", Harness: contracts.HarnessClaude, Command: "/opt/synth/b"}}}
	res := e.setDesired(p)
	found := false
	for _, c := range res.Plan.Conflicts {
		found = found || (c.Kind == "conflict" && strings.Contains(c.Key, "synth-dup"))
	}
	if !found {
		t.Fatalf("same-layer duplicate not a conflict: %+v", res.Plan.Conflicts)
	}
}
