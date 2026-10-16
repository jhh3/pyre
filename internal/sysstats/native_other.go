//go:build !windows

package sysstats

import (
	"errors"
	"time"
)

// errNotWindows: the native (Win32) readers exist on Windows only; Unix
// keeps /proc, sysctl and ps.
var errNotWindows = errors.New("sysstats: native Windows counters are not available on this platform")

func winRaw() (WinRaw, error) { return WinRaw{}, errNotWindows }

// NativeProcTable returns the Win32 process table (Windows only).
func NativeProcTable() (map[int]Proc, error) { return nil, errNotWindows }

// NativeProcess samples one process through Win32 (Windows only).
func NativeProcess(int) (Proc, error) { return Proc{}, errNotWindows }

// NativeSelf returns this process's cumulative CPU and working set
// (Windows only).
func NativeSelf() (time.Duration, int64, error) { return 0, 0, errNotWindows }

// NativeMemory returns physical memory total/available bytes (Windows only).
func NativeMemory() (total, avail uint64, err error) { return 0, 0, errNotWindows }
