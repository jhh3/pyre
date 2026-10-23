//go:build !linux

package headless

// procStart is unknown off Linux: pids are compared by liveness only.
func procStart(pid int) string { return "" }
