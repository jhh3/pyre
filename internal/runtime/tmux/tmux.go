// Package tmux is a small, safe wrapper around the tmux CLI for the isolated
// tmux server a Pyre node daemon owns. Every call addresses the server by an
// explicit socket path (-S) so the user's default tmux server is never
// touched. Commands are passed as argv (never as interpolated shell strings);
// the only shell strings this package builds are the remote attach command
// for ssh, which is quoted with ShellQuote.
package tmux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jhh3/pyre/internal/supervisor"
)

// MaxSocketPath is the longest socket path accepted. sockaddr_un.sun_path is
// 104 bytes on macOS and 108 on Linux (including the NUL terminator).
const MaxSocketPath = 100

// MaxCaptureBytes bounds capture-pane output returned to callers.
const MaxCaptureBytes = 256 << 10

// DefaultCommandTimeout bounds a single tmux client invocation.
const DefaultCommandTimeout = 10 * time.Second

var (
	// ErrNoServer means no tmux server is listening on the socket.
	ErrNoServer = errors.New("tmux: no server running on socket")
	// ErrNoSession means the target session/pane does not exist.
	ErrNoSession = errors.New("tmux: session not found")
)

// Server addresses one isolated tmux server by socket path.
type Server struct {
	// Bin is the tmux executable (absolute path recommended).
	Bin string
	// Socket is the absolute server socket path (-S).
	Socket string
	// Config is loaded when the server starts (-f). Empty means /dev/null.
	Config string
	// Env is the environment for tmux client invocations. The first
	// new-session starts the server with this environment, and every pane
	// inherits it. Nil means os.Environ() without TMUX/TMUX_PANE.
	Env []string
	// Timeout bounds each tmux invocation (default DefaultCommandTimeout).
	Timeout time.Duration
	// ExecHelper is the pane launcher that applies a session's environment
	// from a private file (see execenv.go). Empty means this executable,
	// which links this package and so supports ExecEnvArg.
	ExecHelper string
}

// NewServer validates the socket path and resolves bin (default "tmux" on
// PATH).
func NewServer(bin, socket string) (*Server, error) {
	if bin == "" {
		bin = "tmux"
	}
	if !filepath.IsAbs(bin) {
		p, err := exec.LookPath(bin)
		if err != nil {
			return nil, fmt.Errorf("tmux: executable %q not found: %w", bin, err)
		}
		bin = p
	}
	if err := ValidateSocketPath(socket); err != nil {
		return nil, err
	}
	return &Server{Bin: bin, Socket: socket}, nil
}

// ValidateSocketPath checks the socket path is absolute and short enough.
func ValidateSocketPath(socket string) error {
	if !filepath.IsAbs(socket) {
		return fmt.Errorf("tmux: socket path must be absolute: %q", socket)
	}
	if len(socket) > MaxSocketPath {
		return fmt.Errorf("tmux: socket path too long (%d > %d bytes): %q", len(socket), MaxSocketPath, socket)
	}
	return nil
}

// PrepareDir creates dir with mode 0700 (and tightens an existing dir).
func PrepareDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

// DefaultConfig is the minimal server configuration Pyre uses. remain-on-exit
// keeps a pane after its process exits so the exit status stays observable.
// mouse on (MouseConfig) lets a writable client's mouse reach applications
// that ask for it (Claude Code and Codex scroll their own transcript with
// the wheel) and scroll tmux history for those that do not; read-only
// clients send no mouse input.
const DefaultConfig = `# Pyre-owned isolated tmux server (generated; do not edit)
set-option -g history-limit 10000
set-option -wg remain-on-exit on
set-option -s escape-time 50
set-option -g exit-empty off
` + MouseConfig

// MouseConfig is the config line that turns tmux mouse support on. Servers
// started before it existed get the same option at every attach
// (AttachArgv).
const MouseConfig = "set-option -g mouse on\n"

