//go:build windows

package tmux

import "time"

// terminateGroup: Windows has no tmux and no process groups; the tmux
// backend is never selected there (hostsel picks termhost), and
// TerminateSession falls back to kill-session.
func terminateGroup(pid int, grace time.Duration) {}
