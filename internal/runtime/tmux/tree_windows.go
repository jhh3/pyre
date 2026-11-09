//go:build windows

package tmux

import "errors"

// Windows runs use termhost, not tmux; the tree is not read there.
func platformListProcs() ([]procInfo, error) { return nil, errors.ErrUnsupported }
func platformProcCgroup(int) string          { return "" }
func platformSignal(int, bool) error         { return errors.ErrUnsupported }
