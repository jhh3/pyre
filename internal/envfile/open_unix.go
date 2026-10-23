//go:build !windows

package envfile

import (
	"os"
	"syscall"
)

// openNonBlocking opens path read-only without following a symbolic link
// and without blocking: a FIFO swapped in after the Lstat would otherwise
// hang the open until a writer appears (the descriptor is then refused as
// not regular).
func openNonBlocking(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}
