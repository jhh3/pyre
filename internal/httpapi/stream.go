package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/jhh3/pyre/internal/contracts"
)

// WebSocket close codes used by the event stream (application range).
const (
	// CloseResetRequired follows a reset_required error frame: the cursor is
	// older than retention; re-snapshot and resume from snapshot_cursor.
	CloseResetRequired websocket.StatusCode = 4410
	// CloseStreamError follows any other error frame.
	CloseStreamError websocket.StatusCode = 4500
)

const streamBatch = 200

// serveEventStream upgrades to a WebSocket and pushes Event JSON frames
// (one event per text frame) read from ListEvents after ?after_cursor=<n>.
// Optional filters: kinds=a,b  session_id=  project_id=.
// Any API error is sent as a single {"error":{...}} frame, then the
// connection closes (CloseResetRequired for reset_required).
func (s *server) serveEventStream(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var req contracts.ListEventsRequest
	if v := q.Get("after_cursor"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			WriteError(w, contracts.Errorf(contracts.ErrInvalid, "after_cursor must be a non-negative integer"))
			return
		}
		req.AfterCursor = n
	}
	if v := q.Get("kinds"); v != "" {
		for _, k := range strings.Split(v, ",") {
			if k = strings.TrimSpace(k); k != "" {
				req.Kinds = append(req.Kinds, contracts.EventKind(k))
			}
		}
	}
	req.SessionID = q.Get("session_id")
	if q.Has("project_id") {
		p := q.Get("project_id")
		req.ProjectID = &p
	}

	select {
	case <-s.stop:
		WriteError(w, contracts.Errorf(contracts.ErrUnavailable, "hub is shutting down"))
		return
	default:
	}
	if n := s.open.Add(1); n > int64(s.opts.MaxStreams) {
		s.open.Add(-1)
		WriteError(w, contracts.Errorf(contracts.ErrCapacityExhausted, "too many open event streams (%d); retry later", s.opts.MaxStreams))
		return
	}
	defer s.open.Add(-1)
	s.streams.Add(1)
	defer s.streams.Done()
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: s.opts.AllowedOrigins})
	if err != nil {
		return // Accept already wrote an HTTP error
	}
	defer c.CloseNow()
	c.SetReadLimit(4096)
	// We never expect client data frames; CloseRead handles control frames
	// and cancels ctx when the peer goes away. CloseStreams cancels it too.
	ctx := c.CloseRead(r.Context())
	ctx, stopStream := context.WithCancel(ctx)
	defer stopStream()
	var shutdown atomic.Bool
	go func() {
		select {
		case <-s.stop:
			shutdown.Store(true)
			stopStream()
		case <-ctx.Done():
		}
	}()
	defer func() {
		if shutdown.Load() {
			_ = c.Close(websocket.StatusGoingAway, "hub shutting down")
		}
	}()

	pingDone := make(chan struct{})
	go func() {
		defer close(pingDone)
		t := time.NewTicker(s.opts.StreamPing)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
				err := c.Ping(pctx)
				cancel()
				if err != nil {
					c.CloseNow()
					return
				}
			}
		}
	}()
	defer func() { <-pingDone }()

	wait := int(s.opts.StreamWait / time.Second)
	if wait < 1 {
		wait = 1
	}
	for ctx.Err() == nil {
		req.WaitSeconds = wait
		req.Limit = streamBatch
		resp, err := s.api.ListEvents(ctx, req)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			noteInternal(r, err)
			ce := publicError(err)
			b, _ := json.Marshal(contracts.ErrorBody{Error: ce})
			wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			_ = c.Write(wctx, websocket.MessageText, b)
			cancel()
			code := CloseStreamError
			if ce.Code == contracts.ErrResetRequired {
				code = CloseResetRequired
			}
			_ = c.Close(code, string(ce.Code))
			return
		}
		for _, ev := range resp.Events {
			b, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			wctx, cancel := context.WithTimeout(ctx, s.opts.StreamWriteTimeout)
			err = c.Write(wctx, websocket.MessageText, b)
			cancel()
			if err != nil {
				return // slow or gone: the client resumes from its cursor
			}
		}
		if resp.NextCursor > req.AfterCursor {
			req.AfterCursor = resp.NextCursor
		}
	}
	_ = c.Close(websocket.StatusGoingAway, "")
}