// ExtendedKeysConfig lets modified keys such as Shift-Enter reach the
// harness (Claude Code, Codex) instead of arriving as plain Enter. tmux
// forwards extended keys to a pane only when its application asks for them
// ("on", not "always"), in CSI-u form, and treats xterm-like outer terminals
// of attached humans as able to send them. Requires tmux >= 3.5
// (extended-keys-format); see ConfigFor.
const ExtendedKeysConfig = `set-option -s extended-keys on
set-option -s extended-keys-format csi-u
set-option -as terminal-features 'xterm*:extkeys'
`

// ExtendedKeysMin is the first tmux version that gets ExtendedKeysConfig.
// tmux 3.4 has extended-keys but no extended-keys-format, and is left as is.
var ExtendedKeysMin = Version{Major: 3, Minor: 5}

// ConfigFor returns the server configuration for tmux version v. Extended
// keys are added for v >= ExtendedKeysMin unless PYRE_TMUX_EXTENDED_KEYS=0.
func ConfigFor(v Version, getenv func(string) string) string {
	conf := DefaultConfig
	if getenv == nil {
		getenv = os.Getenv
	}
	if v.AtLeast(ExtendedKeysMin.Major, ExtendedKeysMin.Minor) && getenv("PYRE_TMUX_EXTENDED_KEYS") != "0" {
		conf += ExtendedKeysConfig
	}
	return conf
}

// WriteConfig writes the configuration for this tmux binary (ConfigFor)
// next to the socket (0600) and sets s.Config. The socket's directory is
// created 0700. A tmux whose version cannot be read gets DefaultConfig.
func (s *Server) WriteConfig() error {
	dir := filepath.Dir(s.Socket)
	if err := PrepareDir(dir); err != nil {
		return err
	}
	v, _ := s.Version(context.Background())
	p := filepath.Join(dir, "tmux.conf")
	if err := os.WriteFile(p, []byte(ConfigFor(v, nil)), 0o600); err != nil {
		return err
	}
	s.Config = p
	return nil
}

func (s *Server) env() []string {
	if s.Env != nil {
		return s.Env
	}
	var out []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "TMUX=") || strings.HasPrefix(kv, "TMUX_PANE=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// Run executes one tmux client command against this server and returns
// stdout. stdin may be nil.
func (s *Server) Run(ctx context.Context, stdin []byte, args ...string) (string, error) {
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = DefaultCommandTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conf := s.Config
	if conf == "" {
		conf = "/dev/null"
	}
	full := append([]string{"-u", "-S", s.Socket, "-f", conf}, args...)
	cmd := exec.CommandContext(ctx, s.Bin, full...)
	cmd.Env = s.env()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &limitedWriter{w: &stdout, n: MaxCaptureBytes * 2}
	cmd.Stderr = &limitedWriter{w: &stderr, n: 8 << 10}
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	err := cmd.Run()
	if err != nil {
		return stdout.String(), classify(args, stderr.String(), err)
	}
	return stdout.String(), nil
}

func classify(args []string, stderr string, err error) error {
	msg := strings.TrimSpace(stderr)
	switch {
	case strings.Contains(msg, "no server running"),
		strings.Contains(msg, "error connecting to"),
		strings.Contains(msg, "server exited unexpectedly"):
		return fmt.Errorf("%w (%s)", ErrNoServer, msg)
	case strings.Contains(msg, "can't find session"),
		strings.Contains(msg, "session not found"),
		strings.Contains(msg, "can't find pane"),
		strings.Contains(msg, "can't find window"),
		strings.Contains(msg, "no current target"),
		strings.Contains(msg, "no sessions"):
		return fmt.Errorf("%w (%s)", ErrNoSession, msg)
	}
	name := ""
	if len(args) > 0 {
		name = args[0]
	}
	if msg == "" {
		return fmt.Errorf("tmux %s: %w", name, err)
	}
	return fmt.Errorf("tmux %s: %s: %w", name, msg, err)
}

type limitedWriter struct {
	w *bytes.Buffer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if room := l.n - l.w.Len(); room > 0 {
		if len(p) > room {
			l.w.Write(p[:room])
		} else {
			l.w.Write(p)
		}
	}
	return len(p), nil
}

// Version is a parsed `tmux -V`.
type Version struct {
	Major, Minor int
	Raw          string
}

// AtLeast reports whether v >= major.minor.
func (v Version) AtLeast(major, minor int) bool {
	return v.Major > major || (v.Major == major && v.Minor >= minor)
}

func (v Version) String() string { return v.Raw }

var versionRe = regexp.MustCompile(`(\d+)\.(\d+)`)

// ParseVersion parses output like "tmux 3.4", "tmux 3.3a", "tmux next-3.6".
func ParseVersion(out string) (Version, error) {
	raw := strings.TrimSpace(out)
	m := versionRe.FindStringSubmatch(raw)
	if m == nil {
		return Version{Raw: raw}, fmt.Errorf("tmux: cannot parse version %q", raw)
	}
	maj, _ := strconv.Atoi(m[1])
	min, _ := strconv.Atoi(m[2])
	return Version{Major: maj, Minor: min, Raw: raw}, nil
}

// Version runs `tmux -V` (does not contact the server).
func (s *Server) Version(ctx context.Context) (Version, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, s.Bin, "-V").Output()
	if err != nil {
		return Version{}, fmt.Errorf("tmux -V: %w", err)
	}
	return ParseVersion(string(out))
}

