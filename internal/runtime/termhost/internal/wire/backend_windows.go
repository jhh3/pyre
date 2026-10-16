//go:build windows

package wire

import (
	"fmt"

	"golang.org/x/sys/windows"
)

var procCreatePseudoConsole = windows.NewLazySystemDLL("kernel32.dll").NewProc("CreatePseudoConsole")

// BackendCheck reports whether ConPTY exists (Windows 10 1809+).
func BackendCheck() (string, error) {
	if err := procCreatePseudoConsole.Find(); err != nil {
		return "conpty", fmt.Errorf("ConPTY is not available (Windows 10 1809 or later required): %v", err)
	}
	return "conpty", nil
}
