package transport

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/jhh3/pyre/internal/contracts"
)

// ConnInfo describes one accepted node connection.
type ConnInfo struct {
	// ConnID is unique per connection. Hubs record it with the connected
	// state so a late disconnect of a replaced connection cannot mark a
	// freshly reconnected node offline.
	ConnID      string
	RemoteAddr  string
	Protocol    int
	ConnectedAt time.Time
}

// WelcomeInfo is the hub's answer to a node Hello.
type WelcomeInfo struct {
	// CommittedReportSeq is the highest durable report origin_seq the hub
	// has committed for this node. Reports arrive in order on a connection,
	// so this equals the contiguous committed watermark. The node resends
	// everything after it.
	CommittedReportSeq uint64
	// PendingCommands are sent right after Welcome, in order: typically
	// every queued command with Seq > hello.LastCommandSeq.
	PendingCommands []contracts.NodeCommand
}

// DisconnectInfo describes why a node connection ended.
type DisconnectInfo struct {
	ConnID string
	// LastSeen is when the last frame was received from the node.
	LastSeen time.Time
	// Replaced is true when a newer connection for the same node replaced
	// this one; the node is still connected.
	Replaced bool
	Cause    error
}

// NodeServerConfig configures a NodeServer. Auth, OnHello and OnReport are
// required.
type NodeServerConfig struct {
	Auth  contracts.Authenticator
	HubID string
	Clock contracts.Clock

	HeartbeatInterval time.Duration // ping interval; default 15s
	HeartbeatTimeout  time.Duration // silence before disconnect; default 3x interval
	HelloTimeout      time.Duration // default 10s
	WriteTimeout      time.Duration // default 10s
	MaxFrameBytes     int64         // default DefaultMaxFrameBytes
	Logger            *slog.Logger  // wrapped with redaction

	// OnHello registers the connection (e.g. topology.ObserveHello) and
	// returns the committed report watermark and commands to resend. An
	// error is sent to the node as a fatal FrameError.
	OnHello func(ctx context.Context, nodeID string, hello contracts.NodeHello, info ConnInfo) (WelcomeInfo, error)
	// OnReport commits a report (idempotently by (node_id, origin_seq);
	// duplicates must return nil). The server acks only after it returns
	// nil. A *contracts.Error with code invalid_argument marks the report
	// permanently rejected: it is acknowledged (so it cannot wedge the
	// outbox) and logged. Any other error closes the connection with a
	// retryable unavailable error so the node resends after reconnecting.
	// Status reports (OriginSeq 0) are passed through but never acked.
	OnReport func(ctx context.Context, nodeID string, r contracts.NodeReport) error
	// OnCommandAck records that the node durably persisted a command.
	OnCommandAck func(ctx context.Context, nodeID string, ack contracts.CommandAck) error
	// OnDisconnect is called once per connection after it ends.
	OnDisconnect func(ctx context.Context, nodeID string, info DisconnectInfo)
}

// NodeServer is the hub side of the node control channel: an http.Handler
// for contracts.PathNodeConnect implementing contracts.NodeChannel.
type NodeServer struct {
	cfg NodeServerConfig
	log *slog.Logger

	helloMu sync.Mutex // serialises OnHello+register so the registry matches hub state
	mu      sync.Mutex
	conns   map[string]*nodeConn
	closed  bool
	wg      sync.WaitGroup

	// beforeWelcome, if set (tests only), runs after the connection is
	// registered and before Welcome is written.
	beforeWelcome func(nodeID string)
}

var _ contracts.NodeChannel = (*NodeServer)(nil)

// NewNodeServer validates cfg and returns a server.
func NewNodeServer(cfg NodeServerConfig) (*NodeServer, error) {
	if cfg.Auth == nil || cfg.OnHello == nil || cfg.OnReport == nil {
		return nil, errors.New("transport: NodeServerConfig requires Auth, OnHello and OnReport")
	}
	if cfg.Clock == nil {
		cfg.Clock = contracts.SystemClock{}
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = 15 * time.Second
	}
	if cfg.HeartbeatTimeout <= 0 {
		cfg.HeartbeatTimeout = 3 * cfg.HeartbeatInterval
	}
	if cfg.HelloTimeout <= 0 {
		cfg.HelloTimeout = 10 * time.Second
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = 10 * time.Second
	}
	if cfg.MaxFrameBytes <= 0 {
		cfg.MaxFrameBytes = DefaultMaxFrameBytes
	}
	return &NodeServer{cfg: cfg, log: redactingLogger(cfg.Logger), conns: map[string]*nodeConn{}}, nil
}

