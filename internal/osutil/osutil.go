// Package osutil holds the small process and file-locking primitives that
// differ between Unix (darwin, linux) and Windows. The Unix implementations
// are exactly the syscalls the callers used before (kill(pid, 0), flock,
// process groups); Windows gets the native equivalent.
package osutil

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ProcessAlive reports whether a process with pid exists. On Unix a process
// of another user (EPERM) counts as alive.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return processAlive(pid)
}

// Terminate asks pid to stop (Unix SIGTERM; Windows has no catchable
// equivalent for an arbitrary process, so it is TerminateProcess).
func Terminate(pid int) error {
	if pid <= 0 {
		return os.ErrInvalid
	}
	return terminate(pid)
}

// Lock takes an exclusive advisory lock on f (blocking), Unlock releases it.
// Unix: flock(LOCK_EX); Windows: LockFileEx on the first byte range.
func Lock(f *os.File) error { return lock(f) }

// Unlock releases a lock taken with Lock.
func Unlock(f *os.File) error { return unlock(f) }

// IsExecutable reports whether fi (the stat of name) is an executable file:
// any execute bit on Unix; on Windows, which has no execute bit, a regular
// file with an executable extension (.exe, .com, .cmd, .bat).
func IsExecutable(name string, fi os.FileInfo) bool {
	if fi == nil || fi.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		switch strings.ToLower(filepath.Ext(name)) {
		case ".exe", ".com", ".cmd", ".bat":
			return true
		}
		return false
	}
	return fi.Mode()&0o111 != 0
}

// ExeName is name with the platform's executable suffix (".exe" on
// Windows).
func ExeName(name string) string {
	if runtime.GOOS == "windows" && filepath.Ext(name) == "" {
		return name + ".exe"
	}
	return name
}