// SessionTarget is an exact-match session target ("=name"); plain names are
// prefix-matched by tmux.
func SessionTarget(name string) string { return "=" + name }

// PaneTarget addresses the active pane of the current window of the exact
// session name.
func PaneTarget(name string) string { return "=" + name + ":" }

var nameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// ValidName reports whether name is a safe tmux session/buffer name.
func ValidName(name string) bool { return nameRe.MatchString(name) }

var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// SessionSpec describes a new detached session running argv.
type SessionSpec struct {
	Name string
	Cwd  string
	// Env is added to the pane process's environment. Values may be
	// secrets: they travel through a private 0600 file read (and deleted)
	// by the pane launcher, never through a command line or the tmux
	// session environment, so windows a human opens later in the session
	// do not inherit them.
	Env map[string]string
	// Unset lists inherited variables (panes inherit the server's global
	// environment) removed from the command's environment
	// (ResolvedLaunch.Unset). A variable also present in Env is kept
	// (explicit wins).
	Unset  []string
	Argv   []string
	Width  int // default 200
	Height int // default 50
}

func (spec SessionSpec) validate() error {
	if !ValidName(spec.Name) {
		return fmt.Errorf("tmux: invalid session name %q", spec.Name)
	}
	if len(spec.Argv) == 0 || spec.Argv[0] == "" {
		return errors.New("tmux: empty argv")
	}
	for k, v := range spec.Env {
		if !envKeyRe.MatchString(k) {
			return fmt.Errorf("tmux: invalid environment variable name %q", k)
		}
		if strings.IndexByte(v, 0) >= 0 {
			return fmt.Errorf("%w: %s", errNUL, k)
		}
	}
	for _, k := range spec.Unset {
		if !envKeyRe.MatchString(k) {
			return fmt.Errorf("tmux: invalid environment variable name %q", k)
		}
	}
	return nil
}

// commandArgs renders argv so tmux executes it directly (execvp) rather than
// through `sh -c`: tmux only does that when given two or more arguments, so a
// single-element argv is padded with an empty-free wrapper via `exec`.
func commandArgs(argv []string) []string {
	if len(argv) >= 2 {
		return argv
	}
	// One argument: tmux would run it with sh -c, so quote it safely.
	return []string{"exec " + ShellQuote(argv[0])}
}

