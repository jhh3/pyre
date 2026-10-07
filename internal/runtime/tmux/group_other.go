//go:build !linux

package tmux

import "syscall"

// groupAlive reports whether process group pgid still has a member
// (zombies included: there is no portable way to tell them apart).
func groupAlive(pgid int) bool { return syscall.Kill(-pgid, 0) == nil }
