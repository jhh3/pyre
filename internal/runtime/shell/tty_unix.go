//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package shell

import "golang.org/x/sys/unix"

// lineEditing reports whether the terminal tty is in non-canonical mode,
// as a shell's line editor (readline, ZLE, fish) sets it while it waits
// for input; known is false when the tty cannot be read.
func lineEditing(tty string) (raw, known bool) {
	if tty == "" {
		return false, false
	}
	// O_NOCTTY: never become this terminal's controlling process.
	fd, err := unix.Open(tty, unix.O_RDONLY|unix.O_NOCTTY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return false, false
	}
	defer unix.Close(fd)
	t, err := unix.IoctlGetTermios(fd, termiosGet)
	if err != nil {
		return false, false
	}
	return t.Lflag&unix.ICANON == 0, true
}
