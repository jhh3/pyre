package terminal

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/jhh3/pyre/internal/contracts"
)

// Backend is the hub core as the relay needs it. Implemented by *hub.Hub.
type Backend interface {
	// PrepareTerminal checks that p may open a stream on runID now: the run
	// exists and has not ended, its node is connected, and (for an
	// interactive stream) no other principal holds the run's input lease.
	// It returns the run's node.
	PrepareTerminal(ctx context.Context, p contracts.Principal, runID string, readOnly bool) (nodeID string, err error)
	// QueueTerminalOpen durably queues CmdTerminalOpen for nodeID (which must
	// still own cmd.RunID), expiring after ttl if undelivered.
	QueueTerminalOpen(ctx context.Context, p contracts.Principal, nodeID string, cmd contracts.TerminalOpenCommand, ttl time.Duration) (commandID string, err error)
	// TerminalOpenResult waits up to wait for the command to finish.
	// finished=false means still outstanding; err is the node's failure.
	TerminalOpenResult(ctx context.Context, commandID string, wait time.Duration) (finished bool, err error)
	// TerminalInputAllowed returns nil when p may write raw input to runID
	// (no other principal holds an active input lease).
	TerminalInputAllowed(ctx context.Context, p contracts.Principal, runID string) error
}

// RelayConfig configures a Relay. Zero durations/limits select defaults.
type RelayConfig struct {
	Tickets contracts.TokenIssuer // mints/redeems one-time tickets (required)
	// Auth authenticates node bearer tokens on PathNodeTerminal when the
	// request context carries no Principal (it does when mounted behind
	// httpapi's bearer authentication).
	Auth    contracts.Authenticator
	Backend Backend // required
	Clock   contracts.Clock
	Logger  *slog.Logger

	TicketTTL     time.Duration // one-time ticket lifetime (60s)
	AuthTimeout   time.Duration // wait for the client's auth frame (10s)
	DialWait      time.Duration // wait for the node to dial back (15s)
	CommandTTL    time.Duration // terminal_open expires if undelivered (15s)
	IdleTimeout   time.Duration // close after no traffic either way (30m)
	PingInterval  time.Duration // keepalive pings on both sockets (30s)
	LeaseRecheck  time.Duration // input-lease cache for interactive streams (2s)
	MaxStreams    int           // concurrent streams, pending + active (32)
	MaxGrants     int           // outstanding unredeemed tickets (1024)
	MaxFrameBytes int64         // largest accepted frame (64 KiB)
	BufferFrames  int           // client input frames buffered before the node connects (32)
	// AllowedOrigins are extra browser origins for the client socket (see
	// coder/websocket OriginPatterns). Clients without Origin are allowed.
	AllowedOrigins []string
}

func (c *RelayConfig) defaults() {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&c.TicketTTL, 60*time.Second)
	def(&c.AuthTimeout, 10*time.Second)
	def(&c.DialWait, 15*time.Second)
	def(&c.CommandTTL, 15*time.Second)
	def(&c.IdleTimeout, 30*time.Minute)
	def(&c.PingInterval, 30*time.Second)
	def(&c.LeaseRecheck, 2*time.Second)
	if c.MaxStreams <= 0 {
		c.MaxStreams = 32
	}
	if c.MaxGrants <= 0 {
		c.MaxGrants = 1024
	}
	if c.MaxFrameBytes <= 0 {
		c.MaxFrameBytes = 64 << 10
	}
	if c.BufferFrames <= 0 {
		c.BufferFrames = 32
	}
	if c.Clock == nil {
		c.Clock = contracts.SystemClock{}
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.DiscardHandler)
	}
}

// Relay is the hub side of web terminals: it issues tickets and pairs
// client and node sockets.
type Relay struct {
	cfg RelayConfig

	mu      sync.Mutex
	grants  map[string]grant   // sha256(ticket) -> grant
	streams map[string]*stream // stream ID -> stream
	active  int
}

// grant records what a ticket authorizes. The ticket itself is redeemed
// (exactly once, durably) through the TokenIssuer; only its hash keys this
// in-memory record. A hub restart drops grants, so outstanding tickets stop
// working (they live for a minute).
type grant struct {
	runID     string
	principal contracts.Principal
	readOnly  bool
	expires   time.Time
}

