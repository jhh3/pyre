package daemon

import (
	"os"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

// Every run, whatever its harness, learns this machine's Pyre name
// ($PYRE_NODE_NAME) next to its node ID: an agent seeing the OS hostname
// or Tailscale name must not conclude it is on another machine.
func TestRunEnvNamesTheNode(t *testing.T) {
	d := &Daemon{nodeID: "node_synth-office2", cfg: Config{StateDir: t.TempDir(), PyreBinDir: "/opt/synth/bin", NodeName: "synth-office2"}}
	for _, h := range []contracts.Harness{contracts.HarnessClaude, contracts.HarnessCodex, contracts.HarnessAgy, contracts.HarnessShell} {
		cmd := contracts.StartRunCommand{RunID: "run_synth-" + string(h), Session: contracts.Session{SessionID: "ses_synth-" + string(h), Harness: h}}
		env := d.runEnv(nil, cmd, t.TempDir())
		if env["PYRE_NODE_ID"] != "node_synth-office2" || env["PYRE_NODE_NAME"] != "synth-office2" {
			t.Fatalf("%s env: node id %q name %q", h, env["PYRE_NODE_ID"], env["PYRE_NODE_NAME"])
		}
	}
	// Without --name the node is listed by its OS hostname, so runs say that.
	d.cfg.NodeName = ""
	host, _ := os.Hostname()
	cmd := contracts.StartRunCommand{RunID: "run_synth-noname", Session: contracts.Session{SessionID: "ses_synth-noname", Harness: contracts.HarnessClaude}}
	if env := d.runEnv(nil, cmd, t.TempDir()); env["PYRE_NODE_NAME"] != host {
		t.Fatalf("unnamed node: PYRE_NODE_NAME %q, want hostname %q", env["PYRE_NODE_NAME"], host)
	}
}
