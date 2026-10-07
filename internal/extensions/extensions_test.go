package extensions

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

func w(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

type fakeOutfit []ManagedSkill

func (f fakeOutfit) ManagedSkills(ctx context.Context) ([]ManagedSkill, error) { return f, nil }

func synthHomes(t *testing.T) (Homes, string) {
	root := t.TempDir()
	h := filepath.Join(root, "home")
	w(t, filepath.Join(h, ".claude/settings.json"), `{
  "hooks": {
    "Stop": [{"hooks": [{"type": "command", "command": "/opt/synth/stop --token=synthetic-hook-secret"}]}],
    "PostToolUse": [{"matcher": "Edit", "hooks": [{"type": "command", "command": "/opt/synth/post"}]}]
  },
  "enabledPlugins": {"synth-lsp@synth-market": true, "ghost@synth-market": true}
}`)
	w(t, filepath.Join(h, ".claude/skills/alpha/SKILL.md"), "a")
	w(t, filepath.Join(h, ".claude/skills/synced/beta/SKILL.md"), "b")
	w(t, filepath.Join(h, ".claude/skills/outfit-skill/SKILL.md"), "outfit content")
	w(t, filepath.Join(h, ".claude/skills/managed-one/SKILL.md"), "m")
	d, _ := DigestDir(filepath.Join(h, ".claude/skills/managed-one"))
	mk, _ := json.Marshal(Marker{Owner: "pyre", Name: "managed-one", Kind: KindSkill, Layer: contracts.LayerManagedGlobal, GenerationID: "env_synth-1", Digest: d})
	w(t, filepath.Join(h, ".claude/skills/managed-one", MarkerFile), string(mk))
	w(t, filepath.Join(h, ".claude/commands/ns/deploy.md"), "x")
	w(t, filepath.Join(h, ".claude/commands/review.md"), "x")
	w(t, filepath.Join(h, ".claude/agents/helper.md"), "x")
	pd := filepath.Join(h, ".claude/plugins/cache/synth-market/synth-lsp/1.0.0")
	w(t, filepath.Join(pd, "skills/lsp/SKILL.md"), "p")
	w(t, filepath.Join(h, ".claude/plugins/installed_plugins.json"), `{"version":2,"plugins":{"synth-lsp@synth-market":[{"scope":"user","installPath":"`+pd+`","version":"1.0.0"}]}}`)
	proj := filepath.Join(root, "proj")
	w(t, filepath.Join(h, ".claude.json"), `{"mcpServers":{"tool":{"command":"tool","env":{"TOOL_TOKEN":"synthetic-mcp-secret"}}},
	  "projects":{"`+proj+`":{"mcpServers":{"tool":{"command":"other-tool"}}}}}`)
	w(t, filepath.Join(proj, ".claude/settings.json"), `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/opt/synth/stop --token=synthetic-hook-secret"}]}]}}`)
	w(t, filepath.Join(proj, ".mcp.json"), `{"mcpServers":{"proj-mcp":{"type":"http","url":"https://synth.invalid/mcp"}}}`)
	w(t, filepath.Join(h, ".codex/config.toml"), `
[mcp_servers.node]
command = "node"
args = ["--secret-arg=synthetic"]

[plugins."browser@synth-bundled"]
enabled = true

[[hooks.PreToolUse]]
matcher = "shell"
hooks = [{ type = "command", command = "/opt/synth/codex-pre" }]

[hooks.state."x"]
trusted_hash = "synthetic"
`)
	w(t, filepath.Join(h, ".codex/plugins/cache/synth-bundled/browser/26.1.0/skills/browse/SKILL.md"), "s")
	w(t, filepath.Join(h, ".codex/hooks.json"), `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/opt/synth/codex-stop"}]}]}}`)
	w(t, filepath.Join(h, ".codex/skills/.system/imagegen/SKILL.md"), "sys")
	w(t, filepath.Join(h, ".codex/skills/mine/SKILL.md"), "mine")
	w(t, filepath.Join(h, ".codex/prompts/p1.md"), "p")
	hs := DefaultHomes(h)
	hs.ProjectDirs = []string{proj}
	return hs, h
}

func find(inv Inventory, k Kind, h contracts.Harness, name string) *Item {
	for i, it := range inv.Items {
		if it.Kind == k && it.Harness == h && it.Name == name {
			return &inv.Items[i]
		}
	}
	return nil
}

func TestScanInventory(t *testing.T) {
	hs, _ := synthHomes(t)
	inv, err := Scan(context.Background(), hs, ScanOptions{Managed: fakeOutfit{{Name: "outfit-skill", Version: "2.0.0", Digest: mustDigestMap(map[string][]byte{"SKILL.md": []byte("outfit content")})}, {Name: "remote-only", Version: "1.0"}}})
	if err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		k    Kind
		h    contracts.Harness
		name string
		f    func(*Item) bool
	}{
		{KindSkill, contracts.HarnessClaude, "alpha", func(i *Item) bool {
			return !i.Managed && i.Layer == contracts.LayerAdoptedUser && i.Location == "~/.claude/skills/alpha"
		}},
		{KindSkill, contracts.HarnessClaude, "beta", func(i *Item) bool { return strings.Contains(i.Detail, "synced") }},
		{KindSkill, contracts.HarnessClaude, "managed-one", func(i *Item) bool { return i.Managed && i.Layer == contracts.LayerManagedGlobal }},
		{KindSkill, contracts.HarnessClaude, "outfit-skill", func(i *Item) bool { return i.Managed && i.Version == "2.0.0" }},
		{KindSkill, "", "remote-only", func(i *Item) bool { return i.Managed && i.Scope == "outfit" }},
		{KindSkill, contracts.HarnessClaude, "lsp", func(i *Item) bool { return i.Plugin == "synth-lsp@synth-market" && i.Scope == "plugin" }},
		{KindCommand, contracts.HarnessClaude, "ns:deploy", nil},
		{KindCommand, contracts.HarnessClaude, "review", nil},
		{KindAgent, contracts.HarnessClaude, "helper", nil},
		{KindPlugin, contracts.HarnessClaude, "synth-lsp@synth-market", func(i *Item) bool { return i.Version == "1.0.0" && i.Enabled != nil && *i.Enabled }},
		{KindPlugin, contracts.HarnessClaude, "ghost@synth-market", func(i *Item) bool { return i.Detail == "enabled but not installed" }},
		{KindMCPServer, contracts.HarnessClaude, "tool", nil},
		{KindMCPServer, contracts.HarnessClaude, "proj-mcp", func(i *Item) bool { return i.Scope == "project" && i.MCP.Transport == "http" }},
		{KindMCPServer, contracts.HarnessCodex, "node", nil},
		{KindPlugin, contracts.HarnessCodex, "browser@synth-bundled", func(i *Item) bool { return i.Version == "26.1.0" }},
		{KindSkill, contracts.HarnessCodex, "browse", func(i *Item) bool { return i.Plugin == "browser@synth-bundled" }},
		{KindSkill, contracts.HarnessCodex, "imagegen", func(i *Item) bool { return i.Layer == contracts.LayerProviderDefault && i.Scope == "system" }},
		{KindSkill, contracts.HarnessCodex, "mine", nil},
		{KindPrompt, contracts.HarnessCodex, "p1", nil},
		{KindHook, contracts.HarnessCodex, "PreToolUse[shell]", nil},
		{KindHook, contracts.HarnessCodex, "Stop", nil},
		{KindHook, contracts.HarnessClaude, "PostToolUse[Edit]", nil},
	}
	for _, c := range checks {
		it := find(inv, c.k, c.h, c.name)
		if it == nil {
			t.Errorf("missing %s %s %s", c.k, c.h, c.name)
			continue
		}
		if c.f != nil && !c.f(it) {
			t.Errorf("unexpected %s %s: %+v", c.k, c.name, *it)
		}
	}
	// The Codex hook trust table is harness state, not a hook.
	for _, it := range inv.Items {
		if it.Kind == KindHook && strings.Contains(it.Name, "state") {
			t.Fatalf("hooks.state inventoried: %+v", it)
		}
	}
	// The Stop hook is registered at user and project scope: reported once
	// as a duplicate, never changed.
	dup, toolDup := false, false
	for _, d := range inv.Duplicates {
		if d.Kind == "conflict" && d.Key == "Stop" && strings.Contains(d.Detail, "registered 2 times") {
			dup = true
		}
		if d.Key == "tool" {
			toolDup = true // same-name MCP in two scopes with different definitions
		}
	}
	if !dup || !toolDup {
		t.Fatalf("duplicates not reported: %+v", inv.Duplicates)
	}
	// Commands, args, env values never leave the node.
	b, _ := json.Marshal(inv)
	for _, secret := range []string{"synthetic-hook-secret", "synthetic-mcp-secret", "--secret-arg", "/opt/synth"} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("inventory JSON leaks %q", secret)
		}
	}
	refs := inv.Refs()
	if len(refs) != len(inv.Items) || !strings.HasPrefix(refs[0].Source, string(inv.Items[0].Kind)+"@") {
		t.Fatalf("refs: %+v", refs[0])
	}
	if len(inv.Roots) != 2 || !inv.Roots[0].Exists {
		t.Fatalf("roots: %+v", inv.Roots)
	}
}

