// Package outfit adapts the user's existing Outfit CLI (github.com/jhh3/outfit)
// for Pyre: detection, a metadata-only connection list, availability checks,
// read invocations executed by Outfit on the node that holds the credential,
// managed tool/skill inventory, and session connection planning.
//
// The adapter never asks Outfit for a secret (Outfit has no command that
// reveals one, by design) and never runs Outfit commands that mutate user
// state on its own initiative: it calls only `version`, `inventory --json`
// (proposed upstream; see README), `ls --json`, `tool status`, `skill list`,
// `connector test NAME`, and `run NAME OP --input - --json`. Every call is
// bounded by a timeout and an output limit, runs with a minimal environment,
// and has its diagnostics redacted before they reach an error.
package outfit

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"sync"
	"time"

	"github.com/jhh3/pyre/integrations/bundled"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/credentials"
	"github.com/jhh3/pyre/internal/winenv"
)

// Options configure an Adapter. Zero values are production defaults.
type Options struct {
	// Binary is the outfit executable; empty = the bundled Outfit of this
	// Pyre install (install.sh --with-tools), else "outfit" on PATH.
	Binary string
	// Home sets OUTFIT_HOME for child processes (tests/isolated profiles).
	// Empty inherits the caller's OUTFIT_HOME, if any, else Outfit's default.
	Home string
	// Dir is the working directory for Outfit calls. Outfit applies the
	// nearest .outfit/workspace.yaml policy; the default (os.TempDir) avoids
	// picking up an unrelated repository's workspace.
	Dir string
	// ExtraEnv entries (KEY=VALUE) are appended to the minimal environment.
	ExtraEnv []string
	// Timeout bounds metadata calls (default 20s); CheckTimeout bounds
	// `connector test` (default 45s); InvokeTimeout bounds operations
	// (default 60s).
	Timeout, CheckTimeout, InvokeTimeout time.Duration
	// MaxOutput bounds captured stdout per call (default 256 KiB). Operation
	// results larger than this are spilled by Outfit and reported truncated.
	MaxOutput int
	LookPath  func(string) (string, error)
}

// Adapter calls the Outfit CLI.
type Adapter struct {
	opt Options

	mu           sync.Mutex
	noInventory  bool // learned: this Outfit lacks `inventory`
	resolvedPath string
}

// New returns an adapter.
func New(opt Options) *Adapter {
	if opt.Timeout <= 0 {
		opt.Timeout = 20 * time.Second
	}
	if opt.CheckTimeout <= 0 {
		opt.CheckTimeout = 45 * time.Second
	}
	if opt.InvokeTimeout <= 0 {
		opt.InvokeTimeout = 60 * time.Second
	}
	if opt.MaxOutput <= 0 {
		opt.MaxOutput = 256 << 10
	}
	if opt.Dir == "" {
		opt.Dir = os.TempDir()
	}
	if opt.LookPath == nil {
		opt.LookPath = exec.LookPath
	}
	return &Adapter{opt: opt}
}

const reasonNotInstalled = "not_installed"

// IsNotInstalled reports the error returned when no outfit executable is
// found on this node (as opposed to an installed Outfit that failed).
func IsNotInstalled(err error) bool {
	var ce *contracts.Error
	if !errors.As(err, &ce) || ce == nil || ce.Code != contracts.ErrUnavailable {
		return false
	}
	return ce.Details["reason"] == reasonNotInstalled
}

func (a *Adapter) binary() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.resolvedPath != "" {
		return a.resolvedPath, nil
	}
	name := a.opt.Binary
	if name == "" {
		if p := bundled.Path("outfit"); p != "" {
			a.resolvedPath = p
			return p, nil
		}
		name = "outfit"
	}
	path, err := a.opt.LookPath(name)
	if err != nil {
		return "", contracts.Errorf(contracts.ErrUnavailable, "outfit is not installed on this node (looked for %q on PATH)", name).WithDetail("integration", "outfit").WithDetail("reason", reasonNotInstalled)
	}
	a.resolvedPath = path
	return path, nil
}

// systemEnv returns the Windows system variables (APPDATA, LOCALAPPDATA,
// USERPROFILE, SystemRoot, TEMP, PATHEXT, ...); nil elsewhere. A variable so
// tests can exercise the Windows set on any platform.
var systemEnv = winenv.SystemEnv

