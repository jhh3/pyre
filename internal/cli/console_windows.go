//go:build windows

package cli

import (
	"os"
	"time"

	"github.com/charmbracelet/x/term"
	"golang.org/x/sys/windows"
)

// makeRawConsole puts the console input in raw VT mode (keys arrive as VT
// sequences, Ctrl-C as a byte) and makes the console output interpret VT
// sequences.
func makeRawConsole(in, out *os.File) (func(), error) {
	st, err := term.MakeRaw(in.Fd())
	if err != nil {
		return nil, err
	}
	var mode uint32
	outOK := false
	var h windows.Handle
	if out != nil {
		h = windows.Handle(out.Fd())
		outOK = windows.GetConsoleMode(h, &mode) == nil
	}
	if outOK {
		_ = windows.SetConsoleMode(h, mode|windows.ENABLE_PROCESSED_OUTPUT|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING|windows.DISABLE_NEWLINE_AUTO_RETURN)
	}
	return func() {
		_ = term.Restore(in.Fd(), st)
		if outOK {
			_ = windows.SetConsoleMode(h, mode)
		}
	}, nil
}

// watchConsoleResize polls the console size (consoles have no SIGWINCH).
func watchConsoleResize(stop <-chan struct{}, size func() (int, int), fn func(cols, rows int)) {
	lc, lr := size()
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if c, r := size(); c > 0 && r > 0 && (c != lc || r != lr) {
				lc, lr = c, r
				fn(c, r)
			}
		}
	}
}