// EnsureServer starts the server with a bare `start-server` if it is not
// running. A daemonized tmux server keeps the argv of the client that
// started it for its whole life (visible in ps), so the server must never be
// started by a new-session carrying -e values or a command line. Requires a
// config with exit-empty off (WriteConfig) to stay up without sessions.
//
// On Linux under a systemd user service the server is started in its own
// transient scope (supervisor.DecideScope), so restarting pyred never
// signals runs and pyred's cgroup limits do not apply to them. The
// placement is recorded next to the socket (supervisor.RecordPlacement).
func (s *Server) EnsureServer(ctx context.Context) error {
	if s.up(ctx) {
		return nil
	}
	// Several adapters share one socket; only one of them starts the server.
	mu := startLock(s.Socket)
	mu.Lock()
	defer mu.Unlock()
	if s.up(ctx) {
		return nil
	}
	d := supervisor.DecideScope(ctx)
	if d.Use {
		unit := supervisor.ScopeUnitName(s.Socket)
		stderr, err := s.runArgv(ctx, d.Wrap(unit, s.argv("start-server")))
		if err != nil && supervisor.UnitExists(stderr) {
			unit = strings.TrimSuffix(unit, ".scope") + "-" + strconv.FormatInt(time.Now().UnixNano(), 36) + ".scope"
			stderr, err = s.runArgv(ctx, d.Wrap(unit, s.argv("start-server")))
		}
		if err == nil || s.up(ctx) {
			s.recordPlacement(ctx, "systemd scope "+unit+" in "+d.Slice)
			return nil
		}
		d.Reason = fmt.Sprintf("systemd-run --scope start failed: %v: %s", err, strings.TrimSpace(stderr))
		if d.Required {
			return fmt.Errorf("tmux: %s (PYRE_SYSTEMD_SCOPE=require: not starting runs inside pyred's own cgroup)", d.Reason)
		}
	} else if d.Required {
		return fmt.Errorf("tmux: no systemd scope for the run server: %s (PYRE_SYSTEMD_SCOPE=require: not starting runs inside pyred's own cgroup)", d.Reason)
	}
	_, err := s.Run(ctx, nil, "start-server")
	if err == nil {
		s.recordPlacement(ctx, "direct ("+d.Reason+")")
	}
	return err
}

var (
	startMu    sync.Mutex
	startLocks = map[string]*sync.Mutex{}
)

func startLock(socket string) *sync.Mutex {
	startMu.Lock()
	defer startMu.Unlock()
	m := startLocks[socket]
	if m == nil {
		m = &sync.Mutex{}
		startLocks[socket] = m
	}
	return m
}

// recordPlacement records how the server was started together with the
// cgroup the running server process actually landed in.
func (s *Server) recordPlacement(ctx context.Context, how string) {
	line := how
	if out, err := s.Run(ctx, nil, "display-message", "-p", "#{pid}"); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(out)); err == nil {
			line += fmt.Sprintf("; server pid %d", pid)
			if cg := supervisor.ProcCgroup(pid); cg != "" {
				line += " cgroup " + cg
			}
		}
	}
	supervisor.RecordPlacement(s.Socket, line)
}

// up reports whether a server answers on the socket.
func (s *Server) up(ctx context.Context) bool {
	_, err := s.Run(ctx, nil, "list-sessions", "-F", "#{session_name}")
	return err == nil || !errors.Is(err, ErrNoServer)
}

// argv is the full tmux client argv for args against this server.
func (s *Server) argv(args ...string) []string {
	conf := s.Config
	if conf == "" {
		conf = "/dev/null"
	}
	return append([]string{s.Bin, "-u", "-S", s.Socket, "-f", conf}, args...)
}

// runArgv runs an arbitrary argv with the server environment and returns
// its stderr.
func (s *Server) runArgv(ctx context.Context, argv []string) (string, error) {
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = DefaultCommandTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = s.env()
	var stderr bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &stderr, n: 8 << 10}
	err := cmd.Run()
	return stderr.String(), err
}

// NewSession starts a detached session running spec.Argv. spec.Env and
// spec.Unset are applied by the pane launcher from a private file
// (paneCommand); no command line carries an environment value, and the
// command is never interpreted by a shell.
func (s *Server) NewSession(ctx context.Context, spec SessionSpec) error {
	if err := spec.validate(); err != nil {
		return err
	}
	if s.Config != "" {
		if err := s.EnsureServer(ctx); err != nil {
			return err
		}
	}
	args, envFile, err := s.newSessionArgs(spec)
	if err != nil {
		return err
	}
	if _, err := s.Run(ctx, nil, args...); err != nil {
		if envFile != "" {
			_ = os.Remove(envFile)
		}
		return err
	}
	// Belt and braces when the server predates our config.
	_, _ = s.Run(ctx, nil, "set-option", "-w", "-t", PaneTarget(spec.Name), "remain-on-exit", "on")
	return nil
}

