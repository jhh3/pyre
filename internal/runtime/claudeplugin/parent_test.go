package claudeplugin

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Helper process: behaves like the watcher, exiting 0 once ParentWatch
// reports the session gone (3 if that never happens).
func TestParentWatchHelper(t *testing.T) {
	if os.Getenv("PYRE_TEST_PARENTWATCH") != "1" {
		t.Skip("helper process only")
	}
	alive := ParentWatch()
	end := time.Now().Add(20 * time.Second)
	for time.Now().Before(end) {
		if !alive() {
			os.Exit(0)
		}
		time.Sleep(100 * time.Millisecond)
	}
	os.Exit(3)
}

// The real layout: Claude -> shell (`<shell> -c "… eval '<cmd>' … && pwd"`)
// -> watcher. Killing "Claude" leaves the shell alive (reparented); the
// watcher must still notice and exit.
func TestParentWatchDetectsKilledGrandparent(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	pidFile := t.TempDir() + "/helper.pid"
	inner := exe + " -test.run=^TestParentWatchHelper$ & echo $! > " + pidFile + "; wait; true"
	claude := exec.Command("sh", "-c", "sh -c '"+inner+"'; true")
	claude.Env = append(os.Environ(), "PYRE_TEST_PARENTWATCH=1")
	claude.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := claude.Start(); err != nil {
		t.Fatal(err)
	}
	var helper int
	for i := 0; i < 100 && helper == 0; i++ {
		if b, err := os.ReadFile(pidFile); err == nil {
			helper, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		time.Sleep(50 * time.Millisecond)
	}
	if helper == 0 {
		_ = claude.Process.Kill()
		t.Fatal("helper did not start")
	}
	t.Cleanup(func() { _ = syscall.Kill(helper, syscall.SIGKILL) })
	time.Sleep(500 * time.Millisecond)
	if syscall.Kill(helper, 0) != nil {
		t.Fatal("watcher exited while its session lives")
	}
	_ = claude.Process.Kill() // SIGKILL "Claude" only; the shell survives
	_, _ = claude.Process.Wait()
	end := time.Now().Add(10 * time.Second)
	for time.Now().Before(end) {
		if syscall.Kill(helper, 0) != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("watcher still running after its Claude process was killed")
}
