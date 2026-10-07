package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"

	c "github.com/jhh3/pyre/internal/contracts"
)

// streamFrame is one event-stream WebSocket frame: an Event, or an error.
type streamFrame struct {
	c.Event
	Error *c.Error `json:"error,omitempty"`
}

// WatchEvents streams events after afterCursor over the event-stream
// WebSocket, calling fn for each in cursor order. It reconnects with bounded
// backoff after transport failures, resuming after the last cursor fn
// accepted (events are never skipped; a repeat after reconnect is filtered
// by cursor). It returns:
//
//   - the *contracts.Error with code reset_required when the cursor is
//     older than retention (Details carry oldest_cursor/snapshot_cursor;
//     re-snapshot with list calls and resume from snapshot_cursor);
//   - any non-retryable API error (unauthorized, protocol_mismatch, ...);
//   - fn's error, unchanged, if fn fails (the event is not considered
//     consumed);
//   - ctx.Err() on cancellation.
func (cl *Client) WatchEvents(ctx context.Context, afterCursor int64, fn func(c.Event) error) error {
	return cl.WatchEventsFiltered(ctx, c.ListEventsRequest{AfterCursor: afterCursor}, fn)
}

// WatchEventsFiltered is WatchEvents with kind/session/project filters.
// AfterCursor, Kinds, SessionID and ProjectID are honored.
func (cl *Client) WatchEventsFiltered(ctx context.Context, req c.ListEventsRequest, fn func(c.Event) error) error {
	if cl.cfgErr != nil {
		return cl.cfgErr
	}
	cursor := req.AfterCursor
	failures := 0
	for {
		progressed, err := cl.watchOnce(ctx, req, &cursor, fn)
		if err == nil {
			err = c.Errorf(c.ErrUnavailable, "event stream closed")
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var fnErr *callbackError
		if errors.As(err, &fnErr) {
			return fnErr.err
		}
		if !Retryable(err) {
			return err
		}
		if progressed {
			failures = 0
		}
		wait := cl.backoff(failures)
		failures++
		if err := cl.sleep(ctx, wait); err != nil {
			return err
		}
	}
}

type callbackError struct{ err error }

func (e *callbackError) Error() string { return e.err.Error() }
func (e *callbackError) Unwrap() error { return e.err }

func (cl *Client) streamURL(req c.ListEventsRequest, cursor int64) string {
	u := *cl.base
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	default:
		u.Scheme = "ws"
	}
	u.Path = cl.base.Path + c.PathEventStream
	q := url.Values{"after_cursor": {strconv.FormatInt(cursor, 10)}}
	if len(req.Kinds) > 0 {
		ks := make([]string, len(req.Kinds))
		for i, k := range req.Kinds {
			ks[i] = string(k)
		}
		q.Set("kinds", strings.Join(ks, ","))
	}
	if req.SessionID != "" {
		q.Set("session_id", req.SessionID)
	}
	if req.ProjectID != nil {
		q.Set("project_id", *req.ProjectID)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// streamStableAfter: a connection that stayed up this long counts as
// progress even without events (an idle stream that is eventually cut).
const streamStableAfter = 30 * time.Second

// watchOnce runs one connection. progressed reports whether it made
// progress worth resetting backoff: an event was delivered to fn, or the
// connection stayed up for streamStableAfter. An accepted WebSocket that
// immediately reports an error is not progress.
func (cl *Client) watchOnce(ctx context.Context, req c.ListEventsRequest, cursor *int64, fn func(c.Event) error) (progressed bool, err error) {
	h := http.Header{}
	cl.setHeaders(h)
	h.Del("Accept")
	dctx, cancel := context.WithTimeout(ctx, cl.opts.Timeout)
	conn, resp, err := websocket.Dial(dctx, cl.streamURL(req, *cursor), &websocket.DialOptions{HTTPClient: cl.hc, HTTPHeader: h})
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
			if resp.Body != nil {
				defer resp.Body.Close()
				b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
				var eb c.ErrorBody
				if json.Unmarshal(b, &eb) == nil && eb.Error != nil && eb.Error.Code != "" {
					return false, eb.Error
				}
			}
			return false, statusError(resp.StatusCode)
		}
		return false, transportError(ctx, err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(maxResponseBytes)
	connected := time.Now()
	delivered := false
	defer func() {
		progressed = delivered || time.Since(connected) >= streamStableAfter
	}()
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			return false, &c.Error{Code: c.ErrUnavailable, Message: "event stream interrupted: " + err.Error(), Retryable: true}
		}
		if typ != websocket.MessageText {
			continue
		}
		var f streamFrame
		if err := json.Unmarshal(data, &f); err != nil {
			return false, &c.Error{Code: c.ErrUnavailable, Message: "event stream sent a malformed frame", Retryable: true}
		}
		if f.Error != nil {
			_ = conn.Close(websocket.StatusNormalClosure, "")
			return false, f.Error
		}
		if f.Event.Cursor <= *cursor {
			continue // duplicate after reconnect
		}
		if err := fn(f.Event); err != nil {
			_ = conn.Close(websocket.StatusNormalClosure, "")
			return false, &callbackError{err: err}
		}
		*cursor = f.Event.Cursor
		delivered = true
	}
}

// EventLister is the part of the API that lists hub events.
type EventLister interface {
	ListEvents(ctx context.Context, req c.ListEventsRequest) (c.ListEventsResponse, error)
}

// HeadCursor returns the hub's newest event cursor, for a consumer that
// starts "from now". Once the hub has pruned its event log, a list from
// cursor 0 is answered with reset_required; the reset carries the head as
// snapshot_cursor, which is returned instead.
func HeadCursor(ctx context.Context, l EventLister) (int64, error) {
	resp, err := l.ListEvents(ctx, c.ListEventsRequest{AfterCursor: 0, Limit: 1})
	if err == nil {
		return resp.HeadCursor, nil
	}
	if n, ok := SnapshotCursor(err); ok {
		return n, nil
	}
	return 0, err
}

// SnapshotCursor extracts snapshot_cursor from a reset_required error.
func SnapshotCursor(err error) (int64, bool) {
	var ce *c.Error
	if !errors.As(err, &ce) || ce.Code != c.ErrResetRequired || ce.Details == nil {
		return 0, false
	}
	switch v := ce.Details["snapshot_cursor"].(type) {
	case float64:
		return int64(v), true
	case int64:
		return v, true
	case int:
		return int64(v), true
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	}
	return 0, false
}
