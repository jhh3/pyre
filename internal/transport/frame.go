package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/coder/websocket"

	"github.com/jhh3/pyre/internal/contracts"
)

// DefaultMaxFrameBytes bounds one node-protocol frame.
const DefaultMaxFrameBytes = 4 << 20

func writeFrame(ctx context.Context, c *websocket.Conn, timeout time.Duration, f contracts.NodeFrame) error {
	if f.At.IsZero() {
		f.At = time.Now().UTC()
	}
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return c.Write(wctx, websocket.MessageText, b)
}

func readFrame(ctx context.Context, c *websocket.Conn) (contracts.NodeFrame, error) {
	var f contracts.NodeFrame
	typ, b, err := c.Read(ctx)
	if err != nil {
		return f, err
	}
	if typ != websocket.MessageText {
		return f, contracts.Errorf(contracts.ErrInvalid, "node frames must be JSON text messages")
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return f, contracts.Errorf(contracts.ErrInvalid, "malformed node frame: %v", err)
	}
	if f.Type == "" {
		return f, contracts.Errorf(contracts.ErrInvalid, "node frame without type")
	}
	return f, nil
}

func errorFrame(e *contracts.Error) contracts.NodeFrame {
	return contracts.NodeFrame{Type: contracts.FrameError, Error: e}
}

// closeCodeFor maps a fatal error to a WebSocket close status.
func closeCodeFor(e *contracts.Error) websocket.StatusCode {
	if e == nil {
		return websocket.StatusNormalClosure
	}
	switch e.Code {
	case contracts.ErrUnauthorized, contracts.ErrForbidden, contracts.ErrProtocolMismatch, contracts.ErrConflict:
		return websocket.StatusPolicyViolation
	case contracts.ErrInvalid:
		return websocket.StatusProtocolError
	case contracts.ErrUnavailable:
		return websocket.StatusTryAgainLater
	}
	return websocket.StatusInternalError
}

// closeReason trims a reason to the WebSocket 123-byte limit.
func closeReason(e *contracts.Error) string {
	if e == nil {
		return ""
	}
	s := fmt.Sprintf("%s: %s", e.Code, Redact(e.Message))
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}

// NegotiateProtocol picks the highest protocol both peers support, or
// returns a protocol_mismatch error with an actionable message.
func NegotiateProtocol(peerMax, peerMin int) (int, *contracts.Error) {
	if peerMin <= 0 {
		peerMin = peerMax
	}
	chosen := min(peerMax, contracts.ProtocolVersion)
	if chosen >= max(peerMin, contracts.MinProtocolVersion) && chosen > 0 {
		return chosen, nil
	}
	var action string
	if peerMin > contracts.ProtocolVersion {
		action = "upgrade the hub (pyre-hub) to a release that speaks this protocol, or install a matching older pyred on the node"
	} else {
		action = "upgrade pyred on this node to the hub's release (e.g. re-run the bootstrap/update for this node)"
	}
	e := contracts.Errorf(contracts.ErrProtocolMismatch,
		"node speaks protocol %d (min %d) but the hub supports %d..%d: %s",
		peerMax, peerMin, contracts.MinProtocolVersion, contracts.ProtocolVersion, action)
	e.WithDetail("node_protocol", peerMax).
		WithDetail("node_min_protocol", peerMin).
		WithDetail("hub_protocol", contracts.ProtocolVersion).
		WithDetail("hub_min_protocol", contracts.MinProtocolVersion).
		WithDetail("action", action)
	return 0, e
}
