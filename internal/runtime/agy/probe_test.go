//go:build !windows

package agy

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Probe: offered only where the agy binary runs and a sign-in exists, with
// the models `agy models` lists (default from settings.json) and agy's
// efforts.
func TestProbe(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "agy")
	script := "#!/bin/sh\ncase \"$1\" in\n--version) echo 1.4.2;;\nmodels) printf 'Fetching available models...\\nsynthetic-a-medium\\tSynthetic A (Medium)\\nsynthetic-b-low\\tSynthetic B (Low)\\n';;\nesac\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(dir, "home")
	_ = os.MkdirAll(home, 0o700)
	a, err := New(Options{NodeID: "node_t", TmuxSocket: filepath.Join(dir, "t.sock"), AgyBin: bin, AgyHome: home,
		WorkspaceRoot: filepath.Join(dir, "ws"), HookCommand: []string{"/bin/true"}})
	if err != nil {
		t.Fatal(err)
	}
	pr, _ := a.Probe(context.Background())
	if pr.Available || !strings.Contains(pr.Reason, "not signed in") || pr.Version != "1.4.2" {
		t.Fatalf("unsigned: %+v", pr)
	}
	// Presence only: the token's content is never read.
	_ = os.WriteFile(filepath.Join(home, tokenFile), []byte("synthetic-not-a-token"), 0o600)
	_ = os.WriteFile(filepath.Join(home, "settings.json"), []byte(`{"model":"Synthetic B (Low)"}`), 0o600)
	// Models arrive from a background refresh (agy models is a network
	// call; the probe stays fast).
	for i := 0; i < 50; i++ {
		if pr, _ = a.Probe(context.Background()); len(pr.Models) == 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Off Linux the probe says agy sessions are validated on Linux only.
	if !pr.Available || (pr.Reason != "" && runtime.GOOS == "linux") || pr.Harness != contracts.HarnessAgy || len(pr.Models) != 2 || pr.Models[0].Default || !pr.Models[1].Default || len(pr.Efforts) != 5 {
		t.Fatalf("signed in: %+v", pr)
	}
	if contracts.HasCap(pr.Capabilities, contracts.CapAccountProfile) || contracts.HasCap(pr.Capabilities, contracts.CapApprovals) ||
		contracts.HasCap(pr.Capabilities, contracts.CapInputSteer) || !contracts.HasCap(pr.Capabilities, contracts.CapResumeNative) {
		t.Fatalf("capabilities %v", pr.Capabilities)
	}
	// No agy installed: not offered.
	a2, _ := New(Options{NodeID: "node_t", TmuxSocket: filepath.Join(dir, "t2.sock"), AgyBin: filepath.Join(dir, "missing", "agy"), AgyHome: home,
		WorkspaceRoot: filepath.Join(dir, "ws"), HookCommand: []string{"/bin/true"}})
	if pr, _ := a2.Probe(context.Background()); pr.Available || !pr.Absent || !strings.Contains(pr.Reason, "not installed") {
		t.Fatalf("missing binary: %+v", pr)
	}
}
