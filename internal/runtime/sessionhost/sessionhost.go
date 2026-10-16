// Package sessionhost is the terminal-session host abstraction the runtime
// adapters use: a long-lived process that owns detached sessions (one
// program on a terminal each), outlives pyred restarts, and lets Pyre
// start, inspect, capture, type into, attach to and stop them.
//
// Two implementations exist: the isolated tmux server (package tmux; the
// default on macOS and Linux) and the Pyre-owned ConPTY/PTY session host
// (package termhost; the default on Windows, opt-in elsewhere with
// PYRE_SESSION_HOST=termhost). Package hostsel picks one.
//
// The shared types here were the tmux package's own; tmux re-exports them
// as aliases, so existing callers keep compiling unchanged.
package sessionhost

import (
	"context"
	"errors"
	"fmt"
	"github.com/jhh3/pyre/internal/fileperm"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Host is one session host (a tmux server, or a Pyre termhost) addressed by
// its socket. Sessions are named (ValidName); every session has exactly one
// pane. Methods report a host that is not running as ErrNoServer and a
// missing session as ErrNoSession (errors.Is).
type Host interface {
	// Kind is "tmux" or "termhost".
	Kind() string
	// SocketPath is the host's control socket (recorded in run handles).
	SocketPath() string
	// Check reports whether the host can run sessions on this machine and
	// describes it ("tmux 3.4", "pyre termhost/1 (conpty)").
	Check(ctx context.Context) (string, error)

	NewSession(ctx context.Context, spec SessionSpec) error
	// RespawnPane restarts the session's program (killing a live one).
	RespawnPane(ctx context.Context, name string, spec SessionSpec) error
	HasSession(ctx context.Context, name string) (bool, error)
	ListSessions(ctx context.Context) ([]string, error)
	// ListClients lists attached terminal clients (a writable one is the
	// implicit raw-input lease).
	ListClients(ctx context.Context, name string) ([]Client, error)
	CapturePane(ctx context.Context, name string, o CaptureOptions) (string, error)
	// Paste types text with bracketed paste when the program asked for it.
	Paste(ctx context.Context, name, buffer string, text []byte) error
	// SendKeys sends named keys (never literal text).
	SendKeys(ctx context.Context, name string, keys ...Key) error
	KillSession(ctx context.Context, name string) error
	// TerminateSession ends the session's whole process tree gracefully,
	// forcefully after grace, then removes the session.
	TerminateSession(ctx context.Context, name string, grace time.Duration) error
	KillServer(ctx context.Context) error
	Pane(ctx context.Context, name string) (PaneInfo, error)
	// ResizeWindow sets the session size (used only with no client
	// attached).
	ResizeWindow(ctx context.Context, name string, width, height int) error
	// AttachDescriptor returns how a client reaches session name
	// (local command, or ssh to this node).
	AttachDescriptor(cfg AttachConfig, runID, name string, req contracts.AttachCommand, now time.Time) (contracts.AttachDescriptor, error)
}

// AttachConfig is the node-level part of attach descriptors.
type AttachConfig struct {
	NodeID   string
	SSHAlias string        // alias other machines use to ssh to this node ("" = no remote attach)
	TTL      time.Duration // descriptor lifetime (default 5m)
}

var (
	// ErrNoServer means no host is listening on the socket.
	ErrNoServer = errors.New("tmux: no server running on socket")
	// ErrNoSession means the target session/pane does not exist.
	ErrNoSession = errors.New("tmux: session not found")
	// ErrNUL is an environment value containing a NUL byte.
	ErrNUL = errors.New("tmux: environment value contains a NUL byte")
)

// SessionSpec describes a new detached session running argv.
type SessionSpec struct {
	Name string
	Cwd  string
	// Env is added to the program's environment. Values may be secrets:
	// they never travel on a command line (tmux: a private 0600 file read
	// by the pane launcher; termhost: the private control socket).
	Env map[string]string
	// Unset lists inherited variables removed from the program's
	// environment (ResolvedLaunch.Unset). A variable also in Env is kept.
	Unset  []string
	Argv   []string
	Width  int // default 200
	Height int // default 50
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// ValidName reports whether name is a safe session/buffer name.
func ValidName(name string) bool { return nameRe.MatchString(name) }

var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidEnvKey reports whether k is an acceptable environment variable name.
func ValidEnvKey(k string) bool { return envKeyRe.MatchString(k) }

// Validate checks the name, argv and environment of spec.
func (spec SessionSpec) Validate() error {
	if !ValidName(spec.Name) {
		return fmt.Errorf("tmux: invalid session name %q", spec.Name)
	}
	if len(spec.Argv) == 0 || spec.Argv[0] == "" {
		return errors.New("tmux: empty argv")
	}
	for k, v := range spec.Env {
		if !ValidEnvKey(k) {
			return fmt.Errorf("tmux: invalid environment variable name %q", k)
		}
		if strings.IndexByte(v, 0) >= 0 {
			return fmt.Errorf("%w: %s", ErrNUL, k)
		}
	}
	for _, k := range spec.Unset {
		if !ValidEnvKey(k) {
			return fmt.Errorf("tmux: invalid environment variable name %q", k)
		}
	}
	return nil
}

// CaptureOptions bounds a screen capture.
type CaptureOptions struct {
	// History is the number of scrollback lines above the visible screen to
	// include (0 = visible screen only).
	History int
	// Escapes keeps SGR escape sequences.
	Escapes bool
	// Join joins wrapped lines.
	Join bool
}

// MaxCaptureBytes bounds capture output returned to callers.
const MaxCaptureBytes = 256 << 10

// Key is a named key. Only the keys below may be sent; literal text always
// goes through Paste.
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

// AllowedKey reports whether k may be sent with SendKeys.
func AllowedKey(k Key) bool { return allowedKeys[k] }

// Client is an attached terminal client.
type Client struct {
	TTY      string
	PID      int
	ReadOnly bool
	Flags    string
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

// PaneInfo describes a session's pane.
type PaneInfo struct {
	PaneID         string
	PID            int
	Dead           bool
	ExitStatus     *int
	CurrentCommand string
	// Width and Height are the pane size in cells (0 if unknown).
	Width, Height int
}

// PrepareDir creates dir with mode 0700 (and tightens an existing dir).
// On Windows the mode bits are advisory; the directory's ACL (inherited
// from the per-user state root) is what keeps it private.
func PrepareDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// fileperm also sets an owner-only ACL on Windows, where chmod does not
	// restrict access to the session host's socket directory.
	return fileperm.Restrict(dir, 0o700)
}
