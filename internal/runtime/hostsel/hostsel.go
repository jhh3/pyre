// Package hostsel picks the terminal-session host the runtime adapters
// use: the isolated tmux server on macOS and Linux (unchanged default),
// the Pyre termhost (ConPTY) on Windows. PYRE_SESSION_HOST=tmux|termhost
// overrides the default (termhost works on macOS/Linux too in binaries
// built with -tags pyre_termhost; tmux does not exist on Windows).
//
// Switching kinds while runs are live strands them: a run is re-found only
// through the host kind that started it.
package hostsel

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/jhh3/pyre/internal/runtime/sessionhost"
	"github.com/jhh3/pyre/internal/runtime/termhost"
	"github.com/jhh3/pyre/internal/runtime/tmux"
)

// Env names the override variable.
const Env = "PYRE_SESSION_HOST"

// Kinds.
const (
	KindTmux     = "tmux"
	KindTermhost = "termhost"
)

// Kind is the host kind for this process: $PYRE_SESSION_HOST, else
// termhost on Windows and tmux elsewhere.
func Kind() string {
	switch os.Getenv(Env) {
	case KindTmux:
		return KindTmux
	case KindTermhost:
		return KindTermhost
	}
	if runtime.GOOS == "windows" {
		return KindTermhost
	}
	return KindTmux
}

// TermhostSocket is the termhost socket next to a tmux socket path (the
// adapters are configured with <state>/tmux/pyre.sock; the termhost uses
// <state>/tmux/termhost.sock, so the two never share a socket).
func TermhostSocket(tmuxSocket string) string {
	return filepath.Join(filepath.Dir(tmuxSocket), "termhost.sock")
}

// New returns the session host for socket (a tmux socket path; see
// TermhostSocket). For tmux it also writes the server configuration
// (Server.WriteConfig), as the adapters always did.
func New(tmuxBin, socket string) (sessionhost.Host, error) {
	return NewKind(Kind(), tmuxBin, socket)
}

// NewKind is New for an explicit kind.
func NewKind(kind, tmuxBin, socket string) (sessionhost.Host, error) {
	switch kind {
	case KindTermhost:
		// The host process (emulator and all) is linked only on Windows or
		// with -tags pyre_termhost; fail clearly rather than at first spawn.
		if !termhost.HostEntryBuilt() {
			return nil, fmt.Errorf("%s=%s: %w", Env, KindTermhost, termhost.ErrNotBuilt)
		}
		return termhost.New(TermhostSocket(socket))
	case KindTmux:
		srv, err := tmux.NewServer(tmuxBin, socket)
		if err != nil {
			return nil, err
		}
		if err := srv.WriteConfig(); err != nil {
			return nil, err
		}
		return srv, nil
	}
	return nil, fmt.Errorf("unknown session host kind %q (want %s or %s)", kind, KindTmux, KindTermhost)
}
