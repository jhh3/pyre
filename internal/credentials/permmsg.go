package credentials

import (
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/jhh3/pyre/internal/fileperm"
)

// fixCmd is the command that makes p private ("chmod 700 DIR" / "chmod 600
// FILE" on Unix, an icacls command on Windows).
func fixCmd(p string, dir bool) string {
	if runtime.GOOS != "windows" {
		if dir {
			return "chmod 700 " + p
		}
		return "chmod 600 " + p
	}
	return strings.TrimPrefix(fileperm.FixHint(p, dir), "run ")
}

// modeDesc describes why st is not private: its octal mode on Unix, the
// offending ACL entry on Windows.
func modeDesc(st os.FileInfo, desc string) string {
	if runtime.GOOS != "windows" {
		return fmt.Sprintf("%o", st.Mode().Perm())
	}
	return desc
}
