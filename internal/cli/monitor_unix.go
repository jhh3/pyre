//go:build !windows

package cli

import (
	"os/exec"
	"syscall"
)

// prepareCheck runs a monitor check in its own process group and kills
// the whole group when the check times out (grandchildren included).
func prepareCheck(x *exec.Cmd) {
	x.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	x.Cancel = func() error {
		if x.Process == nil {
			return nil
		}
		return syscall.Kill(-x.Process.Pid, syscall.SIGKILL)
	}
}
