// Package web is `pyre web serve`: the owner's web inbox and project pages
// (W-web-inbox). It serves a static single-page app (static/, embedded)
// and a small JSON API under /api built from the hub with the owner's
// client credentials, plus Server-Sent Events fed by one shared hub event
// subscription. It listens on loopback only; ngrok (Google OAuth, email
// domain check) puts it on the internet and names the signed-in user in
// X-Pyre-Email. docs/web.md describes install, auth and the API.
package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

// DefaultListen is where `pyre web serve` listens (loopback only).
const DefaultListen = "127.0.0.1:7390"

// DefaultOrigin is the public URL ngrok serves the app at.
const DefaultOrigin = "https://pyre-example.ngrok.app"

// EmailDomain is the only domain allowed in (the ngrok policy enforces
// the same rule before a request reaches the server).
const EmailDomain = "example.org"

// HeaderEmail is the header the ngrok policy sets from the OAuth identity.
const HeaderEmail = "X-Pyre-Email"

// Options configures a Server.
type Options struct {
	// API is the hub, used with the owner's credentials.
	API c.API
	// Listen is the loopback address (default DefaultListen).
	Listen string
	// Dev lets loopback requests without X-Pyre-Email in (local testing
	// only; never run --dev behind the tunnel).
	Dev bool
	// StateDir holds the server's own small state (read marks). Empty
	// keeps it in memory.
	StateDir string
	// Origins are the extra origins accepted on POSTs besides the request's
	// own host (default DefaultOrigin).
	Origins []string
	// Static is the app's files (default: the embedded static/).
	Static fs.FS
	// Now is the clock (default time.Now).
	Now func() time.Time
	// Heartbeat is the SSE keep-alive interval (default 20s).
	Heartbeat time.Duration
	// Debounce coalesces hub events before they reach browsers (default 250ms).
	Debounce time.Duration
	// Logf logs operational problems (default log.Printf).
	Logf func(format string, args ...any)
}

// Server is the web app. Create with New, then Run (or use Handler).
type Server struct {
	opts  Options
	api   c.API
	now   func() time.Time
	logf  func(string, ...any)
	read  *readState
	mail  mailIndex
	bus   *broker
	mux   *http.ServeMux
	start sync.Once
	live  atomic.Bool // the hub event stream is delivering

	sessions  cached[[]c.SessionView]
	projects  cached[[]c.Project]
	asks      cached[[]c.Ask]
	attention cached[[]c.AttentionItem]
	held      cached[c.HeldAnswers]
	pagesMu   sync.Mutex
	pages     map[string]*cached[pageInfo]
	knownMu   sync.Mutex
	known     map[string]c.SessionView // sessions outside the active list (archived), by ID
}

// New builds a Server.
func New(opts Options) (*Server, error) {
	if opts.API == nil {
		return nil, errors.New("web: no hub API")
	}
	if opts.Listen == "" {
		opts.Listen = DefaultListen
	}
	if opts.Origins == nil {
		opts.Origins = []string{DefaultOrigin}
	}
	if opts.Static == nil {
		opts.Static = staticFS()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Heartbeat <= 0 {
		opts.Heartbeat = 20 * time.Second
	}
	if opts.Debounce <= 0 {
		opts.Debounce = 250 * time.Millisecond
	}
	if opts.Logf == nil {
		opts.Logf = log.Printf
	}
	rs, err := loadReadState(opts.StateDir)
	if err != nil {
		return nil, fmt.Errorf("web: state dir: %w", err)
	}
	s := &Server{opts: opts, api: opts.API, now: opts.Now, logf: opts.Logf, read: rs,
		pages: map[string]*cached[pageInfo]{}, known: map[string]c.SessionView{}}
	// Events keep these current; maxAge bounds staleness if the stream is down.
	s.sessions.maxAge = time.Minute
	s.projects.maxAge = 5 * time.Minute
	s.asks.maxAge = time.Minute
	s.attention.maxAge = time.Minute
	s.held.maxAge = time.Minute
	s.bus = newBroker(opts.Debounce)
	s.routes()
	return s, nil
}

// CheckListen refuses a listen address that is not loopback: the app is
// reached through the tunnel, never directly.
func CheckListen(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return c.Errorf(c.ErrInvalid, "--listen %q: %v", addr, err)
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return c.Errorf(c.ErrInvalid, "--listen %q: the web app listens on loopback only (127.0.0.1 or ::1); ngrok exposes it", addr)
	}
	return nil
}

