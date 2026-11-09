//go:build !windows

package ghguard

import (
	"fmt"
	"os"
	"syscall"
)

// execReal replaces this process with gh, so signals, the terminal and
// the exit status are gh's own.
func execReal(real string, args []string) int {
	err := syscall.Exec(real, append([]string{"gh"}, args...), os.Environ())
	fmt.Fprintf(os.Stderr, "pyre gh shim: exec %s: %v\n", real, err)
	return 126
}
