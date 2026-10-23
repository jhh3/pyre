//go:build !windows

package cli

import (
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
)

// forwardedSignals reach the child of pyre run.
var forwardedSignals = []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGUSR1, syscall.SIGUSR2}

// stopSignal ends a child whose run was stopped before it started.
var stopSignal os.Signal = syscall.SIGTERM

// fromTerminal: signals a terminal sends to its whole foreground process
// group (the child, in pyre's group, gets them directly).
func fromTerminal(s os.Signal) bool {
	return s == syscall.SIGINT || s == syscall.SIGQUIT || s == syscall.SIGHUP
}

// pipeSigs catches SIGPIPE while pyre run relays output: a handler (not
// SIG_IGN, which the child would inherit) makes the write return EPIPE.
var pipeSigs = make(chan os.Signal, 1)

func ignorePipeSignal()  { signal.Notify(pipeSigs, syscall.SIGPIPE) }
func restorePipeSignal() { signal.Stop(pipeSigs) }

// controllingTTY tells whether pyre's process group is the terminal's
// foreground group (then a Ctrl-C reached the child too).
type controllingTTY struct{ f *os.File }

func openControllingTTY() controllingTTY {
	f, err := os.Open("/dev/tty")
	if err != nil {
		return controllingTTY{}
	}
	return controllingTTY{f: f}
}

func (t controllingTTY) foreground() bool {
	if t.f == nil {
		return false
	}
	pg, err := unix.IoctlGetInt(int(t.f.Fd()), unix.TIOCGPGRP)
	return err == nil && pg == syscall.Getpgrp()
}

func (t controllingTTY) close() {
	if t.f != nil {
		t.f.Close()
	}
}

// exitStatus is the child's exit code, or the signal that killed it.
func exitStatus(ps *os.ProcessState, err error) (int, string) {
	if ps == nil {
		if ee, ok := err.(*exec.ExitError); ok {
			ps = ee.ProcessState
		}
	}
	if ps == nil {
		return 1, ""
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal()), unixSignalName(ws.Signal())
	}
	return ps.ExitCode(), ""
}

func unixSignalName(s syscall.Signal) string {
	switch s {
	case syscall.SIGINT:
		return "SIGINT"
	case syscall.SIGTERM:
		return "SIGTERM"
	case syscall.SIGKILL:
		return "SIGKILL"
	case syscall.SIGHUP:
		return "SIGHUP"
	case syscall.SIGQUIT:
		return "SIGQUIT"
	case syscall.SIGPIPE:
		return "SIGPIPE"
	case syscall.SIGABRT:
		return "SIGABRT"
	case syscall.SIGSEGV:
		return "SIGSEGV"
	}
	return "signal " + s.String()
}

// reraise ends this process by the signal that ended the child, so the
// caller sees the same wait status (Python: returncode -N).
func reraise(ps *os.ProcessState) {
	ws, ok := ps.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return
	}
	sig := ws.Signal()
	signal.Reset(sig)
	_ = syscall.Kill(os.Getpid(), sig)
	// SIGKILL/SIGSTOP or a signal that does not terminate: fall through to
	// the 128+N exit code.
}

// Output kinds of pyre run's stdout.
const (
	outTTY    = iota // a terminal: passed as is
	outStream        // a pipe or socket: relayed and scanned
	outFile          // a regular file or device: passed as is
)

func stdoutKind() int {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return outFile
	}
	m := fi.Mode()
	switch {
	case m&os.ModeCharDevice != 0 && isTTY(os.Stdout):
		return outTTY
	case m&os.ModeNamedPipe != 0 || m&os.ModeSocket != 0:
		return outStream
	}
	return outFile
}