type stream struct {
	id        string
	runID     string
	nodeID    string
	readOnly  bool
	fit       bool // the node sizes the stream to the window (read-only only)
	principal contracts.Principal
	nodeCh    chan *websocket.Conn // capacity 1
	claimed   bool
	done      chan struct{}
}

// NewRelay validates cfg and returns a Relay.
func NewRelay(cfg RelayConfig) (*Relay, error) {
	if cfg.Tickets == nil || cfg.Backend == nil {
		return nil, errors.New("terminal: RelayConfig requires Tickets and Backend")
	}
	cfg.defaults()
	return &Relay{cfg: cfg, grants: map[string]grant{}, streams: map[string]*stream{}}, nil
}

func hashTicket(t string) string {
	s := sha256.Sum256([]byte(t))
	return hex.EncodeToString(s[:])
}

func newStreamID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "tstr_" + hex.EncodeToString(b[:]), nil
}

// IssueTerminalTicket mints a one-time ticket letting p open one web
// terminal stream on runID. The caller has already authorized p for the run
// (sessions.attach). Never log the returned ticket.
func (r *Relay) IssueTerminalTicket(ctx context.Context, p contracts.Principal, runID string, readOnly bool) (string, time.Time, error) {
	if runID == "" {
		return "", time.Time{}, contracts.Errorf(contracts.ErrInvalid, "run_id is required")
	}
	now := r.cfg.Clock.Now()
	r.mu.Lock()
	for k, g := range r.grants {
		if !now.Before(g.expires) {
			delete(r.grants, k)
		}
	}
	full := len(r.grants) >= r.cfg.MaxGrants
	r.mu.Unlock()
	if full {
		return "", time.Time{}, contracts.Errorf(contracts.ErrCapacityExhausted, "too many outstanding terminal tickets")
	}
	tk, exp, err := r.cfg.Tickets.IssueTicket(ctx, TicketPurpose, contracts.ObjectRef{Kind: contracts.KindRun, ID: runID}, r.cfg.TicketTTL)
	if err != nil {
		return "", time.Time{}, err
	}
	r.mu.Lock()
	r.grants[hashTicket(tk)] = grant{runID: runID, principal: p, readOnly: readOnly, expires: exp}
	r.mu.Unlock()
	return tk, exp, nil
}

// redeem consumes a ticket exactly once and returns its grant.
func (r *Relay) redeem(ctx context.Context, ticket string) (grant, error) {
	ref, err := r.cfg.Tickets.RedeemTicket(ctx, ticket, TicketPurpose)
	key := hashTicket(ticket)
	r.mu.Lock()
	g, ok := r.grants[key]
	if err == nil || contracts.IsCode(err, contracts.ErrConflict) || contracts.IsCode(err, contracts.ErrExpired) {
		delete(r.grants, key)
	}
	r.mu.Unlock()
	if err != nil {
		return grant{}, err
	}
	if !ok || ref.Kind != contracts.KindRun || ref.ID != g.runID {
		return grant{}, contracts.Errorf(contracts.ErrExpired, "terminal ticket is no longer valid; request a new attach")
	}
	return g, nil
}

func (r *Relay) acquireSlot() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active >= r.cfg.MaxStreams {
		return false
	}
	r.active++
	return true
}

func (r *Relay) releaseSlot() {
	r.mu.Lock()
	r.active--
	r.mu.Unlock()
}

func (r *Relay) register(st *stream) {
	r.mu.Lock()
	r.streams[st.id] = st
	r.mu.Unlock()
}

// unregister ends a stream: a node socket that arrived too late is closed.
func (r *Relay) unregister(st *stream) {
	r.mu.Lock()
	delete(r.streams, st.id)
	r.mu.Unlock()
	close(st.done)
	select {
	case ws := <-st.nodeCh:
		_ = ws.Close(websocket.StatusNormalClosure, "terminal stream ended")
	default:
	}
}

// Streams reports the number of open (pending or active) streams.
func (r *Relay) Streams() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.streams)
}

// ClientHandler serves contracts.PathTerminal. It is public (no bearer):
// the one-time ticket in the first frame authenticates the stream.
func (r *Relay) ClientHandler() http.Handler { return http.HandlerFunc(r.serveClient) }

// NodeHandler serves contracts.PathNodeTerminal for node principals.
func (r *Relay) NodeHandler() http.Handler { return http.HandlerFunc(r.serveNode) }