type nodeConn struct {
	s      *NodeServer
	nodeID string
	token  string // kept in memory only, for periodic re-authentication
	info   ConnInfo
	ws     *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc

	writeMu sync.Mutex
	// ready is closed once Welcome and the welcome's pending commands have
	// been written; Send waits for it so a live command can never precede
	// them on the wire.
	ready    chan struct{}
	lastSeen atomic.Int64 // unix nanos per cfg.Clock (reported to the hub)
	lastRecv atomic.Int64 // unix nanos per real time (liveness)
	replaced atomic.Bool
	once     sync.Once
	cause    atomic.Pointer[contracts.Error]

	// read-loop owned
	watermark uint64
	pending   map[uint64]struct{}
	sawReport bool
}

func writeHTTPError(w http.ResponseWriter, e *contracts.Error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Code.HTTPStatus())
	_ = json.NewEncoder(w).Encode(contracts.ErrorBody{Error: e})
}

// ServeHTTP accepts a node connection: bearer authentication (node
// principals only), WebSocket upgrade, Hello validation and protocol
// negotiation, OnHello, Welcome, pending commands, then the frame loop.
func (s *NodeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	closed := s.closed
	if !closed {
		s.wg.Add(1)
	}
	s.mu.Unlock()
	if closed {
		writeHTTPError(w, contracts.Errorf(contracts.ErrUnavailable, "hub is shutting down"))
		return
	}
	defer s.wg.Done()

	bearer := BearerToken(r)
	if bearer == "" {
		writeHTTPError(w, contracts.Errorf(contracts.ErrUnauthorized, "node bearer token required"))
		return
	}
	p, err := s.cfg.Auth.Authenticate(r.Context(), bearer)
	if err != nil {
		s.log.Warn("node connect rejected", "remote", r.RemoteAddr, "err", err)
		writeHTTPError(w, contracts.AsError(err))
		return
	}
	if p.Kind != contracts.PrincipalNode {
		writeHTTPError(w, contracts.Errorf(contracts.ErrForbidden, "only node credentials may open the node channel"))
		return
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		s.log.Warn("node websocket accept failed", "remote", r.RemoteAddr, "err", err)
		return
	}
	ws.SetReadLimit(s.cfg.MaxFrameBytes)
	ctx, cancel := context.WithCancel(context.Background())
	c := &nodeConn{s: s, nodeID: p.ID, token: bearer, ws: ws, ctx: ctx, cancel: cancel, ready: make(chan struct{}), pending: map[uint64]struct{}{}}
	c.touch()
	defer cancel()

	hctx, hcancel := context.WithTimeout(ctx, s.cfg.HelloTimeout)
	f, err := readFrame(hctx, ws)
	hcancel()
	if err != nil {
		c.closeWith(contracts.Errorf(contracts.ErrInvalid, "expected hello frame: %v", err))
		return
	}
	if f.Type != contracts.FrameHello || f.Hello == nil {
		c.closeWith(contracts.Errorf(contracts.ErrInvalid, "first frame must be hello, got %q", f.Type))
		return
	}
	hello := *f.Hello
	if hello.NodeID != p.ID {
		c.closeWith(contracts.Errorf(contracts.ErrForbidden, "credential belongs to node %s but hello claims node %s", p.ID, hello.NodeID))
		return
	}
	proto, perr := NegotiateProtocol(hello.Protocol, hello.MinProtocol)
	if perr != nil {
		s.log.Warn("node protocol mismatch", "node", p.ID, "node_protocol", hello.Protocol, "node_min", hello.MinProtocol)
		c.closeWith(perr)
		return
	}
	now := s.cfg.Clock.Now()
	c.info = ConnInfo{ConnID: contracts.NewID("conn"), RemoteAddr: r.RemoteAddr, Protocol: proto, ConnectedAt: now}

	welcome, err := s.register(c, hello)
	if err != nil {
		e := contracts.AsError(err)
		s.log.Warn("node hello rejected", "node", p.ID, "err", e)
		c.closeWith(e)
		return
	}
	defer s.unregister(c)
	if s.beforeWelcome != nil {
		s.beforeWelcome(c.nodeID)
	}

	hb := int((s.cfg.HeartbeatInterval + time.Second - 1) / time.Second)
	if err := c.write(contracts.NodeFrame{Type: contracts.FrameWelcome, Welcome: &contracts.NodeWelcome{
		HubID: s.cfg.HubID, Protocol: proto, MinProtocol: contracts.MinProtocolVersion, ServerTime: now,
		CommittedReportSeq: welcome.CommittedReportSeq, HeartbeatSeconds: hb,
	}}); err != nil {
		c.closeWith(nil)
		return
	}
	for i := range welcome.PendingCommands {
		cmd := welcome.PendingCommands[i]
		if err := c.write(contracts.NodeFrame{Type: contracts.FrameCommand, Command: &cmd}); err != nil {
			c.closeWith(nil)
			return
		}
	}
	close(c.ready)
	c.watermark = welcome.CommittedReportSeq
	s.log.Info("node connected", "node", p.ID, "conn", c.info.ConnID, "remote", r.RemoteAddr, "protocol", proto)

	go c.heartbeat()
	c.readLoop()
}