func mustDigestMap(m map[string][]byte) contracts.Digest { return DigestFileMap(m) }

func TestDigestDirMatchesFileMapAndIgnoresMarker(t *testing.T) {
	d := t.TempDir()
	w(t, filepath.Join(d, "SKILL.md"), "x")
	w(t, filepath.Join(d, "ref/a.md"), "y")
	a, err := DigestDir(d)
	if err != nil {
		t.Fatal(err)
	}
	if a != DigestFileMap(map[string][]byte{"SKILL.md": []byte("x"), "ref/a.md": []byte("y")}) {
		t.Fatal("DigestDir != DigestFileMap")
	}
	w(t, filepath.Join(d, MarkerFile), `{"owner":"pyre"}`)
	if b, _ := DigestDir(d); b != a {
		t.Fatal("marker changed digest")
	}
}

func TestAdoptionPreview(t *testing.T) {
	hs, h := synthHomes(t)
	before, _ := os.ReadFile(filepath.Join(h, ".claude/settings.json"))
	inv, _ := Scan(context.Background(), hs, ScanOptions{})
	p := AdoptionPreview(inv)
	if !strings.HasPrefix(p.Summary, "These ") || !strings.Contains(p.Summary, "will be referenced, nothing moved") {
		t.Fatal(p.Summary)
	}
	if p.Counts[KindSkill] == 0 || p.Counts[KindHook] == 0 || p.Managed != 1 {
		t.Fatalf("%+v", p.Counts)
	}
	after, _ := os.ReadFile(filepath.Join(h, ".claude/settings.json"))
	if string(before) != string(after) {
		t.Fatal("scan modified a user file")
	}
}

func TestMCPDefEquivalence(t *testing.T) {
	a := MCPDef{Name: "x", Command: "c", Args: []string{"1"}, Env: map[string]string{"K": "v"}}
	b := MCPDef{Name: "x", Command: "c", Args: []string{"1"}, Env: map[string]string{"K": "v"}}
	c := MCPDef{Name: "x", Command: "c", Args: []string{"1"}, Env: map[string]string{"K": "other"}}
	a.Normalize()
	b.Normalize()
	c.Normalize()
	if a.DefDigest != b.DefDigest || a.DefDigest == c.DefDigest {
		t.Fatal("def digest equivalence")
	}
	if !c.HasSecretMaterial() {
		t.Fatal("env values are secret material")
	}
	if HookIdentity("claude", "Stop", "", "cmd ") != HookIdentity("claude", "Stop", "", "cmd") {
		t.Fatal("hook identity should ignore surrounding whitespace")
	}
}
