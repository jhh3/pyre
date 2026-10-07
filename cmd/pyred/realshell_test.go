package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/daemon"
	"github.com/jhh3/pyre/internal/supervisor"
)

func cmdOf(kind contracts.CommandKind, payload any) contracts.NodeCommand {
	b, _ := json.Marshal(payload)
	return contracts.NodeCommand{CommandID: contracts.NewID(contracts.PrefixCommand), Kind: kind,
		Actor: contracts.Principal{Kind: contracts.PrincipalOwner, ID: "cli_test"}, IssuedAt: time.Now(), Payload: b}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestRealShellAdapterLifecycle drives pyred's daemon with the real WP07
// shell adapter on the shared isolated tmux server (<state>/tmux/pyre.sock):
// start a projectless shell session, attach descriptor, survive a pyred
// restart (recovered, not relaunched), stop. Never touches the user's tmux.
func TestRealShellAdapterLifecycle(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	state, err := os.MkdirTemp("/tmp", "pyre-rs-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		tm := supervisor.Tmux{Socket: filepath.Join(state, "tmux", "pyre.sock"), OwnedRoot: state}
		tm.Kill(context.Background())
		os.RemoveAll(state)
	})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	open := func() *daemon.Daemon {
		built, err := buildAdapters(runOpts{state: state, sshAlias: "testhost"}, "node_01REALSHELL000000000000", log)
		if err != nil {
			t.Fatal(err)
		}
		d, err := daemon.New(daemon.Config{StateDir: state, NodeID: "node_01REALSHELL000000000000", Adapters: built.list, Logger: log,
			SSHAlias: "testhost", Poll: daemon.PollConfig{Interval: 100 * time.Millisecond, IdleMax: 400 * time.Millisecond}})
		if err != nil {
			t.Fatal(err)
		}
		if err := d.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		return d
	}
	d := open()
	cwd := t.TempDir()
	sc := contracts.StartRunCommand{Session: contracts.Session{SessionID: "ses_realshell", Harness: contracts.HarnessShell, Title: "real shell"},
		RunID: "run_realshell", Cwd: cwd, AccountPolicy: contracts.AccountPolicy{Mode: contracts.AccountEligible}}
	res, err := d.HandleCommand(context.Background(), cmdOf(contracts.CmdStartRun, sc))
	if err != nil || res.Error != nil {
		t.Fatalf("start_run: %v %+v", err, res.Error)
	}
	d.WaitLaunches()
	tm := supervisor.Tmux{Socket: filepath.Join(state, "tmux", "pyre.sock"), OwnedRoot: state}
	waitUntil(t, "tmux session for the run", func() bool { ok, _ := tm.HasSession(context.Background(), "run_realshell"); return ok })
	waitUntil(t, "run observed live", func() bool {
		runs, _ := d.ActiveRuns(context.Background())
		return len(runs) == 1
	})
	st, _ := os.Stat(filepath.Join(state, "runs", "run_realshell"))
	if st == nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("run state dir = %v", st)
	}
	pid, err := tm.ServerPID(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	att, _ := d.HandleCommand(context.Background(), cmdOf(contracts.CmdAttach, contracts.AttachCommand{SessionID: "ses_realshell", ClientNodeID: "node_other"}))
	var desc contracts.AttachDescriptor
	json.Unmarshal(att.Result, &desc)
	if att.Error != nil || desc.Mechanism != contracts.AttachSSHTmux || !strings.Contains(strings.Join(desc.Command, " "), "testhost") {
		t.Fatalf("attach = %+v %+v", desc, att.Error)
	}
	if r, _ := d.HandleCommand(context.Background(), cmdOf(contracts.CmdInterrupt, contracts.RunCommand{SessionID: "ses_realshell"})); r.Outcome != contracts.OutcomeUnsupported {
		t.Fatalf("shell interrupt = %+v", r)
	}

	// pyred restart: the shell survives and is recovered, not relaunched.
	d.Close()
	if !supervisor.Alive(pid) {
		t.Fatal("tmux server died with pyred")
	}
	d = open()
	defer d.Close()
	runs, _ := d.ActiveRuns(context.Background())
	if len(runs) != 1 || runs[0].RunID != "run_realshell" {
		t.Fatalf("after restart = %+v", runs)
	}

	res, err = d.HandleCommand(context.Background(), cmdOf(contracts.CmdStop, contracts.RunCommand{SessionID: "ses_realshell"}))
	if err != nil || res.Error != nil {
		t.Fatalf("stop: %v %+v", err, res.Error)
	}
	waitUntil(t, "run ended", func() bool { r, _ := d.ActiveRuns(context.Background()); return len(r) == 0 })
	waitUntil(t, "tmux session removed", func() bool { ok, _ := tm.HasSession(context.Background(), "run_realshell"); return !ok })
}
