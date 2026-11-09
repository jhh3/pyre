//go:build !linux && !windows

package tmux

import (
	"context"
	"os/exec"
	"syscall"
	"time"
)

// platformListProcs reads the process table from ps (macOS and the BSDs
// have no /proc). The terminal session ID is not available (sid -1).
func platformListProcs() ([]procInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-A", "-o", "pid=,ppid=,pgid=,stat=,lstart=").Output()
	if err != nil {
		return nil, err
	}
	return parsePSTable(string(out)), nil
}

func platformProcCgroup(int) string { return "" }

func platformSignal(pid int, force bool) error {
	sig := syscall.SIGTERM
	if force {
		sig = syscall.SIGKILL
	}
	return syscall.Kill(pid, sig)
}
