//go:build windows

package tmux

import "github.com/jhh3/pyre/internal/osutil"

// groupAlive reports whether the group leader is still running (Windows has
// no process-group signal 0; tmux does not run natively there anyway).
func groupAlive(pgid int) bool { return osutil.ProcessAlive(pgid) }