// newSessionArgs returns the new-session client arguments for spec and the
// environment file the pane launcher will consume (if any).
func (s *Server) newSessionArgs(spec SessionSpec) (args []string, envFile string, err error) {
	w, h := spec.Width, spec.Height
	if w <= 0 {
		w = 200
	}
	if h <= 0 {
		h = 50
	}
	args = []string{"new-session", "-d", "-s", spec.Name, "-x", strconv.Itoa(w), "-y", strconv.Itoa(h)}
	if spec.Cwd != "" {
		args = append(args, "-c", spec.Cwd)
	}
	cmd, envFile, err := s.paneCommand(spec)
	if err != nil {
		return nil, "", err
	}
	return append(append(args, "--"), cmd...), envFile, nil
}

// respawnArgs returns the respawn-pane client arguments for spec.
func (s *Server) respawnArgs(spec SessionSpec) (args []string, envFile string, err error) {
	args = []string{"respawn-pane", "-k", "-t", PaneTarget(spec.Name)}
	if spec.Cwd != "" {
		args = append(args, "-c", spec.Cwd)
	}
	cmd, envFile, err := s.paneCommand(spec)
	if err != nil {
		return nil, "", err
	}
	return append(append(args, "--"), cmd...), envFile, nil
}

// RespawnPane restarts the (dead) pane of session name with argv. Like
// NewSession, spec.Env reaches only the new pane process.
func (s *Server) RespawnPane(ctx context.Context, name string, spec SessionSpec) error {
	spec.Name = name
	if err := spec.validate(); err != nil {
		return err
	}
	args, envFile, err := s.respawnArgs(spec)
	if err != nil {
		return err
	}
	if _, err := s.Run(ctx, nil, args...); err != nil {
		if envFile != "" {
			_ = os.Remove(envFile)
		}
		return err
	}
	return nil
}

// HasSession reports whether the exact session exists. A missing server is
// reported as (false, nil).
func (s *Server) HasSession(ctx context.Context, name string) (bool, error) {
	_, err := s.Run(ctx, nil, "has-session", "-t", SessionTarget(name))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, ErrNoServer) || errors.Is(err, ErrNoSession) {
		return false, nil
	}
	return false, err
}

// ListSessions returns session names (empty if no server).
func (s *Server) ListSessions(ctx context.Context) ([]string, error) {
	out, err := s.Run(ctx, nil, "list-sessions", "-F", "#{session_name}")
	if errors.Is(err, ErrNoServer) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if l != "" {
			names = append(names, l)
		}
	}
	return names, nil
}

// Client is an attached tmux client.
type Client struct {
	TTY      string
	PID      int
	ReadOnly bool
	Flags    string
}

// ListClients lists clients attached to the session. An attached writable
// client is Pyre's signal that a human holds the terminal (input lease).
func (s *Server) ListClients(ctx context.Context, name string) ([]Client, error) {
	out, err := s.Run(ctx, nil, "list-clients", "-t", SessionTarget(name), "-F",
		"#{client_tty}\t#{client_pid}\t#{client_readonly}\t#{client_flags}")
	if err != nil {
		return nil, err
	}
	return parseClients(out), nil
}

func parseClients(out string) []Client {
	var cs []Client
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		f := strings.Split(l, "\t")
		c := Client{TTY: f[0]}
		if len(f) > 1 {
			c.PID, _ = strconv.Atoi(f[1])
		}
		if len(f) > 2 {
			c.ReadOnly = f[2] == "1"
		}
		if len(f) > 3 {
			c.Flags = f[3]
			if strings.Contains(c.Flags, "read-only") {
				c.ReadOnly = true
			}
		}
		cs = append(cs, c)
	}
	return cs
}

// WritableClients counts attached clients that can type.
func WritableClients(cs []Client) int {
	n := 0
	for _, c := range cs {
		if !c.ReadOnly {
			n++
		}
	}
	return n
}

// CaptureOptions bounds capture-pane.
type CaptureOptions struct {
	// History is the number of scrollback lines above the visible screen to
	// include (0 = visible screen only).
	History int
	// Escapes keeps SGR escape sequences (-e).
	Escapes bool
	// Join joins wrapped lines (-J).
	Join bool
}

