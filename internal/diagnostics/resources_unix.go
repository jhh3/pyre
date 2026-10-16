//go:build !windows

package diagnostics

import (
	"syscall"
	"time"
)

// selfCPU is this process's user+system time (getrusage).
func selfCPU() (time.Duration, error) {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0, err
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano()), nil
}
