//go:build !windows

package osutil

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"syscall"
)

func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func terminate(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }

func lock(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_EX) }

func unlock(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }

// NewProcessGroup makes cmd the leader of a new process group (Setpgid), so
// KillGroup reaches everything it spawns.
func NewProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// KillGroup signals the process group led by pid: SIGTERM, or SIGKILL when
// force.
func KillGroup(pid int, force bool) error {
	sig := syscall.SIGTERM
	if force {
		sig = syscall.SIGKILL
	}
	return syscall.Kill(-pid, sig)
}

// Exec replaces the current process with path (execve). It only returns on
// failure.
func Exec(path string, argv, env []string) error { return syscall.Exec(path, argv, env) }

// SignalZero is kill(pid, 0): nil when pid exists and may be signalled.
func SignalZero(pid int) error { return syscall.Kill(pid, 0) }

// ParentPID is implemented for Windows only (Unix callers read /proc or ps).
func ParentPID(pid int) (int, error) { return 0, errors.ErrUnsupported }

// Junction makes link point at the directory target: a symbolic link on
// Unix (target as given), an NTFS junction on Windows (target made
// absolute; no administrator rights or Developer Mode needed).
func Junction(target, link string) error { return os.Symlink(target, link) }

// LinkModes are the file mode bits of a link Symlink creates.
const LinkModes = fs.ModeSymlink

// Symlink creates link -> target (os.Symlink on Unix).
func Symlink(target, link string) error { return os.Symlink(target, link) }
