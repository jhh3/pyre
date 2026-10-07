package credentials

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Store keeps secret values in a node-local custody backend, addressed by Ref.
// Implementations never log values and return *StoreError for every failure.
type Store interface {
	Backend() string
	Get(ctx context.Context, ref Ref) (Secret, error)
	Put(ctx context.Context, ref Ref, value Secret) error
	Delete(ctx context.Context, ref Ref) error
}

// Config selects a store. There is no implicit fallback between backends.
type Config struct {
	// Backend is "auto" (default), "keychain", "secret-service", or "file".
	Backend string
	// Keychain optionally targets a specific keychain file (tests use a
	// throwaway keychain; production uses the default search list).
	Keychain string
	// FileDir and AllowFileStore must both be set to use the plaintext 0600
	// file store; it is never selected automatically.
	FileDir        string
	AllowFileStore bool
	// Timeout bounds each store CLI call (default 10s).
	Timeout time.Duration
	// GOOS and LookPath are injectable for tests.
	GOOS     string
	LookPath func(string) (string, error)
}

// Open returns the configured store or an actionable *StoreError.
func Open(cfg Config) (Store, error) {
	goos := cfg.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	look := cfg.LookPath
	if look == nil {
		look = exec.LookPath
	}
	backend := cfg.Backend
	if backend == "" || backend == "auto" {
		switch goos {
		case "darwin":
			backend = BackendKeychain
		case "linux":
			backend = BackendSecretService
		default:
			return nil, storeErr(KindUnavailable, "auto", "open", Ref{}, "no supported OS credential store on "+goos+"; explicitly opt in to the 0600 file store", "")
		}
	}
	switch backend {
	case BackendKeychain:
		path, err := look("security")
		if err != nil {
			return nil, storeErr(KindUnavailable, BackendKeychain, "open", Ref{}, "the macOS `security` tool was not found on PATH (expected /usr/bin/security)", "")
		}
		return &KeychainStore{Security: path, Keychain: cfg.Keychain, Timeout: cfg.Timeout}, nil
	case BackendSecretService:
		path, err := look("secret-tool")
		if err != nil {
			return nil, storeErr(KindUnavailable, BackendSecretService, "open", Ref{}, secretToolHint, "secret-tool not found on PATH")
		}
		return &SecretServiceStore{Tool: path, Timeout: cfg.Timeout}, nil
	case BackendFile:
		return NewFileStore(cfg.FileDir, cfg.AllowFileStore)
	}
	return nil, storeErr(KindInvalid, backend, "open", Ref{}, "use auto, keychain, secret-service, or file", "unknown backend")
}

const secretToolHint = "install libsecret's secret-tool (Debian/Ubuntu: apt install libsecret-tools; Fedora: dnf install libsecret) and run an unlocked Secret Service provider (gnome-keyring or KeePassXC), or explicitly opt in to the 0600 file store; Pyre never falls back to plaintext on its own"

// Stores routes refs to stores by backend.
type Stores map[string]Store

// For returns the store for ref's backend.
func (s Stores) For(ref Ref) (Store, error) {
	if err := ref.Validate(); err != nil {
		return nil, storeErr(KindInvalid, ref.Backend, "route", ref, "", err.Error())
	}
	st, ok := s[ref.Backend]
	if !ok || st == nil {
		return nil, storeErr(KindUnavailable, ref.Backend, "route", ref, "this node has no "+ref.Backend+" store configured", "")
	}
	return st, nil
}

// Add registers st under its backend name.
func (s Stores) Add(st Store) Stores { s[st.Backend()] = st; return s }

// ---- bounded CLI execution shared by OS-store backends ----

type cliResult struct {
	stdout, stderr []byte
	exit           int
	timedOut       bool
}

type runFunc func(ctx context.Context, path string, args []string, stdin []byte) (cliResult, error)

const maxCLIOutput = 64 << 10

// storeEnv is the minimal environment passed to store CLIs: enough to find the
// user's keychain / session bus and unlock prompter, nothing else.
func storeEnv() []string {
	keep := []string{"HOME", "USER", "LOGNAME", "PATH", "TMPDIR", "LANG", "DBUS_SESSION_BUS_ADDRESS", "XDG_RUNTIME_DIR", "DISPLAY", "WAYLAND_DISPLAY"}
	var env []string
	for _, k := range keep {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

func runCLI(ctx context.Context, path string, args []string, stdin []byte) (cliResult, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = storeEnv()
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb limitedBuffer
	out.max, errb.max = maxCLIOutput, 8<<10
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	res := cliResult{stdout: out.Bytes(), stderr: errb.Bytes()}
	if ctx.Err() != nil {
		res.timedOut = true
		return res, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		res.exit = ee.ExitCode()
		return res, nil
	}
	if err != nil {
		return res, err
	}
	return res, nil
}

type limitedBuffer struct {
	bytes.Buffer
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.Len(); room > 0 {
		if len(p) > room {
			b.Buffer.Write(p[:room])
		} else {
			b.Buffer.Write(p)
		}
	}
	return len(p), nil
}

var _ io.Writer = (*limitedBuffer)(nil)

func withTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		d = 10 * time.Second
	}
	return context.WithTimeout(ctx, d)
}

func containsAny(s string, subs ...string) bool {
	ls := strings.ToLower(s)
	for _, sub := range subs {
		if strings.Contains(ls, strings.ToLower(sub)) {
			return true
		}
	}
	return false
}
