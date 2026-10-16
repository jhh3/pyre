//go:build !windows

package cli

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/charmbracelet/x/term"
)

// makeRawConsole puts the terminal on in in raw mode.
func makeRawConsole(in, _ *os.File) (func(), error) {
	st, err := term.MakeRaw(in.Fd())
	if err != nil {
		return nil, err
	}
	return func() { _ = term.Restore(in.Fd(), st) }, nil
}

// watchConsoleResize calls fn with the terminal size on every SIGWINCH.
func watchConsoleResize(stop <-chan struct{}, size func() (int, int), fn func(cols, rows int)) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	defer signal.Stop(ch)
	for {
		select {
		case <-stop:
			return
		case <-ch:
			if c, r := size(); c > 0 && r > 0 {
				fn(c, r)
			}
		}
	}
}
