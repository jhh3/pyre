package termhost

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/sessionhost"
	"github.com/jhh3/pyre/internal/runtime/termhost/internal/wire"
)

// MaxSocketPath is the longest socket path accepted (sockaddr_un.sun_path
// is 104 bytes on macOS and 108 on Linux and Windows, NUL included).
const MaxSocketPath = 100

// DefaultCommandTimeout bounds one control request.
const DefaultCommandTimeout = 10 * time.Second

// ValidateSocketPath checks the socket path is absolute and short enough.
func ValidateSocketPath(socket string) error {
	if !filepath.IsAbs(socket) {
		return fmt.Errorf("termhost: socket path must be absolute: %q", socket)
	}
	if len(socket) > MaxSocketPath {
		return fmt.Errorf("termhost: socket path too long (%d > %d bytes): %q", len(socket), MaxSocketPath, socket)
	}
	return nil
}

// Host is pyred's handle on one termhost (sessionhost.Host). The host
// process is started on the first NewSession and then outlives pyred.
type Host struct {
	// Socket is the host's control socket (absolute, in a private dir).
	Socket string
	// Exe serves `__pyre_termhost` (default: this executable, when it
	// imports termhost/hostproc; otherwise ErrNotBuilt).
	Exe string
	// Env is the environment the host process starts with, and so the
	// base environment of its sessions (default os.Environ()).
	Env []string
	// Timeout bounds each control request (default DefaultCommandTimeout).
	Timeout time.Duration
}

var _ sessionhost.Host = (*Host)(nil)

// New validates socket and returns a Host for it.
func New(socket string) (*Host, error) {
	if err := ValidateSocketPath(socket); err != nil {
		return nil, err
	}
	return &Host{Socket: socket}, nil
}

func (h *Host) Kind() string       { return "termhost" }
func (h *Host) SocketPath() string { return h.Socket }

