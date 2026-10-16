//go:build !windows

package tui

import "syscall"

// windowsRawCmdLine is only used on Windows.
func windowsRawCmdLine(string) *syscall.SysProcAttr { return nil }
