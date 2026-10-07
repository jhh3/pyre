package transport

import (
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// NodeLinkMigrations are the node-side "nodelink" namespace migrations
// (report outbox, received-command journal, counters).
var NodeLinkMigrations = []sqlitex.Migration{{
	Namespace: "nodelink", Version: 1, Name: "outbox_and_commands", SQL: `
CREATE TABLE nodelink_state (
	key   TEXT PRIMARY KEY,
	value INTEGER NOT NULL
);
INSERT INTO nodelink_state(key, value) VALUES ('report_seq', 0), ('acked_seq', 0), ('command_seq', 0);
CREATE TABLE nodelink_outbox (
	origin_seq INTEGER PRIMARY KEY,
	kind       TEXT NOT NULL,
	at         TEXT NOT NULL,
	payload    BLOB NOT NULL,
	created_at TEXT NOT NULL
);
CREATE TABLE nodelink_commands (
	command_id  TEXT PRIMARY KEY,
	seq         INTEGER NOT NULL,
	kind        TEXT NOT NULL,
	command     BLOB NOT NULL,
	state       TEXT NOT NULL,   -- recorded | executing | done
	result      BLOB,
	result_seq  INTEGER,
	received_at TEXT NOT NULL,
	finished_at TEXT
);
CREATE INDEX nodelink_commands_state ON nodelink_commands(state, seq);
`}}

const (
	cmdRecorded  = "recorded"
	cmdExecuting = "executing"
	cmdDone      = "done"
)

// CommandHandler executes hub commands on the node. It is called at most
// once per CommandID, only after the command is durably recorded and
// acknowledged, never for an expired command, and never again after a crash
// mid-execution (that outcome is reported as indeterminate). Commands run
// sequentially in arrival order, so handlers should return promptly: a
// long-running effect returns an interim outcome (e.g. delivered) and
// reports progress later through NodeLink.Enqueue.
type CommandHandler interface {
	HandleCommand(ctx context.Context, cmd contracts.NodeCommand) (contracts.CommandResult, error)
}

// CommandHandlerFunc adapts a function to CommandHandler.
type CommandHandlerFunc func(ctx context.Context, cmd contracts.NodeCommand) (contracts.CommandResult, error)

func (f CommandHandlerFunc) HandleCommand(ctx context.Context, cmd contracts.NodeCommand) (contracts.CommandResult, error) {
	return f(ctx, cmd)
}

// LinkState is the node link's connection state.
type LinkState string

const (
	LinkIdle       LinkState = "idle"
	LinkConnecting LinkState = "connecting"
	LinkConnected  LinkState = "connected"
	LinkBackoff    LinkState = "backoff"
	// LinkRejected: the hub refused this node (revoked credential, protocol
	// mismatch). The link keeps retrying at the maximum backoff only.
	LinkRejected LinkState = "rejected"
	LinkStopped  LinkState = "stopped"
)

// LinkStatus is a snapshot of the link for diagnostics.
type LinkStatus struct {
	State          LinkState
	ConnectedSince *time.Time
	LastError      *contracts.Error
	Attempt        int
	NextRetry      time.Duration
	// HubClockAhead is the latest conservative estimate of how far the
	// hub's clock is ahead of this node's (negative: behind), measured from
	// the hello/welcome round trip. Command expiry is judged on it.
	HubClockAhead time.Duration
}

// NodeLinkConfig configures a NodeLink. HubURL, NodeID, Token, DB and
// Handler are required.
type NodeLinkConfig struct {
	HubURL string // https://host:port (http:// only for tests)
	NodeID string
	Token  string      // node credential; never logged
	TLS    *tls.Config // e.g. PinnedTLSConfig(pin)
	DB     *sql.DB     // node-local SQLite; "nodelink" namespace is migrated
	Clock  contracts.Clock
	Logger *slog.Logger // wrapped with redaction

	// Hello supplies the dynamic hello fields (hostname, platform,
	// harnesses, versions, resources, LiveRuns). NodeID, protocol, and
	// LastCommandSeq are filled in by the link.
	Hello   func(ctx context.Context) (contracts.NodeHello, error)
	Handler CommandHandler

	Backoff BackoffConfig
	// StableAfter is how long a connection must last before the backoff
	// resets (prevents fast retry loops on flapping links). Default 10s.
	StableAfter time.Duration
	// SilenceTimeout closes a connection with no frames from the hub.
	// Default 3x the hub's advertised heartbeat (45s if none).
	SilenceTimeout time.Duration
	DialTimeout    time.Duration // default 15s
	WriteTimeout   time.Duration // default 10s
	// SendWindow bounds unacknowledged reports in flight. Default 256.
	SendWindow int
	// MaxOutbox bounds unacknowledged durable reports; Enqueue fails with
	// capacity_exhausted beyond it. Default 100000.
	MaxOutbox int
	// CommandRetention keeps finished command records for dedup. Default 30d.
	CommandRetention time.Duration
	// OnStateChange observes state transitions (called synchronously).
	OnStateChange func(LinkStatus)
	// OnHubBehind is called (in its own goroutine) after a Welcome whose
	// committed report watermark is below what the hub acknowledged to this
	// node before: the hub lost reports it once committed (e.g. it was
	// restored from an older backup, or replaced). The daemon answers with a
	// run snapshot report so the hub can reconcile runs and sessions.
	OnHubBehind func(ctx context.Context, hubCommitted, nodeAcked uint64)
}

// NodeLink is the node side of the control channel.
type NodeLink struct {
	cfg   NodeLinkConfig
	log   *slog.Logger
	wsURL string
	http  *http.Client

	protoMax, protoMin int // overridable in tests

	kick    chan struct{} // outbox has new rows
	cmdKick chan struct{} // command journal has recorded rows
	running atomic.Bool

	pruneMu   sync.Mutex
	lastPrune time.Time // last finished-command prune (Clock time)

	mu     sync.Mutex
	status LinkStatus
	cur    *linkConn

	// hubAhead is the upper-bound estimate (nanoseconds) of hub clock minus
	// node clock: hub time is at most node time + hubAhead. hubAheadKnown
	// is false until a Welcome (or a value persisted by an earlier process)
	// supplied it. See HubNow.
	hubAhead      atomic.Int64
	hubAheadKnown atomic.Bool

	// secretCmds holds full copies (command ID -> NodeCommand) of journaled
	// commands whose secret fields were withheld from the journal. Memory
	// only: a node restart forgets them (see journalCopy).
	secretCmds sync.Map
}

// withheldKey marks a journaled payload whose secret fields were removed.
const withheldKey = "pyre_secrets_withheld"

// journalCopy returns the copy of cmd that may be persisted: start/resume
// payloads lose session_token (a session credential that must live only in
// the run's environment and the daemon's secret store). secret reports
// whether anything was withheld.
func journalCopy(cmd contracts.NodeCommand) (contracts.NodeCommand, bool) {
	if (cmd.Kind != contracts.CmdStartRun && cmd.Kind != contracts.CmdResume) || len(cmd.Payload) == 0 {
		return cmd, false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(cmd.Payload, &m); err != nil {
		return cmd, false
	}
	tok, ok := m["session_token"]
	if !ok || string(tok) == `""` || string(tok) == "null" {
		return cmd, false
	}
	delete(m, "session_token")
	m[withheldKey] = json.RawMessage("true")
	b, err := json.Marshal(m)
	if err != nil {
		return cmd, false
	}
	out := cmd
	out.Payload = b
	return out, true
}

// secretsWithheld reports whether a journaled command lost secret fields.
func secretsWithheld(cmd contracts.NodeCommand) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(cmd.Payload, &m) != nil {
		return false
	}
	_, ok := m[withheldKey]
	return ok
}

type linkConn struct {
	ws       *websocket.Conn
	ctx      context.Context
	cancel   context.CancelFunc
	writeMu  sync.Mutex
	ackCh    chan struct{}
	acked    atomic.Uint64
	lastRecv atomic.Int64
	timeout  time.Duration
}

func (lc *linkConn) write(f contracts.NodeFrame) error {
	lc.writeMu.Lock()
	defer lc.writeMu.Unlock()
	return writeFrame(lc.ctx, lc.ws, lc.timeout, f)
}

// NewNodeLink validates cfg, migrates the node database, and returns a link.
// Call Run to connect.
func NewNodeLink(cfg NodeLinkConfig) (*NodeLink, error) {
	if cfg.HubURL == "" || cfg.NodeID == "" || cfg.Token == "" || cfg.DB == nil || cfg.Handler == nil {
		return nil, errors.New("transport: NodeLinkConfig requires HubURL, NodeID, Token, DB and Handler")
	}
	u, err := url.Parse(cfg.HubURL)
	if err != nil {
		return nil, fmt.Errorf("transport: hub url: %w", err)
	}
	switch u.Scheme {
	case "https", "wss":
		u.Scheme = "wss"
	case "http", "ws":
		u.Scheme = "ws"
	default:
		return nil, fmt.Errorf("transport: hub url must be https://, got %q", u.Scheme)
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + contracts.PathNodeConnect
	if cfg.Clock == nil {
		cfg.Clock = contracts.SystemClock{}
	}
	if cfg.StableAfter <= 0 {
		cfg.StableAfter = 10 * time.Second
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 15 * time.Second
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = 10 * time.Second
	}
	if cfg.SendWindow <= 0 {
		cfg.SendWindow = 256
	}
	if cfg.MaxOutbox <= 0 {
		cfg.MaxOutbox = 100000
	}
	if cfg.CommandRetention <= 0 {
		cfg.CommandRetention = 30 * 24 * time.Hour
	}
	if err := sqlitex.Migrate(context.Background(), cfg.DB, NodeLinkMigrations...); err != nil {
		return nil, err
	}
	tlsCfg := cfg.TLS
	if tlsCfg == nil {
		tlsCfg = CATLSConfig(nil)
	}
	l := &NodeLink{
		cfg:      cfg,
		log:      redactingLogger(cfg.Logger),
		wsURL:    u.String(),
		http:     &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg, ForceAttemptHTTP2: false}},
		protoMax: contracts.ProtocolVersion,
		protoMin: contracts.MinProtocolVersion,
		kick:     make(chan struct{}, 1),
		cmdKick:  make(chan struct{}, 1),
		status:   LinkStatus{State: LinkIdle},
	}
	// Commands recorded before a restart may run before the first Welcome
	// of this process: judge them on the last hub clock estimate.
	var ahead int64
	if err := cfg.DB.QueryRowContext(context.Background(), `SELECT value FROM nodelink_state WHERE key=?`, hubAheadKey).Scan(&ahead); err == nil {
		l.hubAhead.Store(ahead)
		l.hubAheadKnown.Store(true)
		l.status.HubClockAhead = time.Duration(ahead)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	return l, nil
}

// hubAheadKey persists the hub clock estimate in nodelink_state.
const hubAheadKey = "hub_clock_ahead_ns"

// HubNow returns a conservative estimate of the hub's current time: never
// earlier than the hub's real clock (up to clock drift since the last
// Welcome). Without an estimate it is the node clock.
//
// The estimate comes from the hello/welcome round trip: the hub stamps
// Welcome.ServerTime after it read the hello, which the node sent at local
// time t0, so at any later local time t the hub clock reads at most
// ServerTime + (t - t0). Judging command expiry on that bound means the node
// never starts a command the hub already considers expired, whichever way
// the node clock is skewed.
func (l *NodeLink) HubNow() time.Time {
	now := l.cfg.Clock.Now()
	if !l.hubAheadKnown.Load() {
		return now
	}
	return now.Add(time.Duration(l.hubAhead.Load()))
}

// noteHubClock records the hub clock estimate from a Welcome.
func (l *NodeLink) noteHubClock(ctx context.Context, helloSent time.Time, w contracts.NodeWelcome) {
	if w.ServerTime.IsZero() {
		return // older hub: keep the previous estimate (or the node clock)
	}
	ahead := w.ServerTime.Sub(helloSent)
	prev, known := time.Duration(l.hubAhead.Load()), l.hubAheadKnown.Load()
	l.hubAhead.Store(int64(ahead))
	l.hubAheadKnown.Store(true)
	l.mu.Lock() // reported with the "connected" transition that follows
	l.status.HubClockAhead = ahead
	l.mu.Unlock()
	if _, err := l.cfg.DB.ExecContext(ctx, `INSERT INTO nodelink_state(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, hubAheadKey, int64(ahead)); err != nil {
		l.log.Warn("could not persist hub clock estimate", "err", err)
	}
	if ahead.Abs() >= clockSkewWarn && (!known || (prev-ahead).Abs() >= clockSkewWarn/2) {
		l.log.Warn("node clock differs from the hub clock; command expiry uses the hub clock",
			"node", l.cfg.NodeID, "hub_ahead_by", ahead.Round(time.Millisecond).String())
	}
}

// clockSkewWarn is the hub/node clock difference worth a log warning.
const clockSkewWarn = 30 * time.Second

func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// Kick wakes the sender after reports were added with EnqueueTx.
func (l *NodeLink) Kick() { signal(l.kick) }

// Status returns a snapshot of the link state.
func (l *NodeLink) Status() LinkStatus {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.status
}

// Connected reports whether the link currently has a welcomed connection.
func (l *NodeLink) Connected() bool { return l.Status().State == LinkConnected }

func (l *NodeLink) setStatus(fn func(*LinkStatus)) {
	l.mu.Lock()
	fn(&l.status)
	st := l.status
	l.mu.Unlock()
	if l.cfg.OnStateChange != nil {
		l.cfg.OnStateChange(st)
	}
}

func (l *NodeLink) current() *linkConn {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cur
}

// ---- durable state helpers ----

func stateGet(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, key string) (uint64, error) {
	var v int64
	err := q.QueryRowContext(ctx, `SELECT value FROM nodelink_state WHERE key=?`, key).Scan(&v)
	return uint64(v), err
}

func stateSet(ctx context.Context, tx *sql.Tx, key string, v uint64) error {
	_, err := tx.ExecContext(ctx, `UPDATE nodelink_state SET value=? WHERE key=?`, int64(v), key)
	return err
}

// LastCommandSeq returns the highest command seq durably recorded.
func (l *NodeLink) LastCommandSeq(ctx context.Context) (int64, error) {
	v, err := stateGet(ctx, l.cfg.DB, "command_seq")
	return int64(v), err
}

// OutboxLen returns the number of unacknowledged durable reports.
func (l *NodeLink) OutboxLen(ctx context.Context) (int, error) {
	var n int
	err := l.cfg.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM nodelink_outbox`).Scan(&n)
	return n, err
}

// EnqueueTx appends a durable report inside tx, assigning the next
// OriginSeq, and returns it. Call Kick after the transaction commits.
// Status reports are replaceable and must use SendStatus instead.
func (l *NodeLink) EnqueueTx(ctx context.Context, tx *sql.Tx, kind contracts.ReportKind, at time.Time, payload any) (uint64, error) {
	if kind == "" || kind == contracts.ReportStatus {
		return 0, contracts.Errorf(contracts.ErrInvalid, "report kind %q cannot be queued durably (status reports use SendStatus)", kind)
	}
	var raw []byte
	switch p := payload.(type) {
	case json.RawMessage:
		raw = p
	case []byte:
		raw = p
	default:
		b, err := json.Marshal(payload)
		if err != nil {
			return 0, err
		}
		raw = b
	}
	if at.IsZero() {
		at = l.cfg.Clock.Now()
	}
	seq, err := stateGet(ctx, tx, "report_seq")
	if err != nil {
		return 0, err
	}
	acked, err := stateGet(ctx, tx, "acked_seq")
	if err != nil {
		return 0, err
	}
	if seq >= acked && seq-acked >= uint64(l.cfg.MaxOutbox) {
		return 0, contracts.Errorf(contracts.ErrCapacityExhausted, "node report outbox is full (%d unacknowledged reports); the hub has been unreachable", seq-acked)
	}
	seq++
	if err := stateSet(ctx, tx, "report_seq", seq); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO nodelink_outbox(origin_seq, kind, at, payload, created_at) VALUES(?,?,?,?,?)`,
		int64(seq), string(kind), sqlitex.FormatTime(at), raw, sqlitex.FormatTime(l.cfg.Clock.Now())); err != nil {
		return 0, err
	}
	return seq, nil
}

// Enqueue appends a durable report and wakes the sender. It succeeds while
// the hub is unreachable; the report is delivered (possibly more than once,
// deduplicated by the hub on origin_seq) until acknowledged.
func (l *NodeLink) Enqueue(ctx context.Context, kind contracts.ReportKind, payload any) (uint64, error) {
	var seq uint64
	err := sqlitex.Tx(ctx, l.cfg.DB, func(tx *sql.Tx) error {
		var err error
		seq, err = l.EnqueueTx(ctx, tx, kind, time.Time{}, payload)
		return err
	})
	if err == nil {
		l.Kick()
	}
	return seq, err
}

// SendStatus sends a replaceable status observation (OriginSeq 0). It is
// never queued: when disconnected it returns ErrUnavailable and the
// observation is dropped.
func (l *NodeLink) SendStatus(ctx context.Context, st contracts.NodeStatusReport) error {
	lc := l.current()
	if lc == nil {
		return contracts.Errorf(contracts.ErrUnavailable, "not connected to the hub; status reports are not queued")
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return lc.write(contracts.NodeFrame{Type: contracts.FrameReport, Report: &contracts.NodeReport{
		OriginSeq: 0, Kind: contracts.ReportStatus, At: l.cfg.Clock.Now(), Payload: b,
	}})
}

// ---- run loop ----

// Run connects and keeps the link up until ctx is cancelled, reconnecting
// with bounded exponential backoff and jitter. It also executes recorded
// commands. Only one Run may be active.
func (l *NodeLink) Run(ctx context.Context) error {
	if !l.running.CompareAndSwap(false, true) {
		return errors.New("transport: NodeLink.Run already active")
	}
	defer l.running.Store(false)
	if err := l.recoverCommands(ctx); err != nil {
		if ctx.Err() != nil {
			// Stopped during startup recovery: a stop, not a failure (as
			// in the connect loop below). What recovery finished is
			// committed; the rest runs again at the next start.
			l.setStatus(func(s *LinkStatus) { s.State = LinkStopped; s.ConnectedSince = nil })
			return nil
		}
		return err
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		l.commandWorker(ctx)
	}()
	defer wg.Wait()

	bo := NewBackoff(l.cfg.Backoff)
	for {
		if ctx.Err() != nil {
			l.setStatus(func(s *LinkStatus) { s.State = LinkStopped; s.ConnectedSince = nil })
			return nil
		}
		l.setStatus(func(s *LinkStatus) { s.State = LinkConnecting; s.Attempt = bo.Attempt() })
		up, rejected, err := l.connectOnce(ctx)
		if ctx.Err() != nil {
			l.setStatus(func(s *LinkStatus) { s.State = LinkStopped; s.ConnectedSince = nil })
			return nil
		}
		if up >= l.cfg.StableAfter {
			bo.Reset()
		}
		var delay time.Duration
		state := LinkBackoff
		if rejected {
			delay = bo.MaxDelay()
			state = LinkRejected
		} else {
			delay = bo.Next()
		}
		e := contracts.AsError(err)
		if e != nil {
			e = &contracts.Error{Code: e.Code, Message: Redact(e.Message), Retryable: e.Retryable, Ref: e.Ref, Details: e.Details}
		}
		l.setStatus(func(s *LinkStatus) {
			s.State = state
			s.ConnectedSince = nil
			s.LastError = e
			s.Attempt = bo.Attempt()
			s.NextRetry = delay
		})
		if rejected {
			l.log.Error("hub rejected this node; retrying at maximum backoff", "node", l.cfg.NodeID, "err", e, "retry_in", delay)
		} else {
			l.log.Warn("hub connection ended; reconnecting", "node", l.cfg.NodeID, "err", e, "retry_in", delay)
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
		case <-t.C:
		}
	}
}

func isRejection(e *contracts.Error) bool {
	if e == nil {
		return false
	}
	switch e.Code {
	case contracts.ErrUnauthorized, contracts.ErrForbidden, contracts.ErrProtocolMismatch:
		return true
	}
	return false
}

// connectOnce dials, performs Hello/Welcome, and serves the connection until
// it ends. It returns how long the connection was welcomed and whether the
// hub rejected the node.
func (l *NodeLink) connectOnce(ctx context.Context) (time.Duration, bool, error) {
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+l.cfg.Token)
	hdr.Set(contracts.ProtocolHeader, strconv.Itoa(l.protoMax))
	dctx, dcancel := context.WithTimeout(ctx, l.cfg.DialTimeout)
	ws, resp, err := websocket.Dial(dctx, l.wsURL, &websocket.DialOptions{
		HTTPClient: l.http, HTTPHeader: hdr, CompressionMode: websocket.CompressionDisabled,
	})
	dcancel()
	if err != nil {
		if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
			e := httpError(resp)
			return 0, isRejection(e), e
		}
		return 0, false, contracts.Errorf(contracts.ErrUnavailable, "dial hub: %v", err)
	}
	ws.SetReadLimit(DefaultMaxFrameBytes)
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	lc := &linkConn{ws: ws, ctx: cctx, cancel: cancel, ackCh: make(chan struct{}, 1), timeout: l.cfg.WriteTimeout}
	defer ws.CloseNow()

	hello := contracts.NodeHello{}
	if l.cfg.Hello != nil {
		if hello, err = l.cfg.Hello(cctx); err != nil {
			_ = ws.Close(websocket.StatusInternalError, "hello unavailable")
			return 0, false, fmt.Errorf("build hello: %w", err)
		}
	}
	hello.NodeID = l.cfg.NodeID
	hello.Protocol = l.protoMax
	hello.MinProtocol = l.protoMin
	if hello.LastCommandSeq, err = l.LastCommandSeq(cctx); err != nil {
		return 0, false, err
	}
	// Taken before the write: an earlier t0 only makes the hub clock
	// estimate more conservative (later).
	helloSent := l.cfg.Clock.Now()
	if err := lc.write(contracts.NodeFrame{Type: contracts.FrameHello, Hello: &hello, At: helloSent}); err != nil {
		return 0, false, contracts.Errorf(contracts.ErrUnavailable, "send hello: %v", err)
	}
	wctx, wcancel := context.WithTimeout(cctx, l.cfg.DialTimeout)
	f, err := readFrame(wctx, ws)
	wcancel()
	if err != nil {
		return 0, false, contracts.Errorf(contracts.ErrUnavailable, "awaiting welcome: %v", err)
	}
	if f.Type == contracts.FrameError && f.Error != nil {
		return 0, isRejection(f.Error), f.Error
	}
	if f.Type != contracts.FrameWelcome || f.Welcome == nil {
		return 0, false, contracts.Errorf(contracts.ErrInvalid, "expected welcome, got %q", f.Type)
	}
	// Before any command of this connection is read (pending commands
	// follow the Welcome).
	l.noteHubClock(cctx, helloSent, *f.Welcome)
	from, behind, err := l.applyWelcome(cctx, *f.Welcome)
	if err != nil {
		return 0, false, err
	}
	lc.acked.Store(from)
	lc.lastRecv.Store(time.Now().UnixNano())
	// Backstop: rescan the journal on every welcomed connection so no
	// recorded command can wait for an unrelated future command.
	signal(l.cmdKick)

	silence := l.cfg.SilenceTimeout
	if silence <= 0 {
		silence = 45 * time.Second
		if f.Welcome.HeartbeatSeconds > 0 {
			silence = 3 * time.Duration(f.Welcome.HeartbeatSeconds) * time.Second
		}
	}

	start := time.Now()
	now := l.cfg.Clock.Now()
	l.mu.Lock()
	l.cur = lc
	l.mu.Unlock()
	l.setStatus(func(s *LinkStatus) {
		s.State = LinkConnected
		s.ConnectedSince = &now
		s.LastError = nil
		s.NextRetry = 0
	})
	l.log.Info("connected to hub", "node", l.cfg.NodeID, "hub", f.Welcome.HubID, "committed_report_seq", f.Welcome.CommittedReportSeq)
	if behind > 0 {
		l.log.Warn("hub is missing reports it acknowledged before (restored or replaced hub); sending a run snapshot",
			"node", l.cfg.NodeID, "hub_committed", f.Welcome.CommittedReportSeq, "node_acked", behind)
		if cb := l.cfg.OnHubBehind; cb != nil {
			go cb(cctx, f.Welcome.CommittedReportSeq, behind)
		}
	}
	defer func() {
		l.mu.Lock()
		if l.cur == lc {
			l.cur = nil
		}
		l.mu.Unlock()
	}()

	var wg sync.WaitGroup
	sendErr := make(chan error, 1)
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := l.sendLoop(lc, from); err != nil {
			sendErr <- err
		}
		cancel()
	}()
	go func() {
		defer wg.Done()
		l.watchdog(lc, silence)
	}()
	readErr := l.readLoop(lc)
	cancel()
	_ = ws.CloseNow()
	wg.Wait()
	up := time.Since(start)
	// readLoop never returns nil: when the send loop failed first, its
	// cancel() makes readLoop report a generic "connection closed", so the
	// send error is the real cause.
	select {
	case err := <-sendErr:
		readErr = err
	default:
	}
	e := contracts.AsError(readErr)
	return up, isRejection(e), readErr
}

func httpError(resp *http.Response) *contracts.Error {
	var body contracts.ErrorBody
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if json.Unmarshal(b, &body) == nil && body.Error != nil {
		return body.Error
	}
	code := contracts.ErrUnavailable
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		code = contracts.ErrUnauthorized
	case http.StatusForbidden:
		code = contracts.ErrForbidden
	case http.StatusUpgradeRequired:
		code = contracts.ErrProtocolMismatch
	}
	return contracts.Errorf(code, "hub returned HTTP %d", resp.StatusCode)
}

// applyWelcome prunes reports the hub has committed and returns the seq to
// resend after. behind is the node's acknowledged watermark when the hub's
// committed watermark regressed below it (the hub lost committed reports),
// else 0.
func (l *NodeLink) applyWelcome(ctx context.Context, w contracts.NodeWelcome) (from, behind uint64, err error) {
	committed := w.CommittedReportSeq
	err = sqlitex.Tx(ctx, l.cfg.DB, func(tx *sql.Tx) error {
		seq, err := stateGet(ctx, tx, "report_seq")
		if err != nil {
			return err
		}
		acked, err := stateGet(ctx, tx, "acked_seq")
		if err != nil {
			return err
		}
		if committed > seq {
			// The hub has committed seqs this node never assigned: local state
			// was reset while keeping the identity. Renumber pending reports
			// above the hub's watermark so they are not mistaken for duplicates.
			shift := committed - acked
			l.log.Warn("hub committed more reports than this node assigned; renumbering outbox", "node", l.cfg.NodeID, "hub_committed", committed, "local_seq", seq)
			if _, err := tx.ExecContext(ctx, `UPDATE nodelink_outbox SET origin_seq = -origin_seq`); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE nodelink_outbox SET origin_seq = -origin_seq + ?`, int64(shift)); err != nil {
				return err
			}
			if err := stateSet(ctx, tx, "report_seq", seq+shift); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE nodelink_commands SET result_seq = result_seq + ? WHERE result_seq > ?`, int64(shift), int64(acked)); err != nil {
				return err
			}
			from = committed
			return stateSet(ctx, tx, "acked_seq", committed)
		}
		if committed < acked {
			behind = acked
		}
		if committed > acked {
			if _, err := tx.ExecContext(ctx, `DELETE FROM nodelink_outbox WHERE origin_seq <= ?`, int64(committed)); err != nil {
				return err
			}
			if err := stateSet(ctx, tx, "acked_seq", committed); err != nil {
				return err
			}
		}
		from = committed
		return nil
	})
	return from, behind, err
}

func (l *NodeLink) pruneAcked(ctx context.Context, upTo uint64) error {
	return sqlitex.Tx(ctx, l.cfg.DB, func(tx *sql.Tx) error {
		acked, err := stateGet(ctx, tx, "acked_seq")
		if err != nil {
			return err
		}
		if upTo <= acked {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM nodelink_outbox WHERE origin_seq <= ?`, int64(upTo)); err != nil {
			return err
		}
		return stateSet(ctx, tx, "acked_seq", upTo)
	})
}

