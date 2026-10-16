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
// owner: it ends by an answer or its default, not by filing it away.
func errOpenAsk() error {
	return c.Errorf(c.ErrConflict, "Answer it or let it default; open questions can't be archived")
}

// archive files thread id away like email: every session message in it
// that nobody acknowledged is acknowledged by the owner (the hub's
// messages.ack, as `pyre ack` does), and its attention items are marked
// read. pendingFromSession then treats the thread as answered, so it moves
// from the open box to done. An open ask is refused; a decided ask's
// thread only has its follow-ups acknowledged (usually none: a no-op).
func (s *Server) archive(ctx context.Context, sn *snap, id, op string) (Thread, error) {
	rt, err := s.resolveThread(ctx, sn, id)
	if err != nil {
		return Thread{}, err
	}
	skip := ""
	if rt.ask != nil {
		if rt.ask.State == c.AskOpen {
			return Thread{}, errOpenAsk()
		}
		skip = rt.ask.MessageID
	}
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
		mv, err := s.api.AckMessage(ctx, c.AckMessageRequest{Mutation: c.Mutation{OperationID: opID(oid)}, MessageID: mid, Note: "archived in the web inbox"})
		if err != nil {
			return Thread{}, err
		}
		s.mail.apply(mv)
		sn.mail.msgs[mid] = mv
		rt.msgs[i] = mv
		acked = true
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

type archiveBody struct {
	OperationID string `json:"operation_id,omitempty"`
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
	t, err := s.archive(ctx, sn, r.PathValue("thread_id"), strings.TrimSpace(body.OperationID))
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
		if t, err := s.archive(ctx, sn, id, op); err != nil {
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
