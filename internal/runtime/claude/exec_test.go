//go:build !windows

package claude

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

func newExecTest(t *testing.T, homes ...string) (*ExecAdapter, *Adapter) {
	t.Helper()
	a, err := New(Options{NodeID: "node_t", TmuxSocket: execSock(t), HookCommand: []string{"true"},
		ProfileAsConfigDir: true, TranscriptHomes: homes})
	if err != nil {
		t.Fatal(err)
	}
	return NewExec(a), a
}

func writeTranscript(t *testing.T, home, cwd, sid string) {
	t.Helper()
	p := guessTranscriptPath(home, cwd, sid)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"type":"user","sessionId":"`+sid+`"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Which account a headless --resume must run on: any when the profiles
// share the transcript store, the profile's account when only it has the
// conversation, the default login when only ~/.claude has it.
func TestConversationOwner(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	shared := filepath.Join(root, "state", "shared", "claude")
	profiles := filepath.Join(root, "state", "profiles")
	pa := filepath.Join(profiles, "acct_synthA", "claude")
	pb := filepath.Join(profiles, "acct_synthB", "claude")
	_ = os.MkdirAll(filepath.Join(shared, "projects"), 0o700)
	_ = os.MkdirAll(pa, 0o700)
	if err := os.Symlink(filepath.Join(shared, "projects"), filepath.Join(pa, "projects")); err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(filepath.Join(pb, "projects"), 0o700) // own store
	_, a := newExecTest(t, shared, filepath.Join(profiles, "*", "claude*"))
	cwd := t.TempDir()
	sShared, sOwn, sDef := "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333"
	writeTranscript(t, shared, cwd, sShared)
	writeTranscript(t, pb, cwd, sOwn)
	writeTranscript(t, filepath.Join(home, ".claude"), cwd, sDef)
	if o := a.ConversationOwner(cwd, sShared); !o.Shared {
		t.Fatalf("shared: %+v", o)
	}
	if o := a.ConversationOwner(cwd, sOwn); o.Shared || o.AccountID != "acct_synthB" {
		t.Fatalf("own store: %+v", o)
	}
	if o := a.ConversationOwner(cwd, sDef); o.Shared || o.AccountID != "" || !o.Default {
		t.Fatalf("default login: %+v", o)
	}
	if o := a.ConversationOwner(cwd, "44444444-4444-4444-8444-444444444444"); o != (ConversationOwner{}) {
		t.Fatalf("missing: %+v", o)
	}
}

// The exec state machine: pending -> claimed -> running -> exited, the
// session id found from the only new transcript when the CLI saw none, a
// vanished CLI ending the run, and Stop signalling the child.
func TestExecAdapterLifecycle(t *testing.T) {
	x, _ := newExecTest(t)
	cfg, cwd := t.TempDir(), t.TempDir()
	ctx := context.Background()
	h, err := x.Launch(ctx, contracts.ResolvedLaunch{RunID: "run_e1", SessionID: "ses_e", Cwd: cwd, StateDir: filepath.Join(t.TempDir(), "r1"), ProfileDir: cfg})
	if err != nil {
		t.Fatal(err)
	}
	if obs, _ := x.Observe(ctx, h, ""); obs.State != contracts.RunStarting {
		t.Fatalf("pending = %+v", obs)
	}
	if err := x.Claimed(h, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if err := x.Claimed(h, os.Getpid()); err == nil {
		t.Fatal("claimed twice")
	}
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	if err := x.Started(h, child.Process.Pid, false); err != nil {
		t.Fatal(err)
	}
	if obs, _ := x.Observe(ctx, h, ""); obs.State != contracts.RunRunning {
		t.Fatalf("running = %+v", obs)
	}
	sid := "55555555-5555-4555-8555-555555555555"
	writeTranscript(t, cfg, cwd, sid)
	if _, err := x.Stop(ctx, h); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not end the child")
	}
	if err := x.Exited(h, nil, "SIGTERM", ""); err != nil {
		t.Fatal(err)
	}
	obs, _ := x.Observe(ctx, h, "")
	if obs.State != contracts.RunExited || obs.ExitCode != nil || obs.NativeRef == nil || obs.NativeRef.ID != sid {
		t.Fatalf("exited = %+v", obs)
	}
	if _, paths := x.TranscriptFiles(h); len(paths) != 1 || filepath.Base(paths[0]) != sid+".jsonl" {
		t.Fatalf("transcript files %q", paths)
	}

	// pyre stop before the child started: the run ends and the CLI's start
	// report is refused (it then ends the child).
	h3, _ := x.Launch(ctx, contracts.ResolvedLaunch{RunID: "run_e3", Cwd: cwd, StateDir: filepath.Join(t.TempDir(), "r3"), ProfileDir: cfg})
	_ = x.Claimed(h3, os.Getpid())
	if _, err := x.Stop(ctx, h3); err != nil {
		t.Fatal(err)
	}
	if err := x.Started(h3, os.Getpid(), false); err == nil || !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("start after stop: %v", err)
	}
	if obs, _ := x.Observe(ctx, h3, ""); obs.State != contracts.RunExited {
		t.Fatalf("stopped before start = %+v", obs)
	}

	// A CLI that vanished (killed) without reporting ends the run.
	gone := exec.Command("true")
	_ = gone.Run()
	h2, _ := x.Launch(ctx, contracts.ResolvedLaunch{RunID: "run_e2", Cwd: cwd, StateDir: filepath.Join(t.TempDir(), "r2"), ProfileDir: cfg})
	_ = x.Claimed(h2, gone.Process.Pid)
	if obs, _ := x.Observe(ctx, h2, ""); obs.State != contracts.RunExited || obs.ExitCode != nil {
		t.Fatalf("vanished CLI = %+v", obs)
	}
}