type outboxRow struct {
	seq     uint64
	kind    string
	at      string
	payload []byte
}

func (l *NodeLink) sendLoop(lc *linkConn, from uint64) error {
	next := from
	window := uint64(l.cfg.SendWindow)
	for {
		if lc.ctx.Err() != nil {
			return nil
		}
		if acked := lc.acked.Load(); next > acked && next-acked >= window {
			select {
			case <-lc.ackCh:
			case <-lc.ctx.Done():
				return nil
			}
			continue
		}
		limit := 64
		if room := int(window - (next - min(next, lc.acked.Load()))); room < limit {
			limit = max(room, 1)
		}
		rows, err := l.loadOutbox(lc.ctx, next, limit)
		if err != nil {
			if lc.ctx.Err() != nil {
				return nil
			}
			return err
		}
		if len(rows) == 0 {
			select {
			case <-l.kick:
			case <-lc.ackCh:
			case <-lc.ctx.Done():
				return nil
			}
			continue
		}
		for _, r := range rows {
			at, _ := sqlitex.ParseTime(r.at)
			if err := lc.write(contracts.NodeFrame{Type: contracts.FrameReport, Report: &contracts.NodeReport{
				OriginSeq: r.seq, Kind: contracts.ReportKind(r.kind), At: at, Payload: r.payload,
			}}); err != nil {
				return nil // read loop observes the broken connection
			}
			next = r.seq
		}
	}
}

