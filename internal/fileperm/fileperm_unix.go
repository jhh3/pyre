//go:build !windows

package fileperm

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
)

func exposed(_ string, fi os.FileInfo) (string, bool) {
	perm := fi.Mode().Perm()
	return fmt.Sprintf("mode %04o", perm), perm&0o077 != 0
}

func foreignOwner(_ string, fi os.FileInfo) (string, bool) {
	if sys, ok := fi.Sys().(*syscall.Stat_t); ok && int(sys.Uid) != os.Getuid() {
		return "uid " + strconv.Itoa(int(sys.Uid)), true
	}
	return "", false
}

func restrict(path string, mode os.FileMode) error { return os.Chmod(path, mode) }

func restrictFile(f *os.File, mode os.FileMode) error { return f.Chmod(mode) }

func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}

func fixHint(path string, dir bool) string {
	if dir {
		return "run `chmod 700 " + path + "`"
	}
	return "run `chmod 600 " + path + "`"
}

func protectNewDir(string) error { return nil }
