package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

// ---- browser fan-out ----

// sseEvent is one Server-Sent Event: name and JSON data.
type sseEvent struct {
	name string
	data string
}

// broker coalesces events for a short debounce and fans them out to every
// connected browser. A browser that does not keep up loses events (it
// refetches on the next one anyway).
type broker struct {
	mu       sync.Mutex
	subs     map[chan sseEvent]struct{}
	pending  []sseEvent
	seen     map[sseEvent]bool
	timer    *time.Timer
	debounce time.Duration
}

func newBroker(debounce time.Duration) *broker {
	return &broker{subs: map[chan sseEvent]struct{}{}, seen: map[sseEvent]bool{}, debounce: debounce}
}

func (b *broker) subscribe() chan sseEvent {
	ch := make(chan sseEvent, 64)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

func (b *broker) unsubscribe(ch chan sseEvent) {
	b.mu.Lock()
	delete(b.subs, ch)
	b.mu.Unlock()
}

// publish queues event name with data (marshalled to JSON); repeats within
// the debounce collapse. "held" keeps only its latest payload.
func (b *broker) publish(name string, data any) {
	raw, err := json.Marshal(data)
	if err != nil {
		return
	}
	ev := sseEvent{name: name, data: string(raw)}
	b.mu.Lock()
	defer b.mu.Unlock()
	if name == "held" {
		for i := range b.pending {
			if b.pending[i].name == "held" {
				delete(b.seen, b.pending[i])
				b.pending[i] = ev
				b.seen[ev] = true
				b.arm()
				return
			}
		}
	}
	if !b.seen[ev] {
		b.seen[ev] = true
		b.pending = append(b.pending, ev)
	}
	b.arm()
}

func (b *broker) arm() {
	if b.timer == nil {
		b.timer = time.AfterFunc(b.debounce, b.flush)
	}
}

func (b *broker) flush() {
	b.mu.Lock()
	evs := b.pending
	b.pending, b.seen, b.timer = nil, map[sseEvent]bool{}, nil
	subs := make([]chan sseEvent, 0, len(b.subs))
	for ch := range b.subs {
		subs = append(subs, ch)
	}
	b.mu.Unlock()
	for _, ch := range subs {
		for _, ev := range evs {
			select {
			case ch <- ev:
			default:
			}
		}
	}
}

// handleEvents streams the brief's events: "inbox", "held", "project",
// "thread", with a comment heartbeat.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, c.Errorf(c.ErrUnsupported, "streaming unsupported"))
		return
	}
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	ch := s.bus.subscribe()
	defer s.bus.unsubscribe(ch)
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "retry: 3000\n: connected\n\n")
	fl.Flush()
	tick := time.NewTicker(s.opts.Heartbeat)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			fl.Flush()
		case ev := <-ch:
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.name, ev.data); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

// ---- hub subscription ----

// streamWatcher is implemented by *client.Client (event-stream WebSocket).
type streamWatcher interface {
	WatchEvents(ctx context.Context, afterCursor int64, fn func(c.Event) error) error
}

