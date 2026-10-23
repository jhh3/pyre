package claude

import (
	"os"
	"path/filepath"
	"strconv"
)

// fileHolder returns a process that has path open (0 when none is seen).
func fileHolder(path string) int {
	want, err := filepath.EvalSymlinks(path)
	if err != nil {
		return 0
	}
	procs, _ := os.ReadDir("/proc")
	for _, p := range procs {
		pid, err := strconv.Atoi(p.Name())
		if err != nil {
			continue
		}
		fds, _ := os.ReadDir(filepath.Join("/proc", p.Name(), "fd"))
		for _, fd := range fds {
			if t, err := os.Readlink(filepath.Join("/proc", p.Name(), "fd", fd.Name())); err == nil && t == want {
				return pid
			}
		}
	}
	return 0
}
