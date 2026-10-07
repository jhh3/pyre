package tmux

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// A run's environment (PYRE_SESSION_TOKEN, connection secrets, API keys from
// environment overlays) must never appear on a command line: `tmux
// new-session -e K=V` would put it in the argv of the tmux client, visible
// in ps to every local user for the client's lifetime, and in the session
// environment, inherited by every window a human opens in that session.
//
// Instead the environment is written to a private file (0600, in the 0700
// socket directory) and the pane runs
//
//	<this executable> __pyre_exec_env <file> -- argv...
//
// which reads and deletes the file, applies it to its own environment, and
// execs argv in place (same PID, same process group). Only the file path is
// on any command line. The pane's process gets exactly the environment
// tmux -e used to give it; the tmux server, its clients and the session
// environment never hold it, so windows opened later by a human do not
// inherit it.

// ExecEnvArg is the argv[1] sentinel of the pane launcher. Every binary that
// links this package (pyred, and the test binaries that start tmux panes)
// acts as the launcher when started with it; see init.
const ExecEnvArg = "__pyre_exec_env"

// envFilePrefix names the private environment files in the socket dir.
const envFilePrefix = "pyre-env-"

// staleEnvFile is the age after which a leftover environment file (its pane
// never started) is removed by the next launch.
const staleEnvFile = 2 * time.Minute

func init() {
	if len(os.Args) > 1 && os.Args[1] == ExecEnvArg {
		os.Exit(execEnvMain(os.Args[2:]))
	}
}

// execEnvMain implements `__pyre_exec_env <file> -- argv...`. It returns
// only on failure (exit status 127, error on stderr, which the pane shows).
func execEnvMain(args []string) int {
	if len(args) < 3 || args[1] != "--" || args[2] == "" {
		fmt.Fprintln(os.Stderr, "pyre: usage: "+ExecEnvArg+" <env-file> -- argv...")
		return 127
	}
	file, argv := args[0], args[2:]
	set, unset, err := readEnvFile(file)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pyre: launch environment: %v\n", err)
		return 127
	}
	for _, k := range unset {
		_ = os.Unsetenv(k)
	}
	for _, kv := range set {
		k, v, _ := strings.Cut(kv, "=")
		if err := os.Setenv(k, v); err != nil {
			fmt.Fprintf(os.Stderr, "pyre: launch environment: %s: %v\n", k, err)
			return 127
		}
	}
	// Resolve argv[0] with the new PATH, as tmux's execvp would.
	path := argv[0]
	if !strings.Contains(path, "/") {
		p, err := exec.LookPath(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "pyre: %v\n", err)
			return 127
		}
		path = p
	}
	err = syscall.Exec(path, argv, os.Environ())
	fmt.Fprintf(os.Stderr, "pyre: exec %s: %v\n", argv[0], err)
	return 127
}

// readEnvFile reads and removes an environment file. It refuses anything
// but a regular file owned by this user and not readable by others.
func readEnvFile(file string) (set, unset []string, err error) {
	st, err := os.Lstat(file)
	if err != nil {
		return nil, nil, err
	}
	defer os.Remove(file)
	if !st.Mode().IsRegular() || st.Mode().Perm()&0o077 != 0 {
		return nil, nil, fmt.Errorf("%s: not a private regular file (%v)", file, st.Mode())
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok && int(sys.Uid) != os.Getuid() {
		return nil, nil, fmt.Errorf("%s: owned by uid %d", file, sys.Uid)
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, nil, err
	}
	for _, rec := range bytes.Split(b, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		body := string(rec[1:])
		switch rec[0] {
		case 'U':
			if !envKeyRe.MatchString(body) {
				return nil, nil, fmt.Errorf("bad unset record")
			}
			unset = append(unset, body)
		case 'S':
			k, _, ok := strings.Cut(body, "=")
			if !ok || !envKeyRe.MatchString(k) {
				return nil, nil, fmt.Errorf("bad set record")
			}
			set = append(set, body)
		default:
			return nil, nil, fmt.Errorf("bad record type %q", rec[0])
		}
	}
	return set, unset, nil
}

// encodeEnv renders the environment file: unset records for Unset names
// not set explicitly, then set records (explicit wins), each NUL-terminated.
func encodeEnv(env map[string]string, unset []string) []byte {
	var b bytes.Buffer
	seen := map[string]bool{}
	for _, k := range unset {
		if _, explicit := env[k]; explicit || seen[k] {
			continue
		}
		seen[k] = true
		b.WriteByte('U')
		b.WriteString(k)
		b.WriteByte(0)
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteByte('S')
		b.WriteString(k + "=" + env[k])
		b.WriteByte(0)
	}
	return b.Bytes()
}

// execHelper returns the launcher executable (ExecHelper or this binary).
func (s *Server) execHelper() (string, error) {
	if s.ExecHelper != "" {
		return s.ExecHelper, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("tmux: locate the pane launcher: %w", err)
	}
	return exe, nil
}

// paneCommand returns the pane argv for spec. A spec with environment
// changes runs through the launcher with a private environment file, whose
// path is returned so a failed tmux call can remove it.
func (s *Server) paneCommand(spec SessionSpec) (argv []string, envFile string, err error) {
	if len(spec.Env) == 0 && len(spec.Unset) == 0 {
		return escapeSeparators(commandArgs(spec.Argv)), "", nil
	}
	helper, err := s.execHelper()
	if err != nil {
		return nil, "", err
	}
	envFile, err = s.writeEnvFile(encodeEnv(spec.Env, spec.Unset))
	if err != nil {
		return nil, "", err
	}
	return escapeSeparators(append([]string{helper, ExecEnvArg, envFile, "--"}, spec.Argv...)), envFile, nil
}

// escapeSeparators keeps pane argv elements that end in ";" (an attach
// command such as AttachArgv's "set-option ... ; attach-session") from
// being read by tmux as its own command separator: tmux takes a trailing
// "\;" as a literal ";".
func escapeSeparators(argv []string) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		if strings.HasSuffix(a, ";") {
			a = a[:len(a)-1] + "\\;"
		}
		out[i] = a
	}
	return out
}

// writeEnvFile writes data to a new 0600 file in the (0700) socket dir and
// removes leftovers of panes that never started.
func (s *Server) writeEnvFile(data []byte) (string, error) {
	dir := filepath.Dir(s.Socket)
	if err := PrepareDir(dir); err != nil {
		return "", err
	}
	if old, _ := filepath.Glob(filepath.Join(dir, envFilePrefix+"*")); len(old) > 0 {
		for _, p := range old {
			if st, err := os.Lstat(p); err == nil && time.Since(st.ModTime()) > staleEnvFile {
				_ = os.Remove(p)
			}
		}
	}
	f, err := os.CreateTemp(dir, envFilePrefix+"*")
	if err != nil {
		return "", err
	}
	name := f.Name()
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		os.Remove(name)
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(name)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}

var errNUL = errors.New("tmux: environment value contains a NUL byte")
