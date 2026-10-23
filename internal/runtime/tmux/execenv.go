package tmux

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/fileperm"
	"github.com/jhh3/pyre/internal/osutil"
)

// A run's environment (PYRE_SESSION_TOKEN, connection secrets, API keys from
// environment overlays) must never appear on a command line: `tmux
// new-session -e K=V` would put it in the argv of the tmux client, visible
// in ps to every local user for the client's lifetime, and in the session
// environment, inherited by every window a human opens in that session.
//
// Instead the environment and the program's argv are written to a private
// launch file (0600, in the 0700 socket directory) and the pane runs
//
//	<this executable> __pyre_exec_env <file> --
//
// which reads and deletes the file, applies it to its own environment, and
// execs the argv it holds in place (same PID, same process group). Only the
// file path is on any command line. The pane's process gets exactly the
// environment tmux -e used to give it; the tmux server, its clients and the
// session environment never hold it, so windows opened later by a human do
// not inherit it.
//
// The argv is in the file too because tmux refuses a client command larger
// than one imsg (MAX_IMSGSIZE, 16 KiB, all arguments included): "command
// too long". A Claude launch carries the agent guide (--append-system-prompt)
// and the initial prompt, which together reach that easily (laptop, tmux
// 3.6, a 5 KB brief). Elements reach the program byte for byte (no shell,
// no tmux parsing), so quotes, newlines, "$", backticks, a trailing ";" and
// non-ASCII text arrive unchanged. A spec without environment changes and
// with a short argv (maxInlineArgv) still goes on the command line.

// ExecEnvArg is the argv[1] sentinel of the pane launcher. Every binary that
// links this package (pyred, and the test binaries that start tmux panes)
// acts as the launcher when started with it; see init.
const ExecEnvArg = "__pyre_exec_env"

// maxInlineArgv bounds the argv (bytes, separators included) a pane command
// without environment changes carries on the tmux command line; a longer one
// goes in a launch file. Well under tmux's 16 KiB client message limit.
const maxInlineArgv = 4 << 10

// envFilePrefix names the private launch files in the socket dir.
const envFilePrefix = "pyre-env-"

// staleEnvFile is the age after which a leftover environment file (its pane
// never started) is removed by the next launch.
const staleEnvFile = 2 * time.Minute

func init() {
	if len(os.Args) > 1 && os.Args[1] == ExecEnvArg {
		os.Exit(execEnvMain(os.Args[2:]))
	}
}

// execEnvMain implements `__pyre_exec_env <file> --` (the argv is in the
// file) and the earlier `__pyre_exec_env <file> -- argv...`. It returns
// only on failure (exit status 127, error on stderr, which the pane shows).
func execEnvMain(args []string) int {
	if len(args) < 2 || args[1] != "--" {
		fmt.Fprintln(os.Stderr, "pyre: usage: "+ExecEnvArg+" <launch-file> --")
		return 127
	}
	file, argv := args[0], args[2:]
	lf, err := readLaunchFile(file)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pyre: launch environment: %v\n", err)
		return 127
	}
	switch {
	case len(lf.Argv) > 0 && len(argv) > 0:
		fmt.Fprintln(os.Stderr, "pyre: launch: argv both in the launch file and on the command line")
		return 127
	case len(lf.Argv) > 0:
		argv = lf.Argv
	}
	if len(argv) == 0 || argv[0] == "" {
		fmt.Fprintln(os.Stderr, "pyre: launch: no program to run")
		return 127
	}
	set, unset := lf.Set, lf.Unset
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
	if !strings.ContainsAny(path, `/\`) {
		p, err := exec.LookPath(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "pyre: %v\n", err)
			return 127
		}
		path = p
	}
	err = osutil.Exec(path, argv, os.Environ())
	fmt.Fprintf(os.Stderr, "pyre: exec %s: %v\n", argv[0], err)
	return 127
}

// launchFile is the content of a launch file: environment changes and,
// in files that carry it, the program's argv.
type launchFile struct {
	Set   []string // KEY=VALUE
	Unset []string
	Argv  []string
}

// readLaunchFile reads and removes a launch file. It refuses anything but
// a regular file owned by this user and not readable by others.
func readLaunchFile(file string) (launchFile, error) {
	var lf launchFile
	st, err := os.Lstat(file)
	if err != nil {
		return lf, err
	}
	defer os.Remove(file)
	if _, bad := fileperm.Exposed(file, st); !st.Mode().IsRegular() || bad {
		return lf, fmt.Errorf("%s: not a private regular file (%v)", file, st.Mode())
	}
	if owner, bad := fileperm.ForeignOwner(file, st); bad {
		return lf, fmt.Errorf("%s: owned by %s", file, owner)
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return lf, err
	}
	for len(b) > 0 {
		// Every record is NUL-terminated and starts with its type byte, so
		// an empty argv element is "A\x00".
		i := bytes.IndexByte(b, 0)
		if i < 0 {
			return launchFile{}, fmt.Errorf("unterminated record")
		}
		rec := b[:i]
		b = b[i+1:]
		if len(rec) == 0 {
			continue
		}
		body := string(rec[1:])
		switch rec[0] {
		case 'U':
			if !envKeyRe.MatchString(body) {
				return launchFile{}, fmt.Errorf("bad unset record")
			}
			lf.Unset = append(lf.Unset, body)
		case 'S':
			k, _, ok := strings.Cut(body, "=")
			if !ok || !envKeyRe.MatchString(k) {
				return launchFile{}, fmt.Errorf("bad set record")
			}
			lf.Set = append(lf.Set, body)
		case 'A':
			lf.Argv = append(lf.Argv, body)
		default:
			return launchFile{}, fmt.Errorf("bad record type %q", rec[0])
		}
	}
	return lf, nil
}

// encodeLaunch renders a launch file: the environment (encodeEnv), then one
// argv record per element, in order.
func encodeLaunch(env map[string]string, unset, argv []string) []byte {
	b := encodeEnv(env, unset)
	for _, a := range argv {
		b = append(b, 'A')
		b = append(b, a...)
		b = append(b, 0)
	}
	return b
}

// encodeEnv renders the environment records of a launch file: unset
// records for Unset names not set explicitly, then set records (explicit
// wins), each NUL-terminated.
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
// changes or a long argv runs through the launcher with a private launch
// file holding both, whose path is returned so a failed tmux call can
// remove it. The tmux command line then carries only the launcher and the
// file path, whatever the size of the prompts in argv.
func (s *Server) paneCommand(spec SessionSpec) (argv []string, envFile string, err error) {
	if len(spec.Env) == 0 && len(spec.Unset) == 0 && argvSize(spec.Argv) <= maxInlineArgv {
		return escapeSeparators(commandArgs(spec.Argv)), "", nil
	}
	helper, err := s.execHelper()
	if err != nil {
		return nil, "", err
	}
	envFile, err = s.writeEnvFile(encodeLaunch(spec.Env, spec.Unset, spec.Argv))
	if err != nil {
		return nil, "", err
	}
	return escapeSeparators([]string{helper, ExecEnvArg, envFile, "--"}), envFile, nil
}

// argvSize is the length of argv joined with one separator per element.
func argvSize(argv []string) int {
	n := 0
	for _, a := range argv {
		n += len(a) + 1
	}
	return n
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
	if err := fileperm.RestrictFile(f, 0o600); err != nil {
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
