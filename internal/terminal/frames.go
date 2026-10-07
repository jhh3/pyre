// Package terminal implements Pyre's authenticated web-terminal relay
// (contracts.PathTerminal / contracts.PathNodeTerminal).
//
// Shape of one stream:
//
//		client ──wss /v1/terminal──▶ hub Relay ◀──wss /v1/node/terminal── node NodeBridge ── PTY: tmux attach
//
//	 1. sessions.attach preferring web_terminal returns a descriptor whose
//	    one-time ticket the hub minted through Relay.IssueTerminalTicket.
//	 2. The client opens PathTerminal (no bearer needed) and sends
//	    {"type":"auth","ticket":...,"cols":N,"rows":N} as its first text frame.
//	 3. The relay redeems the ticket exactly once, picks a random stream ID
//	    and queues a terminal_open command (TerminalOpenCommand) to the run's
//	    node on the hub's durable command path with a short expiry.
//	 4. The node's NodeBridge starts the run's own attach argv (the isolated
//	    tmux socket of that run; -r when read-only) in a PTY and dials
//	    PathNodeTerminal?stream=<id> with its node bearer token.
//	 5. The relay pairs the two sockets: binary frames carry raw terminal
//	    bytes both ways; client resize text frames are forwarded to the node.
//
// Closing either side closes the other. The node kills only its `tmux
// attach` client process, never the tmux server or the harness, so a
// detached web terminal leaves the run running. Tickets and terminal bytes
// are never logged.
package terminal

import (
	"context"
	"encoding/json"
	"errors"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"

	"github.com/jhh3/pyre/internal/contracts"
)

// CmdTerminalOpen is the hub->node command carrying a
// contracts.TerminalOpenCommand. (Pending a contracts constant; the wire
// value is fixed.)
const CmdTerminalOpen contracts.CommandKind = "terminal_open"

// TicketPurpose is the TokenIssuer purpose of web-terminal tickets.
const TicketPurpose = "terminal"

// Frame type names (text frames; terminal bytes travel in binary frames).
const (
	FrameAuth   = "auth"
	FrameResize = "resize"
	FrameError  = "error"
)

// Frame is a text control frame on either socket.
type Frame struct {
	Type   string           `json:"type"`
	Ticket string           `json:"ticket,omitempty"` // auth only; never log
	Cols   int              `json:"cols,omitempty"`
	Rows   int              `json:"rows,omitempty"`
	Error  *contracts.Error `json:"error,omitempty"`
}

// Size limits for terminal dimensions.
const (
	DefaultCols = 80
	DefaultRows = 24
	MaxDim      = 1000
)

// ClampSize returns a usable terminal size: zero/negative values pick the
// defaults and values above MaxDim are capped.
func ClampSize(cols, rows int) (int, int) {
	if cols <= 0 {
		cols = DefaultCols
	}
	if rows <= 0 {
		rows = DefaultRows
	}
	if cols > MaxDim {
		cols = MaxDim
	}
	if rows > MaxDim {
		rows = MaxDim
	}
	return cols, rows
}

// CloseCode maps an error to the WebSocket close code sent after the error
// frame: 4000 + the HTTP status of its code (4401, 4403, 4410, 4503, ...),
// matching the event stream's 4410/4500 convention.
func CloseCode(e *contracts.Error) websocket.StatusCode {
	if e == nil {
		return websocket.StatusNormalClosure
	}
	return websocket.StatusCode(4000 + e.Code.HTTPStatus())
}

const writeTimeout = 10 * time.Second

// writeJSON sends a text frame.
func writeJSON(ctx context.Context, ws *websocket.Conn, f Frame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return ws.Write(wctx, websocket.MessageText, b)
}

// closeWithError sends {"type":"error"} and closes with CloseCode(e).
func closeWithError(ws *websocket.Conn, e *contracts.Error) {
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	_ = writeJSON(ctx, ws, Frame{Type: FrameError, Error: e})
	_ = ws.Close(CloseCode(e), truncateReason(e.Message))
}

// truncateReason keeps a close reason within the 123-byte limit.
func truncateReason(s string) string {
	if len(s) <= 120 {
		return s
	}
	s = s[:120]
	for !utf8.ValidString(s) && len(s) > 0 {
		s = s[:len(s)-1]
	}
	return s
}

// parseFrame decodes a text control frame.
func parseFrame(b []byte) (Frame, error) {
	var f Frame
	if err := json.Unmarshal(b, &f); err != nil {
		return f, contracts.Errorf(contracts.ErrInvalid, "malformed control frame")
	}
	if f.Type == "" {
		return f, contracts.Errorf(contracts.ErrInvalid, "control frame has no type")
	}
	return f, nil
}

// isNormalClose reports whether err is an orderly close or a dropped
// connection rather than a protocol failure worth reporting.
func isNormalClose(err error) bool {
	if err == nil {
		return true
	}
	switch websocket.CloseStatus(err) {
	case websocket.StatusNormalClosure, websocket.StatusGoingAway, websocket.StatusNoStatusRcvd:
		return true
	}
	return errors.Is(err, context.Canceled)
}