func writeHTTPError(w http.ResponseWriter, e *contracts.Error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Code.HTTPStatus())
	_ = json.NewEncoder(w).Encode(contracts.ErrorBody{Error: e})
}

// clientMsg is one frame read from the client socket.
type clientMsg struct {
	typ  websocket.MessageType
	data []byte
}

func (r *Relay) serveClient(w http.ResponseWriter, req *http.Request) {
	ws, err := websocket.Accept(w, req, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled, OriginPatterns: r.cfg.AllowedOrigins})
	if err != nil {
		return
	}
	ws.SetReadLimit(r.cfg.MaxFrameBytes)
	defer ws.CloseNow()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	actx, acancel := context.WithTimeout(ctx, r.cfg.AuthTimeout)
	typ, b, err := ws.Read(actx)
	acancel()
	if err != nil {
		closeWithError(ws, contracts.Errorf(contracts.ErrInvalid, "expected an auth frame"))
		return
	}
	var f Frame
	if typ == websocket.MessageText {
		f, err = parseFrame(b)
	}
	if typ != websocket.MessageText || err != nil || f.Type != FrameAuth || f.Ticket == "" {
		closeWithError(ws, contracts.Errorf(contracts.ErrInvalid, `first frame must be {"type":"auth","ticket":...}`))
		return
	}
	g, err := r.redeem(ctx, f.Ticket)
	if err != nil {
		closeWithError(ws, publicError(err))
		return
	}
	if !r.acquireSlot() {
		closeWithError(ws, contracts.Errorf(contracts.ErrCapacityExhausted, "too many open terminal streams"))
		return
	}
	defer r.releaseSlot()
	cols, rows := ClampSize(f.Cols, f.Rows)
	nodeID, err := r.cfg.Backend.PrepareTerminal(ctx, g.principal, g.runID, g.readOnly)
	if err != nil {
		closeWithError(ws, publicError(err))
		return
	}
	id, err := newStreamID()
	if err != nil {
		closeWithError(ws, contracts.Errorf(contracts.ErrInternal, "could not allocate a stream"))
		return
	}
	st := &stream{id: id, runID: g.runID, nodeID: nodeID, readOnly: g.readOnly, fit: f.Fit && g.readOnly, principal: g.principal,
		nodeCh: make(chan *websocket.Conn, 1), done: make(chan struct{})}
	r.register(st)
	defer r.unregister(st)

	// Read the client from now on so input typed before the node arrives is
	// buffered (bounded: the reader blocks when the buffer is full) and a
	// hang-up is noticed while waiting.
	in := make(chan clientMsg, r.cfg.BufferFrames)
	gone := make(chan struct{})
	var readErr error
	go func() {
		defer close(gone)
		defer close(in)
		for {
			typ, b, err := ws.Read(ctx)
			if err != nil {
				readErr = err
				return
			}
			select {
			case in <- clientMsg{typ, b}:
			case <-ctx.Done():
				return
			}
		}
	}()

	cmdID, err := r.cfg.Backend.QueueTerminalOpen(ctx, g.principal, nodeID, contracts.TerminalOpenCommand{
		StreamID: id, RunID: g.runID, ReadOnly: g.readOnly, Cols: cols, Rows: rows, Fit: st.fit}, r.cfg.CommandTTL)
	if err != nil {
		closeWithError(ws, publicError(err))
		return
	}
	start := r.cfg.Clock.Now()
	node, err := r.awaitNode(ctx, st, cmdID, gone)
	if err != nil {
		if !errors.Is(err, errClientGone) {
			closeWithError(ws, publicError(err))
		}
		r.cfg.Logger.Info("terminal stream not opened", "stream", id, "run", g.runID, "node", nodeID, "err", contracts.CodeOf(err))
		return
	}
	defer node.CloseNow()
	r.cfg.Logger.Info("terminal stream open", "stream", id, "run", g.runID, "node", nodeID,
		"principal", string(g.principal.Kind)+":"+g.principal.ID, "read_only", g.readOnly)
	reason := r.pump(ctx, ws, node, st, in, gone, &readErr)
	r.cfg.Logger.Info("terminal stream closed", "stream", id, "run", g.runID, "reason", reason, "duration", r.cfg.Clock.Now().Sub(start).Round(time.Second).String())
}

var errClientGone = errors.New("client went away")

