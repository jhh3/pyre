//go:build !windows

package tmux

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// readPID waits for the test's own process to write its PID to path.
func readPID(t *testing.T, path string) int {
	t.Helper()
	for i := 0; i < 100; i++ {
		if b, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no pid in %s", path)
	return 0
}

func procRunning(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true // no /proc (macOS): kill(0) is all there is
	}
	i := strings.LastIndexByte(string(b), ')')
	return i < 0 || i+2 >= len(b) || (b[i+2] != 'Z' && b[i+2] != 'X')
}

// The pane exits by itself after MarkTree, as Claude does on /exit,
// leaving a server in its process group and one that started a terminal
// session of its own, both orphaned. TerminateSession ends both; a
// `setsid -f` job the pane started survives (iss_…).
// Only processes this test started are signalled.
func TestTerminateSessionEndsOrphanedTree(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid not installed")
	}
	s := testServer(t)
	ctx := context.Background()
	dir := t.TempDir()
	script := `trap "" HUP
sleep 300 & echo $! > ` + dir + `/grp
setsid sh -c 'echo $$ > ` + dir + `/srv; exec sleep 300' &
setsid -f sh -c 'echo $$ > ` + dir + `/job; exec sleep 300'
read x
exit 0`
	if err := s.NewSession(ctx, SessionSpec{Name: "tree", Argv: []string{"sh", "-c", script}}); err != nil {
		t.Fatal(err)
	}
	grp, srv, job := readPID(t, filepath.Join(dir, "grp")), readPID(t, filepath.Join(dir, "srv")), readPID(t, filepath.Join(dir, "job"))
	t.Cleanup(func() {
		for _, pid := range []int{grp, srv, job} {
			_ = syscall.Kill(pid, syscall.SIGKILL) // the test's own processes
		}
	})
	time.Sleep(200 * time.Millisecond) // setsid -f's parent exits; the job is reparented
	s.MarkTree(ctx, "tree")
	if err := s.SendKeys(ctx, "tree", KeyEnter); err != nil {
		t.Fatal(err)
	}
	for i := 0; ; i++ {
		if p, err := s.Pane(ctx, "tree"); err == nil && p.Dead {
			break
		}
		if i > 100 {
			t.Fatal("pane did not exit")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !procRunning(grp) || !procRunning(srv) {
		t.Fatal("setup: the orphans should outlive the pane")
	}
	if err := s.TerminateSession(ctx, "tree", time.Second); err != nil {
		t.Fatal(err)
	}
	if procRunning(grp) {
		t.Error("the orphan in the pane's process group survived")
	}
	if procRunning(srv) {
		t.Error("the orphan in a terminal session of its own survived")
	}
	if !procRunning(job) {
		t.Error("the setsid -f job was stopped")
	}
}

// Without MarkTree, a pane that already exited still has the rest of its
// process group ended.
func TestTerminateSessionDeadPaneGroup(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	dir := t.TempDir()
	script := `trap "" HUP; sleep 300 & echo $! > ` + dir + `/grp; read x; exit 0`
	if err := s.NewSession(ctx, SessionSpec{Name: "dead", Argv: []string{"sh", "-c", script}}); err != nil {
		t.Fatal(err)
	}
	grp := readPID(t, filepath.Join(dir, "grp"))
	t.Cleanup(func() { _ = syscall.Kill(grp, syscall.SIGKILL) })
	if err := s.SendKeys(ctx, "dead", KeyEnter); err != nil {
		t.Fatal(err)
	}
	for i := 0; ; i++ {
		if p, err := s.Pane(ctx, "dead"); err == nil && p.Dead {
			break
		}
		if i > 100 {
			t.Fatal("pane did not exit")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := s.TerminateSession(ctx, "dead", time.Second); err != nil {
		t.Fatal(err)
	}
	if procRunning(grp) {
		t.Fatal("the orphan in the dead pane's process group survived")
	}
}
