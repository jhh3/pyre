//go:build windows

package tui

import "syscall"

// windowsRawCmdLine passes args to cmd.exe verbatim (cmd.exe does its own
// parsing; Go's argument quoting would escape the quotes).
func windowsRawCmdLine(args string) *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CmdLine: "cmd.exe " + args}
}
