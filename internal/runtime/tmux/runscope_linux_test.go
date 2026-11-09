//go:build linux

package tmux

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/supervisor"
)

// TestScopedPaneInRealSystemdScope is the end-to-end check of per-run
// scopes against the real systemd user manager: a pane moves itself into
// its own pyre-run-*.scope (in a throwaway test slice) with a 96 MiB
// MemoryMax and no swap; a child that allocates 300 MiB is OOM-killed
// inside the scope while the pane's shell (the "harness") and a sibling
// keep running (OOMPolicy=continue). Bounded: the hog can never use more
// than the cap. Skipped without busctl or a reachable user manager.
func TestScopedPaneInRealSystemdScope(t *testing.T) {
	// It really OOM-kills a process under the user manager, which shows an
	// "ran out of memory" notice on a desktop session; run it only on request.
	if os.Getenv("PYRE_TEST_HOST_OOM") != "1" {
		t.Skip("set PYRE_TEST_HOST_OOM=1 to run the real OOM-kill check")
	}
	busctl, err := exec.LookPath("busctl")
	if err != nil {
		t.Skip("busctl not installed")
	}
	ver, err := exec.Command(busctl, "--user", "get-property", "org.freedesktop.systemd1", "/org/freedesktop/systemd1",
		"org.freedesktop.systemd1.Manager", "Version").Output()
	if err != nil {
		t.Skipf("no systemd user manager on the user bus: %v", err)
	}
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	v, _ := strconv.Atoi(strings.SplitN(strings.Trim(strings.TrimPrefix(strings.TrimSpace(string(ver)), "s "), `"`), ".", 2)[0])
	s := testServer(t)
	sc := supervisor.RunScope{Busctl: busctl, Slice: "pyre-test-runscope.slice", MemoryMax: 96 << 20, SwapZero: true, OOMPolicy: v >= 253}
	useRunScope(t, &sc)
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	script := filepath.Join(dir, "pane.sh")
	body := "#!/bin/sh\nsleep 60 &\necho $! > " + filepath.Join(dir, "sibling") + "\n" +
		py + " -I -c \"a = b'x' * (300 << 20); print('hog survived')\"\necho \"hog=$?\" > " + out + "\nsleep 60\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	name := "run_synth-scope"
	// Runs last: the (then empty) throwaway slice is removed.
	t.Cleanup(func() { _ = exec.Command("systemctl", "--user", "stop", sc.Slice).Run() })
	if err := s.NewSession(ctx, SessionSpec{Name: name, Argv: []string{"/bin/sh", script}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.KillSession(context.Background(), name) })
	if !waitFor(t, 15*time.Second, func() bool { b, _ := os.ReadFile(out); return len(b) > 0 }) {
		screen, _ := s.CapturePane(ctx, name, CaptureOptions{Join: true})
		t.Fatalf("pane did not finish the hog; pane: %s", screen)
	}
	res, _ := os.ReadFile(out)
	p, err := s.Pane(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	cg := supervisor.ProcCgroup(p.PID)
	log, _ := os.ReadFile(filepath.Join(filepath.Dir(s.Socket), RunScopeLog))
	t.Logf("pane pid %d cgroup %s; hog: %s; log: %s", p.PID, cg, strings.TrimSpace(string(res)), strings.TrimSpace(string(log)))
	if !strings.Contains(cg, "/pyre-test-runscope.slice/pyre-run-"+name+"-") || !strings.HasSuffix(cg, ".scope") {
		t.Fatalf("pane cgroup %q, want its own pyre-run scope in the test slice (log %q)", cg, log)
	}
	if max, _ := os.ReadFile(filepath.Join("/sys/fs/cgroup", cg, "memory.max")); strings.TrimSpace(string(max)) != strconv.Itoa(96<<20) {
		t.Fatalf("memory.max = %q", max)
	}
	if strings.TrimSpace(string(res)) != "hog=137" {
		t.Fatalf("hog result %q, want killed (137)", res)
	}
	if p.Dead || !syscallAlive(p.PID) {
		t.Fatal("the pane process died with the hog")
	}
	sib, _ := os.ReadFile(filepath.Join(dir, "sibling"))
	if pid, _ := strconv.Atoi(strings.TrimSpace(string(sib))); pid <= 0 || !syscallAlive(pid) {
		t.Fatalf("sibling %q died with the hog", sib)
	}
	if ev, _ := os.ReadFile(filepath.Join("/sys/fs/cgroup", cg, "memory.events")); !strings.Contains(string(ev), "oom_kill 1") {
		t.Fatalf("memory.events %q", ev)
	}
}
