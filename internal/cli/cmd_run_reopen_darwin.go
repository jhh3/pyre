package cli

import (
	"bytes"
	"errors"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// reopenRead opens the file behind f read-only (a shell's `> file` opens
// stdout write-only): fcntl F_GETPATH names it, and the file opened must
// be the same one (not a file renamed into its place).
func reopenRead(f *os.File) (*os.File, error) {
	buf := make([]byte, unix.PathMax)
	_, err := unix.FcntlInt(f.Fd(), unix.F_GETPATH, int(uintptr(unsafe.Pointer(&buf[0]))))
	runtime.KeepAlive(buf)
	if err != nil {
		return nil, err
	}
	path := string(buf[:max(0, bytes.IndexByte(buf, 0))])
	r, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	a, err1 := f.Stat()
	b, err2 := r.Stat()
	if err1 != nil || err2 != nil || !os.SameFile(a, b) {
		r.Close()
		return nil, errors.New("stdout file moved")
	}
	return r, nil
}