func (s *NodeServer) register(c *nodeConn, hello contracts.NodeHello) (WelcomeInfo, error) {
	s.helloMu.Lock()
	defer s.helloMu.Unlock()
	welcome, err := s.cfg.OnHello(c.ctx, c.nodeID, hello, c.info)
	if err != nil {
		return WelcomeInfo{}, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return WelcomeInfo{}, contracts.Errorf(contracts.ErrUnavailable, "hub is shutting down")
	}
	old := s.conns[c.nodeID]
	s.conns[c.nodeID] = c
	s.mu.Unlock()
	if old != nil {
		old.replaced.Store(true)
		go old.closeWith(contracts.Errorf(contracts.ErrConflict, "connection replaced by a newer connection for node %s", c.nodeID))
	}
	return welcome, nil
}

func (s *NodeServer) unregister(c *nodeConn) {
	s.mu.Lock()
	if s.conns[c.nodeID] == c {
		delete(s.conns, c.nodeID)
	}
	s.mu.Unlock()
	if s.cfg.OnDisconnect != nil {
		var cause error
		if e := c.cause.Load(); e != nil {
			cause = e
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		s.cfg.OnDisconnect(ctx, c.nodeID, DisconnectInfo{
			ConnID: c.info.ConnID, LastSeen: c.seen(), Replaced: c.replaced.Load(), Cause: cause,
		})
	}
	s.log.Info("node disconnected", "node", c.nodeID, "conn", c.info.ConnID, "replaced", c.replaced.Load())
}

func (c *nodeConn) touch() {
	c.lastSeen.Store(c.s.cfg.Clock.Now().UnixNano())
	c.lastRecv.Store(time.Now().UnixNano())
}
func (c *nodeConn) seen() time.Time {
	return time.Unix(0, c.lastSeen.Load()).UTC()
}

func (c *nodeConn) write(f contracts.NodeFrame) error {
	if f.At.IsZero() {
		f.At = c.s.cfg.Clock.Now()
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return writeFrame(c.ctx, c.ws, c.s.cfg.WriteTimeout, f)
}

// closeWith sends e (if non-nil) as a fatal FrameError and closes the
// connection. Safe to call repeatedly and concurrently.
func (c *nodeConn) closeWith(e *contracts.Error) {
	c.once.Do(func() {
		// Leave the registry first so Connected/Send stop seeing a closing
		// connection immediately.
		c.s.mu.Lock()
		if c.s.conns[c.nodeID] == c {
			delete(c.s.conns, c.nodeID)
		}
		c.s.mu.Unlock()
		if e != nil {
			c.cause.Store(e)
			_ = c.write(errorFrame(e))
		}
		done := make(chan struct{})
		go func() {
			_ = c.ws.Close(closeCodeFor(e), closeReason(e))
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = c.ws.CloseNow()
		}
		c.cancel()
	})
}

func (c *nodeConn) readLoop() {
	for {
		f, err := readFrame(c.ctx, c.ws)
		if err != nil {
			if ce, ok := err.(*contracts.Error); ok {
				c.closeWith(ce)
			} else {
				c.closeWith(nil)
			}
			return
		}
		c.touch()
		switch f.Type {
		case contracts.FrameReport:
			if f.Report == nil {
				c.closeWith(contracts.Errorf(contracts.ErrInvalid, "report frame without report"))
				return
			}
			if !c.handleReport(*f.Report) {
				return
			}
		case contracts.FrameCommandAck:
			if f.CommandAck != nil && c.s.cfg.OnCommandAck != nil {
				if err := c.s.cfg.OnCommandAck(c.ctx, c.nodeID, *f.CommandAck); err != nil {
					c.s.log.Warn("command ack not recorded", "node", c.nodeID, "command", f.CommandAck.CommandID, "err", err)
				}
			}
		case contracts.FramePing:
			if err := c.write(contracts.NodeFrame{Type: contracts.FramePong}); err != nil {
				c.closeWith(nil)
				return
			}
		case contracts.FramePong:
		case contracts.FrameError:
			c.s.log.Warn("node reported fatal error", "node", c.nodeID, "err", f.Error)
			c.closeWith(nil)
			return
		case contracts.FrameHello:
			c.closeWith(contracts.Errorf(contracts.ErrInvalid, "duplicate hello on an established connection"))
			return
		default:
			// Unknown frame types are ignored for forward compatibility.
			c.s.log.Debug("ignoring unknown node frame", "node", c.nodeID, "type", f.Type)
		}
	}
}

// handleReport commits a report through OnReport and acknowledges the
// contiguous committed watermark. It returns false if the connection was
// closed.
func (c *nodeConn) handleReport(r contracts.NodeReport) bool {
	err := c.s.cfg.OnReport(c.ctx, c.nodeID, r)
	if err != nil {
		if contracts.IsCode(err, contracts.ErrInvalid) {
			c.s.log.Warn("node report rejected permanently", "node", c.nodeID, "seq", r.OriginSeq, "kind", r.Kind, "err", err)
		} else {
			c.s.log.Warn("node report not committed; closing so the node resends", "node", c.nodeID, "seq", r.OriginSeq, "err", err)
			c.closeWith(contracts.Errorf(contracts.ErrUnavailable, "hub could not commit report %d; reconnect and resend", r.OriginSeq))
			return false
		}
	}
	if r.OriginSeq == 0 {
		return true // replaceable observation: never acknowledged
	}
	seq := r.OriginSeq
	if !c.sawReport && seq > c.watermark+1 {
		// The node resends everything after Welcome.CommittedReportSeq in
		// order, so a gap on the first report means the node no longer holds
		// those seqs (e.g. the hub was restored from an older backup).
		c.s.log.Warn("node report gap: earlier reports are unavailable on the node", "node", c.nodeID, "from", c.watermark+1, "to", seq-1)
		c.watermark = seq - 1
	}
	c.sawReport = true
	switch {
	case seq <= c.watermark:
		// duplicate: re-ack so the node can prune
	case seq == c.watermark+1:
		c.watermark = seq
		for {
			if _, ok := c.pending[c.watermark+1]; !ok {
				break
			}
			delete(c.pending, c.watermark+1)
			c.watermark++
		}
	default:
		if len(c.pending) < 65536 {
			c.pending[seq] = struct{}{}
		}
	}
	if err := c.write(contracts.NodeFrame{Type: contracts.FrameReportAck, ReportAck: &contracts.ReportAck{CommittedSeq: c.watermark}}); err != nil {
		c.closeWith(nil)
		return false
	}
	return true
}

func (c *nodeConn) heartbeat() {
	t := time.NewTicker(c.s.cfg.HeartbeatInterval)
	defer t.Stop()
	// Liveness is checked at a tenth of the timeout so a silent node is
	// dropped within ~1.1x HeartbeatTimeout (45-50 s by default), not up to
	// a whole extra heartbeat interval later.
	check := min(c.s.cfg.HeartbeatTimeout/10, c.s.cfg.HeartbeatInterval)
	if check <= 0 {
		check = c.s.cfg.HeartbeatInterval
	}
	lt := time.NewTicker(check)
	defer lt.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-lt.C:
			if c.silent() {
				return
			}
			continue
		case <-t.C:
		}
		if c.silent() {
			return
		}
		// Backstop for revocation: re-check the credential every beat.
		p, err := c.s.cfg.Auth.Authenticate(c.ctx, c.token)
		if err == nil && (p.Kind != contracts.PrincipalNode || p.ID != c.nodeID) {
			err = contracts.Errorf(contracts.ErrUnauthorized, "credential no longer identifies this node")
		}
		if contracts.IsCode(err, contracts.ErrUnauthorized) {
			c.closeWith(contracts.Errorf(contracts.ErrUnauthorized, "node credential revoked or invalid; re-enroll this node"))
			return
		}
		if err := c.write(contracts.NodeFrame{Type: contracts.FramePing}); err != nil {
			c.closeWith(nil)
			return
		}
	}
}

