//go:build windows

package claude

import "os"

// notifyResize: Windows has no SIGWINCH; the fake renders on input only.
func notifyResize(chan os.Signal) {}

// termWidth is the fake's terminal width (the Windows fake does not query
// its console).
func termWidth() int { return 200 }
