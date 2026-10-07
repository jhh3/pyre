package daemon

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/supervisor"
)

// TestPlainShellSession: start, attach (local and remote descriptors),
// agent actions unsupported, stop; uses a real isolated tmux server.
func TestPlainShellSession(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	dir := shortDir(t)
	n := startNode(t, dir, Config{Adapters: []contracts.RuntimeAdapter{&tmuxShell{}}, SSHAlias: "testhost"})
	cwd := t.TempDir()
	sc := startCmd("ses_sh", "run_sh", contracts.HarnessShell, cwd)
	sc.SessionToken = "pyre_s_shell_token_000000000000000000"
	run := n.start(sc)
	rec := n.run("run_sh")
	if rec.Phase != phaseLive || rec.Handle == nil || rec.Run.State != contracts.RunWaitingInput {
		t.Fatalf("shell run = %+v %s", rec.Run, rec.Phase)
	}
	if _, ok := run.AdapterData["pyre.account"]; ok {
		t.Fatal("shell run must not claim an account binding")
	}
	tm := supervisor.Tmux{Socket: rec.Handle.TmuxSocket, OwnedRoot: dir}
	t.Cleanup(func() { tm.Kill(context.Background()) })
	// The run environment reached the shell (token via env, not argv).
	tm.SendKeys(context.Background(), "pyre", true, `echo "sid=$PYRE_SESSION_ID run=$PYRE_RUN_ID tok=${#PYRE_SESSION_TOKEN}"`)
	tm.SendKeys(context.Background(), "pyre", false, "Enter")
	waitFor(t, "env echo", func() bool {
		out, _ := tm.Capture(context.Background(), "pyre", 0)
		return strings.Contains(out, "sid=ses_sh run=run_sh tok=37")
	})

	res := n.do(command(contracts.CmdAttach, contracts.AttachCommand{SessionID: "ses_sh"}))
	var local contracts.AttachDescriptor
	json.Unmarshal(res.Result, &local)
	if res.Error != nil || local.Mechanism != contracts.AttachLocalTmux || local.Command[len(local.Command)-1] != "pyre" {
		t.Fatalf("local attach = %+v %v", local, res.Error)
	}
	res = n.do(command(contracts.CmdAttach, contracts.AttachCommand{SessionID: "ses_sh", ClientNodeID: "node_laptop"}))
	var remote contracts.AttachDescriptor
	json.Unmarshal(res.Result, &remote)
	if remote.Mechanism != contracts.AttachSSHTmux || remote.Command[0] != "ssh" || remote.Command[2] != "testhost" || !strings.Contains(remote.Command[3], "attach-session") {
		t.Fatalf("remote attach = %+v", remote)
	}
	// Agent actions are unsupported; never typed into the prompt.
	if res := n.do(command(contracts.CmdInterrupt, contracts.RunCommand{SessionID: "ses_sh"})); res.Outcome != contracts.OutcomeUnsupported {
		t.Fatalf("interrupt = %+v", res)
	}
	for _, mode := range []contracts.MessageMode{contracts.ModeQueue, contracts.ModeSteer} {
		id := "msg_" + string(mode)
		n.do(command(contracts.CmdDeliver, contracts.DeliverCommand{SessionID: "ses_sh", Message: contracts.MessageEnvelope{SchemaVersion: 1, MessageID: id,
			Sender: contracts.Principal{Kind: contracts.PrincipalOwner, ID: "o"}, Recipient: contracts.Address{Kind: contracts.AddrSession, ID: "ses_sh"},
			Mode: mode, Body: contracts.MessageBody{Text: "echo SHOULD_NOT_APPEAR"}}}))
	}
	n.d.engine.ProcessSession(context.Background(), "ses_sh", true)
	q, _ := n.d.inbox.Get(context.Background(), "msg_queue")
	s, _ := n.d.inbox.Get(context.Background(), "msg_steer")
	if q.State != contracts.DeliveryQueued || s.State != contracts.DeliveryRejected {
		t.Fatalf("shell delivery: queue=%s (%s) steer=%s (%s)", q.State, q.Reason, s.State, s.Reason)
	}
	time.Sleep(100 * time.Millisecond)
	if out, _ := tm.Capture(context.Background(), "pyre", 0); strings.Contains(out, "SHOULD_NOT_APPEAR") {
		t.Fatal("message text was typed into the shell")
	}
	// Restart pyred: the shell survives and is recovered.
	pid, _ := tm.ServerPID(context.Background())
	n = n.restart()
	if r := n.run("run_sh"); r.Phase != phaseLive || !supervisor.Alive(pid) {
		t.Fatalf("after restart = %+v alive=%v", r.Run, supervisor.Alive(pid))
	}
	// Stop ends the run and removes only Pyre's tmux server.
	if res := n.do(command(contracts.CmdStop, contracts.RunCommand{SessionID: "ses_sh"})); res.Error != nil {
		t.Fatalf("stop = %+v", res.Error)
	}
	if r := n.run("run_sh"); r.Phase != phaseEnded || r.Run.State != contracts.RunExited {
		t.Fatalf("after stop = %+v %s", r.Run, r.Phase)
	}
	if _, err := os.Stat(rec.Handle.TmuxSocket); !os.IsNotExist(err) {
		t.Fatal("tmux socket left behind")
	}
}
