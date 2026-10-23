//go:build windows

package envfile

import (
	"os"

	"github.com/jhh3/pyre/internal/fileperm"
)

// openNonBlocking opens path read-only without following a reparse point.
// Named pipes live under \\.\pipe, which CheckPath refuses.
func openNonBlocking(path string) (*os.File, error) { return fileperm.OpenNoFollow(path) }