// silent closes the connection if nothing was received within
// HeartbeatTimeout and reports whether it did.
func (c *nodeConn) silent() bool {
	if time.Since(time.Unix(0, c.lastRecv.Load())) <= c.s.cfg.HeartbeatTimeout {
		return false
	}
	c.s.log.Warn("node heartbeat timeout", "node", c.nodeID, "last_seen", c.seen())
	c.closeWith(contracts.Errorf(contracts.ErrUnavailable, "heartbeat timeout"))
	return true
}

// Send writes a command to a connected node. Call only after the command is
// durably queued; ErrUnavailable means it stays queued for the next Hello.
func (s *NodeServer) Send(ctx context.Context, nodeID string, cmd contracts.NodeCommand) error {
	s.mu.Lock()
	c := s.conns[nodeID]
	s.mu.Unlock()
	if c == nil {
		return contracts.Errorf(contracts.ErrUnavailable, "node %s is not connected; command stays queued", nodeID).WithRef(contracts.KindNode, nodeID)
	}
	// A connection is registered before its Welcome is written; hold the
	// command until Welcome and the resent pending commands are out.
	select {
	case <-c.ready:
	case <-c.ctx.Done():
		return contracts.Errorf(contracts.ErrUnavailable, "node %s connection closed before welcome; command stays queued", nodeID).WithRef(contracts.KindNode, nodeID)
	case <-ctx.Done():
		return contracts.Errorf(contracts.ErrUnavailable, "node %s connection not ready: %v; command stays queued", nodeID, ctx.Err()).WithRef(contracts.KindNode, nodeID)
	}
	if err := c.write(contracts.NodeFrame{Type: contracts.FrameCommand, Command: &cmd}); err != nil {
		c.closeWith(nil)
		return contracts.Errorf(contracts.ErrUnavailable, "node %s connection failed while sending; command stays queued", nodeID).WithRef(contracts.KindNode, nodeID)
	}
	return nil
}