// publicError keeps internal details out of client frames.
func publicError(err error) *contracts.Error {
	e := contracts.AsError(err)
	if e.Code == contracts.ErrInternal {
		return contracts.Errorf(contracts.ErrInternal, "internal error")
	}
	return e
}

// awaitNode waits for the node to dial back, the node to report failure,
// the dial-back deadline, or the client to leave.
func (r *Relay) awaitNode(ctx context.Context, st *stream, cmdID string, gone <-chan struct{}) (*websocket.Conn, error) {
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	failed := make(chan error, 1)
	go func() {
		for wctx.Err() == nil {
			finished, err := r.cfg.Backend.TerminalOpenResult(wctx, cmdID, r.cfg.DialWait)
			if wctx.Err() != nil {
				return
			}
			if finished {
				if err != nil {
					failed <- err
				}
				return
			}
			if err != nil { // lookup failure: let the dial-back deadline decide
				return
			}
		}
	}()
	timer := time.NewTimer(r.cfg.DialWait)
	defer timer.Stop()
	select {
	case ws := <-st.nodeCh:
		return ws, nil
	case err := <-failed:
		return nil, err
	case <-timer.C:
		return nil, contracts.Errorf(contracts.ErrUnavailable, "node did not open the terminal within %s", r.cfg.DialWait).WithRef(contracts.KindNode, st.nodeID)
	case <-gone:
		return nil, errClientGone
	}
}

// pump relays until either side closes, the idle timeout passes, or a
// keepalive fails. It returns a short reason for the log.
func (r *Relay) pump(ctx context.Context, client, node *websocket.Conn, st *stream, in <-chan clientMsg, gone <-chan struct{}, readErr *error) string {
	var last atomic.Int64
	touch := func() { last.Store(time.Now().UnixNano()) }
	touch()

	nodeDone := make(chan error, 1)
	go func() {
		for {
			typ, b, err := node.Read(ctx)
			if err != nil {
				nodeDone <- err
				return
			}
			touch()
			switch typ {
			case websocket.MessageBinary:
				wctx, cancel := context.WithTimeout(ctx, writeTimeout)
				err = client.Write(wctx, websocket.MessageBinary, b)
				cancel()
				if err != nil {
					nodeDone <- nil // client write failed; the client reader reports it
					return
				}
			case websocket.MessageText:
				// The node may report a failure before closing, and a
				// fitted stream's node reports the window's size.
				f, err := parseFrame(b)
				switch {
				case err != nil:
				case f.Type == FrameError && f.Error != nil:
					_ = writeJSON(ctx, client, Frame{Type: FrameError, Error: f.Error})
				case f.Type == FrameResize && st.fit:
					cols, rows := ClampSize(f.Cols, f.Rows)
					if err := writeJSON(ctx, client, Frame{Type: FrameResize, Cols: cols, Rows: rows}); err != nil {
						nodeDone <- nil
						return
					}
				}
			}
		}
	}()

	pingCtx, stopPing := context.WithCancel(ctx)
	defer stopPing()
	for _, c := range []*websocket.Conn{client, node} {
		go keepalive(pingCtx, c, r.cfg.PingInterval)
	}

	var inputOK bool
	var checkedAt time.Time
	allowed := func() bool {
		if st.readOnly {
			return false
		}
		if time.Since(checkedAt) >= r.cfg.LeaseRecheck {
			inputOK = r.cfg.Backend.TerminalInputAllowed(ctx, st.principal, st.runID) == nil
			checkedAt = time.Now()
		}
		return inputOK
	}

	idle := time.NewTicker(idleTick(r.cfg.IdleTimeout))
	defer idle.Stop()
	for {
		select {
		case m, ok := <-in:
			if !ok {
				<-gone
				_ = node.Close(websocket.StatusNormalClosure, "client detached")
				if isNormalClose(*readErr) {
					return "client closed"
				}
				return "client connection lost"
			}
			touch()
			switch m.typ {
			case websocket.MessageBinary:
				if !allowed() {
					continue // read-only, or another principal holds the input lease
				}
				wctx, cancel := context.WithTimeout(ctx, writeTimeout)
				err := node.Write(wctx, websocket.MessageBinary, m.data)
				cancel()
				if err != nil {
					closeWithError(client, contracts.Errorf(contracts.ErrUnavailable, "node terminal connection lost"))
					return "node write failed"
				}
			case websocket.MessageText:
				f, err := parseFrame(m.data)
				if err != nil || f.Type != FrameResize || st.fit {
					continue // unknown control frames are ignored; the node sizes a fitted stream
				}
				cols, rows := ClampSize(f.Cols, f.Rows)
				if err := writeJSON(ctx, node, Frame{Type: FrameResize, Cols: cols, Rows: rows}); err != nil {
					closeWithError(client, contracts.Errorf(contracts.ErrUnavailable, "node terminal connection lost"))
					return "node write failed"
				}
			}
		case err := <-nodeDone:
			if err == nil {
				_ = node.Close(websocket.StatusNormalClosure, "client gone")
				return "client write failed"
			}
			if websocket.CloseStatus(err) == websocket.StatusNormalClosure {
				_ = client.Close(websocket.StatusNormalClosure, "terminal closed")
				return "node closed"
			}
			closeWithError(client, contracts.Errorf(contracts.ErrUnavailable, "node terminal connection lost"))
			return "node connection lost"
		case <-idle.C:
			if time.Since(time.Unix(0, last.Load())) >= r.cfg.IdleTimeout {
				e := contracts.Errorf(contracts.ErrExpired, "terminal idle for %s", r.cfg.IdleTimeout)
				closeWithError(client, e)
				_ = node.Close(websocket.StatusNormalClosure, "idle timeout")
				return "idle timeout"
			}
		}
	}
}

