package supervisor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ErrNotOurs is returned when an operation would touch a tmux server outside
// the daemon's owned root.
var ErrNotOurs = errors.New("supervisor: refusing to touch a tmux server outside the Pyre-owned root")

// Tmux controls one isolated tmux server identified by its socket path.
// The zero Bin means "tmux" on PATH.
type Tmux struct {
	Bin    string // tmux executable
	Socket string // absolute socket path (one server per run)
	// OwnedRoot, when set, must contain Socket for destructive operations
	// (Kill). The daemon sets it to its state directory.
	OwnedRoot string
	// Timeout bounds each tmux invocation (default 10s).
	Timeout time.Duration
}

func (t Tmux) bin() string {
	if t.Bin != "" {
		return t.Bin
	}
	return "tmux"
}

// baseEnv returns the environment for tmux client invocations: the
// daemon's environment without variables that would make tmux talk to the
// caller's own server.
func baseEnv(extra map[string]string) []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if k == "TMUX" || k == "TMUX_PANE" {
			continue
		}
		if _, override := extra[k]; override {
			continue
		}
		env = append(env, kv)
	}
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+extra[k])
	}
	return env
}

func (t Tmux) run(ctx context.Context, env map[string]string, args ...string) (string, error) {
	if t.Socket == "" {
		return "", errors.New("supervisor: tmux socket not set")
	}
	to := t.Timeout
	if to <= 0 {
		to = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()
	full := append([]string{"-S", t.Socket, "-f", "/dev/null"}, args...)
	cmd := exec.CommandContext(ctx, t.bin(), full...)
	cmd.Env = baseEnv(env)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	if err != nil {
		return out.String(), &TmuxError{Args: args, Stderr: strings.TrimSpace(errb.String()), Err: err}
	}
	return out.String(), nil
}

// TmuxError is a failed tmux invocation (stderr retained, env never).
type TmuxError struct {
	Args   []string
	Stderr string
	Err    error
}

func (e *TmuxError) Error() string {
	verb := ""
	if len(e.Args) > 0 {
		verb = e.Args[0]
	}
	return fmt.Sprintf("tmux %s: %v: %s", verb, e.Err, e.Stderr)
}

func (e *TmuxError) Unwrap() error { return e.Err }

// noServer reports whether err means the server/session does not exist.
func noServer(err error) bool {
	var te *TmuxError
	if !errors.As(err, &te) {
		return false
	}
	s := strings.ToLower(te.Stderr)
	return strings.Contains(s, "no server running") ||
		strings.Contains(s, "can't find session") ||
		strings.Contains(s, "session not found") ||
		strings.Contains(s, "error connecting") ||
		strings.Contains(s, "no such file or directory") ||
		strings.Contains(s, "can't find pane") ||
		strings.Contains(s, "can't find window")
}

// SessionSpec describes a new tmux session.
type SessionSpec struct {
	Name string
	Cwd  string
	// Env is the environment of the tmux server (and thus the pane). It is
	// passed through the process environment, never on a command line.
	Env map[string]string
	// Argv is the program to run in the pane. Empty = the user's shell.
	Argv []string
	// RemainOnExit keeps the pane after the program exits so its exit
	// status can be observed.
	RemainOnExit bool
	Width        int // default 200
	Height       int // default 50
}

// NewSession starts the server (if needed) and creates the session. It
// fails if the session already exists; use Ensure to recover-or-create.
func (t Tmux) NewSession(ctx context.Context, s SessionSpec) error {
	if s.Name == "" {
		return errors.New("supervisor: session name required")
	}
	if dir := filepath.Dir(t.Socket); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	w, h := s.Width, s.Height
	if w <= 0 {
		w = 200
	}
	if h <= 0 {
		h = 50
	}
	args := []string{"new-session", "-d", "-s", s.Name, "-x", strconv.Itoa(w), "-y", strconv.Itoa(h)}
	if s.Cwd != "" {
		args = append(args, "-c", s.Cwd)
	}
	if len(s.Argv) > 0 {
		args = append(args, ShellJoin(s.Argv))
	}
	if s.RemainOnExit {
		args = append(args, ";", "set-option", "-w", "-t", s.Name, "remain-on-exit", "on")
	}
	_, err := t.run(ctx, s.Env, args...)
	if err != nil {
		return err
	}
	// The socket must not be reachable by other users.
	_ = os.Chmod(t.Socket, 0o600)
	return nil
}

// Ensure returns recovered=true if the session already exists on this
// server, otherwise creates it.
func (t Tmux) Ensure(ctx context.Context, s SessionSpec) (recovered bool, err error) {
	ok, err := t.HasSession(ctx, s.Name)
	if err != nil {
		return false, err
	}
	if ok {
		return true, nil
	}
	return false, t.NewSession(ctx, s)
}

// HasSession reports whether the named session exists. A missing server or
// socket is (false, nil).
func (t Tmux) HasSession(ctx context.Context, name string) (bool, error) {
	if _, err := os.Stat(t.Socket); errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	_, err := t.run(ctx, nil, "has-session", "-t", name)
	if err == nil {
		return true, nil
	}
	var te *TmuxError
	if noServer(err) || (errors.As(err, &te) && te.Stderr == "") {
		return false, nil
	}
	return false, err
}

func (t Tmux) display(ctx context.Context, target, format string) (string, error) {
	args := []string{"display-message", "-p"}
	if target != "" {
		args = append(args, "-t", target)
	}
	args = append(args, format)
	out, err := t.run(ctx, nil, args...)
	return strings.TrimSpace(out), err
}

// ServerPID returns the tmux server's PID.
func (t Tmux) ServerPID(ctx context.Context) (int, error) {
	s, err := t.display(ctx, "", "#{pid}")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(s)
}

// PaneInfo describes the first pane of a target.
type PaneInfo struct {
	PID        int
	Dead       bool
	ExitStatus int
	Command    string
}

// Pane returns information about the target's active pane.
func (t Tmux) Pane(ctx context.Context, target string) (PaneInfo, error) {
	// "|", not a tab: tmux 3.6 prints control characters in
	// display-message output as "_" (server: "4242_0_..."), so tab-separated
	// fields never split. The command name comes last and keeps any "|".
	s, err := t.display(ctx, target, "#{pane_pid}|#{pane_dead}|#{pane_dead_status}|#{pane_current_command}")
	if err != nil {
		return PaneInfo{}, err
	}
	parts := strings.SplitN(s, "|", 4)
	var p PaneInfo
	if len(parts) > 0 {
		p.PID, _ = strconv.Atoi(parts[0])
	}
	if len(parts) > 1 {
		p.Dead = parts[1] == "1"
	}
	if len(parts) > 2 {
		p.ExitStatus, _ = strconv.Atoi(parts[2])
	}
	if len(parts) > 3 {
		p.Command = parts[3]
	}
	return p, nil
}

// Capture returns the visible pane text plus up to history lines of
// scrollback.
func (t Tmux) Capture(ctx context.Context, target string, history int) (string, error) {
	args := []string{"capture-pane", "-p", "-t", target}
	if history > 0 {
		args = append(args, "-S", "-"+strconv.Itoa(history))
	}
	return t.run(ctx, nil, args...)
}

// Clients returns the number of clients attached to the target session.
// An attached human client acts as an implicit raw-input lease.
func (t Tmux) Clients(ctx context.Context, target string) (int, error) {
	out, err := t.run(ctx, nil, "list-clients", "-t", target, "-F", "#{client_tty}")
	if err != nil {
		if noServer(err) {
			return 0, nil
		}
		return 0, err
	}
	n := 0
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) != "" {
			n++
		}
	}
	return n, nil
}

