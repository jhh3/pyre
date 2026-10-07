// Package bundled locates the optional tools (Hive, Outfit, Rota) that
// `install.sh --with-tools` places beside a Pyre installation:
//
//	<prefix>/tools/<tool>/bin/<tool>   with the marker <prefix>/tools/.pyre-tools
//
// where <prefix> is the directory holding the version directories
// (~/.local/share/pyre), i.e. three levels above the running executable
// (<prefix>/<version>/bin/pyred). Integrations prefer a bundled tool and fall
// back to their usual lookup (PATH, plugin caches) when none is installed, so
// nodes with separately installed tools behave exactly as before.
package bundled

import (
	"os"
	"path/filepath"
)

// Marker is the file install.sh writes into <prefix>/tools.
const Marker = ".pyre-tools"

var executable = os.Executable

// Path returns the bundled executable for tool ("hive", "outfit", "rota"), or
// "" when this Pyre installation has none.
func Path(tool string) string {
	exe, err := executable()
	if err != nil {
		return ""
	}
	return PathFrom(exe, tool)
}

// PathFrom is Path for an explicit Pyre executable path.
func PathFrom(exe, tool string) string {
	if tool == "" || filepath.Base(tool) != tool {
		return ""
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	prefix := filepath.Dir(filepath.Dir(filepath.Dir(exe)))
	dir := filepath.Join(prefix, "tools")
	if fi, err := os.Stat(filepath.Join(dir, Marker)); err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	p := filepath.Join(dir, tool, "bin", tool)
	fi, err := os.Stat(p)
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		return ""
	}
	return p
}
