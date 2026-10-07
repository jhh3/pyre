// Package httpapi binds a contracts.API implementation to the v1 HTTP
// protocol described in internal/contracts/routes.go:
//
//   - POST /v1/<op> JSON RPC for every operation (strict JSON decoding)
//   - PUT  /v1/uploads/{upload_id}?offset=<n> raw upload chunks
//   - GET  /v1/content/{version_id} immutable artifact bytes with Range
//   - GET  /v1/events/stream WebSocket of Event frames
//
// Other endpoints (node control channel, web terminal, preview, one-time
// exchanges, health) are mounted from Options.Extra by their owners.
//
// The server is generic over contracts.API: the real hub, the deterministic
// mock, and test fakes are all served the same way.
package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Default limits.
const (
	DefaultMaxBodyBytes  = 4 << 20 // JSON request bodies
	DefaultMaxChunkBytes = 8 << 20 // one upload chunk
	DefaultStreamWait    = 25 * time.Second
	DefaultStreamPing    = 30 * time.Second
	// DefaultStreamWriteTimeout bounds one event frame write: a subscriber
	// that stops reading for this long is disconnected (it reconnects from
	// its cursor), so a slow consumer never holds server memory or blocks
	// other streams.
	DefaultStreamWriteTimeout = 10 * time.Second
	// DefaultMaxStreams bounds concurrently open event streams.
	DefaultMaxStreams = 256
)

// Options configures NewServer. The zero value is usable.
type Options struct {
	// Extra mounts additional handlers keyed by a net/http ServeMux pattern,
	// normally one of the contracts.Path* constants (PathNodeConnect,
	// PathTerminal, PathPreview, PathEnrollExchange, PathPairExchange,
	// PathHealth). Handlers for PathNodeConnect (and any non-public path)
	// run after bearer authentication with the Principal in the request
	// context. PathHealth, PathEnrollExchange, PathPairExchange, PathPreview
	// and PathTerminal are public: they authenticate by their own one-time
	// secret/ticket/signed token and receive no Principal.
	Extra map[string]http.Handler

	// MaxBodyBytes bounds JSON request bodies (default DefaultMaxBodyBytes).
	MaxBodyBytes int64
	// MaxChunkBytes bounds a single upload chunk (default DefaultMaxChunkBytes).
	MaxChunkBytes int64

	// Logger receives one access-log line per request (method, redacted
	// path, op, status, bytes, duration, principal) and server-side details
	// of internal errors. It never receives Authorization headers, tickets,
	// preview tokens, or request/response bodies. Nil disables logging.
	Logger *slog.Logger

	// StreamWait is the ListEvents long-poll used by the event stream
	// (default 25s, max 60s). StreamPing is the WebSocket keepalive interval.
	StreamWait time.Duration
	StreamPing time.Duration
	// StreamWriteTimeout bounds one frame write (default
	// DefaultStreamWriteTimeout). MaxStreams bounds concurrently open
	// streams (default DefaultMaxStreams); more are refused with
	// capacity_exhausted. Each stream holds at most one ListEvents batch
	// (streamBatch events) in memory whatever its consumer's speed.
	StreamWriteTimeout time.Duration
	MaxStreams         int

	// AllowedOrigins lists host patterns allowed to open WebSockets from a
	// browser (see coder/websocket OriginPatterns). Empty = same host only;
	// non-browser clients that send no Origin are always allowed.
	AllowedOrigins []string
}

