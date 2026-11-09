//go:build !linux

package daemon

// lowerThreadPriority is a no-op off Linux (priorities are per process
// there, and the rate limit still applies).
func lowerThreadPriority() error { return nil }
