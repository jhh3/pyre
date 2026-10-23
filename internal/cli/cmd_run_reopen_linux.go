package cli

import (
	"fmt"
	"os"
)

// reopenRead opens the file behind f read-only (a shell's `> file` opens
// stdout write-only): /proc/self/fd/N names it.
func reopenRead(f *os.File) (*os.File, error) {
	return os.Open(fmt.Sprintf("/proc/self/fd/%d", f.Fd()))
}