func (o *Options) defaults() {
	if o.MaxBodyBytes <= 0 {
		o.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if o.MaxChunkBytes <= 0 {
		o.MaxChunkBytes = DefaultMaxChunkBytes
	}
	if o.StreamWait <= 0 {
		o.StreamWait = DefaultStreamWait
	}
	if o.StreamWait > 60*time.Second {
		o.StreamWait = 60 * time.Second
	}
	if o.StreamPing <= 0 {
		o.StreamPing = DefaultStreamPing
	}
	if o.StreamWriteTimeout <= 0 {
		o.StreamWriteTimeout = DefaultStreamWriteTimeout
	}
	if o.MaxStreams <= 0 {
		o.MaxStreams = DefaultMaxStreams
	}
}

// publicPaths authenticate by their own means and never see a bearer token.
var publicPaths = map[string]bool{
	contracts.PathHealth:         true,
	contracts.PathEnrollExchange: true,
	contracts.PathPairExchange:   true,
	contracts.PathPreview:        true,
	contracts.PathTerminal:       true,
}

// exchangePaths redeem a one-time secret. They check the protocol header
// before the handler runs: an incompatible client (an old app against an
// upgraded hub, say) is refused with protocol_mismatch without consuming
// the code or minting a credential it could not use.
var exchangePaths = map[string]bool{
	contracts.PathEnrollExchange: true,
	contracts.PathPairExchange:   true,
}

func protocolChecked(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := CheckProtocol(r.Header.Get(contracts.ProtocolHeader)); err != nil {
			WriteError(w, err)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type server struct {
	api  contracts.API
	auth contracts.Authenticator
	opts Options
	ops  map[string]rpcFunc

	stopOnce sync.Once
	stop     chan struct{}  // closed by CloseStreams
	streams  sync.WaitGroup // live event streams
	open     atomic.Int64   // open event streams (MaxStreams)
}

// OpenStreams returns the number of event streams currently open.
func (sv *Server) OpenStreams() int { return int(sv.s.open.Load()) }

// Server is the HTTP binding plus shutdown control for its long-lived
// event streams (http.Server.Shutdown does not track hijacked WebSockets).
type Server struct {
	http.Handler
	s *server
}

// CloseStreams ends every open event stream (close code 1001, going away:
// clients reconnect) and refuses new ones, then waits until they have
// returned or ctx ends. Call it before closing the API's storage.
func (sv *Server) CloseStreams(ctx context.Context) error {
	sv.s.stopOnce.Do(func() { close(sv.s.stop) })
	done := make(chan struct{})
	go func() { sv.s.streams.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// NewServer returns the HTTP handler for api. auth resolves bearer tokens;
// it must not be nil.
func NewServer(api contracts.API, auth contracts.Authenticator, opts Options) http.Handler {
	return New(api, auth, opts)
}

// New is NewServer returning the *Server (with CloseStreams).
func New(api contracts.API, auth contracts.Authenticator, opts Options) *Server {
	if api == nil || auth == nil {
		panic("httpapi: NewServer requires api and auth")
	}
	opts.defaults()
	s := &server{api: api, auth: auth, opts: opts, ops: rpcTable(api), stop: make(chan struct{})}
	mux := http.NewServeMux()
	for op := range s.ops {
		op := op
		mux.Handle("POST "+contracts.RPCPath(op), s.authed(op, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s.serveRPC(w, r, op)
		})))
	}
	mux.Handle("PUT "+contracts.PathUploadChunk, s.authed("upload", http.HandlerFunc(s.serveUpload)))
	mux.Handle("GET "+contracts.PathContent, s.authed("content", http.HandlerFunc(s.serveContent)))
	mux.Handle("GET "+contracts.PathEventStream, s.authed("events.stream", http.HandlerFunc(s.serveEventStream)))
	hasHealth := false
	for pattern, h := range opts.Extra {
		path := patternPath(pattern)
		if path == contracts.PathHealth {
			hasHealth = true
		}
		if exchangePaths[path] {
			mux.Handle(pattern, protocolChecked(h))
		} else if publicPaths[path] {
			mux.Handle(pattern, h)
		} else {
			mux.Handle(pattern, s.authed(path, h))
		}
	}
	if !hasHealth {
		mux.HandleFunc("GET "+contracts.PathHealth, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		})
	}
	mux.HandleFunc("/", s.notFound)
	return &Server{Handler: s.wrap(mux), s: s}
}

// patternPath strips an optional "METHOD " prefix and host from a ServeMux pattern.
func patternPath(pattern string) string {
	if i := strings.IndexByte(pattern, ' '); i >= 0 {
		pattern = strings.TrimSpace(pattern[i+1:])
	}
	if i := strings.IndexByte(pattern, '/'); i > 0 {
		pattern = pattern[i:]
	}
	return pattern
}

func (s *server) notFound(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/v1/") {
		op := strings.TrimPrefix(r.URL.Path, "/v1/")
		if _, ok := s.ops[op]; ok {
			w.Header().Set("Allow", http.MethodPost)
			writeErrorStatus(w, http.StatusMethodNotAllowed, contracts.Errorf(contracts.ErrInvalid, "%s requires POST", r.URL.Path))
			return
		}
	}
	WriteError(w, contracts.Errorf(contracts.ErrNotFound, "no such endpoint"))
}

// ---- middleware ----

type ctxKey int

const logInfoKey ctxKey = 1

// logInfo is filled in by inner handlers for the access log.
type logInfo struct {
	op        string
	principal string
	internal  error
	cancelled error // the client cancelled or disconnected; not a server fault
}

func (s *server) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		info := &logInfo{}
		rec := &statusRecorder{ResponseWriter: w}
		r = r.WithContext(context.WithValue(r.Context(), logInfoKey, info))
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				info.internal = fmt.Errorf("panic: %v\n%s", v, debug.Stack())
				if !rec.wroteHeader {
					WriteError(rec, contracts.Errorf(contracts.ErrInternal, "internal error"))
				}
			}
			s.accessLog(r, rec, info, time.Since(start))
		}()
		rec.Header().Set(contracts.ProtocolHeader, strconv.Itoa(contracts.ProtocolVersion))
		next.ServeHTTP(rec, r)
	})
}

