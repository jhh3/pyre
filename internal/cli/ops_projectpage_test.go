package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/mock"
)

// update_project_page / get_project_page through the MCP server (owner on
// the mock; the hub-side permission rules are tested in internal/hub).
func TestMCPProjectPage(t *testing.T) {
	b := mock.New(mock.Options{Clock: mock.NewOffsetClock(mock.FixtureEpoch), DefaultPrincipal: &testOwner})
	cl := startMCP(t, &Ops{API: b, PollInterval: 10 * time.Millisecond})
	cl.call("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "0"}})
	out, isErr := cl.tool("create_project", map[string]any{"name": "synthetic-page"})
	var prj c.Project
	if isErr || json.Unmarshal(out, &prj) != nil {
		t.Fatalf("create_project: %s", out)
	}
	if out, isErr = cl.tool("get_project_page", map[string]any{"project": "synthetic-page"}); isErr || !strings.Contains(string(out), "has no page yet") {
		t.Fatalf("empty page: %s", out)
	}
	if out, isErr = cl.tool("update_project_page", map[string]any{"project": "synthetic-page", "content": "# Synthetic page\n\nGoal: synthetic.", "summary": "first"}); isErr || !strings.Contains(string(out), "revision 1") {
		t.Fatalf("update: %s", out)
	}
	out, isErr = cl.tool("get_project_page", map[string]any{"project": prj.ProjectID, "format": "json"})
	var pg c.ProjectPage
	if isErr || json.Unmarshal(out, &pg) != nil || pg.Revision == nil || pg.Content != "# Synthetic page\n\nGoal: synthetic." || pg.Revision.Summary != "first" {
		t.Fatalf("get: %s", out)
	}
	if out, isErr = cl.tool("update_project_page", map[string]any{"project": prj.ProjectID, "content": "stale", "base_revision_id": "rev_synthstale"}); !isErr {
		t.Fatalf("stale base: %s", out)
	}
	if out, isErr = cl.tool("update_project_page", map[string]any{"project": prj.ProjectID, "content": " "}); !isErr {
		t.Fatalf("empty content: %s", out)
	}
	// The status card alone, then shown with the page.
	status := map[string]any{"progress": 55, "phase": "synthetic phase", "shipped": []any{map[string]any{"title": "synthetic PR", "state": "merged", "url": "https://example.invalid/1"}}}
	if out, isErr = cl.tool("update_project_page", map[string]any{"project": prj.ProjectID, "status": status}); isErr || !strings.Contains(string(out), "status card set") {
		t.Fatalf("status: %s", out)
	}
	if out, isErr = cl.tool("get_project_page", map[string]any{"project": prj.ProjectID}); isErr || !strings.Contains(string(out), "progress: 55%") || !strings.Contains(string(out), "synthetic PR · merged") || !strings.Contains(string(out), "Goal: synthetic.") {
		t.Fatalf("get with status: %s", out)
	}
	if out, isErr = cl.tool("update_project_page", map[string]any{"project": prj.ProjectID, "status": map[string]any{"progress": 120}}); !isErr {
		t.Fatalf("progress 120: %s", out)
	}
	if out, isErr = cl.tool("update_project_page", map[string]any{"project": prj.ProjectID}); !isErr {
		t.Fatalf("neither content nor status: %s", out)
	}
}

// `pyre project page status` flags change only those fields of the current
// card; --file replaces it.
func TestProjectPageStatusCommand(t *testing.T) {
	b := mock.New(mock.Options{Clock: mock.NewOffsetClock(mock.FixtureEpoch), DefaultPrincipal: &testOwner})
	app, e := &App{API: b}, newEnv(t, nil)
	expectCode(t, run(t, e, app, "project", "new", "synthetic-status"), 0)
	app.Stdin = strings.NewReader(`{"progress":20,"phase":"synthetic","shipped":[{"title":"synthetic PR","url":"https://example.invalid/1","state":"merged"}]}`)
	expectCode(t, run(t, e, app, "project", "page", "status", "--project", "synthetic-status", "--file", "-"), 0)
	app.Stdin = nil
	expectCode(t, run(t, e, app, "project", "page", "status", "--project", "synthetic-status", "--progress", "60"), 0)
	r := run(t, e, app, "project", "page", "show", "--project", "synthetic-status")
	expectCode(t, r, 0)
	for _, want := range []string{"progress: 60%", "phase: synthetic", "synthetic PR · merged"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("missing %q\n%s", want, r.stdout)
		}
	}
	app.Stdin = strings.NewReader(`{"shipped":[{"title":"x","url":"javascript:alert(1)"}]}`)
	if r := run(t, e, app, "project", "page", "status", "--project", "synthetic-status", "--file", "-"); r.code == 0 || !strings.Contains(r.stderr, "http(s)") {
		t.Errorf("bad url accepted: %s", r)
	}
}