// Connected reports whether nodeID has a live connection.
func (s *NodeServer) Connected(nodeID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.conns[nodeID]
	return ok
}

// ConnectedNodes lists node IDs with live connections.
func (s *NodeServer) ConnectedNodes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.conns))
	for id := range s.conns {
		out = append(out, id)
	}
	return out
}

// Disconnect closes nodeID's live connection, sending reason as a fatal
// error frame. It reports whether a connection existed. Wire it to
// AuthStore.OnRevoke so revocation takes effect immediately.
func (s *NodeServer) Disconnect(nodeID string, reason *contracts.Error) bool {
	s.mu.Lock()
	c := s.conns[nodeID]
	s.mu.Unlock()
	if c == nil {
		return false
	}
	c.closeWith(reason)
	return true
}

// DisconnectOnRevoke registers with src (an *AuthStore) so revoking a node
// principal immediately closes its live connection with an unauthorized
// error. Heartbeat re-authentication remains as a backstop.
func (s *NodeServer) DisconnectOnRevoke(src interface {
	OnRevoke(func(contracts.Principal))
}) {
	src.OnRevoke(func(p contracts.Principal) {
		if p.Kind == contracts.PrincipalNode {
			s.Disconnect(p.ID, contracts.Errorf(contracts.ErrUnauthorized, "node credential revoked; re-enroll this node"))
		}
	})
}

// Close disconnects every node with a retryable error, rejects new
// connections, and waits for connection handlers to finish.
func (s *NodeServer) Close() error {
	s.mu.Lock()
	s.closed = true
	conns := make([]*nodeConn, 0, len(s.conns))
	for _, c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, c := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.closeWith(contracts.Errorf(contracts.ErrUnavailable, "hub is shutting down"))
		}()
	}
	wg.Wait()
	s.wg.Wait()
	return nil
}
