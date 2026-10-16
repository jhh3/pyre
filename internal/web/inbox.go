package web

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"email": emailOf(r)})
}

// ---- threads ----

// askThread builds the thread of ask a from the mail in its thread.
func (s *Server) askThread(sn *snap, a c.Ask, msgs []c.MessageView) Thread {
	tid := askThreadID(a)
	t := Thread{ThreadID: tid, Kind: "ask", Title: a.Title, Project: sn.projectRef(a.ProjectID), Session: sn.sessionRef(a.SessionID),
		LastAt: a.UpdatedAt, MessageCount: max(1, len(msgs))}
	ask := a
	t.Ask = &ask
	if h, ok := sn.heldBy[a.AskID]; ok && a.State == c.AskOpen {
		t.Held = &h
	}
	t.Preview = firstLine(a.Context, 200)
	if n := len(a.Updates); n > 0 {
		t.Preview = firstLine(a.Updates[n-1].Text, 200)
	}
	// Follow-ups in the thread after the ask (not the carrier, not the
	// decision): a session message the owner has not answered keeps the
	// thread open.
	followUp := pendingFromSession(msgs, a.MessageID)
	var incoming = a.CreatedAt
	if n := len(a.Updates); n > 0 && a.Updates[n-1].At.After(incoming) {
		incoming = a.Updates[n-1].At
	}
	for _, m := range msgs {
		at := msgTime(m)
		if at.After(t.LastAt) {
			t.LastAt = at
		}
		if fromSession(m) != "" && m.Envelope.MessageID != a.MessageID {
			if at.After(incoming) {
				incoming = at
			}
			t.Preview = excerpt(m.Envelope.Body.Text, 200)
		}
	}
	open := a.State == c.AskOpen
	t.open = open || followUp
	t.NeedsReply = (open && t.Held == nil) || followUp
	if t.open {
		t.Unread = s.unread(sn, tid, incoming, func(it c.AttentionItem) bool {
			return (a.AttentionID != "" && it.AttentionID == a.AttentionID) || (it.Object.Kind == c.KindAsk && it.Object.ID == a.AskID)
		})
	}
	return t
}

// pendingFromSession reports whether a session wrote to the owner after
// the owner's last message in msgs (oldest first) without being answered.
// skip is a message that does not count (an ask's carrier).
func pendingFromSession(msgs []c.MessageView, skip string) bool {
	pending := false
	for _, m := range msgs {
		switch {
		case fromOwner(m):
			pending = false
		case fromSession(m) != "" && m.Envelope.MessageID != skip && m.Status.Ack == c.AckNone:
			pending = true
		}
	}
	return pending
}

// messageThread builds a thread of plain mail (no ask). ok is false when
// no session wrote to the owner in it.
func (s *Server) messageThread(sn *snap, key string, msgs []c.MessageView) (Thread, bool) {
	var first, last *c.MessageView
	session := ""
	for i := range msgs {
		if sid := fromSession(msgs[i]); sid != "" {
			if first == nil {
				first = &msgs[i]
			}
			last = &msgs[i]
			session = sid
		}
	}
	if first == nil {
		return Thread{}, false
	}
	newest := msgs[len(msgs)-1]
	t := Thread{ThreadID: key, Kind: "message", Title: firstLine(first.Envelope.Body.Text, 100), Preview: excerpt(newest.Envelope.Body.Text, 200),
		Session: sn.sessionRef(session), LastAt: msgTime(newest), MessageCount: len(msgs)}
	if t.Title == "" {
		t.Title = "Message from " + sn.title(session)
	}
	proj := last.Envelope.ProjectID
	if proj == nil {
		proj = sn.sessions[session].Session.ProjectID
	}
	t.Project = sn.projectRef(proj)
	// Plain mail stays in the inbox while it is recent: an unanswered status
	// report from days ago (or from an archived session) is history, not
	// work. Asks are never aged out; they end by decision or deadline.
	t.open = pendingFromSession(msgs, "") && sn.now.Sub(t.LastAt) <= messageOpenWindow &&
		!sn.sessions[session].Session.Archived
	t.NeedsReply = t.open
	if t.open {
		ids := map[string]bool{}
		for _, m := range msgs {
			ids[m.Envelope.MessageID] = true
		}
		t.Unread = s.unread(sn, key, msgTime(*last), func(it c.AttentionItem) bool {
			return (it.Object.Kind == c.KindThread && it.Object.ID == key) || (it.Object.Kind == c.KindMessage && ids[it.Object.ID])
		})
	}
	return t, true
}