func execSock(t *testing.T) string {
	d, err := os.MkdirTemp("", "px")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return filepath.Join(d, "t.sock")
}

// A pyre run killed by SIGKILL forwards nothing: the node sees its client
// gone and stops the claude it left behind (macOS has no parent-death
// signal), then ends the run.
func TestExecAdapterStopsOrphanedChild(t *testing.T) {
	x, _ := newExecTest(t)
	ctx := context.Background()
	client := exec.Command("sleep", "30")
	child := exec.Command("sleep", "30")
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	h, _ := x.Launch(ctx, contracts.ResolvedLaunch{RunID: "run_orphan", Cwd: t.TempDir(), StateDir: filepath.Join(t.TempDir(), "r"), ProfileDir: t.TempDir()})
	_ = x.Claimed(h, client.Process.Pid)
	_ = x.Started(h, child.Process.Pid, true)
	_ = client.Process.Kill()
	_ = client.Wait()
	obs, _ := x.Observe(ctx, h, "")
	if obs.State != contracts.RunExited || !strings.Contains(obs.StateReason, "its claude was stopped") {
		t.Fatalf("observe = %+v", obs)
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = child.Process.Kill()
		t.Fatal("orphaned child still running")
	}
}

// --native-resume after the owner went on outside Pyre: a newer transcript
// that extends the stale copy replaces it; a diverged one never does. A
// conversation written moments ago or held open counts as live.
func TestAdoptionRefreshAndInUse(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	shared := filepath.Join(root, "shared")
	_, a := newExecTest(t, shared)
	cwd := t.TempDir()
	sid := "66666666-6666-4666-8666-666666666666"
	own := guessTranscriptPath(filepath.Join(home, ".claude"), cwd, sid)
	dst := guessTranscriptPath(shared, cwd, sid)
	write := func(p, body string, age time.Duration) {
		_ = os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(-age)
		_ = os.Chtimes(p, when, when)
	}
	write(dst, "line1\n", 2*time.Hour)
	write(own, "line1\nline2\n", time.Hour)
	if err := a.importTranscript(shared, cwd, sid); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "line1\nline2\n" {
		t.Fatalf("stale copy kept: %q", b)
	}
	write(dst, "lineX\n", 2*time.Hour)
	write(own, "line1\nline2\nline3\n", time.Hour)
	_ = a.importTranscript(shared, cwd, sid)
	if b, _ := os.ReadFile(dst); string(b) != "lineX\n" {
		t.Fatalf("diverged copy replaced: %q", b)
	}
	if busy, why := a.ConversationInUse(cwd, sid); busy {
		t.Fatalf("idle conversation in use: %s", why)
	}
	write(own, "line1\nline2\nline3\nline4\n", 0)
	if busy, _ := a.ConversationInUse(cwd, sid); !busy {
		t.Fatal("just-written conversation not in use")
	}
	_ = os.Chtimes(own, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour))
	f, _ := os.Open(own) // this test process holds it open
	defer f.Close()
	if busy, why := a.ConversationInUse(cwd, sid); !busy {
		t.Fatal("held-open conversation not in use")
	} else if !strings.Contains(why, "open") {
		t.Fatalf("why = %q", why)
	}
}