// CapturePane returns the pane contents (bounded to MaxCaptureBytes).
func (s *Server) CapturePane(ctx context.Context, name string, o CaptureOptions) (string, error) {
	args := []string{"capture-pane", "-p", "-t", PaneTarget(name)}
	if o.History > 0 {
		args = append(args, "-S", strconv.Itoa(-o.History))
	}
	if o.Escapes {
		args = append(args, "-e")
	}
	if o.Join {
		args = append(args, "-J")
	}
	out, err := s.Run(ctx, nil, args...)
	if err != nil {
		return "", err
	}
	if len(out) > MaxCaptureBytes {
		out = out[len(out)-MaxCaptureBytes:]
	}
	return out, nil
}

// Paste loads text into a named buffer and pastes it into the pane with
// bracketed paste (-p), deleting the buffer afterwards (-d). The text goes
// through stdin, never through argv or a shell.
func (s *Server) Paste(ctx context.Context, name, buffer string, text []byte) error {
	if !ValidName(buffer) {
		return fmt.Errorf("tmux: invalid buffer name %q", buffer)
	}
	if _, err := s.Run(ctx, text, "load-buffer", "-b", buffer, "-"); err != nil {
		return err
	}
	// Leave any copy-mode first (see SendKeys), so the paste is visible.
	_, err := s.Run(ctx, nil, "copy-mode", "-q", "-t", PaneTarget(name), ";", "paste-buffer", "-p", "-d", "-b", buffer, "-t", PaneTarget(name))
	return err
}

// Key is a named tmux key. Only the keys below may be sent; literal text
// always goes through Paste.
type Key string

const (
	KeyEnter     Key = "Enter"
	KeyEscape    Key = "Escape"
	KeyCtrlU     Key = "C-u"
	KeyCtrlC     Key = "C-c"
	KeyCtrlD     Key = "C-d"
	KeyBackspace Key = "BSpace"
	KeyUp        Key = "Up"
	KeyDown      Key = "Down"
	KeyTab       Key = "Tab"
	KeyEnd       Key = "End"
)

var allowedKeys = map[Key]bool{
	KeyEnter: true, KeyEscape: true, KeyCtrlU: true, KeyCtrlC: true, KeyCtrlD: true,
	KeyBackspace: true, KeyUp: true, KeyDown: true, KeyTab: true, KeyEnd: true,
}

// SendKeys sends named keys (never literal text) to the pane. A pane left
// in copy-mode (mouse on: a wheel over an application without mouse
// reporting enters it) would take the keys as copy-mode commands, so the
// same tmux invocation cancels any mode first (copy-mode -q).
func (s *Server) SendKeys(ctx context.Context, name string, keys ...Key) error {
	if len(keys) == 0 {
		return nil
	}
	for _, k := range keys {
		if !allowedKeys[k] {
			return fmt.Errorf("tmux: key %q is not an allowed named key", k)
		}
	}
	build := func(noClient bool) []string {
		args := []string{"copy-mode", "-q", "-t", PaneTarget(name), ";", "send-keys"}
		if noClient {
			args = append(args, "-c", "")
		}
		args = append(args, "-t", PaneTarget(name))
		for _, k := range keys {
			args = append(args, string(k))
		}
		return args
	}
	if _, old := noClientFlagUnsupported.Load(s.Bin); old {
		_, err := s.Run(ctx, nil, build(false)...)
		return err
	}
	_, err := s.Run(ctx, nil, build(true)...)
	if err != nil && IsUnknownClientFlag(err) {
		// The whole command list failed to parse, so nothing ran: retrying
		// cannot repeat a key.
		noClientFlagUnsupported.Store(s.Bin, true)
		_, err = s.Run(ctx, nil, build(false)...)
	}
	return err
}

// send-keys runs against a target client: the one named by -c or, without
// -c, the "best" client attached to the session. tmux 3.7 refuses
// send-keys (except -X) when that client is read-only, so a read-only
// viewer (tmux attach -r: Pyre's TUI, web and Android terminals) made every
// injected key fail with "client is read-only". Pyre's keys come from no
// client, so SendKeys names one that cannot match (-c ""): the target
// client is then NULL, which tmux allows. -c exists since tmux 3.4; older
// servers reject it at parse time and get the plain form (they have no
// read-only check).
var noClientFlagUnsupported sync.Map // tmux binary path -> true

