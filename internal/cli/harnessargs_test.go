package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/daemon"
)

// pyre start -- ARGS / --harness-arg / MCP harness_args reach the node in
// canonical form after Pyre's own args; refused flags fail before any
// request; --native-resume travels as the node's adoption arg.
func TestStartHarnessArgs(t *testing.T) {
	if nativeResumeArg != daemon.NativeResumeArg {
		t.Fatalf("CLI %q != node %q", nativeResumeArg, daemon.NativeResumeArg)
	}
	node := c.Node{NodeID: "node_synth-mac", Name: "synth-mac", Connection: c.NodeConnection{State: c.ConnConnected},
		Harnesses: []c.HarnessInfo{{Harness: c.HarnessClaude, Available: true, Capabilities: []string{c.CapLaunchHarnessArgs, c.CapResumeOutside}}}}
	api := &startCapture{nodes: []c.Node{node}}
	o := &Ops{API: api, Self: Identity{NodeID: "node_synth-mac"}}
	ctx := context.Background()
	sid := "0b7c1f3e-1111-4222-8333-444455556666"
	_, err := o.StartSession(ctx, StartParams{Harness: "claude", Cwd: "/synthetic", Permissions: "bypass", NativeResume: sid,
		HarnessArgs: []string{"--dangerously-load-development-channels", "plugin:sample-bridge@sample", "--append-system-prompt-file", "/synthetic/aide.md"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--pyre-permissions=bypass", "--dangerously-load-development-channels=plugin:sample-bridge@sample",
		"--append-system-prompt-file=/synthetic/aide.md", "--pyre-native-resume=" + sid}
	if !slices.Equal(api.req.Args, want) {
		t.Fatalf("args %q", api.req.Args)
	}
	api.req = c.StartSessionRequest{}
	for _, p := range []StartParams{
		{Harness: "claude", Cwd: "/synthetic", HarnessArgs: []string{"--session-id", sid}},
		{Harness: "codex", Cwd: "/synthetic", HarnessArgs: []string{"-c", "model=x"}},
		{Harness: "codex", Cwd: "/synthetic", NativeResume: sid},
		{Harness: "claude", Cwd: "/synthetic", NativeResume: "not-a-uuid"},
	} {
		if _, err := o.StartSession(ctx, p); err == nil || ExitCodeFor(err) != 2 {
			t.Errorf("%+v: err %v", p, err)
		}
	}
	if api.req.Harness != "" {
		t.Fatal("a refused start reached the hub")
	}
	// An older pyred (no launch.harness_args / resume.outside) is refused
	// instead of being handed args it would pass to Claude unchecked.
	api.nodes[0].Harnesses[0].Capabilities = nil
	for _, p := range []StartParams{
		{Harness: "claude", Cwd: "/synthetic", HarnessArgs: []string{"--add-dir", "/x"}},
		{Harness: "claude", Cwd: "/synthetic", NativeResume: sid},
	} {
		if _, err := o.StartSession(ctx, p); err == nil || !strings.Contains(err.Error(), "older version") {
			t.Errorf("old node %+v: %v", p, err)
		}
	}
	if api.req.Harness != "" {
		t.Fatal("a start for an old node reached the hub")
	}
}

func TestStartDryRunPrintsCanonicalArgs(t *testing.T) {
	var out bytes.Buffer
	a := &App{Stdout: &out, jsonOut: true}
	err := a.startDryRun(StartParams{Harness: "claude", Node: "laptop", Permissions: "bypass",
		HarnessArgs: []string{"--dangerously-load-development-channels", "plugin:sample-bridge@sample", "--append-system-prompt-file", "/synthetic/aide.md"}})
	if err != nil {
		t.Fatal(err)
	}
	var plan StartPlan
	if err := json.Unmarshal(out.Bytes(), &plan); err != nil {
		t.Fatal(err, out.String())
	}
	line := strings.Join(plan.Argv, " ")
	if !strings.Contains(line, "--dangerously-skip-permissions") ||
		!strings.Contains(line, "--plugin-dir=<run>/pyre-plugin --dangerously-load-development-channels=plugin:sample-bridge@sample --append-system-prompt-file=/synthetic/aide.md --mcp-config=") {
		t.Fatalf("plan argv %q", plan.Argv)
	}
}

// A shell session's arguments are its command line, passed unchanged (no
// capability needed: every node runs `SHELL ARGS`).
func TestStartShellArgsAreTheCommandLine(t *testing.T) {
	node := c.Node{NodeID: "node_synth-linux", Name: "synth-linux", Connection: c.NodeConnection{State: c.ConnConnected},
		Harnesses: []c.HarnessInfo{{Harness: c.HarnessShell, Available: true}}}
	api := &startCapture{nodes: []c.Node{node}}
	o := &Ops{API: api, Self: Identity{NodeID: "node_synth-linux"}}
	if _, err := o.StartSession(context.Background(), StartParams{Harness: "shell", Cwd: "/synthetic", HarnessArgs: []string{"-lc", "echo 'synthetic run' > out"}}); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(api.req.Args, "-lc") || !slices.Contains(api.req.Args, "echo 'synthetic run' > out") {
		t.Fatalf("args %q", api.req.Args)
	}
}