func (s *server) accessLog(r *http.Request, rec *statusRecorder, info *logInfo, d time.Duration) {
	if s.opts.Logger == nil {
		return
	}
	status := rec.status
	if status == 0 {
		status = http.StatusOK
	}
	attrs := []any{
		"method", r.Method,
		"path", RedactPath(r.URL.Path),
		"status", status,
		"bytes", rec.bytes,
		"dur_ms", d.Milliseconds(),
	}
	if info.op != "" {
		attrs = append(attrs, "op", info.op)
	}
	if info.principal != "" {
		attrs = append(attrs, "principal", info.principal)
	}
	s.opts.Logger.Info("http", attrs...)
	if info.internal != nil {
		s.opts.Logger.Error("internal error", "path", RedactPath(r.URL.Path), "op", info.op, "err", info.internal.Error())
	} else if info.cancelled != nil {
		s.opts.Logger.Info("client cancelled request", "path", RedactPath(r.URL.Path), "op", info.op, "err", info.cancelled.Error())
	}
}

// RedactPath removes secrets that some endpoints carry in the path (preview
// tokens) so the path is safe to log.
func RedactPath(p string) string {
	if strings.HasPrefix(p, "/preview/") {
		parts := strings.SplitN(strings.TrimPrefix(p, "/preview/"), "/", 2)
		if len(parts) == 2 {
			return "/preview/" + parts[0] + "/[redacted]"
		}
	}
	return p
}