// IsUnknownClientFlag reports whether err is tmux rejecting send-keys -c
// (tmux before 3.4).
func IsUnknownClientFlag(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "unknown flag -c") || strings.Contains(msg, "usage: send-keys")
}

// KillSession kills one session (no error if it does not exist).
func (s *Server) KillSession(ctx context.Context, name string) error {
	_, err := s.Run(ctx, nil, "kill-session", "-t", SessionTarget(name))
	if errors.Is(err, ErrNoServer) || errors.Is(err, ErrNoSession) {
		return nil
	}
	return err
}

// TerminateSession ends the session's pane process group (each pane leads
// its own group: tmux setsid()s it) with SIGTERM, waits up to grace for the
// whole group to exit, escalates to SIGKILL, then kills the session. Unlike
// kill-session alone (SIGHUP), it returns only once the processes are gone.
func (s *Server) TerminateSession(ctx context.Context, name string, grace time.Duration) error {
	if p, err := s.Pane(ctx, name); err == nil && p.PID > 0 && !p.Dead {
		pg := -p.PID
		_ = syscall.Kill(pg, syscall.SIGTERM)
		end := time.Now().Add(grace)
		for time.Now().Before(end) && groupAlive(p.PID) {
			time.Sleep(50 * time.Millisecond)
		}
		if groupAlive(p.PID) {
			_ = syscall.Kill(pg, syscall.SIGKILL)
			for i := 0; i < 40 && groupAlive(p.PID); i++ {
				time.Sleep(50 * time.Millisecond)
			}
		}
	}
	return s.KillSession(ctx, name)
}

// KillServer kills the whole isolated server (no error if not running).
func (s *Server) KillServer(ctx context.Context) error {
	_, err := s.Run(ctx, nil, "kill-server")
	if errors.Is(err, ErrNoServer) {
		return nil
	}
	return err
}

// PaneInfo describes a session's active pane.
type PaneInfo struct {
	PaneID         string
	PID            int
	Dead           bool
	ExitStatus     *int
	CurrentCommand string
	// Width and Height are the pane size in cells (0 if unknown). A pane
	// takes the size of the last active attached client (window-size
	// latest) and keeps it after that client detaches.
	Width, Height int
}

// Pane returns the active pane of the session's current window.
//
// tmux marks a pane dead when its pty closes but records the exit status
// only once it has reaped the process. Normally that is a moment later;
// tmux 3.4 on Ubuntu 24.04 (the GitHub runner, reproduced in a container)
// can leave the exited process a zombie with no status until some other
// child of the server exits, so a pane that just ended reads as dead with
// no exit status indefinitely. When Pane sees that (and no signal ended the
// process, which would never give a status), it runs `run-shell true`: the
// job's exit makes tmux reap every exited child, the pane's included. It
// then re-reads for up to 200 ms.
func (s *Server) Pane(ctx context.Context, name string) (PaneInfo, error) {
	p, signal, err := s.pane(ctx, name)
	for i := 0; i < 10 && err == nil && p.Dead && p.ExitStatus == nil && signal == ""; i++ {
		if i == 0 {
			_, _ = s.Run(ctx, nil, "run-shell", "true")
		} else {
			select {
			case <-ctx.Done():
				return p, nil
			case <-time.After(20 * time.Millisecond):
			}
		}
		p, signal, err = s.pane(ctx, name)
	}
	return p, err
}

