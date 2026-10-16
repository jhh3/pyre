package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/claude"
	"github.com/jhh3/pyre/internal/runtime/mock"
)

// create_project and associate_session_project use the same Ops as
// `pyre project new` and `pyre associate`; start_session takes a project.
func TestMCPCreateProjectAndAssociate(t *testing.T) {
	b := mock.New(mock.Options{Clock: mock.NewOffsetClock(mock.FixtureEpoch), DefaultPrincipal: &testOwner})
	cl := startMCP(t, &Ops{API: b, PollInterval: 10 * time.Millisecond})
	cl.call("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "0"}})

	// Name only: no repository needed.
	out, isErr := cl.tool("create_project", map[string]any{"name": "synthetic-bare"})
	var bare c.Project
	if isErr || json.Unmarshal(out, &bare) != nil || bare.ProjectID == "" || bare.Name != "synthetic-bare" || len(bare.Repos) != 0 {
		t.Fatalf("create_project (name only): %s", out)
	}

	// Repos and checkouts, node given by name, as `pyre project new --checkout`.
	out, isErr = cl.tool("create_project", map[string]any{"name": "synthetic-mcp-proj", "description": "synthetic",
		"repos":     []string{"app=https://example.invalid/synthetic/app.git"},
		"checkouts": []string{"synth-mac:/srv/synth/app:app:shared", "node_synth-linux:/home/synth/app:app:worktree:main"}})
	var prj c.Project
	if isErr || json.Unmarshal(out, &prj) != nil {
		t.Fatalf("create_project: %s", out)
	}
	want := []c.CheckoutBinding{
		{NodeID: "node_synth-mac", Path: "/srv/synth/app", RepoID: "app", Mode: c.CheckoutShared},
		{NodeID: "node_synth-linux", Path: "/home/synth/app", RepoID: "app", Mode: c.CheckoutWorktree, Branch: "main"},
	}
	if len(prj.Repos) != 1 || prj.Repos[0].RepoID != "app" || len(prj.Checkouts) != 2 || prj.Checkouts[0] != want[0] || prj.Checkouts[1] != want[1] {
		t.Fatalf("project bindings: %+v", prj)
	}

	// Invalid bindings are rejected before anything is created.
	for _, args := range []map[string]any{
		{"name": ""},
		{"name": "synthetic-bad", "repos": []string{"app"}},
		{"name": "synthetic-bad", "repos": []string{"app=https://example.invalid/a.git"}, "checkouts": []string{"synth-mac:/x:other:shared"}},
		{"name": "synthetic-bad", "repos": []string{"app=https://example.invalid/a.git"}, "checkouts": []string{"synth-mac:relative:app:shared"}},
	} {
		out, isErr := cl.tool("create_project", args)
		var te ToolError
		if !isErr || json.Unmarshal(out, &te) != nil || te.ExitCode != ExitUsage {
			t.Fatalf("create_project %v must be a usage error: %s", args, out)
		}
	}
	out, _ = cl.tool("list_projects", map[string]any{"query": "synthetic-bad"})
	if strings.Contains(string(out), "synthetic-bad") {
		t.Fatalf("a rejected create_project created a project: %s", out)
	}

	// start_session into the project, by name.
	out, isErr = cl.tool("start_session", map[string]any{"node": "synth-mac", "harness": "claude", "cwd": "/home/synth/mcp-proj", "project": "synthetic-mcp-proj"})
	var started c.StartSessionResponse
	if isErr || json.Unmarshal(out, &started) != nil || started.Session.Session.ProjectID == nil || *started.Session.Session.ProjectID != prj.ProjectID {
		t.Fatalf("start_session with project: %s", out)
	}

	// Attach an existing standalone session, then detach it.
	out, isErr = cl.tool("start_session", map[string]any{"node": "synth-mac", "harness": "claude", "cwd": "/home/synth/mcp-solo"})
	var solo c.StartSessionResponse
	if isErr || json.Unmarshal(out, &solo) != nil || solo.Session.Session.ProjectID != nil {
		t.Fatalf("standalone start_session: %s", out)
	}
	sid := solo.Session.Session.SessionID
	out, isErr = cl.tool("associate_session_project", map[string]any{"session": sid, "project": prj.ProjectID})
	var sv c.SessionView
	if isErr || json.Unmarshal(out, &sv) != nil || sv.Session.ProjectID == nil || *sv.Session.ProjectID != prj.ProjectID {
		t.Fatalf("associate_session_project: %s", out)
	}
	out, isErr = cl.tool("associate_session_project", map[string]any{"session": sid, "none": true})
	sv = c.SessionView{}
	if isErr || json.Unmarshal(out, &sv) != nil || sv.Session.ProjectID != nil {
		t.Fatalf("detach: %s", out)
	}
	for _, args := range []map[string]any{{"session": sid}, {"session": sid, "project": prj.ProjectID, "none": true}} {
		out, isErr := cl.tool("associate_session_project", args)
		var te ToolError
		if !isErr || json.Unmarshal(out, &te) != nil || te.ExitCode != ExitUsage {
			t.Fatalf("associate_session_project %v must be a usage error: %s", args, out)
		}
	}
	out, isErr = cl.tool("associate_session_project", map[string]any{"session": sid, "project": "prj_synth-missing"})
	if te := (ToolError{}); !isErr || json.Unmarshal(out, &te) != nil || te.ExitCode != ExitNotFound {
		t.Fatalf("unknown project: %s", out)
	}
}

// Creating, associating and archiving projects never prompt (owner,
// W-archive2): all three are pre-approved for Claude runs, and none is
// read-only.
func TestProjectToolsArePreapproved(t *testing.T) {
	byName := map[string]MCPTool{}
	for _, tl := range MCPTools() {
		byName[tl.Name] = tl
	}
	allowed := map[string]bool{}
	for _, name := range claude.PyreMessagingAllowedTools {
		allowed[name] = true
	}
	for _, name := range []string{"create_project", "associate_session_project", "archive_project"} {
		tl, ok := byName[name]
		if !ok {
			t.Fatalf("missing tool %s", name)
		}
		if !allowed["mcp__pyre__"+name] {
			t.Errorf("%s must be pre-approved", name)
		}
		if tl.Annotations == nil || tl.Annotations.ReadOnlyHint || tl.Annotations.OpenWorldHint {
			t.Errorf("%s annotations: %+v", name, tl.Annotations)
		}
	}
}