func (l *NodeLink) loadOutbox(ctx context.Context, after uint64, limit int) ([]outboxRow, error) {
	rs, err := l.cfg.DB.QueryContext(ctx, `SELECT origin_seq, kind, at, payload FROM nodelink_outbox WHERE origin_seq > ? ORDER BY origin_seq LIMIT ?`, int64(after), limit)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []outboxRow
	for rs.Next() {
		var r outboxRow
		var seq int64
		if err := rs.Scan(&seq, &r.kind, &r.at, &r.payload); err != nil {
			return nil, err
		}
		r.seq = uint64(seq)
		out = append(out, r)
	}
	return out, rs.Err()
}

func (l *NodeLink) watchdog(lc *linkConn, silence time.Duration) {
	tick := silence / 10 // detect within ~1.1x the silence timeout
	if tick <= 0 {
		tick = time.Second
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-lc.ctx.Done():
			return
		case <-t.C:
			if time.Since(time.Unix(0, lc.lastRecv.Load())) > silence {
				l.log.Warn("hub silent; reconnecting", "node", l.cfg.NodeID, "silence", silence)
				lc.cancel()
				_ = lc.ws.CloseNow()
				return
			}
		}
	}
}

func (l *NodeLink) readLoop(lc *linkConn) error {
	for {
		f, err := readFrame(lc.ctx, lc.ws)
		if err != nil {
			if lc.ctx.Err() != nil {
				return contracts.Errorf(contracts.ErrUnavailable, "connection closed")
			}
			return contracts.Errorf(contracts.ErrUnavailable, "hub connection lost: %v", err)
		}
		lc.lastRecv.Store(time.Now().UnixNano())
		switch f.Type {
		case contracts.FrameCommand:
			if f.Command == nil {
				continue
			}
			if err := l.receiveCommand(lc, *f.Command); err != nil {
				// Not durably recorded: drop the connection so the hub resends.
				return contracts.Errorf(contracts.ErrUnavailable, "could not record command: %v", err)
			}
		case contracts.FrameReportAck:
			if f.ReportAck == nil {
				continue
			}
			upTo := f.ReportAck.CommittedSeq
			if err := l.pruneAcked(context.Background(), upTo); err != nil {
				l.log.Warn("could not prune acknowledged reports", "err", err)
			}
			for {
				cur := lc.acked.Load()
				if upTo <= cur || lc.acked.CompareAndSwap(cur, upTo) {
					break
				}
			}
			signal(lc.ackCh)
		case contracts.FramePing:
			if err := lc.write(contracts.NodeFrame{Type: contracts.FramePong, At: l.cfg.Clock.Now()}); err != nil {
				return contracts.Errorf(contracts.ErrUnavailable, "write pong: %v", err)
			}
		case contracts.FramePong, contracts.FrameWelcome:
		case contracts.FrameError:
			if f.Error == nil {
				return contracts.Errorf(contracts.ErrUnavailable, "hub closed the connection")
			}
			return f.Error
		}
	}
}

