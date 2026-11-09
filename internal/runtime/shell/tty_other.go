//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly)

package shell

// lineEditing is unknown here (Windows: the termhost's ConPTY has no tty
// to inspect); the screen decides when the shell is ready.
func lineEditing(string) (raw, known bool) { return false, false }