// childEnv is the minimal environment for Outfit. Outfit needs HOME/config
// dirs (on Windows %AppData% and the profile variables), PATH for
// executor-custody CLIs (gh, gws), the session bus for the Linux keyring, and
// its own OUTFIT_* settings (vault backend, home); it must not inherit Pyre
// session tokens or unrelated secrets.
func (a *Adapter) childEnv(extra ...string) []string {
	keep := []string{"HOME", "USER", "LOGNAME", "PATH", "TMPDIR", "LANG", "LC_ALL", "TZ", "SHELL",
		"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS",
		"DISPLAY", "WAYLAND_DISPLAY", "SSH_AUTH_SOCK",
		// Outfit resolves harness skill roots from these, falling back to HOME.
		"CODEX_HOME", "CLAUDE_CONFIG_DIR"}
	var env []string
	for _, k := range keep {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	env = append(env, systemEnv()...)
	for _, k := range outfitEnvForwarded {
		if v, ok := os.LookupEnv(k); ok && !(k == "OUTFIT_HOME" && a.opt.Home != "") {
			env = append(env, k+"="+v)
		}
	}
	if a.opt.Home != "" {
		env = append(env, "OUTFIT_HOME="+a.opt.Home)
	}
	env = append(env, a.opt.ExtraEnv...)
	return append(env, extra...)
}

// outfitEnvForwarded are the OUTFIT_* settings pyred passes on from its own
// environment (an allowlist: a future secret-bearing OUTFIT_* variable is
// never forwarded by accident).
var outfitEnvForwarded = []string{"OUTFIT_HOME", "OUTFIT_VAULT_BACKEND"}

type runResult struct {
	stdout, stderr []byte
	exit           int
	truncated      bool
	timedOut       bool
}

// run executes outfit with bounded time and output.
func (a *Adapter) run(ctx context.Context, timeout time.Duration, stdin []byte, extraEnv []string, args ...string) (runResult, error) {
	bin, err := a.binary()
	if err != nil {
		return runResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = a.childEnv(extraEnv...)
	cmd.Dir = a.opt.Dir
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	out := &capBuffer{max: a.opt.MaxOutput}
	errb := &capBuffer{max: 16 << 10}
	cmd.Stdout, cmd.Stderr = out, errb
	configureProcessGroup(cmd)
	cmd.WaitDelay = 2 * time.Second
	runErr := cmd.Run()
	res := runResult{stdout: out.Bytes(), stderr: errb.Bytes(), truncated: out.over}
	if ctx.Err() == context.DeadlineExceeded {
		res.timedOut = true
		return res, contracts.Errorf(contracts.ErrUnavailable, "outfit %s timed out after %s", firstArg(args), timeout).WithDetail("integration", "outfit")
	}
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	var ee *exec.ExitError
	if errors.As(runErr, &ee) {
		res.exit = ee.ExitCode()
		return res, nil
	}
	if runErr != nil {
		return res, contracts.Errorf(contracts.ErrUnavailable, "could not run outfit: %s", credentials.Diagnostic(nil, []byte(runErr.Error()))).WithDetail("integration", "outfit")
	}
	return res, nil
}

func firstArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

type capBuffer struct {
	bytes.Buffer
	max  int
	over bool
}

func (b *capBuffer) Write(p []byte) (int, error) {
	room := b.max - b.Len()
	if room <= 0 {
		b.over = b.over || len(p) > 0
		return len(p), nil
	}
	if len(p) > room {
		b.Buffer.Write(p[:room])
		b.over = true
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

var (
	namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	opPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
	argKey      = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,63}$`)
)

// ValidName reports whether name is a safe Outfit connection name (it is
// passed as an argv element, so it must not look like a flag).
func ValidName(name string) bool { return namePattern.MatchString(name) }

func diag(raw ...[]byte) string {
	return credentials.Diagnostic(nil, bytes.Join(raw, []byte(" ")))
}

func unavailable(format string, args ...any) *contracts.Error {
	return contracts.Errorf(contracts.ErrUnavailable, format, args...).WithDetail("integration", "outfit")
}
