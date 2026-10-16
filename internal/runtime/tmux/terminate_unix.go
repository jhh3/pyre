//go:build !windows

package tmux

import (
	"syscall"
	"time"
)

// terminateGroup ends the pane's process group (each pane leads its own
// group: tmux setsid()s it) with SIGTERM, waits up to grace for the whole
// group to exit, then escalates to SIGKILL.
func terminateGroup(pid int, grace time.Duration) {
	pg := -pid
	_ = syscall.Kill(pg, syscall.SIGTERM)
	end := time.Now().Add(grace)
	for time.Now().Before(end) && groupAlive(pid) {
		time.Sleep(50 * time.Millisecond)
	}
	if groupAlive(pid) {
		_ = syscall.Kill(pg, syscall.SIGKILL)
		for i := 0; i < 40 && groupAlive(pid); i++ {
			time.Sleep(50 * time.Millisecond)
		}
	}
}
