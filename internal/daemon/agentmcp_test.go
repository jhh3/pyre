package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestAgentMCPArgs(t *testing.T) {
	dir := t.TempDir()
	d := &Daemon{cfg: Config{AgentMCP: []string{"/opt/pyre/bin/pyre", "mcp"}}}
	args, err := d.agentMCPArgs([]string{"--plugin-dir", "/x"}, dir)
	if err != nil || len(args) != 1 || args[0] != "--mcp-config="+filepath.Join(dir, "pyre-mcp.json") {
		t.Fatalf("args=%v err=%v", args, err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "pyre-mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &spec); err != nil || spec.MCPServers["pyre"].Command != "/opt/pyre/bin/pyre" || spec.MCPServers["pyre"].Args[0] != "mcp" {
		t.Fatalf("spec %s err %v", b, err)
	}
	// An overlay-provided --mcp-config wins; nothing is added.
	if args, _ := d.agentMCPArgs([]string{"--mcp-config", "/overlay.json"}, dir); args != nil {
		t.Fatalf("duplicate mcp config: %v", args)
	}
	// No MCP command configured: nothing added.
	if args, _ := (&Daemon{}).agentMCPArgs(nil, dir); args != nil {
		t.Fatalf("unexpected %v", args)
	}
}
