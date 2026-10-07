package skillgen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhh3/outfit/internal/kit"
	"github.com/jhh3/outfit/internal/state"
)

func TestRenderIncludesOnlySelectedMode(t *testing.T) {
	pkg := kit.Package{Display: "Example", Skill: kit.Skill{Summary: "Use Example."}, Modes: []kit.Mode{{ID: "rest", Label: "REST"}, {ID: "mcp", Label: "MCP"}}}
	connection := state.Connection{Name: "work", SelectedMode: "rest"}
	rendered := Render(connection, pkg, kit.Mode{ID: "rest", Label: "REST", Operations: []kit.Operation{{Name: "read_item", Class: "read", Description: "Read one."}}})
	if !strings.Contains(rendered, "read_item") || strings.Contains(rendered, "selected in **MCP** mode") {
		t.Fatalf("skill leaked another mode: %s", rendered)
	}
}

func TestRenderUsesOperationsAsACompactRoutingIndex(t *testing.T) {
	pkg := kit.Package{
		Display: "Example",
		Links: []kit.LinkResolver{{
			ID: "item", ResourceType: "example.item",
			Lookups: []kit.LinkLookup{{Mode: "mcp", Operation: "find_item"}},
		}},
		Skill: kit.Skill{
			Summary: "Use Example.",
			Workflows: []kit.SkillWorkflow{{
				Title: "Investigate an item",
				Steps: []string{"find the exact item", "read related evidence", "report stable identifiers"},
			}},
		},
	}
	connection := state.Connection{Name: "work", SelectedMode: "mcp"}
	longDescription := strings.Repeat("provider schema detail ", 500)
	rendered := Render(connection, pkg, kit.Mode{ID: "mcp", Label: "Hosted MCP", Operations: []kit.Operation{
		{Name: "find_item", Class: "read", Description: longDescription},
		{Name: "update_item", Class: "write", Description: longDescription},
		{Name: "delete_item", Class: "destructive", Description: longDescription},
	}})
	for _, expected := range []string{
		"outfit search '<task>'",
		"outfit work <operation> --help",
		"Prefer the typed Outfit CLI whenever a matching operation is listed",
		"Use `outfit exec` only",
		"## Typed CLI entry points",
		"`find_item`: `outfit work find-item --help`",
		"outfit workflow show work investigate-an-item",
		"Do not guess argument names",
		"## Pasted links",
		"outfit get 'URL'",
		"Do not manually parse a supported URL",
		"example.item",
		"### Read (1)",
		"`find_item`",
		"### Write (1)",
		"`update_item`",
		"### Destructive (1)",
		"`delete_item`",
		"## Common workflows",
		"### Investigate an item",
		"1. find the exact item",
	} {
		if !strings.Contains(rendered, expected) {
			t.Fatalf("compact skill is missing %q: %s", expected, rendered)
		}
	}
	if strings.Contains(rendered, "work__find_item") {
		t.Fatalf("skill advertised a redundant per-operation MCP tool: %s", rendered)
	}
	if strings.Contains(rendered, longDescription) || len(rendered) > 4_096 {
		t.Fatalf("skill copied operation manuals instead of routing to outfit describe: %d bytes", len(rendered))
	}
}

func TestSyncHonorsHostRootsAndPreservesUserSkill(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	userSkill := filepath.Join(root, "codex", "skills", "outfit-personal")
	if err := os.MkdirAll(userSkill, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userSkill, "SKILL.md"), []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	legacyGenerated := filepath.Join(root, "codex", "skills", "cw-work")
	legacyUser := filepath.Join(root, "codex", "skills", "cw-personal")
	for _, directory := range []string{legacyGenerated, legacyUser} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(legacyGenerated, ".cw-generated"), []byte("owned"), 0o600); err != nil {
		t.Fatal(err)
	}
	connections := []state.Connection{{Name: "work", ConnectorKey: "sample", SelectedMode: "rest", Status: "active"}}
	packages := map[string]kit.Package{"sample": {Display: "Sample", Skill: kit.Skill{Summary: "Use Sample."}, Modes: []kit.Mode{{ID: "rest", Label: "REST"}}}}
	written, err := Sync("codex", connections, packages, nil, nil)
	if err != nil || len(written) != 1 {
		t.Fatalf("sync: %#v %v", written, err)
	}
	raw, _ := os.ReadFile(filepath.Join(userSkill, "SKILL.md"))
	if string(raw) != "mine" {
		t.Fatal("unowned outfit-prefixed skill was deleted")
	}
	if _, err := os.Stat(legacyGenerated); !os.IsNotExist(err) {
		t.Fatal("owned legacy generated skill was not removed")
	}
	if _, err := os.Stat(legacyUser); err != nil {
		t.Fatal("unowned legacy skill was removed")
	}
}

func TestSyncFiltersWorkspaceOperationsAndExamples(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	connections := []state.Connection{{Name: "work", ConnectorKey: "sample", SelectedMode: "rest", Status: "active"}}
	packages := map[string]kit.Package{
		"sample": {
			Display: "Sample",
			Skill: kit.Skill{
				Summary:   "Use Sample.",
				Examples:  []string{"outfit write sample delete_item"},
				Workflows: []kit.SkillWorkflow{{Title: "Delete an item", Steps: []string{"call delete_item"}}},
			},
			Modes: []kit.Mode{{
				ID:    "rest",
				Label: "REST",
				Operations: []kit.Operation{
					{Name: "read_item", Class: "read", Description: "Read"},
					{Name: "delete_item", Class: "destructive", Description: "Delete"},
				},
			}},
		},
	}
	written, err := Sync("codex", connections, packages, nil, func(_ string, operation string) bool { return operation == "read_item" })
	if err != nil || len(written) != 1 {
		t.Fatalf("sync: %#v %v", written, err)
	}
	raw, err := os.ReadFile(written[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "read_item") || strings.Contains(string(raw), "delete_item") || strings.Contains(string(raw), "Delete an item") {
		t.Fatalf("skill escaped workspace operation policy: %s", raw)
	}
}
