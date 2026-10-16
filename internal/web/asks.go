package web

import (
	"context"
	"net/http"
	"strings"

	c "github.com/jhh3/pyre/internal/contracts"
)

type answerBody struct {
	OptionID    string `json:"option_id,omitempty"`
	Text        string `json:"text,omitempty"`
	Mode        string `json:"mode,omitempty"` // "now" (default) | "hold"
	OperationID string `json:"operation_id,omitempty"`
}

// errNoHold is the answer when the hub has no held-answer queue.
func errNoHold() error {
	return c.Errorf(c.ErrUnsupported, "This hub cannot hold answers yet; send the answer now instead.")
}

// handleAnswer answers an ask now (AnswerAsks, one item) or holds the
// answer in the hub's batch queue.
func (s *Server) handleAnswer(w http.ResponseWriter, r *http.Request) {
	var body answerBody
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	item := c.AnswerAskItem{AskID: r.PathValue("ask_id"), OptionID: strings.TrimSpace(body.OptionID), Text: strings.TrimSpace(body.Text)}
	if item.OptionID == "" && item.Text == "" {
		writeErr(w, c.Errorf(c.ErrInvalid, "Pick an option or write an answer."))
		return
	}
	api, ok := s.asksAPI()
	if !ok {
		writeErr(w, c.Errorf(c.ErrUnsupported, "This hub has no asks."))
		return
	}
	ctx, cancel := hubCtx(r)
	defer cancel()
	var (
		ask       c.Ask
		held      c.HeldAnswers
		heldKnown bool
	)
	switch body.Mode {
	case "", "now":
		resp, err := api.AnswerAsks(ctx, c.AnswerAsksRequest{Mutation: c.Mutation{OperationID: opID(body.OperationID)}, Answers: []c.AnswerAskItem{item}})
		if err != nil {
			writeErr(w, err)
			return
		}
		if len(resp.Results) != 1 {
			writeErr(w, c.Errorf(c.ErrInternal, "The hub returned %d results for one answer.", len(resp.Results)))
			return
		}
		res := resp.Results[0]
		if res.Error != nil {
			writeErr(w, res.Error)
			return
		}
		if res.Ask != nil {
			ask = *res.Ask
		} else if ask, err = api.GetAsk(ctx, c.GetRequest{ID: item.AskID}); err != nil {
			writeErr(w, err)
			return
		}
	case "hold":
		h, ok := s.heldAPI()
		if !ok {
			writeErr(w, errNoHold())
			return
		}
		hv, err := h.HoldAnswer(ctx, c.HoldAnswerRequest{Mutation: c.Mutation{OperationID: opID(body.OperationID)}, AnswerAskItem: item})
		if err != nil {
			writeErr(w, err)
			return
		}
		held, heldKnown = normHeld(hv), true
		s.held.set(held, s.now())
		if ask, err = api.GetAsk(ctx, c.GetRequest{ID: item.AskID}); err != nil {
			writeErr(w, err)
			return
		}
	default:
		writeErr(w, c.Errorf(c.ErrInvalid, "mode must be now or hold"))
		return
	}
	s.asks.invalidate()
	_ = s.read.mark(askThreadID(ask), s.now())
	if !heldKnown {
		var err error
		if held, err = s.freshHeld(ctx); err != nil {
			writeErr(w, err)
			return
		}
	}
	s.bus.publish("inbox", struct{}{})
	writeJSON(w, http.StatusOK, map[string]any{"ask": ask, "held": held})
}

// freshHeld reads the held queue from the hub now.
func (s *Server) freshHeld(ctx context.Context) (c.HeldAnswers, error) {
	h, ok := s.heldAPI()
	if !ok {
		return emptyHeld(), nil
	}
	held, err := h.ListHeldAnswers(ctx, struct{}{})
	if c.IsCode(err, c.ErrUnsupported) || c.IsCode(err, c.ErrNotFound) {
		return emptyHeld(), nil
	}
	if err != nil {
		return c.HeldAnswers{}, err
	}
	held = normHeld(held)
	s.held.set(held, s.now())
	return held, nil
}

func normHeld(h c.HeldAnswers) c.HeldAnswers {
	if h.Items == nil {
		h.Items = []c.HeldAnswer{}
	}
	return h
}

func (s *Server) handleHeldUndo(w http.ResponseWriter, r *http.Request) {
	h, ok := s.heldAPI()
	if !ok {
		writeErr(w, errNoHold())
		return
	}
	var body struct {
		OperationID string `json:"operation_id,omitempty"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	ctx, cancel := hubCtx(r)
	defer cancel()
	held, err := h.UnholdAnswer(ctx, c.UnholdAnswerRequest{Mutation: c.Mutation{OperationID: opID(body.OperationID)}, AskID: r.PathValue("ask_id")})
	if c.IsCode(err, c.ErrNotFound) {
		err = c.Errorf(c.ErrNotFound, "That answer is no longer held: it was already sent (or taken back).")
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	held = normHeld(held)
	s.held.set(held, s.now())
	s.bus.publish("inbox", struct{}{})
	writeJSON(w, http.StatusOK, map[string]any{"held": held})
}

func (s *Server) handleHeldSend(w http.ResponseWriter, r *http.Request) {
	h, ok := s.heldAPI()
	if !ok {
		writeErr(w, errNoHold())
		return
	}
	var body struct {
		OperationID string `json:"operation_id,omitempty"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	ctx, cancel := hubCtx(r)
	defer cancel()
	resp, err := h.FlushHeldAnswers(ctx, c.FlushHeldAnswersRequest{Mutation: c.Mutation{OperationID: opID(body.OperationID)}})
	if err != nil {
		writeErr(w, err)
		return
	}
	s.asks.invalidate()
	held, err := s.freshHeld(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	results := resp.Results
	if results == nil {
		results = []c.AnswerAskResult{}
	}
	s.bus.publish("inbox", struct{}{})
	writeJSON(w, http.StatusOK, map[string]any{"results": results, "held": held})
}