// unread: something arrived after the owner last opened the thread here,
// and no other surface marked its attention item read.
func (s *Server) unread(sn *snap, tid string, incoming time.Time, match func(c.AttentionItem) bool) bool {
	if !incoming.After(s.read.readAt(tid)) {
		return false
	}
	matched, open := false, false
	for _, it := range sn.attention {
		if match(it) {
			matched = true
			open = open || it.State == c.AttnOpen
		}
	}
	return !matched || open
}

// inboxThreads builds every thread for box "open" or "done".
func (s *Server) inboxThreads(sn *snap, box string) []Thread {
	var out []Thread
	keep := func(t Thread) {
		if box == "open" && t.open {
			out = append(out, t)
		} else if box == "done" && !t.open && sn.now.Sub(t.LastAt) <= doneWindow {
			out = append(out, t)
		}
	}
	for _, a := range sn.asks {
		var msgs []c.MessageView
		if k := askMailKey(a); k != "" {
			msgs = sn.mail.by[k]
		}
		keep(s.askThread(sn, a, msgs))
	}
	for key, msgs := range sn.mail.by {
		if _, isAsk := sn.askThread[key]; isAsk || c.ValidID(key, c.PrefixAsk) {
			continue // an ask's thread (an ask outside the window is not listed)
		}
		if t, ok := s.messageThread(sn, key, msgs); ok {
			keep(t)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].LastAt.Equal(out[j].LastAt) {
			return out[i].LastAt.After(out[j].LastAt)
		}
		return out[i].ThreadID < out[j].ThreadID
	})
	if out == nil {
		out = []Thread{}
	}
	return out
}

func (s *Server) handleInbox(w http.ResponseWriter, r *http.Request) {
	box := r.URL.Query().Get("box")
	if box == "" {
		box = "open"
	}
	if box != "open" && box != "done" {
		writeErr(w, c.Errorf(c.ErrInvalid, "box must be open or done"))
		return
	}
	ctx, cancel := hubCtx(r)
	defer cancel()
	sn, err := s.loadSnap(ctx, need{mail: true, asks: true, attention: true, held: true, projects: true})
	if err != nil {
		writeErr(w, err)
		return
	}
	threads := s.inboxThreads(sn, box)
	q := r.URL.Query()
	project, session := strings.TrimSpace(q.Get("project")), strings.TrimSpace(q.Get("session"))
	if project != "" || session != "" {
		scope := sn.descendants(session)
		kept := []Thread{}
		for _, t := range threads {
			if project != "" && (t.Project == nil || t.Project.ID != project) {
				continue
			}
			if session != "" && (t.Session == nil || !scope[t.Session.ID]) {
				continue
			}
			kept = append(kept, t)
		}
		threads = kept
	}
	writeJSON(w, http.StatusOK, map[string]any{"threads": threads, "held": sn.held})
}

// descendants is session id and every session under it (nil for "").
func (sn *snap) descendants(id string) map[string]bool {
	if id == "" {
		return nil
	}
	in := map[string]bool{id: true}
	for changed := true; changed; {
		changed = false
		for sid, v := range sn.sessions {
			if p := v.Session.Parent(); p != "" && in[p] && !in[sid] {
				in[sid] = true
				changed = true
			}
		}
	}
	return in
}

// resolved is one thread with its messages (oldest first).
type resolved struct {
	thread Thread
	ask    *c.Ask
	msgs   []c.MessageView
}

