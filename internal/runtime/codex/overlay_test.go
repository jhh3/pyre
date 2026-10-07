package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

func TestSplitConfigArgs(t *testing.T) {
	rest, cfg, err := splitConfigArgs([]string{"-c", `model_reasoning_effort="high"`, "-m", "gpt-x",
		"-c", `mcp_servers.synthetic-alpha.args=["/x/synth.py", "alpha"]`, "--config=features.synthetic=true",
		"-c", "tool_output_token_limit=1234", "-c", "plain=not toml at all"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(rest, " ") != "-m gpt-x" {
		t.Fatalf("rest = %q", rest)
	}
	if cfg["model_reasoning_effort"] != "high" || cfg["features.synthetic"] != true || cfg["tool_output_token_limit"] != int64(1234) || cfg["plain"] != "not toml at all" {
		t.Fatalf("cfg = %#v", cfg)
	}
	if fmt.Sprint(cfg["mcp_servers.synthetic-alpha.args"]) != "[/x/synth.py alpha]" {
		t.Fatalf("array = %#v", cfg["mcp_servers.synthetic-alpha.args"])
	}
	for _, bad := range [][]string{{"-c"}, {"-c", "noequals"}, {"-c", "a=1", "-c", "a=2"}, {"-c", "a=1", "-c", "a.b=2"}, {"-c", ".a=1"}, {"-c", "a b=1"}} {
		if _, _, err := splitConfigArgs(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	// Managed keys never overlap the run's Pyre identity.
	pyre := threadConfig(map[string]string{"PYRE_SESSION_ID": "ses_x", "PYRE_CLI": "/synthetic/pyre"})
	for _, k := range []string{"mcp_servers.pyre", "mcp_servers.pyre.command", "shell_environment_policy.set", "shell_environment_policy"} {
		if _, err := mergeThreadConfig(pyre, map[string]any{k: "x"}); !contracts.IsCode(err, contracts.ErrConflict) {
			t.Errorf("%s: %v, want conflict", k, err)
		}
	}
	if m, err := mergeThreadConfig(pyre, map[string]any{"shell_environment_policy.set.SYNTHETIC": "1"}); err != nil || m["shell_environment_policy.set.SYNTHETIC"] != "1" || m["mcp_servers.pyre"] == nil {
		t.Fatalf("merge = %v %v", m, err)
	}
}

// The environment generation's -c overlays become per-thread config on
// thread/start (never app-server flags or CODEX_HOME edits), are persisted
// with the run, and are re-sent on resume after a pyred restart: a running
// thread keeps its generation.
func TestCodexManagedConfigPerThread(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	ctx := context.Background()
	work := filepath.Join(e.dir, "work")
	_ = os.MkdirAll(work, 0o755)
	env := map[string]string{"PYRE_SESSION_ID": "ses_x", "PYRE_CLI": "/synthetic/bin/pyre", "PYRE_SESSION_TOKEN": "synthetic-session-token-0000"}
	h, err := e.a.Launch(ctx, contracts.ResolvedLaunch{RunID: "run_m1", SessionID: "ses_x", Harness: contracts.HarnessCodex, Cwd: work,
		StateDir: filepath.Join(e.dir, "runs", "run_m1"), ProfileDir: e.home, Env: env,
		Args: []string{"-c", `mcp_servers.synthetic-alpha.command="/usr/bin/true"`, "-c", `model_reasoning_effort="low"`, "-s", "read-only"}})
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(e.o.StateRoot, "codex", homeKey(e.home), "fake-config.log")
	b, _ := os.ReadFile(logPath)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	method, raw, _ := strings.Cut(lines[0], " ")
	var cfg map[string]any
	if method != "thread/start" || json.Unmarshal([]byte(raw), &cfg) != nil {
		t.Fatalf("thread/start: %v", lines)
	}
	if cfg["mcp_servers.synthetic-alpha.command"] != "/usr/bin/true" || cfg["model_reasoning_effort"] != "low" || cfg["mcp_servers.pyre"] == nil {
		t.Fatalf("thread config = %v", cfg)
	}
	if starts(t, e, e.home) != 1 {
		t.Fatal("expected one app-server")
	}
	a2 := e.restart()
	if _, err := a2.Recover(ctx, h); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(logPath)
	lines = strings.Split(strings.TrimSpace(string(b)), "\n")
	if last := lines[len(lines)-1]; !strings.HasPrefix(last, "thread/resume ") || !strings.Contains(last, "synthetic-alpha") || !strings.Contains(last, `"model_reasoning_effort":"low"`) {
		t.Fatalf("resume lacks the managed config: %v", lines)
	}
	_, _ = a2.Stop(ctx, h)
}

// Codex runs pinned to different executables (a Codex version activated
// between two launches) get separate app-servers on the same CODEX_HOME:
// the running thread stays on its binary, the new run uses the new one.
func TestCodexRunsPinnedToTheirExecutable(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	ctx := context.Background()
	wrapper := filepath.Join(e.dir, "codex-v2")
	marker := filepath.Join(e.dir, "v2-ran")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\necho \"$1\" >>"+marker+"\nexec "+e.exe+" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(e.dir, "work")
	_ = os.MkdirAll(work, 0o755)
	launch := func(runID, exe string) contracts.RunHandle {
		h, err := e.a.Launch(ctx, contracts.ResolvedLaunch{RunID: runID, SessionID: "ses_" + runID, Harness: contracts.HarnessCodex, Cwd: work,
			StateDir: filepath.Join(e.dir, "runs", runID), ProfileDir: e.home, Executable: exe})
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	h1 := launch("run_v1", e.exe) // the adapter default: legacy home key
	h2 := launch("run_v2", wrapper)
	if h1.Data["app_server_socket"] == h2.Data["app_server_socket"] {
		t.Fatalf("both runs on one app-server: %v", h1.Data)
	}
	if starts(t, e, e.home) != 1 {
		t.Fatalf("default-binary server started %d times", starts(t, e, e.home))
	}
	k2 := serverKey(e.home, wrapper)
	if b, _ := os.ReadFile(filepath.Join(e.o.StateRoot, "codex", k2, "fake-starts.log")); strings.Count(string(b), "\n") != 1 {
		t.Fatalf("pinned server for %s not started (%q)", wrapper, b)
	}
	if b, _ := os.ReadFile(marker); strings.TrimSpace(string(b)) != "app-server" {
		t.Fatalf("pinned binary ran %q, want one app-server", b)
	}
	e.wait(e.a, h1, 5*time.Second, idle)
	e.wait(e.a, h2, 5*time.Second, idle)
	// After a pyred restart the run finds its own server again.
	a2 := e.restart()
	h2r, err := a2.Recover(ctx, h2)
	if err != nil {
		t.Fatal(err)
	}
	if h2r.Data["app_server_socket"] != h2.Data["app_server_socket"] && h2r.Data["app_server_socket"] != "" {
		t.Fatalf("recovered on another server: %v vs %v", h2r.Data, h2.Data)
	}
	if b, _ := os.ReadFile(filepath.Join(e.o.StateRoot, "codex", k2, "fake-starts.log")); strings.Count(string(b), "\n") != 1 {
		t.Fatalf("recover restarted the pinned server: %q", b)
	}
	// HomeBusy sees the pinned server even when the default one is gone.
	if _, err := a2.Stop(ctx, h1); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if busy, _ := a2.HomeBusy(ctx, e.home); !busy {
		t.Fatal("HomeBusy missed the pinned app-server")
	}
	if _, err := a2.Stop(ctx, h2); err != nil {
		t.Fatal(err)
	}
}
