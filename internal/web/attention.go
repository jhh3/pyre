package web

import (
	"context"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

// Session items as inbox threads (owner request 2026-10-19: "why are a few
// sessions stuck with needs you state??"). A session's "waiting on you"
// (it ended its turn with a question in its own chat) and its own "needs
// you" flag mark it needs-you without an ask or a message to show for it,
// so each is a thread here: its title and excerpt, a reply that goes to
// the session as chat (which answers the item), and Archive, which
// dismisses the item (handled at the hub).

// attentionThreadKinds are the items shown as threads.
var attentionThreadKinds = map[c.AttentionKind]bool{c.AttnWaiting: true, c.AttnFlag: true}

func activeItem(it c.AttentionItem) bool {
	return it.State == c.AttnOpen || it.State == c.AttnRead
}

// attentionThread is item it as a thread (thread ID = attention ID).
func (s *Server) attentionThread(sn *snap, it c.AttentionItem) Thread {
	item := it
	t := Thread{ThreadID: it.AttentionID, Kind: "attention", Title: it.Title, Preview: it.Summary, Project: sn.projectRef(it.ProjectID),
		Session: sn.sessionRef(it.SessionID), LastAt: it.UpdatedAt, Attention: &item}
	if t.Preview == "" {
		t.Preview = it.Title
	}
	if t.Project == nil && it.SessionID != "" {
		t.Project = sn.projectRef(sn.sessions[it.SessionID].Session.ProjectID)
	}
	t.open = activeItem(it) && !sn.sessions[it.SessionID].Session.Archived
	t.NeedsReply = t.open
	t.Unread = it.State == c.AttnOpen && it.UpdatedAt.After(s.read.readAt(it.AttentionID))
	return t
}

// attentionThreads are the active session items of sn as threads.
func (s *Server) attentionThreads(sn *snap) []Thread {
	var out []Thread
	for _, it := range sn.attention {
		if attentionThreadKinds[it.Kind] && it.SessionID != "" && activeItem(it) {
			out = append(out, s.attentionThread(sn, it))
		}
	}
	return out
}

// resolveAttention reads item id live (any state); only the kinds shown
// as threads resolve. withMsgs adds its conversation.
func (s *Server) resolveAttention(ctx context.Context, sn *snap, id string, withMsgs bool) (*resolved, error) {
	it, err := s.api.GetAttention(ctx, c.GetRequest{ID: id})
	if err != nil {
		return nil, err
	}
	if !attentionThreadKinds[it.Kind] || it.SessionID == "" {
		return nil, c.Errorf(c.ErrNotFound, "No conversation %s in your inbox.", id)
	}
	rt := &resolved{thread: s.attentionThread(sn, it), attention: &it}
	// The conversation is what the owner sent the session since the item
	// was raised (replies here are chat to the session, not a thread) and
	// before the session's next item of its kind: a new question is a new
	// item and conversation (owner report 2026-10-19: one item per session
	// showed its whole history as one thread).
	// The owner's mail index holds all of it (a live read of the session's
	// inbox returned only its oldest page: a coordinator receives
	// thousands of child reports).
	if !withMsgs {
		return rt, nil
	}
	// A reply typed in this thread stays in it, and one typed in another
	// item's thread stays there, whenever it was sent.
	end := s.nextItemAt(ctx, it)
	for _, m := range sn.mail.msgs {
		e := m.Envelope
		if !fromOwner(m) || e.Recipient.Kind != c.AddrSession || e.Recipient.ID != it.SessionID {
			continue
		}
		at := msgTime(m)
		if th := s.read.replyThread(e.MessageID); th == it.AttentionID ||
			th == "" && !at.Before(it.CreatedAt) && (end.IsZero() || at.Before(end)) {
			rt.msgs = append(rt.msgs, m)
		}
	}
	sortMsgs(rt.msgs)
	rt.thread.MessageCount = len(rt.msgs)
	return rt, nil
}

// nextItemAt is when the session's next item of its kind (and group) was
// created after it (zero when there is none, or the hub cannot say).
func (s *Server) nextItemAt(ctx context.Context, it c.AttentionItem) time.Time {
	var next time.Time
	items, err := all(ctx, func(ctx context.Context, cur string) (c.Page[c.AttentionItem], error) {
		return s.api.ListAttention(ctx, c.ListAttentionRequest{SessionID: it.SessionID, Kinds: []c.AttentionKind{it.Kind},
			States:      []c.AttentionState{c.AttnOpen, c.AttnRead, c.AttnSnoozed, c.AttnHandled, c.AttnWithdrawn},
			PageRequest: c.PageRequest{Cursor: cur, Limit: pageLimit}})
	})
	if err != nil {
		return next
	}
	for _, o := range items {
		if o.AttentionID != it.AttentionID && o.GroupKey == it.GroupKey && o.CreatedAt.After(it.CreatedAt) && (next.IsZero() || o.CreatedAt.Before(next)) {
			next = o.CreatedAt
		}
	}
	return next
}

// dismissAttention archives an item thread: handled at the hub.
func (s *Server) dismissAttention(ctx context.Context, sn *snap, rt *resolved, op string) (Thread, error) {
	it := *rt.attention
	if activeItem(it) {
		var err error
		if it, err = s.api.UpdateAttention(ctx, c.UpdateAttentionRequest{Mutation: c.Mutation{OperationID: opID(op)},
			AttentionID: it.AttentionID, State: c.AttnHandled}); err != nil {
			return Thread{}, err
		}
		s.attention.invalidate()
	}
	_ = s.read.mark(it.AttentionID, s.now())
	return s.attentionThread(sn, it), nil
}
