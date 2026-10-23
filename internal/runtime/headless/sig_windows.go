//go:build windows

package headless

import "os"

func terminateGroup(pgid int) error { return os.ErrInvalid }
