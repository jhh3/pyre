//go:build !windows

package envfile

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

type fakeInfo struct {
	mode fs.FileMode
	uid  int
}

func (f fakeInfo) Name() string       { return "aide.env" }
func (f fakeInfo) Size() int64        { return 0 }
func (f fakeInfo) Mode() fs.FileMode  { return f.mode }
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return false }
func (f fakeInfo) Sys() any           { return &syscall.Stat_t{Uid: uint32(f.uid)} }

func TestCheckFileOwner(t *testing.T) {
	if err := CheckFile("/x/aide.env", fakeInfo{mode: 0o600, uid: os.Getuid()}); err != nil {
		t.Fatal(err)
	}
	err := CheckFile("/x/aide.env", fakeInfo{mode: 0o600, uid: os.Getuid() + 1})
	if err == nil || !strings.Contains(err.Error(), "not this node's user") {
		t.Fatalf("foreign owner: %v", err)
	}
}

// A named pipe is refused at once, never opened: an open with no writer
// would block the node's command queue (review H1).
func TestReadRefusesFIFOWithoutBlocking(t *testing.T) {
	p := filepath.Join(t.TempDir(), "aide.env")
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan error, 1)
	go func() { _, err := Read(p); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "named pipe") {
			t.Fatalf("fifo: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Read blocked on a FIFO")
	}
	// The descriptor check: a FIFO opened non-blocking is refused as well.
	f, err := openNonBlocking(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fi, _ := f.Stat()
	if err := CheckFile(p, fi); err == nil {
		t.Fatal("fifo descriptor accepted")
	}
}
