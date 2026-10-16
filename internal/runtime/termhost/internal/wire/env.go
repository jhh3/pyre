package wire

import (
	"runtime"
	"strings"
)

// WindowsEnv: variable names compare case-insensitively (Windows).
var WindowsEnv = runtime.GOOS == "windows"

// EnvKey is the name of an environment entry, upper-cased on Windows.
func EnvKey(kv string) string {
	k, _, _ := strings.Cut(kv, "=")
	if WindowsEnv {
		// "=C:=C:\dir" style entries start with '='.
		if strings.HasPrefix(kv, "=") {
			if i := strings.IndexByte(kv[1:], '='); i >= 0 {
				k = kv[:i+1]
			}
		}
		return strings.ToUpper(k)
	}
	return k
}

// HostBaseEnv is the environment sessions start from: the host's own,
// without variables that would point programs at another multiplexer.
func HostBaseEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		switch EnvKey(kv) {
		case "TMUX", "TMUX_PANE":
			continue
		}
		out = append(out, kv)
	}
	return out
}
