package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

func TestTrustFuncOnlyConfiguredDirs(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "proj")
	os.MkdirAll(inside, 0o755)
	trust := trustFunc([]string{root})
	if !trust(root) || !trust(inside) {
		t.Fatal("configured dir not trusted")
	}
	if trust(root+"-other") || trust(t.TempDir()) {
		t.Fatal("unconfigured dir trusted")
	}
	if trustFunc(nil)(inside) {
		t.Fatal("default must trust nothing")
	}
}

func TestBuildAdaptersSharedSocket(t *testing.T) {
	state, err := os.MkdirTemp("/tmp", "pyre-ad-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(state)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	b, err := buildAdapters(runOpts{state: state, devMock: true}, "node_01TEST", log)
	if err != nil {
		t.Fatal(err)
	}
	names := map[contracts.Harness]string{}
	for _, a := range b.list {
		names[a.Harness()] = a.Name()
	}
	for _, h := range []contracts.Harness{contracts.HarnessClaude, contracts.HarnessCodex, contracts.HarnessShell, contracts.HarnessMock} {
		if names[h] == "" {
			t.Fatalf("missing %s adapter: %v", h, names)
		}
	}
	if b.codex == nil {
		t.Fatal("codex handle not kept for the accounts rate-limit reader")
	}
	long := "/tmp/" + strings.Repeat("x", 100)
	if _, err := buildAdapters(runOpts{state: long}, "node_01TEST", log); err == nil {
		t.Fatal("over-long socket path accepted")
	}
}

func TestDoorbellCommandGating(t *testing.T) {
	t.Setenv(EnvDoorbell, "")
	cmd, off := doorbellCommand(runOpts{}, "/abs/pyred", "/abs/pyre")
	if len(cmd) != 2 || cmd[0] != "/abs/pyred" || cmd[1] != "inbox-watch" || off != "" {
		t.Fatalf("enabled: %v %q", cmd, off)
	}
	if cmd, off := doorbellCommand(runOpts{noDoorbell: true}, "/abs/pyred", "/abs/pyre"); cmd != nil || off == "" {
		t.Fatalf("--no-doorbell: %v %q", cmd, off)
	}
	if cmd, off := doorbellCommand(runOpts{}, "/abs/pyred", ""); cmd != nil || !strings.Contains(off, "pyre CLI") {
		t.Fatalf("no CLI: %v %q", cmd, off)
	}
	for _, v := range []string{"0", "false", "OFF"} {
		t.Setenv(EnvDoorbell, v)
		if cmd, _ := doorbellCommand(runOpts{}, "/abs/pyred", "/abs/pyre"); cmd != nil {
			t.Fatalf("%s=%s: %v", EnvDoorbell, v, cmd)
		}
	}
}

// input.monitor is advertised only when the doorbell is actually enabled.
func TestClaudeAdvertisesInputMonitorOnlyWhenEnabled(t *testing.T) {
	state, err := os.MkdirTemp("/tmp", "pyre-ad-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(state)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	caps := func(o runOpts) ([]string, builtAdapters) {
		t.Helper()
		b, err := buildAdapters(o, "node_01TEST", log)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range b.list {
			if a.Harness() == contracts.HarnessClaude {
				type capper interface{ Capabilities() []string }
				return a.(capper).Capabilities(), b
			}
		}
		t.Fatal("no claude adapter")
		return nil, b
	}
	t.Setenv(EnvDoorbell, "0")
	if c, b := caps(runOpts{state: state}); contracts.HasCap(c, contracts.CapInputMonitor) || b.doorbell != nil {
		t.Fatalf("disabled doorbell advertised: %v", c)
	}
	t.Setenv(EnvDoorbell, "")
	c, b := caps(runOpts{state: state})
	if (findPyreCLI() != "") != contracts.HasCap(c, contracts.CapInputMonitor) {
		t.Fatalf("input.monitor=%v but pyre CLI %q (off: %q)", contracts.HasCap(c, contracts.CapInputMonitor), findPyreCLI(), b.doorbellOff)
	}
	if c, _ := caps(runOpts{state: state, noDoorbell: true}); contracts.HasCap(c, contracts.CapInputMonitor) {
		t.Fatal("--no-doorbell advertised input.monitor")
	}
}
