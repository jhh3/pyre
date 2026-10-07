// Package stablepath maps a binary inside a versioned Pyre install
// (…/share/pyre/v0.1.0-rcN/bin/pyred) to the same binary through the
// install's "current" link (…/share/pyre/current/bin/pyred). Paths written
// into long-lived places (Claude hooks, run environments) must use the
// stable form, so removing an old version never breaks running sessions.
package stablepath

import (
	"os"
	"path/filepath"
)

// Of returns the "current"-link form of p when p lives in a versioned
// install whose current link points at the same file, else p unchanged.
func Of(p string) string {
	bin := filepath.Dir(p)
	if filepath.Base(bin) != "bin" {
		return p
	}
	ver := filepath.Dir(bin)
	root := filepath.Dir(ver)
	if filepath.Base(ver) == "current" {
		return p
	}
	alt := filepath.Join(root, "current", "bin", filepath.Base(p))
	a, err1 := os.Stat(alt)
	b, err2 := os.Stat(p)
	if err1 != nil || err2 != nil || !os.SameFile(a, b) {
		return p
	}
	return alt
}
