//go:build linux || darwin

package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"syscall"
	"testing"
	"time"
)

func TestSweepTmpDir(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tmp", "claude")
	now := time.Now()
	old := now.Add(-4 * 24 * time.Hour)
	mk := func(rel string, at time.Time) string {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("synthetic scratch"), 0o600); err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(p, at, at)
		return p
	}
	const (
		stale = "11111111-1111-4111-8111-111111111111"
		fresh = "22222222-2222-4222-8222-222222222222"
		live  = "33333333-3333-4333-8333-333333333333"
	)
	mk("-home-synth-a/"+stale+"/tasks/out.txt", old)
	mk("-home-synth-a/"+fresh+"/tasks/out.txt", now)
	mk("-home-synth-b/"+live+"/tasks/out.txt", old)
	mk("-home-synth-c/notes.txt", old) // not a session folder: never touched
	// A live run works in /home/synth/e: after /clear its Claude session ID
	// changed, so the old-looking folder there is still kept.
	const cleared = "44444444-4444-4444-8444-444444444444"
	mk("-home-synth-e/"+cleared+"/tasks/out.txt", old)
	n, freed, err := sweepTmpDir(root, 72*time.Hour, now, map[string]bool{live: true}, map[string]bool{"-home-synth-e": true}, false)
	if err != nil || n != 1 || freed != int64(len("synthetic scratch")) {
		t.Fatalf("removed %d (%d bytes), err %v", n, freed, err)
	}
	for rel, want := range map[string]bool{
		"-home-synth-a/" + stale: false, "-home-synth-a/" + fresh: true, "-home-synth-b/" + live: true, "-home-synth-c/notes.txt": true,
		"-home-synth-e/" + cleared: true,
	} {
		if _, err := os.Stat(filepath.Join(root, rel)); (err == nil) != want {
			t.Errorf("%s exists=%v, want %v", rel, err == nil, want)
		}
	}
	// A cwd-key folder emptied by the sweep goes too.
	mk("-home-synth-d/"+stale+"/x", old)
	if n, _, _ := sweepTmpDir(root, 72*time.Hour, now, map[string]bool{live: true}, map[string]bool{"-home-synth-e": true}, false); n != 1 {
		t.Fatalf("second pass removed %d", n)
	}
	if _, err := os.Stat(filepath.Join(root, "-home-synth-d")); err == nil {
		t.Error("empty parent kept")
	}
	if _, err := os.Stat(root); err != nil {
		t.Error("the root itself is kept")
	}
}

// Only processes in the run's own terminal session are stopped: a daemon
// the session started that detached into its own terminal session (tmux
// server, adb, gradle) carries PYRE_SESSION_ID too and must survive.
func TestSessionProcessesStopped(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("process scan is unix-only")
	}
	const sid = "ses_01SYNTHLIFECYCLETEST00000"
	env := append(os.Environ(), "PYRE_SESSION_ID="+sid)
	start := func(args ...string) (*exec.Cmd, chan struct{}) {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Env = env
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		return cmd, done
	}
	// The "pane": a session leader with a background child left behind.
	pane, paneDone := start("sh", "-c", "sleep 60 & wait")
	// A shared server the session started: its own terminal session.
	server, serverDone := start("sleep", "60")
	var pids []int
	for i := 0; i < 50; i++ {
		if pids = sessionProcesses(sid, []int{pane.Process.Pid}); len(pids) >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !slices.Contains(pids, pane.Process.Pid) || len(pids) < 2 {
		t.Fatalf("the pane and its background child: %v (pane %d)", pids, pane.Process.Pid)
	}
	if slices.Contains(pids, server.Process.Pid) {
		t.Fatal("a detached server in its own terminal session was matched")
	}
	if other := sessionProcesses(sid+"X", []int{pane.Process.Pid}); len(other) != 0 {
		t.Fatalf("matched another session's ID by prefix: %v", other)
	}
	if n := stopProcesses(pids, 3*time.Second); n < 2 {
		t.Fatalf("stopped %d", n)
	}
	select {
	case <-paneDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the pane's processes still run")
	}
	select {
	case <-serverDone:
		t.Fatal("the shared server was stopped")
	default:
	}
}
