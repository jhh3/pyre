package hostproc

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/runtime/sessionhost"
	"github.com/jhh3/pyre/internal/runtime/termhost"
	"github.com/jhh3/pyre/internal/runtime/termhost/internal/wire"
)

// Server is the session host process (`<pyred> __pyre_termhost serve`): it
// owns every session of one socket, the way one isolated tmux server does.
// It runs detached from pyred, so runs survive pyred restarts; pyred talks
// to it through Host.
type Server struct {
	Socket string
	// Env is the base environment of every session (default os.Environ(),
	// captured when the host starts, like a tmux server's global
	// environment).
	Env []string
	// Logf receives lifecycle lines (never terminal bytes, input or
	// environment values). Nil discards them.
	Logf func(format string, args ...any)

	start startFunc // tests substitute fake programs

	mu       sync.Mutex
	sessions map[string]*session
	nextPane int
	ln       net.Listener
	closed   bool
	stopped  chan struct{}
}

// ErrAlreadyRunning means another host already serves the socket.
var ErrAlreadyRunning = errors.New("termhost: a host is already serving this socket")

func (srv *Server) logf(format string, args ...any) {
	if srv.Logf != nil {
		srv.Logf(format, args...)
	}
}

// baseEnv is the sessions' base environment (fixed by Listen).
func (srv *Server) baseEnv() []string { return srv.Env }

// Listen binds the socket (removing a stale one). It fails with
// ErrAlreadyRunning when a live host answers on it.
func (srv *Server) Listen() error {
	if err := termhost.ValidateSocketPath(srv.Socket); err != nil {
		return err
	}
	if err := sessionhost.PrepareDir(filepath.Dir(srv.Socket)); err != nil {
		return err
	}
	if _, err := os.Lstat(srv.Socket); err == nil {
		if c, err := net.DialTimeout("unix", srv.Socket, time.Second); err == nil {
			c.Close()
			return ErrAlreadyRunning
		}
		if err := os.Remove(srv.Socket); err != nil {
			return fmt.Errorf("termhost: remove stale socket: %w", err)
		}
	}
	ln, err := net.Listen("unix", srv.Socket)
	if err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		_ = os.Chmod(srv.Socket, 0o600)
	}
	srv.mu.Lock()
	srv.ln = ln
	if srv.sessions == nil {
		srv.sessions = map[string]*session{}
	}
	if srv.start == nil {
		srv.start = startProc
	}
	if srv.Env == nil {
		srv.Env = wire.HostBaseEnv(os.Environ())
	}
	srv.stopped = make(chan struct{})
	srv.mu.Unlock()
	return nil
}

