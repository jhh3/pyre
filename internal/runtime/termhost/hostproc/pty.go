package hostproc

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/runtime/termhost/internal/wire"
)

// proc is one program running on a pseudo-terminal (a ConPTY on Windows,
// a Unix PTY elsewhere).
type proc interface {
	// Read returns terminal output; it fails once the terminal is gone.
	Read(p []byte) (int, error)
	// Write sends terminal input.
	Write(p []byte) (int, error)
	Resize(cols, rows int) error
	Pid() int
	// Wait blocks until the program exits. exit is nil when a signal ended
	// it. It is called exactly once, by the session's wait goroutine.
	Wait() (exit *int)
	// Exited is called after Wait returns: a Windows pseudoconsole is
	// closed then (after a short drain) so Read reaches EOF.
	Exited()
	// Terminate ends the program's whole process tree gracefully
	// (SIGTERM to the group / CTRL_CLOSE_EVENT), forcefully after grace.
	Terminate(grace time.Duration)
	// Kill ends the process tree at once.
	Kill()
	// Close releases the terminal.
	Close() error
}

// startFunc starts argv on a new pseudo-terminal of cols x rows. env is the
// program's complete environment.
type startFunc func(argv []string, cwd string, env []string, cols, rows int) (proc, error)

// childEnv is base without unset (unless explicitly set), plus set.
// Variable names compare case-insensitively on Windows.
func childEnv(base []string, set map[string]string, unset []string) []string {
	drop := map[string]bool{}
	for _, k := range unset {
		drop[wire.EnvKey(k)] = true
	}
	for k := range set {
		drop[wire.EnvKey(k)] = true // replaced below
	}
	out := make([]string, 0, len(base)+len(set))
	for _, kv := range base {
		if drop[wire.EnvKey(kv)] {
			continue
		}
		out = append(out, kv)
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+set[k])
	}
	return out
}

func envLookup(env []string, key string) string {
	want := key
	if wire.WindowsEnv {
		want = strings.ToUpper(key)
	}
	for i := len(env) - 1; i >= 0; i-- {
		if wire.EnvKey(env[i]) == want {
			_, v, _ := strings.Cut(env[i], "=")
			return v
		}
	}
	return ""
}

var errNotFound = errors.New("executable file not found in the session's PATH")

// lookPathEnv resolves argv[0] the way the program's own environment would
// (its PATH, and PATHEXT on Windows), as tmux's execvp does for a pane.
func lookPathEnv(file string, env []string) (string, error) {
	var exts []string
	if wire.WindowsEnv {
		pe := envLookup(env, "PATHEXT")
		if pe == "" {
			pe = ".COM;.EXE;.BAT;.CMD"
		}
		for _, e := range strings.Split(strings.ToLower(pe), ";") {
			if e != "" && strings.HasPrefix(e, ".") {
				exts = append(exts, e)
			}
		}
	}
	try := func(p string) (string, bool) {
		if wire.WindowsEnv {
			if filepath.Ext(p) != "" && isExec(p) {
				return p, true
			}
			for _, e := range exts {
				if isExec(p + e) {
					return p + e, true
				}
			}
			return "", false
		}
		return p, isExec(p)
	}
	if strings.ContainsAny(file, `/\`) || filepath.IsAbs(file) {
		if p, ok := try(file); ok {
			return p, nil
		}
		return "", &os.PathError{Op: "exec", Path: file, Err: errNotFound}
	}
	for _, dir := range filepath.SplitList(envLookup(env, "PATH")) {
		if dir == "" {
			continue
		}
		if p, ok := try(filepath.Join(dir, file)); ok {
			return p, nil
		}
	}
	return "", &os.PathError{Op: "exec", Path: file, Err: errNotFound}
}

func isExec(p string) bool {
	st, err := os.Stat(p)
	if err != nil || st.IsDir() {
		return false
	}
	if wire.WindowsEnv {
		return true
	}
	return st.Mode().Perm()&0o111 != 0
}