// pane reads the active pane once; signal is #{pane_dead_signal}.
func (s *Server) pane(ctx context.Context, name string) (PaneInfo, string, error) {
	// display-message silently prints nothing for a missing target, so use
	// list-panes, which fails with "can't find session".
	out, err := s.Run(ctx, nil, "list-panes", "-t", PaneTarget(name), "-F",
		"#{pane_active}\t#{pane_id}\t#{pane_pid}\t#{pane_dead}\t#{pane_dead_status}\t#{pane_current_command}\t#{pane_width}\t#{pane_height}\t#{pane_dead_signal}")
	if err != nil {
		return PaneInfo{}, "", err
	}
	var first string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		active, rest, ok := strings.Cut(l, "\t")
		if !ok {
			continue
		}
		if first == "" {
			first = rest
		}
		if active == "1" {
			first = rest
			break
		}
	}
	if first == "" {
		return PaneInfo{}, "", fmt.Errorf("%w (no panes)", ErrNoSession)
	}
	p, err := parsePane(first)
	signal := ""
	if f := strings.Split(strings.TrimRight(first, "\n"), "\t"); len(f) > 7 {
		signal = f[7]
	}
	return p, signal, err
}

func parsePane(out string) (PaneInfo, error) {
	f := strings.Split(strings.TrimRight(out, "\n"), "\t")
	if len(f) < 3 {
		return PaneInfo{}, fmt.Errorf("tmux: unexpected pane info %q", out)
	}
	p := PaneInfo{PaneID: f[0]}
	p.PID, _ = strconv.Atoi(f[1])
	p.Dead = f[2] == "1"
	if len(f) > 3 && p.Dead && f[3] != "" {
		if n, err := strconv.Atoi(f[3]); err == nil {
			p.ExitStatus = &n
		}
	}
	if len(f) > 4 {
		p.CurrentCommand = f[4]
	}
	if len(f) > 6 {
		p.Width, _ = strconv.Atoi(f[5])
		p.Height, _ = strconv.Atoi(f[6])
	}
	return p, nil
}

// ResizeWindow sets the session's window to width x height, then hands the
// size back to tmux's window-size policy (latest): resize-window switches
// the window to window-size manual, and unsetting the window option
// restores the server default. With no client attached tmux keeps the new
// size; the next client to attach resizes the window to its own terminal
// as usual. Callers use it only when no client is attached.
func (s *Server) ResizeWindow(ctx context.Context, name string, width, height int) error {
	if width <= 0 || height <= 0 {
		return fmt.Errorf("tmux: invalid window size %dx%d", width, height)
	}
	if _, err := s.Run(ctx, nil, "resize-window", "-t", PaneTarget(name), "-x", strconv.Itoa(width), "-y", strconv.Itoa(height)); err != nil {
		return err
	}
	_, err := s.Run(ctx, nil, "set-option", "-w", "-u", "-t", PaneTarget(name), "window-size")
	return err
}

// AttachArgv builds a local attach command for the exact session. It sets
// mouse on first (one tmux invocation, commands separated by ";"), so a
// server started by an older Pyre gets it too.
func AttachArgv(bin, socket, name string, readOnly bool) []string {
	argv := []string{bin, "-u", "-S", socket, "set-option", "-g", "mouse", "on", ";", "attach-session", "-t", SessionTarget(name)}
	if readOnly {
		argv = append(argv, "-r")
	}
	return argv
}

var sshAliasRe = regexp.MustCompile(`^[A-Za-z0-9_.@-]+$`)

// RemoteAttachArgv builds `ssh -t <alias> "bash -lc '<tmux attach>'"`. The
// remote command is assembled from ShellQuote'd parts only. remoteBin is the
// tmux path on the remote node.
func RemoteAttachArgv(sshAlias, remoteBin, socket, name string, readOnly bool) ([]string, error) {
	if !sshAliasRe.MatchString(sshAlias) || strings.HasPrefix(sshAlias, "-") {
		return nil, fmt.Errorf("tmux: invalid ssh alias %q", sshAlias)
	}
	parts := AttachArgv(remoteBin, socket, name, readOnly)
	quoted := make([]string, len(parts))
	for i, p := range parts {
		quoted[i] = ShellQuote(p)
	}
	inner := strings.Join(quoted, " ")
	return []string{"ssh", "-t", sshAlias, "bash -lc " + ShellQuote(inner)}, nil
}

var safeShellRe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// ShellQuote quotes s for POSIX sh using single quotes.
func ShellQuote(s string) string {
	if s != "" && safeShellRe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// ShellJoin quotes and joins argv for a POSIX shell command line.
func ShellJoin(argv []string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = ShellQuote(a)
	}
	return strings.Join(q, " ")
}
