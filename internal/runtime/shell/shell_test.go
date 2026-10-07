package shell

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/tmux"
)

func newTestAdapter(t *testing.T) (*Adapter, string) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	dir, err := os.MkdirTemp("", "ps")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	a, err := New(Options{NodeID: "node_a", TmuxSocket: filepath.Join(dir, "tmux", "t.sock"), SSHAlias: "nodea"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.srv.KillServer(context.Background()) })
	return a, dir
}

func eventually(t *testing.T, d time.Duration, f func() bool) bool {
	t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if f() {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return f()
}

// A plain shell executable without args starts as a login shell, so the
// user's ~/.profile (PATH etc.) applies, as the package doc promises.
func TestPlainShellIsLoginShell(t *testing.T) {
	a, dir := newTestAdapter(t)
	ctx := context.Background()
	home := filepath.Join(dir, "home")
	marker := filepath.Join(dir, "profile-ran")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".profile"), []byte("echo yes > '"+marker+"'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Launch(ctx, contracts.ResolvedLaunch{RunID: "run_login", SessionID: "ses_login", Harness: contracts.HarnessShell, Cwd: dir,
		Executable: "/bin/sh", StateDir: filepath.Join(dir, "runs", "run_login"), Env: map[string]string{"HOME": home, "ENV": ""}}); err != nil {
		t.Fatal(err)
	}
	if !eventually(t, 5*time.Second, func() bool { _, err := os.Stat(marker); return err == nil }) {
		t.Fatal("~/.profile did not run: shell not started as a login shell")
	}
}

func TestShellLifecycle(t *testing.T) {
	a, dir := newTestAdapter(t)
	ctx := context.Background()
	pr, err := a.Probe(ctx)
	if err != nil || !pr.Available {
		t.Fatalf("probe %+v %v", pr, err)
	}
	l := contracts.ResolvedLaunch{RunID: "run_shell1", SessionID: "ses_1", Harness: contracts.HarnessShell, Cwd: dir,
		Executable: "/bin/sh", Args: []string{"-c", "echo shell-ready; exec cat"}, StateDir: filepath.Join(dir, "runs", "run_shell1"),
		Env: map[string]string{"PYRE_SESSION_ID": "ses_1"}}
	h, err := a.Launch(ctx, l)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Launch(ctx, l); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("duplicate launch: %v", err)
	}
	var obs contracts.Observation
	if !eventually(t, 3*time.Second, func() bool {
		obs, err = a.Observe(ctx, h, "")
		return err == nil && obs.Activity == "shell-ready"
	}) {
		t.Fatalf("observe %+v %v", obs, err)
	}
	if obs.State != contracts.RunRunning || !strings.Contains(obs.StateReason, "changed") {
		t.Fatalf("state %+v", obs)
	}
	obs2, _ := a.Observe(ctx, h, obs.NextCursor)
	if !strings.Contains(obs2.StateReason, "unchanged") {
		t.Fatalf("second observe %+v", obs2)
	}
	// Agent actions are unsupported, never typed.
	if _, err := a.Deliver(ctx, h, contracts.DeliveryInput{MessageID: "msg_1", Text: "x"}); !contracts.IsCode(err, contracts.ErrUnsupported) {
		t.Fatalf("deliver: %v", err)
	}
	if _, err := a.Interrupt(ctx, h); !contracts.IsCode(err, contracts.ErrUnsupported) {
		t.Fatalf("interrupt: %v", err)
	}
	if _, err := a.Answer(ctx, h, contracts.Approval{}, "allow", ""); !contracts.IsCode(err, contracts.ErrUnsupported) {
		t.Fatalf("answer: %v", err)
	}
	if _, _, err := a.ResumeSpec(ctx, contracts.Run{}, true); !contracts.IsCode(err, contracts.ErrUnsupported) {
		t.Fatalf("resume: %v", err)
	}
	// Attach descriptors.
	d, err := a.Attach(ctx, h, contracts.AttachCommand{RunID: h.RunID})
	if err != nil || d.Mechanism != contracts.AttachLocalTmux || d.Command[len(d.Command)-1] != "=run_shell1" {
		t.Fatalf("local attach %+v %v", d, err)
	}
	d, err = a.Attach(ctx, h, contracts.AttachCommand{RunID: h.RunID, ClientNodeID: "node_b", ReadOnly: true})
	if err != nil || d.Mechanism != contracts.AttachSSHTmux || d.Command[2] != "nodea" || !strings.Contains(d.Command[3], "-r") || !d.ReadOnly {
		t.Fatalf("remote attach %+v %v", d, err)
	}
	// The local attach argv really attaches: run it in a pty provided by an
	// outer isolated tmux server and check list-clients on the inner one.
	d, _ = a.Attach(ctx, h, contracts.AttachCommand{RunID: h.RunID})
	outer, err := tmux.NewServer("", filepath.Join(dir, "outer.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer outer.KillServer(ctx)
	if err := outer.NewSession(ctx, tmux.SessionSpec{Name: "outer", Argv: d.Command}); err != nil {
		t.Fatal(err)
	}
	if !eventually(t, 3*time.Second, func() bool {
		cs, _ := a.srv.ListClients(ctx, h.TmuxTarget)
		return tmux.WritableClients(cs) == 1
	}) {
		t.Fatal("attach via argv did not register a client")
	}
	_ = outer.KillServer(ctx)
	if !eventually(t, 3*time.Second, func() bool {
		cs, _ := a.srv.ListClients(ctx, h.TmuxTarget)
		return len(cs) == 0
	}) {
		t.Fatal("client did not detach")
	}
	// Recover finds the live run without relaunching.
	h2, err := a.Recover(ctx, h)
	if err != nil || h2.PID != h.PID {
		t.Fatalf("recover %+v %v", h2, err)
	}
	r, err := a.Stop(ctx, h)
	if err != nil || r.Outcome != contracts.OutcomeSucceeded {
		t.Fatalf("stop %+v %v", r, err)
	}
	obs, _ = a.Observe(ctx, h, "")
	if obs.State != contracts.RunExited {
		t.Fatalf("after stop %+v", obs)
	}
	if _, err := a.Recover(ctx, h); !contracts.IsCode(err, contracts.ErrNotFound) {
		t.Fatalf("recover after stop: %v", err)
	}
}

func TestShellExitCode(t *testing.T) {
	a, dir := newTestAdapter(t)
	ctx := context.Background()
	h, err := a.Launch(ctx, contracts.ResolvedLaunch{RunID: "run_shell2", Cwd: dir, Executable: "/bin/sh", Args: []string{"-c", "exit 3"},
		StateDir: filepath.Join(dir, "runs", "2")})
	if err != nil {
		t.Fatal(err)
	}
	var obs contracts.Observation
	if !eventually(t, 3*time.Second, func() bool {
		obs, _ = a.Observe(ctx, h, "")
		return obs.State == contracts.RunExited
	}) || obs.ExitCode == nil || *obs.ExitCode != 3 {
		t.Fatalf("%+v", obs)
	}
	// Exit status survives the session being killed.
	_, _ = a.Stop(ctx, h)
	obs, _ = a.Observe(ctx, h, "")
	if obs.State != contracts.RunExited || obs.ExitCode == nil || *obs.ExitCode != 3 {
		t.Fatalf("after kill %+v", obs)
	}
}
