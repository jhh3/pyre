//go:build windows

package cli

import (
	"os/exec"
	"strconv"
)

// prepareCheck kills a timed-out monitor check's whole process tree
// (taskkill /T), not only the shell.
func prepareCheck(x *exec.Cmd) {
	x.Cancel = func() error {
		if x.Process == nil {
			return nil
		}
		_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(x.Process.Pid)).Run()
		return x.Process.Kill()
	}
}
