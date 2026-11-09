//go:build windows

package ghguard

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
)

// execReal runs gh with this process's stdio and returns its exit code
// (Windows has no exec).
func execReal(real string, args []string) int {
	cmd := exec.Command(real, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "pyre gh shim: run %s: %v\n", real, err)
		return 126
	}
	return 0
}