// Run serves until ctx ends: it starts the hub event subscription and the
// HTTP listener.
func (s *Server) Run(ctx context.Context) error {
	if err := CheckListen(s.opts.Listen); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", s.opts.Listen)
	if err != nil {
		return err
	}
	return s.Serve(ctx, ln)
}

// Serve is Run on an existing listener.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.Start(ctx)
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer scancel()
		_ = srv.Shutdown(sctx)
		return nil
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// Start begins the shared hub event subscription (once).
func (s *Server) Start(ctx context.Context) {
	s.start.Do(func() { go s.watch(ctx) })
}

// Handler is the whole app with auth and security headers.
func (s *Server) Handler() http.Handler { return s.secure(s.mux) }

func (s *Server) routes() {
	m := http.NewServeMux()
	m.HandleFunc("GET /api/me", s.handleMe)
	m.HandleFunc("GET /api/inbox", s.handleInbox)
	m.HandleFunc("GET /api/threads/{thread_id}", s.handleThread)
	m.HandleFunc("POST /api/threads/{thread_id}/reply", s.handleReply)
	m.HandleFunc("POST /api/threads/{thread_id}/read", s.handleRead)
	m.HandleFunc("POST /api/threads/{thread_id}/archive", s.handleArchive)
	m.HandleFunc("POST /api/inbox/archive", s.handleArchiveMany)
	m.HandleFunc("POST /api/asks/{ask_id}/answer", s.handleAnswer)
	m.HandleFunc("POST /api/held/send", s.handleHeldSend)
	m.HandleFunc("POST /api/held/{ask_id}/undo", s.handleHeldUndo)
	m.HandleFunc("GET /api/projects", s.handleProjects)
	m.HandleFunc("GET /api/projects/{project_id}", s.handleProject)
	m.HandleFunc("GET /api/artifacts/{artifact_id}/content", s.handleArtifactContent)
	m.HandleFunc("GET /api/events", s.handleEvents)
	m.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, c.Errorf(c.ErrNotFound, "no such API endpoint: %s %s", r.Method, r.URL.Path))
	})
	m.Handle("/", s.staticHandler())
	s.mux = m
}

// ---- JSON helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeErr writes {"error":{"code","message"}} with the code's status.
func writeErr(w http.ResponseWriter, err error) {
	e := c.AsError(err)
	if errors.Is(err, context.DeadlineExceeded) {
		e = c.Errorf(c.ErrUnavailable, "The hub did not answer in time. Try again.")
	}
	status := e.Code.HTTPStatus()
	writeJSON(w, status, c.ErrorBody{Error: &c.Error{Code: e.Code, Message: plainError(e), Retryable: e.Retryable}})
}

// plainError is the message of a hub error without Go-isms.
func plainError(e *c.Error) string {
	msg := strings.TrimSpace(e.Message)
	if msg == "" {
		msg = strings.ReplaceAll(string(e.Code), "_", " ")
	}
	return strings.ToUpper(msg[:1]) + msg[1:]
}

// maxBody bounds a request body.
const maxBody = 256 << 10

// decodeBody reads a JSON request body into v.
func decodeBody(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBody))
	if err := dec.Decode(v); err != nil {
		var mb *http.MaxBytesError
		if errors.As(err, &mb) {
			return c.Errorf(c.ErrPayloadTooLarge, "The request is too large (limit %d KB).", maxBody>>10)
		}
		if errors.Is(err, io.EOF) {
			return nil // an empty body is an empty object
		}
		return c.Errorf(c.ErrInvalid, "The request body is not valid JSON: %v", err)
	}
	return nil
}

// opID is the request's operation ID (stable across a browser retry when
// the app sends one), else a fresh one.
func opID(given string) string {
	if given = strings.TrimSpace(given); given != "" {
		if len(given) > 120 {
			given = given[:120]
		}
		return "op_web-" + given
	}
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "op_web-" + hex.EncodeToString(b[:])
}

// hubCtx bounds one page load's hub calls; it outlives a closed browser
// request so a shared fetch other readers wait on is not cancelled.
func hubCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
}
