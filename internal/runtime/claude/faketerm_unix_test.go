//go:build !windows

package claude

import (
	"os"
	"os/signal"
	"syscall"
	"unsafe"
)

func notifyResize(c chan os.Signal) { signal.Notify(c, syscall.SIGWINCH) }

// termWidth is the fake's terminal width (200 if unknown).
func termWidth() int {
	var ws struct{ Row, Col, X, Y uint16 }
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, 1, uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws)))
	if e != 0 || ws.Col == 0 {
		return 200
	}
	return int(ws.Col)
}
