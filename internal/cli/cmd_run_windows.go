//go:build windows

package cli

import (
	"os"
	"os/exec"
	"syscall"
)

var forwardedSignals = []os.Signal{os.Interrupt}

var stopSignal os.Signal = os.Kill

func fromTerminal(s os.Signal) bool { return s == os.Interrupt }

func ignorePipeSignal()  {}
func restorePipeSignal() {}

// On Windows a console Ctrl-C reaches every process attached to it.
type controllingTTY struct{}

func openControllingTTY() controllingTTY { return controllingTTY{} }
func (controllingTTY) foreground() bool  { return true }
func (controllingTTY) close()            {}

func exitStatus(ps *os.ProcessState, err error) (int, string) {
	if ps == nil {
		if ee, ok := err.(*exec.ExitError); ok {
			ps = ee.ProcessState
		}
	}
	if ps == nil {
		return 1, ""
	}
	return ps.ExitCode(), ""
}

func reraise(ps *os.ProcessState) {}

const (
	outTTY = iota
	outStream
	outFile
)

func stdoutKind() int {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return outFile
	}
	if fi.Mode()&os.ModeNamedPipe != 0 {
		return outStream
	}
	if fi.Mode()&os.ModeCharDevice != 0 {
		return outTTY
	}
	return outFile
}

func childSysProcAttr(group bool) *syscall.SysProcAttr { return nil }