func idleTick(d time.Duration) time.Duration {
	t := d / 4
	if t < 10*time.Millisecond {
		t = 10 * time.Millisecond
	}
	if t > 30*time.Second {
		t = 30 * time.Second
	}
	return t
}

// keepalive pings c every interval; a missing pong closes it, which ends
// the relay through the reader's error.
func keepalive(ctx context.Context, c *websocket.Conn, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		pctx, cancel := context.WithTimeout(ctx, 2*every)
		err := c.Ping(pctx)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				c.CloseNow()
			}
			return
		}
	}
}

func (r *Relay) serveNode(w http.ResponseWriter, req *http.Request) {
	p, ok := contracts.PrincipalFrom(req.Context())
	if !ok || p.Kind == "" {
		if r.cfg.Auth == nil {
			writeHTTPError(w, contracts.Errorf(contracts.ErrUnauthorized, "node bearer token required"))
			return
		}
		bearer := bearerToken(req)
		if bearer == "" {
			writeHTTPError(w, contracts.Errorf(contracts.ErrUnauthorized, "node bearer token required"))
			return
		}
		var err error
		if p, err = r.cfg.Auth.Authenticate(req.Context(), bearer); err != nil {
			writeHTTPError(w, contracts.Errorf(contracts.ErrUnauthorized, "invalid or revoked credential"))
			return
		}
	}
	if p.Kind != contracts.PrincipalNode {
		writeHTTPError(w, contracts.Errorf(contracts.ErrForbidden, "only node credentials may serve terminal streams"))
		return
	}
	id := req.URL.Query().Get("stream")
	r.mu.Lock()
	st := r.streams[id]
	switch {
	case id == "" || st == nil:
		r.mu.Unlock()
		writeHTTPError(w, contracts.Errorf(contracts.ErrNotFound, "unknown or finished terminal stream"))
		return
	case st.nodeID != p.ID:
		r.mu.Unlock()
		r.cfg.Logger.Warn("terminal stream claimed by the wrong node", "stream", id, "node", p.ID)
		writeHTTPError(w, contracts.Errorf(contracts.ErrForbidden, "terminal stream belongs to another node"))
		return
	case st.claimed:
		r.mu.Unlock()
		writeHTTPError(w, contracts.Errorf(contracts.ErrConflict, "terminal stream already connected"))
		return
	}
	st.claimed = true
	r.mu.Unlock()
	ws, err := websocket.Accept(w, req, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	ws.SetReadLimit(r.cfg.MaxFrameBytes)
	select {
	case st.nodeCh <- ws:
	case <-st.done:
		ws.CloseNow()
		return
	}
	// The relay owns the socket; keep the handler alive until it ends.
	<-st.done
}

func bearerToken(req *http.Request) string {
	h := req.Header.Get("Authorization")
	const p = "bearer "
	if len(h) <= len(p) || !strings.EqualFold(h[:len(p)], p) {
		return ""
	}
	return strings.TrimSpace(h[len(p):])
}
