package web

import (
	"context"
	"sort"
	"sync"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

// mailIndex is the owner's mail (messages to the owner and from the owner
// to sessions), loaded once from the hub and then kept current from the
// message events' payloads (message.stored/delivery/ack carry the
// MessageView). Re-reading a session's message with GetMessage would mark
// it read for the owner at the hub, so the index never does that.
type mailIndex struct {
	mu      sync.Mutex
	msgs    map[string]c.MessageView
	loaded  bool
	at      time.Time // when the last full load finished
	stale   bool      // the event stream lost continuity: reload on next use
	loading chan struct{}
	err     error
}

// ownerMail reports whether m is mail the inbox shows: to the owner, or
// from the owner to a session.
func ownerMail(m c.MessageView) bool {
	e := m.Envelope
	if e.MessageID == "" {
		return false
	}
	if e.Recipient.Kind == c.AddrOwner {
		return true
	}
	return fromOwner(m) && e.Recipient.Kind == c.AddrSession
}

func fromOwner(m c.MessageView) bool {
	return m.Envelope.Sender.Kind == c.PrincipalOwner
}

// fromSession returns the sending session of mail to the owner ("" when
// it was not a session writing to the owner).
func fromSession(m c.MessageView) string {
	e := m.Envelope
	if e.Recipient.Kind == c.AddrOwner && e.Sender.Kind == c.PrincipalSession {
		return e.Sender.ID
	}
	return ""
}

// msgTime is when the hub stored m (the client time when not stored yet).
func msgTime(m c.MessageView) time.Time {
	if m.Status.StoredAt != nil {
		return *m.Status.StoredAt
	}
	return m.Envelope.CreatedAt
}

// newer reports whether a is at least as current as b. On a tie (a hub
// clock that did not advance between the two) an acknowledgement is never
// undone: the message.stored event, carrying the unacknowledged status,
// can arrive after the ack was applied and would reopen the thread.
func newer(a, b c.MessageView) bool {
	if !a.Status.UpdatedAt.Equal(b.Status.UpdatedAt) {
		return a.Status.UpdatedAt.After(b.Status.UpdatedAt)
	}
	return ackedState(a.Status.Ack) || !ackedState(b.Status.Ack)
}

func ackedState(s c.AckState) bool { return s == c.AckAcknowledged || s == c.AckReplied }

// apply merges one message (newest status wins).
func (x *mailIndex) apply(m c.MessageView) {
	if !ownerMail(m) {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.msgs == nil {
		x.msgs = map[string]c.MessageView{}
	}
	if old, ok := x.msgs[m.Envelope.MessageID]; ok && !newer(m, old) {
		return
	}
	x.msgs[m.Envelope.MessageID] = m
}

// staleAfter marks the index stale when its last full load is older than d.
func (x *mailIndex) staleAfter(d time.Duration) {
	x.mu.Lock()
	if x.loaded && time.Since(x.at) > d {
		x.stale = true
	}
	x.mu.Unlock()
}

// markStale makes the next reader reload everything.
func (x *mailIndex) markStale() {
	x.mu.Lock()
	x.stale = true
	x.mu.Unlock()
}

// snapshot returns every indexed message, loading the index first when
// needed (concurrent callers share one load).
func (x *mailIndex) snapshot(ctx context.Context, api c.API) (map[string]c.MessageView, error) {
	x.mu.Lock()
	if x.loaded && !x.stale {
		out := make(map[string]c.MessageView, len(x.msgs))
		for k, v := range x.msgs {
			out[k] = v
		}
		x.mu.Unlock()
		return out, nil
	}
	if ch := x.loading; ch != nil {
		x.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		x.mu.Lock()
		err := x.err
		x.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return x.snapshot(ctx, api)
	}
	ch := make(chan struct{})
	x.loading = ch
	x.stale = false
	x.mu.Unlock()

	all, err := loadOwnerMail(ctx, api)

	x.mu.Lock()
	x.loading, x.err = nil, err
	if err == nil {
		if x.msgs == nil {
			x.msgs = map[string]c.MessageView{}
		}
		for _, m := range all {
			if old, ok := x.msgs[m.Envelope.MessageID]; !ok || newer(m, old) {
				x.msgs[m.Envelope.MessageID] = m
			}
		}
		x.loaded, x.at = true, time.Now()
	} else {
		x.stale = true
	}
	close(ch)
	x.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return x.snapshot(ctx, api)
}

// loadOwnerMail pages through the owner's inbox and outbox.
func loadOwnerMail(ctx context.Context, api c.API) ([]c.MessageView, error) {
	owner := &c.Address{Kind: c.AddrOwner, ID: "owner"}
	var out []c.MessageView
	for _, box := range []string{"inbox", "outbox"} {
		cur := ""
		for i := 0; i < 1000; i++ {
			p, err := api.ListMessages(ctx, c.ListMessagesRequest{Box: box, Address: owner, PageRequest: c.PageRequest{Cursor: cur, Limit: 500}})
			if err != nil {
				return nil, err
			}
			for _, m := range p.Items {
				if ownerMail(m) {
					out = append(out, m)
				}
			}
			if p.NextCursor == "" || p.NextCursor == cur {
				break
			}
			cur = p.NextCursor
		}
	}
	return out, nil
}

// threads groups owner mail by thread key (see threadKeys).
type threads struct {
	key  map[string]string          // message ID -> thread key
	by   map[string][]c.MessageView // thread key -> messages, oldest first
	msgs map[string]c.MessageView   // message ID -> message
}

// groupThreads assigns every message its thread key: the envelope's
// ThreadID, else the key of the message it replies to, else its own ID
// (a conversation the session started without a thread is keyed by its
// first message; replies sent from the web carry that key as ThreadID).
func groupThreads(msgs map[string]c.MessageView) threads {
	if msgs == nil {
		msgs = map[string]c.MessageView{}
	}
	t := threads{key: map[string]string{}, by: map[string][]c.MessageView{}, msgs: msgs}
	var keyOf func(id string, depth int) string
	keyOf = func(id string, depth int) string {
		if k, ok := t.key[id]; ok {
			return k
		}
		m, ok := msgs[id]
		if !ok {
			return id
		}
		k := m.Envelope.ThreadID
		if k == "" && m.Envelope.InReplyTo != "" && depth < 64 {
			if _, ok := msgs[m.Envelope.InReplyTo]; ok {
				k = keyOf(m.Envelope.InReplyTo, depth+1)
			}
		}
		if k == "" {
			k = id
		}
		t.key[id] = k
		return k
	}
	for id, m := range msgs {
		k := keyOf(id, 0)
		t.by[k] = append(t.by[k], m)
	}
	for k := range t.by {
		sortMsgs(t.by[k])
	}
	return t
}

func sortMsgs(ms []c.MessageView) {
	sort.SliceStable(ms, func(i, j int) bool {
		ti, tj := msgTime(ms[i]), msgTime(ms[j])
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		return ms[i].Envelope.MessageID < ms[j].Envelope.MessageID
	})
}
