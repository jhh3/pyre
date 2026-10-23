//go:build !linux && !windows

package cli

import "syscall"

// childSysProcAttr: no parent-death signal off Linux (the node stops an
// orphaned child); with group the child leads its own process group.
func childSysProcAttr(group bool) *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: group}
}
