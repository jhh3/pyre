// Package termhost is the client of Pyre's own terminal-session host: the
// Windows replacement for the isolated tmux server (and an opt-in
// alternative on macOS/Linux, PYRE_SESSION_HOST=termhost, in binaries
// built with the pyre_termhost tag).
//
// One host process per socket (`<pyred> __pyre_termhost serve -S sock`)
// owns that socket's sessions. Each session is a program on a pseudo-
// terminal (ConPTY on Windows, a Unix PTY elsewhere) whose output feeds a
// VT emulator for screen captures and for repainting newly attached
// clients. Clients attach over the same private AF_UNIX socket: any number
// of read-only viewers plus writers; the newest writer sizes the session
// (tmux window-size latest), viewers never do. The host runs detached from
// pyred (own session/process group; on Windows DETACHED_PROCESS and out of
// pyred's job), so restarting pyred leaves every run running; pyred
// re-finds sessions by name.
//
// This package is the light side pyred links on every hook invocation:
// Host (sessionhost.Host over the socket), the attach stream (DialAttach),
// and attach descriptors. The host process itself, with the emulator, is
// package termhost/hostproc; a binary serves `__pyre_termhost` only when it
// imports hostproc (pyred on Windows, or with -tags pyre_termhost).
//
// Run environments (session tokens, API keys) reach the program only
// through the private socket and process creation, never a command line.
// Nothing here logs terminal bytes, input, or environment values.
package termhost

import (
	"errors"
	"sync/atomic"

	"github.com/jhh3/pyre/internal/runtime/termhost/internal/wire"
)

// ServeArg is argv[1] of the host and attach-client commands, served by
// binaries that import termhost/hostproc.
const ServeArg = "__pyre_termhost"

// ProtocolVersion is the wire protocol version this client speaks.
const ProtocolVersion = wire.ProtocolVersion

// ErrNotBuilt means this executable does not serve the host process.
var ErrNotBuilt = errors.New("termhost is built only on Windows; rebuild with -tags pyre_termhost")

var hostEntry atomic.Bool

// RegisterHostEntry records that this executable serves `__pyre_termhost`
// (called by termhost/hostproc's init).
func RegisterHostEntry() { hostEntry.Store(true) }

// HostEntryBuilt reports whether this executable serves `__pyre_termhost`.
func HostEntryBuilt() bool { return hostEntry.Load() }
