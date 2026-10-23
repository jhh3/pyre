//go:build !linux && !darwin

package cli

import (
	"errors"
	"os"
)

// reopenRead is not available here: a write-only stdout file leaves the
// native id to the node's own lookup.
func reopenRead(f *os.File) (*os.File, error) { return nil, errors.ErrUnsupported }
