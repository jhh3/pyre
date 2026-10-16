//go:build !windows

package termhost

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jhh3/pyre/internal/supervisor"
)

// spawnHost starts the host detached: its own session (setsid), stdio on
// the log file, and under a systemd user service in its own transient
// scope (supervisor.DecideScope, as for the tmux server) so stopping pyred
// does not take the runs with it.
func spawnHost(ctx context.Context, exe string, args, env []string, logPath, socket string) error {
	argv := append([]string{exe}, args...)
	if d := supervisor.DecideScope(ctx); d.Use {
		// A unique unit: an earlier host's scope may still exist.
		unit := strings.TrimSuffix(supervisor.ScopeUnitName(socket), ".scope") + "-th" + strconv.FormatInt(time.Now().UnixNano(), 36) + ".scope"
		argv = d.Wrap(unit, argv)
	}
	logf, err := openLog(logPath)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }() // reap it if it exits while pyred runs
	return nil
}

// forgetSessionTask: session-host tasks exist only on Windows.
func forgetSessionTask(socket string) {}