// SendKeys sends literal text (no Enter) or, with keys=true, tmux key names.
// Callers must hold the raw-input right (human lease or adapter-gated
// automation); the supervisor does not decide that.
//
// The keys come from no tmux client, so send-keys names one that cannot
// match (-c ""): tmux 3.7 refuses send-keys when its implied target client
// (the best attached one) is read-only. tmux before 3.4 has no -c and gets
// the plain form; see tmux.Server.SendKeys.
func (t Tmux) SendKeys(ctx context.Context, target string, literal bool, keys ...string) error {
	build := func(noClient bool) []string {
		args := []string{"send-keys"}
		if noClient {
			args = append(args, "-c", "")
		}
		args = append(args, "-t", target)
		if literal {
			args = append(args, "-l")
		}
		return append(args, keys...)
	}
	_, err := t.run(ctx, nil, build(true)...)
	if err != nil && (strings.Contains(err.Error(), "unknown flag -c") || strings.Contains(err.Error(), "usage: send-keys")) {
		// Rejected at parse time, so nothing was sent.
		_, err = t.run(ctx, nil, build(false)...)
	}
	return err
}

// Owned reports whether the socket lies under OwnedRoot.
func (t Tmux) Owned() bool {
	if t.OwnedRoot == "" || t.Socket == "" {
		return false
	}
	root, err1 := filepath.Abs(t.OwnedRoot)
	sock, err2 := filepath.Abs(t.Socket)
	if err1 != nil || err2 != nil {
		return false
	}
	rel, err := filepath.Rel(root, sock)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return false
	}
	return true
}

// Kill terminates this tmux server and removes its socket. It refuses
// unless the socket is under OwnedRoot (never the user's default server).
// A missing server is not an error.
func (t Tmux) Kill(ctx context.Context) error {
	if !t.Owned() {
		return ErrNotOurs
	}
	if _, err := os.Stat(t.Socket); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	_, err := t.run(ctx, nil, "kill-server")
	if err != nil && !noServer(err) {
		return err
	}
	_ = os.Remove(t.Socket)
	return nil
}

// AttachArgv returns the argv a local client runs to attach.
func (t Tmux) AttachArgv(target string, readOnly bool) []string {
	argv := []string{t.bin(), "-S", t.Socket, "attach-session", "-t", target}
	if readOnly {
		argv = append(argv, "-r")
	}
	return argv
}

// ShellJoin quotes argv for a POSIX shell.
func ShellJoin(argv []string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = ShellQuote(a)
	}
	return strings.Join(q, " ")
}

// ShellQuote single-quotes s for a POSIX shell when needed.
func ShellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-_./=:@%+,", c)) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
