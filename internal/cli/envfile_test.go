package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/daemon"
)

const envSecret = "synthetic-bridge-token-0002"

// pyre start --env-file sends only the path (the node reads the file);
// relative paths are refused; an older pyred (no
// launch.env_file) is refused before anything reaches the hub.
func TestStartEnvFile(t *testing.T) {
	if envFileArg != daemon.EnvFileArg {
		t.Fatalf("CLI %q != node %q", envFileArg, daemon.EnvFileArg)
	}
	node := c.Node{NodeID: "node_synth-mac", Name: "synth-mac", Connection: c.NodeConnection{State: c.ConnConnected},
		Harnesses: []c.HarnessInfo{
			{Harness: c.HarnessClaude, Available: true, Capabilities: []string{c.CapLaunchEnvFile}},
			{Harness: c.HarnessCodex, Available: true, Capabilities: []string{c.CapLaunchEnvFile}},
		}}
	api := &startCapture{nodes: []c.Node{node}}
	o := &Ops{API: api, Self: Identity{NodeID: "node_synth-mac"}}
	ctx := context.Background()
	if _, err := o.StartSession(ctx, StartParams{Harness: "claude", Cwd: "/synthetic", EnvFile: "/Users/synthetic/.config/aide.env"}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(api.req.Args, []string{"--pyre-env-file=/Users/synthetic/.config/aide.env"}) {
		t.Fatalf("args %q", api.req.Args)
	}
	api.req = c.StartSessionRequest{}
	if _, err := o.StartSession(ctx, StartParams{Harness: "codex", Cwd: "/synthetic", EnvFile: `C:\Users\synthetic\aide.env`}); err != nil {
		t.Fatal(err)
	}
	api.req = c.StartSessionRequest{}
	for _, p := range []StartParams{
		{Harness: "claude", Cwd: "/synthetic", EnvFile: "aide.env"},
		{Harness: "claude", Cwd: "/synthetic", EnvFile: "~/aide.env"},
	} {
		if _, err := o.StartSession(ctx, p); err == nil || ExitCodeFor(err) != 2 {
			t.Errorf("%+v: err %v", p, err)
		}
	}
	api.nodes[0].Harnesses[0].Capabilities = nil
	if _, err := o.StartSession(ctx, StartParams{Harness: "claude", Cwd: "/synthetic", EnvFile: "/synthetic/aide.env"}); err == nil || !strings.Contains(err.Error(), "older version") {
		t.Fatalf("old node: %v", err)
	}
	if api.req.Harness != "" {
		t.Fatal("a refused start reached the hub")
	}
}

// --dry-run shows an env file readable here with its values redacted.
func TestStartDryRunRedactsEnvFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "aide.env")
	if err := os.WriteFile(p, []byte("AGENT_BRIDGE_TOKEN="+envSecret+"\nAGENT_INTAKE_URL=http://127.0.0.1:8091\nANTHROPIC_API_KEY=\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, jsonOut := range []bool{true, false} {
		var out bytes.Buffer
		a := &App{Stdout: &out, jsonOut: jsonOut}
		if err := a.startDryRun(StartParams{Harness: "claude", Node: "laptop", EnvFile: p}); err != nil {
			t.Fatal(err)
		}
		s := out.String()
		if strings.Contains(s, envSecret) || strings.Contains(s, "127.0.0.1:8091") {
			t.Fatalf("dry run shows a value: %s", s)
		}
		if jsonOut {
			var plan StartPlan
			if err := json.Unmarshal(out.Bytes(), &plan); err != nil || plan.EnvFile != p {
				t.Fatalf("plan %+v %v", plan, err)
			}
			s = strings.Join(plan.EnvFileHere, "\n")
		}
		if !strings.Contains(s, "AGENT_BRIDGE_TOKEN=<redacted>") || !strings.Contains(s, "ANTHROPIC_API_KEY (unset)") {
			t.Fatalf("dry run: %s", s)
		}
	}
	// Not on this machine: the node checks it; a refused key here fails.
	var out bytes.Buffer
	a := &App{Stdout: &out, jsonOut: true}
	if err := a.startDryRun(StartParams{Harness: "claude", EnvFile: filepath.Join(t.TempDir(), "absent.env")}); err != nil || !strings.Contains(out.String(), "not on this machine") {
		t.Fatalf("absent: %v %s", err, out.String())
	}
	if err := os.WriteFile(p, []byte("ANTHROPIC_API_KEY="+envSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.startDryRun(StartParams{Harness: "claude", EnvFile: p}); err == nil || strings.Contains(err.Error(), envSecret) {
		t.Fatalf("refused key: %v", err)
	}
}

type resumeCapture struct {
	c.API
	req c.ResumeRequest
}

func (r *resumeCapture) ResumeSession(_ context.Context, req c.ResumeRequest) (c.StartSessionResponse, error) {
	r.req = req
	return c.StartSessionResponse{}, nil
}

// pyre resume --env-file PATH|none sends the path or none; anything else
// (relative, network paths) is refused locally.
func TestResumeEnvFile(t *testing.T) {
	api := &resumeCapture{}
	o := &Ops{API: api}
	sid := c.NewID(c.PrefixSession)
	for _, ok := range []string{"/Users/synthetic/aide.env", "none", `C:\Users\synthetic\aide.env`} {
		if _, err := o.Resume(context.Background(), ResumeParams{Session: sid, EnvFile: ok}); err != nil || api.req.EnvFile != ok {
			t.Fatalf("%s: %v %q", ok, err, api.req.EnvFile)
		}
	}
	api.req = c.ResumeRequest{}
	for _, bad := range []string{"aide.env", `\\server\share\aide.env`, "//server/share/aide.env", `\\.\pipe\x`} {
		if _, err := o.Resume(context.Background(), ResumeParams{Session: sid, EnvFile: bad}); err == nil || ExitCodeFor(err) != 2 || api.req.SessionID != "" {
			t.Fatalf("%s accepted: %v", bad, err)
		}
	}
}

// Dry run: a path for another OS is left to the node; Claude's monitor
// blockers in the file are warned about.
func TestStartDryRunEnvFileOtherOSAndWarnings(t *testing.T) {
	var out bytes.Buffer
	a := &App{Stdout: &out, jsonOut: true}
	if runtime.GOOS != "windows" {
		if err := a.startDryRun(StartParams{Harness: "claude", EnvFile: `C:\Users\synthetic\aide.env`}); err != nil || !strings.Contains(out.String(), "another OS") {
			t.Fatalf("windows path from unix: %v %s", err, out.String())
		}
	}
	p := filepath.Join(t.TempDir(), "aide.env")
	if err := os.WriteFile(p, []byte("DISABLE_TELEMETRY=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := a.startDryRun(StartParams{Harness: "claude", EnvFile: p}); err != nil {
		t.Fatal(err)
	}
	var plan StartPlan
	if err := json.Unmarshal(out.Bytes(), &plan); err != nil || len(plan.EnvFileWarnings) != 1 || !strings.Contains(plan.EnvFileWarnings[0], "DISABLE_TELEMETRY") {
		t.Fatalf("plan %+v %v", plan, err)
	}
	if err := os.WriteFile(p, []byte("K=v\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.startDryRun(StartParams{Harness: "claude", EnvFile: p}); err == nil || !strings.Contains(err.Error(), "accessible to other users") {
		t.Fatalf("0644: %v", err)
	}
}
