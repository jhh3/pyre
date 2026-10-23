//go:build !windows

package headless

import "syscall"

// terminateGroup sends SIGTERM to the process group pgid leads.
func terminateGroup(pgid int) error { return syscall.Kill(-pgid, syscall.SIGTERM) }
