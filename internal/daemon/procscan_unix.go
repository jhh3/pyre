//go:build linux || darwin

package daemon

import (
	"syscall"
	"time"
)

// inSessions reports whether pid's terminal session is one of sids.
func inSessions(pid int, sids []int) bool {
	sid, err := getsid(pid)
	if err != nil {
		return false
	}
	for _, s := range sids {
		if s > 0 && s == sid {
			return true
		}
	}
	return false
}

// stopProcesses sends SIGTERM, waits up to grace, then SIGKILLs the rest.
// It returns how many were running.
func stopProcesses(pids []int, grace time.Duration) int {
	n := 0
	for _, pid := range pids {
		if syscall.Kill(pid, syscall.SIGTERM) == nil {
			n++
		}
	}
	end := time.Now().Add(grace)
	for time.Now().Before(end) {
		alive := false
		for _, pid := range pids {
			if syscall.Kill(pid, 0) == nil {
				alive = true
				break
			}
		}
		if !alive {
			return n
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, pid := range pids {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	return n
}
