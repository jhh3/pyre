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
}
