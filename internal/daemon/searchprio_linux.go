package daemon

import (
	"golang.org/x/sys/unix"
)

// lowerThreadPriority puts the calling OS thread at nice 19 and the lowest
// best-effort I/O priority. Call it with the goroutine locked to its thread
// (runtime.LockOSThread): Linux priorities are per thread, and SQLite
// (modernc, pure Go) and the transcript reads run on the calling goroutine.
// Best-effort rather than idle I/O: the indexer holds SQLite's write lock
// while it commits, and idle I/O could starve it there under load.
func lowerThreadPriority() error {
	tid := unix.Gettid()
	if err := unix.Setpriority(unix.PRIO_PROCESS, tid, 19); err != nil {
		return err
	}
	const (
		ioprioWhoProcess = 1
		ioprioClassBE    = 2
		ioprioClassShift = 13
	)
	_, _, errno := unix.Syscall(unix.SYS_IOPRIO_SET, ioprioWhoProcess, uintptr(tid), ioprioClassBE<<ioprioClassShift|7)
	if errno != 0 {
		return errno
	}
	return nil
}
