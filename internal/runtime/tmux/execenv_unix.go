//go:build !windows

package tmux

import (
	"fmt"
	"os"
	"syscall"
)

// execReplace execs argv in place (same PID, same process group). It
// returns only on failure.
func execReplace(path string, argv, env []string) (int, error) {
	return 127, syscall.Exec(path, argv, env)
}

// ownedByMe refuses an environment file owned by another user.
func ownedByMe(file string, st os.FileInfo) error {
	if sys, ok := st.Sys().(*syscall.Stat_t); ok && int(sys.Uid) != os.Getuid() {
		return fmt.Errorf("%s: owned by uid %d", file, sys.Uid)
	}
	return nil
}
