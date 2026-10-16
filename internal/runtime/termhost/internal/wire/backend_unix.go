//go:build !windows

package wire

// BackendCheck: Unix PTYs are always there.
func BackendCheck() (string, error) { return "pty", nil }
