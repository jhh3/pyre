//go:build linux || darwin

package sysstats

import (
	"os"
	"syscall"
)

func statfs(path string) (FSInfo, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return FSInfo{}, err
	}
	bs := uint64(st.Bsize) //nolint:gosec // block size is positive
	fi := FSInfo{Total: st.Blocks * bs, Avail: st.Bavail * bs}
	if info, err := os.Stat(path); err == nil {
		if sys, ok := info.Sys().(*syscall.Stat_t); ok {
			fi.Dev = uint64(sys.Dev) //nolint:gosec,unconvert // int32 on darwin
		}
	}
	return fi, nil
}
