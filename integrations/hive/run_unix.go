//go:build unix

package hive

import (
	"os/exec"
	"syscall"
)

func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative pid: signal the whole group.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// lowerPriority renices the started process group (its pgid is its pid,
// see setProcessGroup) to BackgroundNice. Raising nice needs no privilege.
func lowerPriority(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Setpriority(syscall.PRIO_PGRP, cmd.Process.Pid, BackgroundNice)
}