// authed checks the protocol header and bearer token, then runs next with
// the Principal in context.
func (s *server) authed(op string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info, _ := r.Context().Value(logInfoKey).(*logInfo)
		if info != nil {
			info.op = op
		}
		if err := CheckProtocol(r.Header.Get(contracts.ProtocolHeader)); err != nil {
			WriteError(w, err)
			return
		}
		token, ok := bearerToken(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="pyre"`)
			WriteError(w, contracts.Errorf(contracts.ErrUnauthorized, "missing bearer token"))
			return
		}
		p, err := s.auth.Authenticate(r.Context(), token)
		if err != nil {
			ce := contracts.AsError(err)
			if ce.Code == contracts.ErrInternal || ce.Code == contracts.ErrUnavailable {
				if r.Context().Err() != nil {
					// The client went away while it was being
					// authenticated (the store query was cancelled).
					noteCancelled(r, err)
				} else if info != nil {
					info.internal = err
				}
				WriteError(w, contracts.Errorf(contracts.ErrUnavailable, "authentication temporarily unavailable"))
				return
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="pyre", error="invalid_token"`)
			WriteError(w, contracts.Errorf(contracts.ErrUnauthorized, "invalid or revoked credential"))
			return
		}
		if info != nil {
			info.principal = string(p.Kind) + ":" + p.ID
		}
		next.ServeHTTP(w, r.WithContext(contracts.WithPrincipal(r.Context(), p)))
	})
}

func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	t := strings.TrimSpace(h[len(prefix):])
	return t, t != ""
}

// CheckProtocol validates an X-Pyre-Protocol header value. A missing header
// is treated as the current protocol.
func CheckProtocol(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return ProtocolMismatch(-1)
	}
	if n < contracts.MinProtocolVersion || n > contracts.ProtocolVersion {
		return ProtocolMismatch(n)
	}
	return nil
}

// ProtocolMismatch builds the standard protocol_mismatch error.
func ProtocolMismatch(client int) *contracts.Error {
	msg := fmt.Sprintf("client protocol %d not supported; hub supports %d..%d", client, contracts.MinProtocolVersion, contracts.ProtocolVersion)
	if client < 0 {
		msg = fmt.Sprintf("unparseable %s header; hub supports %d..%d", contracts.ProtocolHeader, contracts.MinProtocolVersion, contracts.ProtocolVersion)
	}
	return contracts.Errorf(contracts.ErrProtocolMismatch, "%s", msg).
		WithDetail("min_protocol", contracts.MinProtocolVersion).
		WithDetail("protocol", contracts.ProtocolVersion)
}

// ---- errors ----

// WriteError writes err as an ErrorBody with the status from its code.
// Errors that are not *contracts.Error, and internal errors, are reduced to
// a generic message so implementation details never reach clients.
func WriteError(w http.ResponseWriter, err error) {
	ce := publicError(err)
	writeErrorStatus(w, ce.Code.HTTPStatus(), ce)
}

func publicError(err error) *contracts.Error {
	var ce *contracts.Error
	if !errors.As(err, &ce) || ce.Code == contracts.ErrInternal || ce.Code == "" {
		return &contracts.Error{Code: contracts.ErrInternal, Message: "internal error"}
	}
	return ce
}

func writeErrorStatus(w http.ResponseWriter, status int, ce *contracts.Error) {
	if ce.Code == contracts.ErrUnavailable || ce.Code == contracts.ErrCapacityExhausted {
		if w.Header().Get("Retry-After") == "" {
			w.Header().Set("Retry-After", "1")
		}
	}
	writeJSON(w, status, contracts.ErrorBody{Error: ce})
}

// noteInternal records a non-public error for the server-side log. An
// error caused by the client cancelling or disconnecting is recorded as
// such (logged quietly), not as an internal error.
func noteInternal(r *http.Request, err error) {
	if err == nil {
		return
	}
	if r.Context().Err() != nil || errors.Is(err, context.Canceled) {
		noteCancelled(r, err)
		return
	}
	var ce *contracts.Error
	if errors.As(err, &ce) && ce.Code != contracts.ErrInternal {
		return
	}
	if info, ok := r.Context().Value(logInfoKey).(*logInfo); ok {
		info.internal = err
	}
}

// noteCancelled records that the request failed because the client
// cancelled it or went away.
func noteCancelled(r *http.Request, err error) {
	if info, ok := r.Context().Value(logInfoKey).(*logInfo); ok {
		info.cancelled = err
	}
}

// ---- response recorder ----

type statusRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.status = http.StatusOK
		r.wroteHeader = true
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController and coder/websocket reach Hijacker/Flusher.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