func (h *Host) exe() (string, error) {
	if h.Exe != "" {
		return h.Exe, nil
	}
	if !HostEntryBuilt() {
		return "", ErrNotBuilt
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("termhost: locate the host executable: %w", err)
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	return exe, nil
}

// Check reports whether this machine can host sessions.
func (h *Host) Check(ctx context.Context) (string, error) {
	backend, err := wire.BackendCheck()
	desc := fmt.Sprintf("pyre termhost/%d (%s)", wire.ProtocolVersion, backend)
	if err != nil {
		return desc, err
	}
	if _, err := h.exe(); err != nil {
		return desc, err
	}
	return desc, nil
}

func (h *Host) timeout() time.Duration {
	if h.Timeout > 0 {
		return h.Timeout
	}
	return DefaultCommandTimeout
}

// call sends one control request and reads its response.
func (h *Host) call(ctx context.Context, req wire.Request, timeout time.Duration) (wire.Response, error) {
	req.V = wire.ProtocolVersion
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var d net.Dialer
	c, err := d.DialContext(dctx, "unix", h.Socket)
	if err != nil {
		return wire.Response{}, fmt.Errorf("%w (%s: %v)", sessionhost.ErrNoServer, h.Socket, err)
	}
	defer c.Close()
	if dl, ok := dctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	if err := wire.WriteLine(c, req); err != nil {
		return wire.Response{}, fmt.Errorf("termhost %s: %w", req.Op, err)
	}
	var resp wire.Response
	if err := wire.ReadLine(bufio.NewReaderSize(c, 64<<10), 2*sessionhost.MaxCaptureBytes+(1<<20), &resp); err != nil {
		return wire.Response{}, fmt.Errorf("termhost %s: %w", req.Op, err)
	}
	if !resp.OK {
		return resp, respErr(req.Op, resp)
	}
	return resp, nil
}

func respErr(op string, resp wire.Response) error {
	switch resp.Code {
	case wire.CodeNoSession:
		return fmt.Errorf("%w (%s)", sessionhost.ErrNoSession, resp.Err)
	case "":
		return fmt.Errorf("termhost %s: failed", op)
	}
	return fmt.Errorf("termhost %s: %s", op, resp.Err)
}

func (h *Host) ping(ctx context.Context) (wire.Response, error) {
	return h.call(ctx, wire.Request{Op: wire.OpPing}, 2*time.Second)
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

// EnsureServer starts the host process if none answers on the socket. The
// host runs detached (its own session / process group, out of pyred's job
// or cgroup — on Windows through its own Task Scheduler task when pyred's
// job forbids breakaway), so restarting pyred leaves it and its sessions
// running.
func (h *Host) EnsureServer(ctx context.Context) error {
	if _, err := h.ping(ctx); err == nil || !errors.Is(err, sessionhost.ErrNoServer) {
		return err
	}
	mu := startLock(h.Socket)
	mu.Lock()
	defer mu.Unlock()
	if _, err := h.ping(ctx); err == nil || !errors.Is(err, sessionhost.ErrNoServer) {
		return err
	}
	exe, err := h.exe()
	if err != nil {
		return err
	}
	dir := filepath.Dir(h.Socket)
	if err := sessionhost.PrepareDir(dir); err != nil {
		return err
	}
	env := h.Env
	if env == nil {
		env = os.Environ()
	}
	if err := spawnHost(ctx, exe, []string{ServeArg, "serve", "-S", h.Socket}, wire.HostBaseEnv(env), filepath.Join(dir, "termhost.log"), h.Socket); err != nil {
		return fmt.Errorf("termhost: start host: %w", err)
	}
	// A start through Task Scheduler (Windows) takes longer than a spawn.
	deadline := time.Now().Add(15 * time.Second)
	for {
		_, err := h.ping(ctx)
		if err == nil {
			return nil
		}
		if !errors.Is(err, sessionhost.ErrNoServer) {
			return err // e.g. ConPTY unavailable
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return fmt.Errorf("termhost: host did not come up on %s (see %s): %w", h.Socket, filepath.Join(dir, "termhost.log"), err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func toWire(spec sessionhost.SessionSpec) *wire.Spec {
	return &wire.Spec{Argv: spec.Argv, Cwd: spec.Cwd, Env: spec.Env, Unset: spec.Unset, Width: spec.Width, Height: spec.Height}
}

// NewSession starts a detached session. spec.Env travels only over the
// private socket to the host, which hands it to the program's process
// creation; it is never on a command line.
func (h *Host) NewSession(ctx context.Context, spec sessionhost.SessionSpec) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	if err := h.EnsureServer(ctx); err != nil {
		return err
	}
	_, err := h.call(ctx, wire.Request{Op: wire.OpNew, Name: spec.Name, Spec: toWire(spec)}, h.timeout())
	return err
}

// RespawnPane restarts session name's program (killing a live one).
func (h *Host) RespawnPane(ctx context.Context, name string, spec sessionhost.SessionSpec) error {
	spec.Name = name
	if err := spec.Validate(); err != nil {
		return err
	}
	_, err := h.call(ctx, wire.Request{Op: wire.OpRespawn, Name: name, Spec: toWire(spec)}, h.timeout())
	return err
}

func (h *Host) HasSession(ctx context.Context, name string) (bool, error) {
	resp, err := h.call(ctx, wire.Request{Op: wire.OpHas, Name: name}, h.timeout())
	if errors.Is(err, sessionhost.ErrNoServer) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return resp.Exists, nil
}

func (h *Host) ListSessions(ctx context.Context) ([]string, error) {
	resp, err := h.call(ctx, wire.Request{Op: wire.OpList}, h.timeout())
	if errors.Is(err, sessionhost.ErrNoServer) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return resp.Names, nil
}

func (h *Host) ListClients(ctx context.Context, name string) ([]sessionhost.Client, error) {
	resp, err := h.call(ctx, wire.Request{Op: wire.OpClients, Name: name}, h.timeout())
	if err != nil {
		return nil, err
	}
	out := make([]sessionhost.Client, 0, len(resp.Clients))
	for _, c := range resp.Clients {
		out = append(out, sessionhost.Client{TTY: c.ID, PID: c.PID, ReadOnly: c.ReadOnly, Flags: c.Flags})
	}
	return out, nil
}

func (h *Host) CapturePane(ctx context.Context, name string, o sessionhost.CaptureOptions) (string, error) {
	resp, err := h.call(ctx, wire.Request{Op: wire.OpCapture, Name: name, History: o.History, Escapes: o.Escapes, Join: o.Join}, h.timeout())
	if err != nil {
		return "", err
	}
	return resp.Text, nil
}

func (h *Host) Paste(ctx context.Context, name, buffer string, text []byte) error {
	if !sessionhost.ValidName(buffer) {
		return fmt.Errorf("termhost: invalid buffer name %q", buffer)
	}
	_, err := h.call(ctx, wire.Request{Op: wire.OpPaste, Name: name, Text: text}, h.timeout())
	return err
}

func (h *Host) SendKeys(ctx context.Context, name string, keys ...sessionhost.Key) error {
	if len(keys) == 0 {
		return nil
	}
	ks := make([]string, len(keys))
	for i, k := range keys {
		if !sessionhost.AllowedKey(k) {
			return fmt.Errorf("termhost: key %q is not an allowed named key", k)
		}
		ks[i] = string(k)
	}
	_, err := h.call(ctx, wire.Request{Op: wire.OpKeys, Name: name, Keys: ks}, h.timeout())
	return err
}

func ignoreGone(err error) error {
	if errors.Is(err, sessionhost.ErrNoServer) || errors.Is(err, sessionhost.ErrNoSession) {
		return nil
	}
	return err
}

func (h *Host) KillSession(ctx context.Context, name string) error {
	_, err := h.call(ctx, wire.Request{Op: wire.OpKill, Name: name}, h.timeout())
	return ignoreGone(err)
}

// TerminateSession ends the session's process tree (SIGTERM to its group /
// CTRL_CLOSE_EVENT to its console), forcefully after grace, and returns
// once it is gone.
func (h *Host) TerminateSession(ctx context.Context, name string, grace time.Duration) error {
	_, err := h.call(ctx, wire.Request{Op: wire.OpTerminate, Name: name, GraceMS: grace.Milliseconds()}, grace+h.timeout())
	return ignoreGone(err)
}

// KillServer kills every session and stops the host (no error if it is
// not running).
func (h *Host) KillServer(ctx context.Context) error {
	_, err := h.call(ctx, wire.Request{Op: wire.OpShutdown}, h.timeout())
	if errors.Is(err, sessionhost.ErrNoServer) {
		err = nil
	}
	if err == nil {
		forgetSessionTask(h.Socket)
	}
	return err
}

func (h *Host) Pane(ctx context.Context, name string) (sessionhost.PaneInfo, error) {
	resp, err := h.call(ctx, wire.Request{Op: wire.OpPane, Name: name}, h.timeout())
	if err != nil {
		return sessionhost.PaneInfo{}, err
	}
	p := resp.Pane
	if p == nil {
		return sessionhost.PaneInfo{}, fmt.Errorf("%w (no pane)", sessionhost.ErrNoSession)
	}
	return sessionhost.PaneInfo{PaneID: p.ID, PID: p.PID, Dead: p.Dead, ExitStatus: p.Exit, CurrentCommand: p.Command, Width: p.Width, Height: p.Height}, nil
}

func (h *Host) ResizeWindow(ctx context.Context, name string, width, height int) error {
	if width <= 0 || height <= 0 {
		return fmt.Errorf("termhost: invalid window size %dx%d", width, height)
	}
	_, err := h.call(ctx, wire.Request{Op: wire.OpResize, Name: name, Cols: width, Rows: height}, h.timeout())
	return err
}

// DetachNotes is the human hint shown with termhost attach descriptors.
const DetachNotes = "Pyre terminal host attach; detach with Ctrl-b d (the run keeps going)"

// AttachDescriptor returns a local attach command (the `__pyre_termhost
// attach` client of this executable), or for another node an ssh command
// running it there. The mechanism names stay local_tmux/ssh_tmux (the
// contract's native-terminal mechanisms): clients run the command on a
// terminal exactly as they run a tmux attach, and the web-terminal bridge
// and the TUI recognise it (ParseAttachArgv) and connect to the host
// directly instead.
func (h *Host) AttachDescriptor(cfg sessionhost.AttachConfig, runID, name string, req contracts.AttachCommand, now time.Time) (contracts.AttachDescriptor, error) {
	readOnly := req.ReadOnly
	wantLocal, wantSSH, wantRO, other := false, false, false, false
	for _, m := range req.Preferred {
		switch m {
		case contracts.AttachLocalTmux:
			wantLocal = true
		case contracts.AttachSSHTmux:
			wantSSH = true
		case contracts.AttachReadOnly:
			wantRO = true
		default:
			other = true
		}
	}
	if len(req.Preferred) > 0 && !wantLocal && !wantSSH && !wantRO && other {
		return contracts.AttachDescriptor{}, contracts.Errorf(contracts.ErrUnsupported,
			"attach mechanisms %v unsupported; this adapter offers local_tmux and ssh_tmux (Pyre terminal host)", req.Preferred)
	}
	if wantRO && !wantLocal && !wantSSH {
		readOnly = true
	}
	exe, err := h.exe()
	if err != nil {
		return contracts.AttachDescriptor{}, contracts.Errorf(contracts.ErrUnavailable, "%v", err)
	}
	ttl := cfg.TTL
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	d := contracts.AttachDescriptor{RunID: runID, NodeID: cfg.NodeID, ExpiresAt: now.Add(ttl), ReadOnly: readOnly, Notes: DetachNotes}
	argv := AttachArgv(exe, h.Socket, name, readOnly)
	remote := req.ClientNodeID != "" && req.ClientNodeID != cfg.NodeID
	if wantSSH && !wantLocal {
		remote = true
	}
	if !remote {
		d.Mechanism, d.Command = contracts.AttachLocalTmux, argv
		return d, nil
	}
	if cfg.SSHAlias == "" {
		return contracts.AttachDescriptor{}, contracts.Errorf(contracts.ErrUnavailable,
			"remote attach needs an ssh alias for node %s (not configured)", cfg.NodeID)
	}
	if strings.HasPrefix(cfg.SSHAlias, "-") || strings.ContainsAny(cfg.SSHAlias, " \t\"'`$;&|<>") {
		return contracts.AttachDescriptor{}, contracts.Errorf(contracts.ErrInvalid, "invalid ssh alias %q", cfg.SSHAlias)
	}
	d.Mechanism = contracts.AttachSSHTmux
	d.Command = []string{"ssh", "-t", cfg.SSHAlias, remoteCommand(argv)}
	return d, nil
}

// AttachArgv is the attach client command for session name.
func AttachArgv(exe, socket, name string, readOnly bool) []string {
	argv := []string{exe, ServeArg, "attach", "-S", socket, "-t", name}
	if readOnly {
		argv = append(argv, "-r")
	}
	return argv
}

// AttachTarget is a parsed attach command.
type AttachTarget struct {
	Socket   string
	Name     string
	ReadOnly bool
}

// ParseAttachArgv recognises an AttachArgv command.
func ParseAttachArgv(argv []string) (AttachTarget, bool) {
	if len(argv) < 3 || argv[1] != ServeArg || argv[2] != "attach" {
		return AttachTarget{}, false
	}
	var t AttachTarget
	for i := 3; i < len(argv); i++ {
		switch argv[i] {
		case "-S", "-t":
			if i+1 >= len(argv) {
				return AttachTarget{}, false
			}
			if argv[i] == "-S" {
				t.Socket = argv[i+1]
			} else {
				t.Name = argv[i+1]
			}
			i++
		case "-r":
			t.ReadOnly = true
		default:
			return AttachTarget{}, false
		}
	}
	return t, t.Socket != "" && sessionhost.ValidName(t.Name)
}