// watch keeps one hub event subscription for the whole server: it updates
// the caches and the mail index and tells browsers what changed. It
// restarts with backoff after errors and re-snapshots after a gap.
func (s *Server) watch(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		cursor, err := s.headCursor(ctx)
		if err == nil {
			// Everything read before now may have missed events: reload.
			s.invalidateAll()
			s.bus.publish("inbox", struct{}{})
			// The client's stream resumes from its cursor across reconnects,
			// so the caches stay exact while follow runs.
			s.live.Store(true)
			err = s.follow(ctx, cursor, func() { backoff = time.Second })
		}
		s.live.Store(false)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			s.logf("pyre web: hub event stream: %v (retrying in %s)", err, backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// headCursor is the hub's current event head.
func (s *Server) headCursor(ctx context.Context) (int64, error) {
	hctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	resp, err := s.api.ListEvents(hctx, c.ListEventsRequest{AfterCursor: 0, Limit: 1})
	if err != nil {
		if e := c.AsError(err); e.Code == c.ErrResetRequired {
			if n, ok := detailInt(e.Details, "snapshot_cursor"); ok {
				return n, nil
			}
		}
		return 0, err
	}
	return resp.HeadCursor, nil
}

func detailInt(d map[string]any, key string) (int64, bool) {
	switch v := d[key].(type) {
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

// follow streams events after cursor until an error. It uses the client's
// WebSocket stream when available, else long-polls ListEvents (each call
// waits up to 25s at the hub, so this is never a tight loop).
func (s *Server) follow(ctx context.Context, cursor int64, progressed func()) error {
	if w, ok := s.api.(streamWatcher); ok {
		return w.WatchEvents(ctx, cursor, func(ev c.Event) error {
			progressed()
			s.onEvent(ev)
			return nil
		})
	}
	for {
		resp, err := s.api.ListEvents(ctx, c.ListEventsRequest{AfterCursor: cursor, Limit: 500, WaitSeconds: 25})
		if err != nil {
			return err
		}
		progressed()
		for _, ev := range resp.Events {
			s.onEvent(ev)
		}
		if resp.NextCursor > cursor {
			cursor = resp.NextCursor
		} else if len(resp.Events) == 0 {
			// Nothing new and the hub returned at once (a hub that ignores
			// wait_seconds): pace ourselves.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}
	}
}

func (s *Server) invalidateAll() {
	s.sessions.invalidate()
	s.projects.invalidate()
	s.asks.invalidate()
	s.attention.invalidate()
	s.held.invalidate()
	s.mail.markStale()
	s.pagesMu.Lock()
	for _, p := range s.pages {
		p.invalidate()
	}
	s.pagesMu.Unlock()
}

// onEvent applies one hub event to the caches and tells browsers.
func (s *Server) onEvent(ev c.Event) {
	project := func() {
		if ev.ProjectID != nil && *ev.ProjectID != "" {
			s.bus.publish("project", map[string]string{"project_id": *ev.ProjectID})
		}
	}
	switch ev.Kind {
	case c.EvSessionCreated, c.EvSessionUpdated, c.EvRunStarted, c.EvRunObserved, c.EvRunEnded:
		s.sessions.invalidate()
		project()
	case c.EvProjectChanged:
		s.projects.invalidate()
		if ev.Object.ID != "" {
			s.bus.publish("project", map[string]string{"project_id": ev.Object.ID})
		}
		project()
	case c.EvAskChanged:
		s.asks.invalidate()
		var a c.Ask
		if json.Unmarshal(ev.Payload, &a) == nil && a.AskID != "" {
			s.bus.publish("thread", map[string]string{"thread_id": askThreadID(a)})
		}
		s.bus.publish("inbox", struct{}{})
		project()
	case c.EvAskHeldChanged:
		var h c.HeldAnswers
		if json.Unmarshal(ev.Payload, &h) == nil {
			h = normHeld(h)
			s.held.set(h, s.now())
			s.bus.publish("held", h)
		} else {
			s.held.invalidate()
		}
		s.bus.publish("inbox", struct{}{})
	case c.EvAttentionChanged:
		s.attention.invalidate()
		s.bus.publish("inbox", struct{}{})
	case c.EvMessageStored, c.EvMessageDelivery, c.EvMessageAck:
		var m c.MessageView
		if json.Unmarshal(ev.Payload, &m) != nil || !ownerMail(m) {
			return
		}
		s.mail.apply(m)
		s.bus.publish("inbox", struct{}{})
		key := s.threadKeyOf(m)
		if asks, ok := s.asks.peek(); ok {
			for _, a := range asks {
				if askMailKey(a) == key {
					key = askThreadID(a)
					break
				}
			}
		}
		s.bus.publish("thread", map[string]string{"thread_id": key})
	case c.EvDocumentRevised:
		if ev.ProjectID != nil {
			s.invalidatePage(*ev.ProjectID)
		}
		project()
	case c.EvArtifactPublished, c.EvArtifactChanged:
		project()
	}
}

// threadKeyOf is m's thread key from the index (see groupThreads).
func (s *Server) threadKeyOf(m c.MessageView) string {
	s.mail.mu.Lock()
	defer s.mail.mu.Unlock()
	id := m.Envelope.MessageID
	for i := 0; i < 64; i++ {
		cur, ok := s.mail.msgs[id]
		if !ok {
			return id
		}
		if cur.Envelope.ThreadID != "" {
			return cur.Envelope.ThreadID
		}
		if cur.Envelope.InReplyTo == "" {
			return id
		}
		if _, ok := s.mail.msgs[cur.Envelope.InReplyTo]; !ok {
			return id
		}
		id = cur.Envelope.InReplyTo
	}
	return id
}
