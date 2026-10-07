//go:build unix

package outfit

import (
	"os/exec"
	"syscall"
)

// configureProcessGroup runs outfit in its own process group so a timeout
// also stops provider CLIs it spawned (gh, gws, MCP servers).
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
