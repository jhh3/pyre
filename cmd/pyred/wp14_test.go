package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/configuration"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/daemon"
)

func TestEnvWiringResolveAndInventory(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".claude"), 0o700)
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir()) // no real claude/codex/outfit found or executed
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	in := &integrations{ctl: &runController{}, rep: &reportBridge{}, grants: &pushedGrants{active: map[grantKey]bool{}}}
	cfg := &daemon.Config{NodeID: "node_01TESTNODE00000000000000"}
	w := wireEnvironment(in, cfg, runOpts{state: t.TempDir()}, "", "", "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if w == nil || cfg.Environments == nil {
		t.Fatal("environment wiring failed")
	}
	ctx := context.Background()
	res, err := w.Resolve(ctx, daemon.EnvRequest{NodeID: cfg.NodeID, Harness: contracts.HarnessClaude, SessionID: "ses_1", RunID: "run_1"})
	if err != nil {
		t.Fatalf("resolve claude: %v", err)
	}
	if res.GenerationID == "" {
		t.Fatalf("no generation: %+v", res)
	}
	again, err := w.Resolve(ctx, daemon.EnvRequest{NodeID: cfg.NodeID, Harness: contracts.HarnessClaude, SessionID: "ses_2", RunID: "run_2"})
	if err != nil || again.GenerationID != res.GenerationID {
		t.Fatalf("identical resolution should reuse the generation: %v %+v", err, again)
	}
	sh, err := w.Resolve(ctx, daemon.EnvRequest{Harness: contracts.HarnessShell})
	if err != nil || sh.GenerationID != "" {
		t.Fatalf("shell = %+v %v", sh, err)
	}
	if _, err := w.Resolve(ctx, daemon.EnvRequest{Harness: contracts.HarnessShell, Profile: "x"}); !contracts.IsCode(err, contracts.ErrUnsupported) {
		t.Fatalf("shell profile = %v", err)
	}
	base := contracts.NodeInventory{Tools: []contracts.ToolVersion{{Tool: "tmux", Version: "3.4"}},
		Integrations: map[string]contracts.IntegrationStatus{"hive": {Health: "unavailable"}, "rota": {Health: "unchecked"}}}
	inv, err := mergeInventory(ctx, base, w.inv)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tv := range inv.Tools {
		found = found || tv.Tool == "tmux"
	}
	if !found || inv.Integrations["hive"].Health != "unavailable" || inv.CurrentGeneration == "" && len(inv.ConfigRoots) == 0 {
		t.Fatalf("merged inventory = %+v", inv)
	}
}

func TestCodexOverlayDefaultArgs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("CODEX_HOME", "")
	in := &integrations{ctl: &runController{}, rep: &reportBridge{}, grants: &pushedGrants{active: map[grantKey]bool{}}}
	cfg := &daemon.Config{NodeID: "node_01TESTNODE00000000000000"}
	w := wireEnvironment(in, cfg, runOpts{state: t.TempDir()}, "", "", "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	res, err := w.Resolve(context.Background(), daemon.EnvRequest{NodeID: cfg.NodeID, Harness: contracts.HarnessCodex, SessionID: "ses_1", RunID: "run_1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("codex default overlay args: %q", res.Args)
	if len(res.Args) != 0 {
		t.Fatalf("codex runs without a profile take no overlay args: %q", res.Args)
	}
}