// resolveThread finds thread id: an ask's ("ask:<id>", an ask ID, or an
// ask's ThreadID) or a mail thread, reading the thread's messages live.
func (s *Server) resolveThread(ctx context.Context, sn *snap, id string) (*resolved, error) {
	askID := ""
	switch {
	case strings.HasPrefix(id, "ask:"):
		askID = strings.TrimPrefix(id, "ask:")
	case c.ValidID(id, c.PrefixAsk):
		askID = id
	default:
		if i, ok := sn.askThread[id]; ok {
			askID = sn.asks[i].AskID
		}
	}
	if askID != "" {
		api, ok := s.asksAPI()
		if !ok {
			return nil, c.Errorf(c.ErrUnsupported, "This hub has no asks.")
		}
		a, err := api.GetAsk(ctx, c.GetRequest{ID: askID})
		if err != nil {
			return nil, err
		}
		var msgs []c.MessageView
		if k := askMailKey(a); k != "" {
			if msgs, err = s.threadMail(ctx, sn, k); err != nil {
				return nil, err
			}
		}
		t := s.askThread(sn, a, msgs)
		return &resolved{thread: t, ask: &a, msgs: msgs}, nil
	}
	msgs, err := s.threadMail(ctx, sn, id)
	if err != nil {
		return nil, err
	}
	t, ok := s.messageThread(sn, id, msgs)
	if !ok {
		return nil, c.Errorf(c.ErrNotFound, "No conversation %s in your inbox.", id)
	}
	return &resolved{thread: t, msgs: msgs}, nil
}

// threadMail is the owner's mail in thread key: the index's, merged with
// a live read of the hub thread (authoritative for delivery states).
func (s *Server) threadMail(ctx context.Context, sn *snap, key string) ([]c.MessageView, error) {
	byID := map[string]c.MessageView{}
	for _, m := range sn.mail.by[key] {
		byID[m.Envelope.MessageID] = m
	}
	live, err := all(ctx, func(ctx context.Context, cur string) (c.Page[c.MessageView], error) {
		return s.api.ListMessages(ctx, c.ListMessagesRequest{ThreadID: key, PageRequest: c.PageRequest{Cursor: cur, Limit: pageLimit}})
	})
	if err != nil {
		return nil, err
	}
	for _, m := range live {
		if !ownerMail(m) {
			continue
		}
		s.mail.apply(m)
		if old, ok := byID[m.Envelope.MessageID]; !ok || newer(m, old) {
			byID[m.Envelope.MessageID] = m
		}
	}
	out := make([]c.MessageView, 0, len(byID))
	for _, m := range byID {
		out = append(out, m)
	}
	sortMsgs(out)
	return out, nil
}

