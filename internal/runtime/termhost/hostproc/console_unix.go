//go:build !windows

package hostproc

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/charmbracelet/x/term"
)

func makeRaw() (func(), error) {
	fd := os.Stdin.Fd()
	st, err := term.MakeRaw(fd)
	if err != nil {
		return nil, err
	}
	return func() { _ = term.Restore(fd, st) }, nil
}

func termSize() (int, int, error) { return term.GetSize(os.Stdout.Fd()) }

// watchResize calls fn with the terminal size on every SIGWINCH.
func watchResize(stop <-chan struct{}, fn func(cols, rows int)) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	defer signal.Stop(ch)
	for {
		select {
		case <-stop:
			return
		case <-ch:
			if c, r, err := termSize(); err == nil {
				fn(c, r)
			}
		}
	}
}

// detachConsole: only Windows task-started hosts have a console to leave.
func detachConsole() {}

// redirectStderr points os.Stderr at f (unix hosts are spawned with their
// stdio on the log already; --log is for Windows task-started hosts).
func redirectStderr(f *os.File) { os.Stderr = f }