// Serve accepts connections until Shutdown.
func (srv *Server) Serve() error {
	if srv.ln == nil {
		if err := srv.Listen(); err != nil {
			return err
		}
	}
	srv.logf("termhost: serving %s (pid %d, protocol %d)", srv.Socket, os.Getpid(), wire.ProtocolVersion)
	for {
		c, err := srv.ln.Accept()
		if err != nil {
			srv.mu.Lock()
			closed := srv.closed
			srv.mu.Unlock()
			if closed {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		go srv.handle(c)
	}
}

// Shutdown kills every session and stops serving.
func (srv *Server) Shutdown() {
	srv.mu.Lock()
	if srv.closed {
		srv.mu.Unlock()
		return
	}
	srv.closed = true
	ss := make([]*session, 0, len(srv.sessions))
	for _, s := range srv.sessions {
		ss = append(ss, s)
	}
	srv.sessions = map[string]*session{}
	ln := srv.ln
	srv.mu.Unlock()
	for _, s := range ss {
		s.kill("host shut down")
	}
	if ln != nil {
		_ = ln.Close()
	}
	_ = os.Remove(srv.Socket)
	if srv.stopped != nil {
		close(srv.stopped)
	}
}

func (srv *Server) get(name string) *session {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	return srv.sessions[name]
}

func (srv *Server) handle(c net.Conn) {
	_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
	r := bufio.NewReaderSize(c, 64<<10)
	var req wire.Request
	if err := wire.ReadLine(r, wire.MaxRequestBytes, &req); err != nil {
		c.Close()
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	if req.V != wire.ProtocolVersion {
		_ = wire.WriteLine(c, wire.ErrResponse(wire.CodeInvalid, fmt.Errorf("termhost: protocol %d not supported (host speaks %d)", req.V, wire.ProtocolVersion)))
		c.Close()
		return
	}
	if req.Op == wire.OpAttach {
		srv.attach(c, r, req)
		return
	}
	resp := srv.do(req)
	_ = c.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_ = wire.WriteLine(c, resp)
	c.Close()
	if req.Op == wire.OpShutdown && resp.OK {
		srv.Shutdown()
	}
}

func noSession(name string) wire.Response {
	return wire.Response{Code: wire.CodeNoSession, Err: "can't find session: " + name}
}

func validSpec(name string, sp *wire.Spec) error {
	if sp == nil {
		return errors.New("termhost: missing spec")
	}
	return sessionhost.SessionSpec{Name: name, Argv: sp.Argv, Env: sp.Env, Unset: sp.Unset}.Validate()
}

func (srv *Server) do(req wire.Request) wire.Response {
	switch req.Op {
	case wire.OpPing:
		backend, err := wire.BackendCheck()
		resp := wire.Response{OK: err == nil, V: wire.ProtocolVersion, PID: os.Getpid(), Backend: backend}
		if err != nil {
			resp.Code, resp.Err = wire.CodeFailed, err.Error()
		}
		return resp
	case wire.OpList:
		srv.mu.Lock()
		names := make([]string, 0, len(srv.sessions))
		for n := range srv.sessions {
			names = append(names, n)
		}
		srv.mu.Unlock()
		sort.Strings(names)
		return wire.Response{OK: true, Names: names}
	case wire.OpShutdown:
		return wire.Response{OK: true}
	case wire.OpNew:
		return srv.newSession(req)
	}
	s := srv.get(req.Name)
	if s == nil {
		if req.Op == wire.OpHas {
			return wire.Response{OK: true}
		}
		return noSession(req.Name)
	}
	switch req.Op {
	case wire.OpHas:
		return wire.Response{OK: true, Exists: true}
	case wire.OpRespawn:
		return srv.respawn(s, req)
	case wire.OpClients:
		return wire.Response{OK: true, Clients: s.listClients()}
	case wire.OpCapture:
		s.mu.Lock()
		text := s.scr.capture(req.History, req.Escapes)
		s.mu.Unlock()
		if len(text) > sessionhost.MaxCaptureBytes {
			text = text[len(text)-sessionhost.MaxCaptureBytes:]
		}
		return wire.Response{OK: true, Text: text}
	case wire.OpPaste:
		if err := s.writeInput(s.pasteBytes(req.Text)); err != nil {
			return wire.ErrResponse(wire.CodeFailed, err)
		}
		return wire.Response{OK: true}
	case wire.OpKeys:
		b, err := s.keyBytes(req.Keys)
		if err != nil {
			return wire.ErrResponse(wire.CodeInvalid, err)
		}
		if err := s.writeInput(b); err != nil {
			return wire.ErrResponse(wire.CodeFailed, err)
		}
		return wire.Response{OK: true}
	case wire.OpPane:
		return wire.Response{OK: true, Pane: s.pane()}
	case wire.OpResize:
		if req.Cols <= 0 || req.Rows <= 0 {
			return wire.ErrResponse(wire.CodeInvalid, fmt.Errorf("termhost: invalid window size %dx%d", req.Cols, req.Rows))
		}
		s.resize(req.Cols, req.Rows)
		return wire.Response{OK: true}
	case wire.OpKill, wire.OpTerminate:
		srv.mu.Lock()
		if srv.sessions[req.Name] == s {
			delete(srv.sessions, req.Name)
		}
		srv.mu.Unlock()
		if req.Op == wire.OpKill {
			s.kill("session killed")
		} else {
			s.terminate(time.Duration(req.GraceMS) * time.Millisecond)
		}
		srv.logf("session %s: closed (%s)", s.name, req.Op)
		return wire.Response{OK: true}
	}
	return wire.ErrResponse(wire.CodeInvalid, fmt.Errorf("termhost: unknown op %q", req.Op))
}

func (srv *Server) newSession(req wire.Request) wire.Response {
	if err := validSpec(req.Name, req.Spec); err != nil {
		return wire.ErrResponse(wire.CodeInvalid, err)
	}
	w, h := req.Spec.Width, req.Spec.Height
	if w <= 0 {
		w = 200
	}
	if h <= 0 {
		h = 50
	}
	w, h = wire.ClampDims(w, h)
	srv.mu.Lock()
	if srv.closed {
		srv.mu.Unlock()
		return wire.ErrResponse(wire.CodeFailed, errors.New("termhost: host is shutting down"))
	}
	if srv.sessions[req.Name] != nil {
		srv.mu.Unlock()
		return wire.Response{Code: wire.CodeConflict, Err: "duplicate session: " + req.Name}
	}
	srv.nextPane++
	s := newSession(srv, req.Name, "%"+strconv.Itoa(srv.nextPane), w, h)
	srv.sessions[req.Name] = s
	srv.mu.Unlock()

	s.mu.Lock()
	err := s.spawnLocked(req.Spec)
	s.mu.Unlock()
	if err != nil {
		srv.mu.Lock()
		delete(srv.sessions, req.Name)
		srv.mu.Unlock()
		s.kill("start failed")
		return wire.ErrResponse(wire.CodeFailed, fmt.Errorf("start %s: %w", filepath.Base(req.Spec.Argv[0]), err))
	}
	srv.logf("session %s: started %s", req.Name, filepath.Base(req.Spec.Argv[0]))
	return wire.Response{OK: true}
}

func (srv *Server) respawn(s *session, req wire.Request) wire.Response {
	if err := validSpec(req.Name, req.Spec); err != nil {
		return wire.ErrResponse(wire.CodeInvalid, err)
	}
	s.mu.Lock()
	old, live := s.p, s.p != nil && !s.dead
	s.mu.Unlock()
	if live {
		old.Kill() // respawn-pane -k
		for i := 0; i < 100 && s.alive(); i++ {
			time.Sleep(20 * time.Millisecond)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return noSession(req.Name)
	}
	if old != nil {
		_ = old.Close()
	}
	s.scr.write([]byte("\r\n"))
	if err := s.spawnLocked(req.Spec); err != nil {
		return wire.ErrResponse(wire.CodeFailed, fmt.Errorf("respawn %s: %w", filepath.Base(req.Spec.Argv[0]), err))
	}
	return wire.Response{OK: true}
}

// attach serves one attached client until it disconnects or the session
// ends.
func (srv *Server) attach(c net.Conn, r *bufio.Reader, req wire.Request) {
	s := srv.get(req.Name)
	if s == nil {
		_ = wire.WriteLine(c, noSession(req.Name))
		c.Close()
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = wire.WriteLine(c, noSession(req.Name))
		c.Close()
		return
	}
	s.nextCID++
	cl := newAttachClient("termhost:"+s.name+"/"+strconv.Itoa(s.nextCID), c, req)
	if !cl.readOnly && !cl.ignoreSize && req.Cols > 0 && req.Rows > 0 {
		s.resizeLocked(req.Cols, req.Rows) // window-size latest: the newest writer sizes the session
	}
	s.clients[cl] = true
	if !cl.readOnly {
		s.writers.Add(1)
	}
	cl.send(wire.FrameOutput, s.scr.repaint())
	cols, rows := s.cols, s.rows
	s.mu.Unlock()
	srv.logf("session %s: client %s attached (read-only %v)", s.name, cl.id, cl.readOnly)

	_ = c.SetWriteDeadline(time.Now().Add(10 * time.Second))
	err := wire.WriteLine(c, wire.Response{OK: true, Cols: cols, Rows: rows})
	_ = c.SetWriteDeadline(time.Time{})
	go cl.writeLoop()
	if err == nil {
		buf := make([]byte, 0, 32<<10)
		for {
			typ, b, err := wire.ReadFrame(r, buf)
			if err != nil {
				break
			}
			switch typ {
			case wire.FrameInput:
				if !cl.readOnly {
					_ = s.writeInput(b)
				}
			case wire.FrameSize:
				if cols, rows, ok := wire.ParseSize(b); ok && !cl.readOnly && !cl.ignoreSize {
					s.resize(cols, rows)
				}
			}
		}
	}
	s.mu.Lock()
	if s.clients[cl] {
		delete(s.clients, cl)
		if !cl.readOnly {
			s.writers.Add(-1)
		}
	}
	s.mu.Unlock()
	cl.closeAfterFlush()
	<-cl.done
	srv.logf("session %s: client %s detached", s.name, cl.id)
}