// ---- commands ----

func (l *NodeLink) receiveCommand(lc *linkConn, cmd contracts.NodeCommand) error {
	if cmd.CommandID == "" {
		l.log.Warn("ignoring command without command_id", "kind", cmd.Kind, "seq", cmd.Seq)
		return nil
	}
	ctx := context.Background()
	jcmd, secret := journalCopy(cmd)
	raw, err := json.Marshal(jcmd)
	if err != nil {
		return err
	}
	if secret {
		// Held before the journal row exists so the worker always finds it;
		// a resend refreshes it (the hub mints a new token per send).
		l.secretCmds.Store(cmd.CommandID, cmd)
	}
	var dup bool
	var state string
	var resultSeq sql.NullInt64
	var result []byte
	err = sqlitex.Tx(ctx, l.cfg.DB, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT state, result_seq, result FROM nodelink_commands WHERE command_id=?`, cmd.CommandID).Scan(&state, &resultSeq, &result)
		if err == nil {
			dup = true
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO nodelink_commands(command_id, seq, kind, command, state, received_at) VALUES(?,?,?,?,?,?)`,
			cmd.CommandID, cmd.Seq, string(cmd.Kind), raw, cmdRecorded, sqlitex.FormatTime(l.cfg.Clock.Now())); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE nodelink_state SET value = MAX(value, ?) WHERE key='command_seq'`, cmd.Seq); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	// Execution depends only on the durable record, never on the ack reaching
	// the hub: wake the worker for every recorded command (new or a
	// duplicate still waiting) before writing the ack. Once recorded, the
	// hub will not resend it (LastCommandSeq covers it), so a missed wake-up
	// would strand the command until the node restarts.
	if !dup || state == cmdRecorded {
		signal(l.cmdKick)
	}
	// Ack only after the durable record. A failed write means the
	// connection is broken; the read loop notices, and the hub resends
	// unacknowledged commands it still considers pending.
	_ = lc.write(contracts.NodeFrame{Type: contracts.FrameCommandAck, CommandAck: &contracts.CommandAck{CommandID: cmd.CommandID, Seq: cmd.Seq}, At: l.cfg.Clock.Now()})
	if !dup {
		return nil
	}
	if state != cmdRecorded {
		l.secretCmds.Delete(cmd.CommandID)
	}
	l.log.Info("duplicate command ignored (not re-executed)", "command", cmd.CommandID, "state", state)
	if state == cmdDone && len(result) > 0 {
		acked, err := stateGet(ctx, l.cfg.DB, "acked_seq")
		if err != nil {
			return err
		}
		if !resultSeq.Valid || uint64(resultSeq.Int64) <= acked {
			// The stored result was already acknowledged once; return it
			// again so a hub that lost it can complete the command.
			if _, err := l.Enqueue(ctx, contracts.ReportCommandResult, json.RawMessage(result)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (l *NodeLink) commandWorker(ctx context.Context) {
	signal(l.cmdKick)
	for {
		select {
		case <-ctx.Done():
			return
		case <-l.cmdKick:
		}
		for ctx.Err() == nil {
			cmd, ok, err := l.nextRecorded(ctx)
			if err != nil {
				if ctx.Err() == nil {
					l.log.Warn("command journal read failed", "err", err)
				}
				break
			}
			if !ok {
				break
			}
			l.execute(ctx, cmd)
		}
		l.maybePruneCommands(ctx)
	}
}

func (l *NodeLink) nextRecorded(ctx context.Context) (contracts.NodeCommand, bool, error) {
	var raw []byte
	err := l.cfg.DB.QueryRowContext(ctx, `SELECT command FROM nodelink_commands WHERE state=? ORDER BY seq, received_at LIMIT 1`, cmdRecorded).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return contracts.NodeCommand{}, false, nil
	}
	if err != nil {
		return contracts.NodeCommand{}, false, err
	}
	var cmd contracts.NodeCommand
	if err := json.Unmarshal(raw, &cmd); err != nil {
		return contracts.NodeCommand{}, false, err
	}
	return cmd, true, nil
}

func (l *NodeLink) execute(ctx context.Context, cmd contracts.NodeCommand) {
	bg := context.Background()
	if secretsWithheld(cmd) {
		full, ok := l.secretCmds.Load(cmd.CommandID)
		if !ok {
			// Recorded by a previous process: its secret was never
			// persisted. Do not run it without its credential.
			res := contracts.CommandResult{Outcome: contracts.OutcomeFailed,
				Error: contracts.Errorf(contracts.ErrUnavailable, "the node restarted before starting this command and does not persist its session credential; not executed, retry the operation")}
			if err := l.finish(bg, cmd, cmdRecorded, res); err != nil {
				l.log.Warn("could not record command without secret", "command", cmd.CommandID, "err", err)
			}
			return
		}
		cmd = full.(contracts.NodeCommand)
	}
	defer l.secretCmds.Delete(cmd.CommandID)
	// ExpiresAt is on the hub clock: compare it with the conservative hub
	// time, never the node clock (a skewed node would refuse valid commands
	// or start expired ones).
	if hubNow := l.HubNow(); cmd.ExpiresAt != nil && !hubNow.Before(*cmd.ExpiresAt) {
		res := contracts.CommandResult{Outcome: contracts.OutcomeFailed,
			Error: contracts.Errorf(contracts.ErrExpired, "command expired at %s (hub clock) before the node could start it; not executed", cmd.ExpiresAt.UTC().Format(time.RFC3339)).
				WithDetail("hub_now_estimate", hubNow.UTC().Format(time.RFC3339Nano))}
		if err := l.finish(bg, cmd, cmdRecorded, res); err != nil {
			l.log.Warn("could not record expired command", "command", cmd.CommandID, "err", err)
		}
		return
	}
	res, err := l.cfg.DB.ExecContext(bg, `UPDATE nodelink_commands SET state=? WHERE command_id=? AND state=?`, cmdExecuting, cmd.CommandID, cmdRecorded)
	if err != nil {
		l.log.Warn("could not mark command executing", "command", cmd.CommandID, "err", err)
		return
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return
	}
	result := l.invoke(ctx, cmd)
	if err := l.finish(bg, cmd, cmdExecuting, result); err != nil {
		l.log.Error("could not record command result", "command", cmd.CommandID, "err", err)
	}
}

func (l *NodeLink) invoke(ctx context.Context, cmd contracts.NodeCommand) (res contracts.CommandResult) {
	defer func() {
		if r := recover(); r != nil {
			l.log.Error("command handler panicked", "command", cmd.CommandID, "panic", fmt.Sprint(r))
			res = contracts.CommandResult{Outcome: contracts.OutcomeIndeterminate,
				Error: contracts.Errorf(contracts.ErrOutcomeIndeterminate, "command handler crashed; outcome unknown")}
		}
	}()
	r, err := l.cfg.Handler.HandleCommand(ctx, cmd)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return contracts.CommandResult{Outcome: contracts.OutcomeIndeterminate,
				Error: contracts.Errorf(contracts.ErrOutcomeIndeterminate, "command interrupted (node shutting down); outcome unknown")}
		}
		return contracts.CommandResult{Outcome: contracts.OutcomeFailed, Error: contracts.AsError(err)}
	}
	if r.Outcome == "" {
		r.Outcome = contracts.OutcomeSucceeded
	}
	return r
}

// finish stores the result and enqueues it as a durable command_result
// report in one transaction.
func (l *NodeLink) finish(ctx context.Context, cmd contracts.NodeCommand, from string, res contracts.CommandResult) error {
	res.CommandID = cmd.CommandID
	res.Kind = cmd.Kind
	if res.At.IsZero() {
		res.At = l.cfg.Clock.Now()
	}
	raw, err := json.Marshal(res)
	if err != nil {
		return err
	}
	err = sqlitex.Tx(ctx, l.cfg.DB, func(tx *sql.Tx) error {
		seq, err := l.EnqueueTx(ctx, tx, contracts.ReportCommandResult, res.At, json.RawMessage(raw))
		if err != nil {
			return err
		}
		r, err := tx.ExecContext(ctx, `UPDATE nodelink_commands SET state=?, result=?, result_seq=?, finished_at=? WHERE command_id=? AND state=?`,
			cmdDone, raw, int64(seq), sqlitex.FormatTime(l.cfg.Clock.Now()), cmd.CommandID, from)
		if err != nil {
			return err
		}
		if n, _ := r.RowsAffected(); n != 1 {
			return fmt.Errorf("command %s not in state %s", cmd.CommandID, from)
		}
		return nil
	})
	if err == nil {
		l.Kick()
	}
	return err
}

// recoverCommands runs at startup: commands that were executing when the
// node stopped have an unknown outcome and are reported indeterminate (never
// re-executed); old finished records are pruned.
func (l *NodeLink) recoverCommands(ctx context.Context) error {
	rs, err := l.cfg.DB.QueryContext(ctx, `SELECT command FROM nodelink_commands WHERE state=?`, cmdExecuting)
	if err != nil {
		return err
	}
	var stuck []contracts.NodeCommand
	for rs.Next() {
		var raw []byte
		if err := rs.Scan(&raw); err != nil {
			rs.Close()
			return err
		}
		var cmd contracts.NodeCommand
		if json.Unmarshal(raw, &cmd) == nil {
			stuck = append(stuck, cmd)
		}
	}
	rs.Close()
	for _, cmd := range stuck {
		res := contracts.CommandResult{Outcome: contracts.OutcomeIndeterminate,
			Error: contracts.Errorf(contracts.ErrOutcomeIndeterminate, "node stopped while the command was executing; outcome unknown and the command was not re-executed")}
		if err := l.finish(ctx, cmd, cmdExecuting, res); err != nil {
			return err
		}
		l.log.Warn("command outcome indeterminate after restart", "command", cmd.CommandID, "kind", cmd.Kind)
	}
	return l.pruneFinishedCommands(ctx)
}

// commandPruneInterval is how often a running link prunes finished command
// records (besides at startup).
const commandPruneInterval = time.Hour

// pruneFinishedCommands deletes finished command records older than
// CommandRetention whose results the hub acknowledged.
func (l *NodeLink) pruneFinishedCommands(ctx context.Context) error {
	now := l.cfg.Clock.Now()
	l.pruneMu.Lock()
	l.lastPrune = now
	l.pruneMu.Unlock()
	cutoff := sqlitex.FormatTime(now.Add(-l.cfg.CommandRetention))
	_, err := l.cfg.DB.ExecContext(ctx, `DELETE FROM nodelink_commands WHERE state=? AND finished_at < ? AND result_seq <= (SELECT value FROM nodelink_state WHERE key='acked_seq')`, cmdDone, cutoff)
	return err
}

// maybePruneCommands prunes at most once per commandPruneInterval. Before
// the A20 soak, finished records were pruned only at startup, so a
// long-running pyred kept one row per command (about one per delivered
// message) until it restarted.
func (l *NodeLink) maybePruneCommands(ctx context.Context) {
	l.pruneMu.Lock()
	due := l.cfg.Clock.Now().Sub(l.lastPrune) >= commandPruneInterval
	l.pruneMu.Unlock()
	if !due {
		return
	}
	if err := l.pruneFinishedCommands(ctx); err != nil && ctx.Err() == nil {
		l.log.Warn("command journal prune failed", "err", err)
	}
}
