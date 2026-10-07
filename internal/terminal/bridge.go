package terminal

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/creack/pty"

	"github.com/jhh3/pyre/internal/contracts"
)

// BridgeConfig configures a NodeBridge.
type BridgeConfig struct {
	HubURL string      // https://host:port (http:// only for tests)
	Token  string      // node bearer token; never logged
	TLS    *tls.Config // pinned hub TLS (transport.ClientTLSConfig)
	Logger *slog.Logger

	DialTimeout   time.Duration // dial-back to the hub (10s)
	KillGrace     time.Duration // wait after SIGHUP before SIGKILL of the attach client (2s)
	MaxSessions   int           // concurrent streams on this node (16)
	MaxFrameBytes int64         // largest accepted frame from the hub (64 KiB)
	// Term is TERM for the attach client (default xterm-256color).
	Term string
	// Env is the base environment for the attach client (default
	// os.Environ()); TMUX and TMUX_PANE are always removed so the client
	// cannot talk to the caller's own tmux server.
	Env []string
}

// NodeBridge is the node side of web terminals. The daemon calls Open for
// each terminal_open command with the run's local attach argv.
type NodeBridge struct {
	cfg    BridgeConfig
	wsURL  string
	client *http.Client

	mu       sync.Mutex
	sessions map[string]*nodeSession
	closed   bool
	wg       sync.WaitGroup
}

// NewNodeBridge validates cfg and returns a bridge.
func NewNodeBridge(cfg BridgeConfig) (*NodeBridge, error) {
	if cfg.HubURL == "" || cfg.Token == "" {
		return nil, errors.New("terminal: BridgeConfig requires HubURL and Token")
	}
	u, err := url.Parse(cfg.HubURL)
	if err != nil {
		return nil, fmt.Errorf("terminal: hub url: %w", err)
	}
	switch u.Scheme {
	case "https", "wss":
		u.Scheme = "wss"
	case "http", "ws":
		u.Scheme = "ws"
	default:
		return nil, fmt.Errorf("terminal: hub url must be https://, got %q", u.Scheme)
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + contracts.PathNodeTerminal
	u.RawQuery = ""
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 10 * time.Second
	}
	if cfg.KillGrace <= 0 {
		cfg.KillGrace = 2 * time.Second
	}
	if cfg.MaxSessions <= 0 {
		cfg.MaxSessions = 16
	}
	if cfg.MaxFrameBytes <= 0 {
		cfg.MaxFrameBytes = 64 << 10
	}
	if cfg.Term == "" {
		cfg.Term = "xterm-256color"
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	tr := &http.Transport{TLSClientConfig: cfg.TLS, ForceAttemptHTTP2: false}
	return &NodeBridge{cfg: cfg, wsURL: u.String(), client: &http.Client{Transport: tr}, sessions: map[string]*nodeSession{}}, nil
}

// nodeSession is one PTY-backed attach client relayed to the hub.
type nodeSession struct {
	b        *NodeBridge
	id       string
	runID    string
	readOnly bool
	cmd      *exec.Cmd
	ptmx     *os.File
	ws       *websocket.Conn
	exited   chan struct{}
	once     sync.Once
	ctx      context.Context
	cancel   context.CancelFunc
}

// Open starts argv (the run's `tmux attach` client) in a PTY sized
// cols x rows, dials the hub's PathNodeTerminal for tc.StreamID, and relays
// in the background. It returns once the stream is connected (or failed),
// so the node's sequential command worker is never held by a terminal.
func (b *NodeBridge) Open(ctx context.Context, tc contracts.TerminalOpenCommand, argv []string) error {
	if tc.StreamID == "" || tc.RunID == "" {
		return contracts.Errorf(contracts.ErrInvalid, "terminal_open requires stream_id and run_id")
	}
	if len(argv) == 0 {
		return contracts.Errorf(contracts.ErrUnsupported, "run has no local attach command").WithRef(contracts.KindRun, tc.RunID)
	}
	b.mu.Lock()
	switch {
	case b.closed:
		b.mu.Unlock()
		return contracts.Errorf(contracts.ErrUnavailable, "terminal bridge is shutting down")
	case b.sessions[tc.StreamID] != nil:
		b.mu.Unlock()
		return contracts.Errorf(contracts.ErrConflict, "terminal stream already open")
	case len(b.sessions) >= b.cfg.MaxSessions:
		b.mu.Unlock()
		return contracts.Errorf(contracts.ErrCapacityExhausted, "node has %d open terminal streams (limit %d)", len(b.sessions), b.cfg.MaxSessions)
	}
	s := &nodeSession{b: b, id: tc.StreamID, runID: tc.RunID, readOnly: tc.ReadOnly, exited: make(chan struct{})}
	b.sessions[tc.StreamID] = s // reserve
	b.mu.Unlock()
	release := func() {
		b.mu.Lock()
		delete(b.sessions, tc.StreamID)
		b.mu.Unlock()
	}

	cols, rows := ClampSize(tc.Cols, tc.Rows)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = b.env()
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		release()
		return contracts.Errorf(contracts.ErrUnavailable, "start attach client: %v", err).WithRef(contracts.KindRun, tc.RunID)
	}
	s.cmd, s.ptmx = cmd, ptmx
	go func() {
		_ = cmd.Wait()
		close(s.exited)
	}()

	ws, err := b.dial(ctx, tc.StreamID)
	if err != nil {
		s.stopChild()
		_ = ptmx.Close()
		release()
		return err
	}
	ws.SetReadLimit(b.cfg.MaxFrameBytes)
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		ws.CloseNow()
		s.stopChild()
		_ = ptmx.Close()
		release()
		return contracts.Errorf(contracts.ErrUnavailable, "terminal bridge is shutting down")
	}
	s.ws = ws
	s.ctx, s.cancel = context.WithCancel(context.Background())
	b.wg.Add(2)
	b.mu.Unlock()
	go s.ptyToHub()
	go s.hubToPTY()
	b.cfg.Logger.Info("terminal stream connected", "stream", tc.StreamID, "run", tc.RunID, "read_only", tc.ReadOnly)
	return nil
}