func TestClaudeGetsPyreMCPAndGuide(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	in := &integrations{ctl: &runController{}, rep: &reportBridge{}, grants: &pushedGrants{active: map[grantKey]bool{}}}
	cfg := &daemon.Config{NodeID: "node_01TESTNODE00000000000000"}
	w := wireEnvironment(in, cfg, runOpts{state: t.TempDir()}, "", "", "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	w.pyreBin = "/opt/pyre/bin/pyre"
	res, err := w.Resolve(context.Background(), daemon.EnvRequest{NodeID: cfg.NodeID, Harness: contracts.HarnessClaude, SessionID: "ses_1", RunID: "run_1"})
	if err != nil {
		t.Fatal(err)
	}
	var mcpFile string
	for i, a := range res.Args {
		if a == "--settings" || strings.HasPrefix(a, "--settings=") {
			t.Fatalf("unexpected --settings in %q", res.Args)
		}
		if a == "--mcp-config" && i+1 < len(res.Args) {
			mcpFile = res.Args[i+1]
		}
		if strings.HasPrefix(a, "--mcp-config=") {
			mcpFile = strings.TrimPrefix(a, "--mcp-config=")
		}
	}
	b, err := os.ReadFile(mcpFile)
	if err != nil || !strings.Contains(string(b), "/opt/pyre/bin/pyre") || !strings.Contains(string(b), `"mcp"`) {
		t.Fatalf("mcp config %q: %s %v", mcpFile, b, err)
	}
	if g := cfg.AgentGuide(contracts.HarnessCodex, contracts.Session{}); g == "" || strings.Contains(g, "MCP tools") {
		t.Fatalf("codex guide should be CLI-only: %q", g)
	}
}

// A Claude run on a Pyre-managed (pinned) version must not run Claude's own
// native auto-updater: found on server, where a run pinned to 2.1.288 updated
// the owner's own install pointer to 2.1.289. External (unmanaged) claude
// keeps the owner's own updater behaviour.
func TestManagedClaudeRunsDisableAutoUpdater(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".claude"), 0o700)
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	in := &integrations{ctl: &runController{}, rep: &reportBridge{}, grants: &pushedGrants{active: map[grantKey]bool{}}}
	cfg := &daemon.Config{NodeID: "node_01TESTNODE00000000000000"}
	w := wireEnvironment(in, cfg, runOpts{state: t.TempDir()}, "", "", "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	res, err := w.Resolve(ctx, daemon.EnvRequest{NodeID: cfg.NodeID, Harness: contracts.HarnessClaude, SessionID: "ses_1", RunID: "run_1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Env["DISABLE_AUTOUPDATER"]; ok {
		t.Fatalf("no managed claude: auto-updater must be left alone: %v", res.Env)
	}
	src := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(src, []byte("#!/bin/sh\necho '9.9.9-synthetic (Claude Code)'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := w.upd.Stage(ctx, contracts.UpdateParams{Tool: "claude", Version: "9.9.9-synthetic", Source: src}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.upd.Activate(ctx, contracts.UpdateParams{Tool: "claude", Version: "9.9.9-synthetic"}); err != nil {
		t.Fatal(err)
	}
	res, err = w.Resolve(ctx, daemon.EnvRequest{NodeID: cfg.NodeID, Harness: contracts.HarnessClaude, SessionID: "ses_2", RunID: "run_2"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Env["DISABLE_AUTOUPDATER"] != "1" || res.RuntimeVersion != "9.9.9-synthetic" || !strings.Contains(res.Executable, "9.9.9-synthetic") {
		t.Fatalf("managed claude launch: exe %q version %q env %v", res.Executable, res.RuntimeVersion, res.Env)
	}
}

// Managed claude.* settings/hooks and Codex overlays are no longer refused:
// the plan applies cleanly, Claude launches get the generation's --settings
// (merged by the adapter into the run's own file) and Codex launches get -c
// overlays (turned into per-thread config by the adapter). Nothing is
// written to the owner's config homes.
func TestManagedClaudeSettingsAndCodexOverlaysResolve(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".claude"), 0o700)
	os.MkdirAll(filepath.Join(home, ".codex"), 0o700)
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	in := &integrations{ctl: &runController{}, rep: &reportBridge{}, grants: &pushedGrants{active: map[grantKey]bool{}}}
	cfg := &daemon.Config{NodeID: "node_01TESTNODE00000000000000"}
	w := wireEnvironment(in, cfg, runOpts{state: t.TempDir()}, "", "", "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	prof := configuration.Profile{Name: "default",
		Settings: map[string]any{"claude.env.SYNTHETIC_MANAGED": "p3", "codex.model_reasoning_effort": "low"},
		Hooks:    []configuration.HookSpec{{Harness: contracts.HarnessClaude, Event: "SessionStart", Command: "echo synthetic-managed-hook"}}}
	set, err := w.cfg.SetProfile(ctx, configuration.ProfileSetParams{Profile: prof})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range set.Plan.Conflicts {
		if c.Kind == "conflict" {
			t.Fatalf("plan conflict: %+v", c)
		}
	}
	if _, err := w.cfg.Apply(ctx, contracts.EnvApplyParams{PlanID: set.Plan.PlanID, ResultDigest: set.Plan.ResultDigest}); err != nil {
		t.Fatal(err)
	}
	cl, err := w.Resolve(ctx, daemon.EnvRequest{NodeID: cfg.NodeID, Harness: contracts.HarnessClaude, SessionID: "ses_1", RunID: "run_1"})
	if err != nil {
		t.Fatalf("claude launch refused: %v", err)
	}
	var settings string
	for _, a := range cl.Args {
		if strings.HasPrefix(a, "--settings=") {
			settings = strings.TrimPrefix(a, "--settings=")
		}
	}
	b, err := os.ReadFile(settings)
	if err != nil || !strings.Contains(string(b), "SYNTHETIC_MANAGED") || !strings.Contains(string(b), "synthetic-managed-hook") {
		t.Fatalf("claude overlay %q: %s %v (args %q)", settings, b, err, cl.Args)
	}
	cx, err := w.Resolve(ctx, daemon.EnvRequest{NodeID: cfg.NodeID, Harness: contracts.HarnessCodex, SessionID: "ses_2", RunID: "run_2"})
	if err != nil {
		t.Fatalf("codex launch refused: %v", err)
	}
	if strings.Join(cx.Args, " ") != `-c model_reasoning_effort="low"` {
		t.Fatalf("codex args = %q", cx.Args)
	}
	if ents, _ := os.ReadDir(filepath.Join(home, ".codex")); len(ents) != 0 {
		t.Fatalf("owner's ~/.codex written: %v", ents)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "settings.json")); !os.IsNotExist(err) {
		t.Fatalf("owner's ~/.claude/settings.json written: %v", err)
	}
}

