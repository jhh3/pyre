package agentguide

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompiledGuidanceKeepsTypedAndRawSurfacesCoherent(t *testing.T) {
	for name, text := range map[string]string{
		"generated": GeneratedRouting("work") + " " + GeneratedExecutionPolicy() + " " + WriteRule,
		"scoped":    ScopedUpstreamNotice("work"),
		"session":   SessionContext([]string{"work (slack, rest)"}, []string{"old (broken)"}, 1),
	} {
		if err := Audit(text); err != nil {
			t.Fatalf("%s: %v\n%s", name, err, text)
		}
	}
}

func TestSessionContextExplainsAnAllBrokenFreshState(t *testing.T) {
	text := SessionContext(nil, []string{"work (broken)"}, 0)
	if !strings.Contains(text, "No active local Outfit connectors") || !strings.Contains(text, "work (broken)") || strings.Contains(text, "available: .") {
		t.Fatalf("session context = %q", text)
	}
}

func TestStaticPluginGuidanceFollowsCompiledPolicy(t *testing.T) {
	path := filepath.Join("..", "..", "plugins", "outfit", "skills", "using-outfit", "SKILL.md")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	for _, expected := range []string{"typed `outfit` CLI", "outfit get URL", "no listed typed operation", "--write"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("static skill is missing compiled invariant %q", expected)
		}
	}
	if err := Audit(text); err != nil {
		t.Fatal(err)
	}
}

func TestAuditRejectsRawFirstAndMCPFirstGuidance(t *testing.T) {
	for _, text := range []string{
		"Use outfit exec work -- provider args.",
		"Prefer MCP for this connector.",
		"Use Outfit for mutations.",
	} {
		if err := Audit(text); err == nil {
			t.Fatalf("expected contradiction: %s", text)
		}
	}
}
