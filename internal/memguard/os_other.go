//go:build !linux && !darwin

package memguard

import (
	"context"
	"errors"
	"os"
	"syscall"

	"github.com/jhh3/pyre/internal/sysstats"
)

// Supported reports whether the guard can run on this platform (not on
// Windows: no process tree of runs to read, and termhost runs have no
// session-host root the guard could use).
func Supported() bool { return false }

type noOS struct{}

// NewOS returns a view that never reports low memory.
func NewOS() OS { return noOS{} }

func selfPID() int { return os.Getpid() }

func (noOS) Memory(context.Context) sysstats.Host { return sysstats.Host{} }
func (noOS) Table(context.Context) (map[int]Proc, error) {
	return nil, errors.New("memguard: unsupported platform")
}
func (noOS) Lookup(Proc) (Proc, bool) { return Proc{}, false }
func (noOS) Signal(Proc, syscall.Signal) error {
	return errors.New("memguard: unsupported platform")
}
func (noOS) Describe(Proc) (string, string, string) { return "", "", "" }
func (noOS) RunEnv(int) string                      { return "" }
