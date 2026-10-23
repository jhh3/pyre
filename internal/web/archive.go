package web

import (
	"context"
	"net/http"
	"strings"

	c "github.com/jhh3/pyre/internal/contracts"
)

// maxArchive bounds one bulk archive request.
const maxArchive = 200

// errOpenAsk is the answer to archiving a question still waiting on the
// owner without saying to dismiss it (a bulk archive never dismisses).
func errOpenAsk() error {
	return c.Errorf(c.ErrConflict, "Answer it, or dismiss it: open questions are archived only by dismissing them")
}

// archive files thread id away like email: every session message in it
// that nobody acknowledged is acknowledged by the owner (the hub's
// messages.ack, as `pyre ack` does), and its attention items are marked
// read. pendingFromSession then treats the thread as answered, so it moves
// from the open box to done. An open ask is refused; a decided ask's
// thread only has its follow-ups acknowledged (usually none: a no-op).
// With dismiss, an open ask is dismissed first (withdrawn by the owner
// with contracts.AskDismissReason: no option, the default never applies,
// the session is told); a session item's thread (attention.go) is marked
// handled.
func (s *Server) archive(ctx context.Context, sn *snap, id, op string, dismiss bool) (Thread, error) {
	if c.ValidID(id, c.PrefixAttention) {
		rt, err := s.resolveAttention(ctx, sn, id, false) // dismissing needs no conversation
		if err != nil {
			return Thread{}, err
		}
		return s.dismissAttention(ctx, sn, rt, op)
	}
	rt, err := s.resolveThread(ctx, sn, id)
	if err != nil {
		return Thread{}, err
	}
	skip := ""
	if rt.ask != nil {
		if rt.ask.State == c.AskOpen {
			if !dismiss {
				return Thread{}, errOpenAsk()
			}
			if err := s.dismissAsk(ctx, rt, op); err != nil {
				return Thread{}, err
			}
		}
		skip = rt.ask.MessageID
	}
	acked, err := s.ackSessionMail(ctx, sn, rt, skip, op, "archived in the web inbox")
	if err != nil {
		return Thread{}, err
	}
	s.markRead(ctx, sn, rt)
	t := rt.thread
	if rt.ask != nil {
		t = s.askThread(sn, *rt.ask, rt.msgs)
	} else if mt, ok := s.messageThread(sn, id, rt.msgs); ok {
		t = mt
	}
	if acked {
		s.bus.publish("thread", map[string]string{"thread_id": t.ThreadID})
	}
	return t, nil
}

// ackSessionMail acknowledges, as the owner (the hub's messages.ack), every
// session message in rt nobody acknowledged except skip (an ask's carrier,
// which closes with its ask). It reports whether it acknowledged any.
func (s *Server) ackSessionMail(ctx context.Context, sn *snap, rt *resolved, skip, op, note string) (bool, error) {
	acked := false
	for i, m := range rt.msgs {
		mid := m.Envelope.MessageID
		if fromSession(m) == "" || mid == skip || m.Status.Ack != c.AckNone {
			continue
		}
		oid := ""
		if op != "" {
			oid = op + "-" + mid
		}
		mv, err := s.api.AckMessage(ctx, c.AckMessageRequest{Mutation: c.Mutation{OperationID: opID(oid)}, MessageID: mid, Note: note})
		if err != nil {
			return acked, err
		}
		s.mail.apply(mv)
		sn.mail.msgs[mid] = mv
		rt.msgs[i] = mv
		acked = true
	}
	return acked, nil
}

// dismissAsk withdraws rt's open ask as the owner and refreshes rt.ask.
func (s *Server) dismissAsk(ctx context.Context, rt *resolved, op string) error {
	api, ok := s.asksAPI()
	if !ok {
		return c.Errorf(c.ErrUnsupported, "This hub has no asks.")
	}
	oid := ""
	if op != "" {
		oid = op + "-dismiss"
	}
	a, err := api.WithdrawAsk(ctx, c.WithdrawAskRequest{Mutation: c.Mutation{OperationID: opID(oid)}, AskID: rt.ask.AskID, Reason: c.AskDismissReason})
	if c.IsCode(err, c.ErrConflict) {
		a, err = api.GetAsk(ctx, c.GetRequest{ID: rt.ask.AskID}) // decided meanwhile: that decision stands
	}
	if err != nil {
		return err
	}
	rt.ask = &a
	s.asks.invalidate()
	return nil
}

type archiveBody struct {
	OperationID string `json:"operation_id,omitempty"`
	// Dismiss allows archiving an open ask: it is dismissed (withdrawn,
	// default not applied). Without it an open ask is refused.
	Dismiss bool `json:"dismiss,omitempty"`
}

// handleArchive archives one thread: {thread}.
func (s *Server) handleArchive(w http.ResponseWriter, r *http.Request) {
	var body archiveBody
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	ctx, cancel := hubCtx(r)
	defer cancel()
	sn, err := s.loadSnap(ctx, need{mail: true, asks: true, attention: true, held: true, projects: true})
	if err != nil {
		writeErr(w, err)
		return
	}
	t, err := s.archive(ctx, sn, r.PathValue("thread_id"), strings.TrimSpace(body.OperationID), body.Dismiss)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.bus.publish("inbox", struct{}{})
	writeJSON(w, http.StatusOK, map[string]any{"thread": t})
}

type archiveManyBody struct {
	ThreadIDs   []string `json:"thread_ids"`
	OperationID string   `json:"operation_id,omitempty"`
}

// ArchiveResult is one thread's outcome in a bulk archive.
type ArchiveResult struct {
	ThreadID string   `json:"thread_id"`
	Thread   *Thread  `json:"thread,omitempty"`
	Error    *c.Error `json:"error,omitempty"`
}

// handleArchiveMany archives several threads with the same rules as
// handleArchive: {results:[ArchiveResult]} in request order.
func (s *Server) handleArchiveMany(w http.ResponseWriter, r *http.Request) {
	var body archiveManyBody
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	if len(body.ThreadIDs) == 0 {
		writeErr(w, c.Errorf(c.ErrInvalid, "Choose at least one conversation to archive."))
		return
	}
	if len(body.ThreadIDs) > maxArchive {
		writeErr(w, c.Errorf(c.ErrInvalid, "Archive at most %d conversations at a time.", maxArchive))
		return
	}
	ctx, cancel := hubCtx(r)
	defer cancel()
	sn, err := s.loadSnap(ctx, need{mail: true, asks: true, attention: true, held: true, projects: true})
	if err != nil {
		writeErr(w, err)
		return
	}
	op := strings.TrimSpace(body.OperationID)
	results := make([]ArchiveResult, 0, len(body.ThreadIDs))
	seen := map[string]bool{}
	for _, id := range body.ThreadIDs {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		res := ArchiveResult{ThreadID: id}
		if t, err := s.archive(ctx, sn, id, op, false); err != nil {
			e := c.AsError(err)
			res.Error = &c.Error{Code: e.Code, Message: plainError(e), Retryable: e.Retryable}
		} else {
			res.Thread = &t
		}
		results = append(results, res)
	}
	s.bus.publish("inbox", struct{}{})
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}