func (s *Server) handleThread(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := hubCtx(r)
	defer cancel()
	sn, err := s.loadSnap(ctx, need{mail: true, asks: true, attention: true, held: true, projects: true})
	if err != nil {
		writeErr(w, err)
		return
	}
	rt, err := s.resolveThread(ctx, sn, r.PathValue("thread_id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	msgs := make([]Msg, 0, len(rt.msgs))
	for _, m := range rt.msgs {
		askID := ""
		if rt.ask != nil && (m.Envelope.MessageID == rt.ask.MessageID || m.Envelope.MessageID == rt.ask.DecisionMessageID) {
			askID = rt.ask.AskID
		}
		v := sn.msgView(m, askID)
		if askID != "" && m.Envelope.MessageID == rt.ask.MessageID {
			v.Text = stripAskTag(v.Text)
		}
		msgs = append(msgs, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"thread": rt.thread, "messages": msgs})
}

type replyBody struct {
	Text        string `json:"text"`
	OperationID string `json:"operation_id,omitempty"`
}

// handleReply sends the owner's reply to the thread's session: a queue
// message replying to the newest message that session wrote in the thread
// (an ask's carrier when it has not written since), so the agent's reply
// with reply_to lands back in the same thread.
func (s *Server) handleReply(w http.ResponseWriter, r *http.Request) {
	var body replyBody
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		writeErr(w, c.Errorf(c.ErrInvalid, "Write something to send."))
		return
	}
	ctx, cancel := hubCtx(r)
	defer cancel()
	sn, err := s.loadSnap(ctx, need{mail: true, asks: true, attention: true, held: true, projects: true})
	if err != nil {
		writeErr(w, err)
		return
	}
	id := r.PathValue("thread_id")
	rt, err := s.resolveThread(ctx, sn, id)
	if err != nil {
		writeErr(w, err)
		return
	}
	if rt.thread.Session == nil {
		writeErr(w, c.Errorf(c.ErrConflict, "This conversation has no session to reply to."))
		return
	}
	sid := rt.thread.Session.ID
	req := c.SendMessageRequest{Mutation: c.Mutation{OperationID: opID(body.OperationID)},
		Recipient: c.Address{Kind: c.AddrSession, ID: sid}, Mode: c.ModeQueue, Body: c.MessageBody{Text: text}}
	if rt.thread.Project != nil {
		p := rt.thread.Project.ID
		req.ProjectID = &p
	}
	for i := len(rt.msgs) - 1; i >= 0; i-- {
		if fromSession(rt.msgs[i]) == sid {
			req.InReplyTo = rt.msgs[i].Envelope.MessageID
			break
		}
	}
	if rt.ask != nil {
		req.ThreadID = rt.ask.ThreadID
		if req.InReplyTo == "" {
			req.InReplyTo = rt.ask.MessageID
		}
	} else {
		req.ThreadID = id // a thread keyed by its first message gets that key
	}
	mv, err := s.api.SendMessage(ctx, req)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.mail.apply(mv)
	sn.mail.msgs[mv.Envelope.MessageID] = mv
	_ = s.read.mark(rt.thread.ThreadID, s.now())
	s.bus.publish("inbox", struct{}{})
	s.bus.publish("thread", map[string]string{"thread_id": rt.thread.ThreadID})
	writeJSON(w, http.StatusOK, map[string]any{"message": sn.msgView(mv, "")})
}

// handleRead marks a thread read here and its attention items read at the
// hub (so the TUI and phone agree).
func (s *Server) handleRead(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := hubCtx(r)
	defer cancel()
	sn, err := s.loadSnap(ctx, need{mail: true, asks: true, attention: true})
	if err != nil {
		writeErr(w, err)
		return
	}
	id := r.PathValue("thread_id")
	rt, err := s.resolveThread(ctx, sn, id)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.markRead(ctx, sn, rt)
	s.bus.publish("inbox", struct{}{})
	writeJSON(w, http.StatusOK, struct{}{})
}

// markRead marks thread rt read here and its open attention items read at
// the hub.
func (s *Server) markRead(ctx context.Context, sn *snap, rt *resolved) {
	if err := s.read.mark(rt.thread.ThreadID, s.now()); err != nil {
		s.logf("pyre web: saving read marks: %v", err)
	}
	ids := map[string]bool{}
	for _, m := range rt.msgs {
		ids[m.Envelope.MessageID] = true
	}
	changed := false
	for i, it := range sn.attention {
		if it.State != c.AttnOpen {
			continue
		}
		hit := (it.Object.Kind == c.KindThread && it.Object.ID == rt.thread.ThreadID) || (it.Object.Kind == c.KindMessage && ids[it.Object.ID])
		if rt.ask != nil {
			hit = hit || it.AttentionID == rt.ask.AttentionID || (it.Object.Kind == c.KindAsk && it.Object.ID == rt.ask.AskID)
		}
		if !hit {
			continue
		}
		if _, err := s.api.UpdateAttention(ctx, c.UpdateAttentionRequest{Mutation: c.Mutation{OperationID: opID("")},
			AttentionID: it.AttentionID, State: c.AttnRead}); err != nil {
			s.logf("pyre web: marking %s read: %v", it.AttentionID, err)
			continue
		}
		if !changed { // the snapshot's slice is the cache's: copy before editing
			sn.attention = append([]c.AttentionItem(nil), sn.attention...)
		}
		sn.attention[i].State = c.AttnRead
		changed = true
	}
	if changed {
		s.attention.invalidate()
	}
}
