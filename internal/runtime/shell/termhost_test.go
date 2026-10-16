//go:build !windows

package shell

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/hostsel"
	"github.com/jhh3/pyre/internal/runtime/sessionhost"
	"github.com/jhh3/pyre/internal/runtime/termhost"
	_ "github.com/jhh3/pyre/internal/runtime/termhost/hostproc" // this test binary serves __pyre_termhost
)

// TestShellOnTermhost runs the adapter on the Pyre terminal host (the
// Windows backend; a real PTY here): launch, observe, attach descriptor,
// direct attach, recover, stop, exit status.
func TestShellOnTermhost(t *testing.T) {
	t.Setenv(hostsel.Env, hostsel.KindTermhost)
	dir, err := os.MkdirTemp("/tmp", "pt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	a, err := New(Options{NodeID: "node_a", TmuxSocket: filepath.Join(dir, "tmux", "t.sock"), SSHAlias: "nodea"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.srv.KillServer(context.Background()) })
	if a.srv.Kind() != hostsel.KindTermhost {
		t.Fatalf("kind %s", a.srv.Kind())
	}
	ctx := context.Background()
	pr, err := a.Probe(ctx)
	if err != nil || !pr.Available {
		t.Fatalf("probe %+v %v", pr, err)
	}
	h, err := a.Launch(ctx, contracts.ResolvedLaunch{RunID: "run_th1", SessionID: "ses_1", Harness: contracts.HarnessShell, Cwd: dir,
		Executable: "/bin/sh", Args: []string{"-c", "echo shell-ready; exec cat"}, StateDir: filepath.Join(dir, "runs", "run_th1"),
		Env: map[string]string{"PYRE_SESSION_ID": "ses_1"}})
	if err != nil {
		t.Fatal(err)
	}
	if h.TmuxSocket != hostsel.TermhostSocket(filepath.Join(dir, "tmux", "t.sock")) {
		t.Fatalf("handle socket %s", h.TmuxSocket)
	}
	var obs contracts.Observation
	if !eventually(t, 3*time.Second, func() bool {
		obs, err = a.Observe(ctx, h, "")
		return err == nil && obs.Activity == "shell-ready"
	}) {
		t.Fatalf("observe %+v %v", obs, err)
	}
	d, err := a.Attach(ctx, h, contracts.AttachCommand{RunID: h.RunID})
	ta, ok := termhost.ParseAttachArgv(d.Command)
	if err != nil || d.Mechanism != contracts.AttachLocalTmux || !ok || ta.Name != "run_th1" {
		t.Fatalf("local attach %+v %v", d, err)
	}
	c, err := termhost.DialAttach(ctx, termhost.DialOptions{Socket: ta.Socket, Name: ta.Name, Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	if !eventually(t, 3*time.Second, func() bool {
		cs, _ := a.srv.ListClients(ctx, h.TmuxTarget)
		return sessionhost.WritableClients(cs) == 1
	}) {
		t.Fatal("attach did not register a writable client")
	}
	c.Close()
	if !eventually(t, 3*time.Second, func() bool {
		cs, _ := a.srv.ListClients(ctx, h.TmuxTarget)
		return len(cs) == 0
	}) {
		t.Fatal("client did not detach")
	}
	h2, err := a.Recover(ctx, h)
	if err != nil || h2.PID != h.PID {
		t.Fatalf("recover %+v %v", h2, err)
	}
	r, err := a.Stop(ctx, h)
	if err != nil || r.Outcome != contracts.OutcomeSucceeded {
		t.Fatalf("stop %+v %v", r, err)
	}
	if _, err := a.Recover(ctx, h); !contracts.IsCode(err, contracts.ErrNotFound) {
		t.Fatalf("recover after stop: %v", err)
	}
	h3, err := a.Launch(ctx, contracts.ResolvedLaunch{RunID: "run_th2", Cwd: dir, Executable: "/bin/sh", Args: []string{"-c", "exit 3"},
		StateDir: filepath.Join(dir, "runs", "2")})
	if err != nil {
		t.Fatal(err)
	}
	if !eventually(t, 3*time.Second, func() bool {
		obs, _ = a.Observe(ctx, h3, "")
		return obs.State == contracts.RunExited
	}) || obs.ExitCode == nil || *obs.ExitCode != 3 {
		t.Fatalf("%+v", obs)
	}
}
