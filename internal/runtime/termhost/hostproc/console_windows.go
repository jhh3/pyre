//go:build windows

package hostproc

import (
	"os"
	"time"

	"github.com/charmbracelet/x/term"
	"golang.org/x/sys/windows"
)

// makeRaw puts the console input in raw VT mode (keys arrive as VT
// sequences, Ctrl-C as a byte) and makes the console output interpret VT
// sequences, as Windows Terminal and conhost do with these modes.
func makeRaw() (func(), error) {
	in := os.Stdin.Fd()
	st, err := term.MakeRaw(in)
	if err != nil {
		return nil, err
	}
	out := windows.Handle(os.Stdout.Fd())
	var mode uint32
	outOK := windows.GetConsoleMode(out, &mode) == nil
	if outOK {
		_ = windows.SetConsoleMode(out, mode|windows.ENABLE_PROCESSED_OUTPUT|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING|windows.DISABLE_NEWLINE_AUTO_RETURN)
	}
	return func() {
		_ = term.Restore(in, st)
		if outOK {
			_ = windows.SetConsoleMode(out, mode)
		}
	}, nil
}

func termSize() (int, int, error) { return term.GetSize(os.Stdout.Fd()) }

// watchResize polls the console size (consoles have no SIGWINCH; window
// buffer events would need ReadConsoleInput, which raw VT input bypasses).
func watchResize(stop <-chan struct{}, fn func(cols, rows int)) {
	lc, lr, _ := termSize()
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if c, r, err := termSize(); err == nil && (c != lc || r != lr) {
				lc, lr = c, r
				fn(c, r)
			}
		}
	}
}

var procFreeConsole = windows.NewLazySystemDLL("kernel32.dll").NewProc("FreeConsole")

// detachConsole leaves the console window Task Scheduler opens for a
// console program in the interactive session (it closes once no process is
// attached); the host then runs console-less, as with DETACHED_PROCESS.
func detachConsole() { _, _, _ = procFreeConsole.Call() }

// redirectStderr points os.Stderr and the process's standard error handle
// (where the runtime writes panics) at f.
func redirectStderr(f *os.File) {
	_ = windows.SetStdHandle(windows.STD_ERROR_HANDLE, windows.Handle(f.Fd()))
	os.Stderr = f
}