func (b *NodeBridge) env() []string {
	base := b.cfg.Env
	if base == nil {
		base = os.Environ()
	}
	out := make([]string, 0, len(base)+1)
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if k == "TMUX" || k == "TMUX_PANE" || k == "TERM" {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "TERM="+b.cfg.Term)
}

func (b *NodeBridge) dial(ctx context.Context, streamID string) (*websocket.Conn, error) {
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+b.cfg.Token)
	hdr.Set(contracts.ProtocolHeader, strconv.Itoa(contracts.ProtocolVersion))
	dctx, cancel := context.WithTimeout(ctx, b.cfg.DialTimeout)
	defer cancel()
	ws, resp, err := websocket.Dial(dctx, b.wsURL+"?stream="+url.QueryEscape(streamID), &websocket.DialOptions{
		HTTPClient: b.client, HTTPHeader: hdr, CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
			if resp.Body != nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
				resp.Body.Close()
			}
			return nil, contracts.Errorf(contracts.ErrUnavailable, "hub refused the terminal stream (HTTP %d)", resp.StatusCode)
		}
		return nil, contracts.Errorf(contracts.ErrUnavailable, "dial hub terminal endpoint: %v", err)
	}
	return ws, nil
}

// ptyToHub copies attach-client output to the hub as binary frames.
func (s *nodeSession) ptyToHub() {
	defer s.b.wg.Done()
	buf := make([]byte, 32<<10)
	for {
		n, err := s.ptmx.Read(buf)
		if n > 0 {
			wctx, cancel := context.WithTimeout(s.ctx, writeTimeout)
			werr := s.ws.Write(wctx, websocket.MessageBinary, buf[:n])
			cancel()
			if werr != nil {
				s.close("hub write failed")
				return
			}
		}
		if err != nil {
			s.close("terminal exited") // EOF/EIO: the attach client is gone
			return
		}
	}
}

// hubToPTY applies hub frames: input bytes (dropped when read-only) and
// resize requests.
func (s *nodeSession) hubToPTY() {
	defer s.b.wg.Done()
	for {
		typ, b, err := s.ws.Read(s.ctx)
		if err != nil {
			s.close("hub closed")
			return
		}
		switch typ {
		case websocket.MessageBinary:
			if s.readOnly {
				continue
			}
			if _, err := s.ptmx.Write(b); err != nil {
				s.close("terminal write failed")
				return
			}
		case websocket.MessageText:
			f, err := parseFrame(b)
			if err != nil || f.Type != FrameResize {
				continue
			}
			cols, rows := ClampSize(f.Cols, f.Rows)
			_ = pty.Setsize(s.ptmx, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
		}
	}
}

// close ends the stream once: the hub socket is closed, then only the
// attach client process is stopped (SIGHUP, which makes tmux detach, then
// SIGKILL after the grace period). The tmux server and the harness are
// never signalled, so the run keeps going.
func (s *nodeSession) close(reason string) {
	s.once.Do(func() {
		go func() {
			_ = s.ws.Close(websocket.StatusNormalClosure, reason)
			s.cancel()
		}()
		s.stopChild()
		_ = s.ptmx.Close()
		s.b.mu.Lock()
		delete(s.b.sessions, s.id)
		s.b.mu.Unlock()
		s.b.cfg.Logger.Info("terminal stream closed", "stream", s.id, "run", s.runID, "reason", reason)
	})
}

// stopChild signals the attach client by PID (not its process group).
func (s *nodeSession) stopChild() {
	select {
	case <-s.exited:
		return
	default:
	}
	_ = s.cmd.Process.Signal(syscall.SIGHUP)
	select {
	case <-s.exited:
		return
	case <-time.After(s.b.cfg.KillGrace):
	}
	_ = s.cmd.Process.Kill()
	<-s.exited
}

// Sessions reports the number of open streams.
func (b *NodeBridge) Sessions() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.sessions)
}

// Close ends every stream (detaching their attach clients) and refuses new
// ones.
func (b *NodeBridge) Close() error {
	b.mu.Lock()
	b.closed = true
	var open []*nodeSession
	for _, s := range b.sessions {
		if s.ws != nil {
			open = append(open, s)
		}
	}
	b.mu.Unlock()
	for _, s := range open {
		s.close("node shutting down")
	}
	b.wg.Wait()
	return nil
}