// TestShellRunsGetGrantedConnections: a plain shell has no configuration
// overlay, but the default profile's connections that are granted for the
// session still reach LaunchPrep (credential provisioning into the shell).
func TestShellRunsGetGrantedConnections(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	in := &integrations{ctl: &runController{}, rep: &reportBridge{}, grants: &pushedGrants{active: map[grantKey]bool{}}}
	cfg := &daemon.Config{NodeID: "node_01TESTNODE00000000000000"}
	w := wireEnvironment(in, cfg, runOpts{state: t.TempDir()}, "", "", "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if w == nil {
		t.Fatal("environment wiring failed")
	}
	ctx := context.Background()
	sh, err := w.Resolve(ctx, daemon.EnvRequest{NodeID: cfg.NodeID, Harness: contracts.HarnessShell, SessionID: "ses_1"})
	if err != nil || len(sh.ConnectionGrants) != 0 {
		t.Fatalf("no profile: %+v %v", sh, err)
	}
	if err := w.cfg.Profiles().Save("", configuration.Profile{Name: "default", Connections: []string{"conn_synth-a", "conn_synth-b"}}); err != nil {
		t.Fatal(err)
	}
	in.grants.nodeID = cfg.NodeID // set by attach in production
	in.grants.record(contracts.ConnectionGrantParams{ConnectionID: "conn_synth-a", NodeID: cfg.NodeID, SessionID: "ses_1"}, true)
	sh, err = w.Resolve(ctx, daemon.EnvRequest{NodeID: cfg.NodeID, Harness: contracts.HarnessShell, SessionID: "ses_1"})
	if err != nil || len(sh.ConnectionGrants) != 1 || sh.ConnectionGrants[0] != "conn_synth-a" || sh.GenerationID != "" || len(sh.Args) != 0 {
		t.Fatalf("granted shell: %+v %v", sh, err)
	}
	other, err := w.Resolve(ctx, daemon.EnvRequest{NodeID: cfg.NodeID, Harness: contracts.HarnessShell, SessionID: "ses_2"})
	if err != nil || len(other.ConnectionGrants) != 0 {
		t.Fatalf("ungranted session got connections: %+v %v", other, err)
	}
	in.grants.record(contracts.ConnectionGrantParams{ConnectionID: "conn_synth-a", NodeID: cfg.NodeID, SessionID: "ses_1"}, false)
	sh, err = w.Resolve(ctx, daemon.EnvRequest{NodeID: cfg.NodeID, Harness: contracts.HarnessShell, SessionID: "ses_1"})
	if err != nil || len(sh.ConnectionGrants) != 0 {
		t.Fatalf("revoked grant still applied: %+v %v", sh, err)
	}
}

// Launch provisioning uses only explicit pushed grants, never the hub's
// no-grant owner default (decision I37): without a grant, nothing is
// injected into a run's environment.
func TestLaunchProvisioningNeedsAnExplicitGrant(t *testing.T) {
	in := &integrations{grants: &pushedGrants{active: map[grantKey]bool{}, nodeID: "node_01TESTNODE00000000000000"}}
	src := launchGrantSource(in)
	if _, ok := src.(*pushedGrants); !ok {
		t.Fatalf("launch grant source is %T, want the pushed-grant mirror", src)
	}
	got, err := src.GrantedConnections(context.Background(), configuration.GrantScope{SessionID: "ses_1"}, []string{"conn_synth-a"})
	if err != nil || len(got) != 0 {
		t.Fatalf("ungranted connection provisioned: %v %v", got, err)
	}
}
