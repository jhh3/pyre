//go:build linux

package tmux

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/supervisor"
)

// TestEnsureServerInRealSystemdScope starts a real tmux server through a
// real `systemd-run --user --scope` (PYRE_SYSTEMD_SCOPE=on), checks that the
// server process landed in its named scope inside the runs slice and that
// the placement record says so, then that killing the server releases the
// --collect scope. One idle tmux server; skipped without a systemd user
// manager (containers, CI runners without a user bus).
func TestEnsureServerInRealSystemdScope(t *testing.T) {
	run, err := exec.LookPath("systemd-run")
	if err != nil {
		t.Skip("systemd-run not installed")
	}
	if out, err := exec.Command(run, "--user", "--scope", "--quiet", "--collect", "--", "true").CombinedOutput(); err != nil {
		t.Skipf("no usable systemd user manager: %v: %s", err, strings.TrimSpace(string(out)))
	}
	s := testServer(t)
	t.Setenv("PYRE_SYSTEMD_SCOPE", "on")
	t.Setenv("PYRE_SYSTEMD_SLICE", "")
	ctx := context.Background()
	if err := s.EnsureServer(ctx); err != nil {
		t.Fatal(err)
	}
	out, err := s.Run(ctx, nil, "display-message", "-p", "#{pid}")
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		t.Fatalf("server pid %q", out)
	}
	unit := supervisor.ScopeUnitName(s.Socket)
	cg := supervisor.ProcCgroup(pid)
	if !strings.Contains(cg, "/"+supervisor.DefaultRunsSlice+"/") || !strings.HasSuffix(cg, "/"+unit) {
		t.Fatalf("tmux server pid %d cgroup %q, want .../%s/%s", pid, cg, supervisor.DefaultRunsSlice, unit)
	}
	pl := supervisor.ReadPlacement(s.Socket)
	if !strings.Contains(pl, "systemd scope "+unit) || !strings.Contains(pl, "cgroup "+cg) {
		t.Fatalf("placement %q does not record scope %s and cgroup %s", pl, unit, cg)
	}
	t.Logf("placement: %s", pl)

	if err := s.KillServer(ctx); err != nil {
		t.Fatal(err)
	}
	gone := waitFor(t, 5*time.Second, func() bool {
		st, _ := exec.Command("systemctl", "--user", "is-active", unit).Output()
		return strings.TrimSpace(string(st)) != "active" && !syscallAlive(pid)
	})
	if !gone {
		t.Fatalf("scope %s or server pid %d still alive after kill-server", unit, pid)
	}
}
