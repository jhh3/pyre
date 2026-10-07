package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// rpcError is a JSON-RPC error response. It is definitive: the server
// received and refused the request.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("codex app-server error %d: %s", e.Code, e.Message)
}

// errConnLost means the outcome of an in-flight request is unknown.
var errConnLost = errors.New("codex app-server connection lost")

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpcClient speaks JSON-RPC 2.0 to `codex app-server --listen unix://…`: an
// HTTP/1.1 WebSocket upgrade on the unix socket, one message per text frame.
type rpcClient struct {
	conn   *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	nextID  int64
	pending map[string]chan rpcMessage
	closed  bool
	err     error
	done    chan struct{}

	onNotify  func(method string, params json.RawMessage)
	onRequest func(id json.RawMessage, method string, params json.RawMessage)
}

// dialRPC connects to the socket and starts the reader.
func dialRPC(ctx context.Context, sock string, onNotify func(string, json.RawMessage), onRequest func(json.RawMessage, string, json.RawMessage)) (*rpcClient, error) {
	hc := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(dctx, "ws://localhost/", &websocket.DialOptions{HTTPClient: hc})
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(64 << 20)
	cctx, ccancel := context.WithCancel(context.Background())
	c := &rpcClient{conn: conn, ctx: cctx, cancel: ccancel, pending: map[string]chan rpcMessage{},
		done: make(chan struct{}), onNotify: onNotify, onRequest: onRequest}
	go c.readLoop()
	return c, nil
}

func (c *rpcClient) readLoop() {
	defer close(c.done)
	for {
		_, data, err := c.conn.Read(c.ctx)
		if err != nil {
			c.fail(err)
			return
		}
		var m rpcMessage
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		switch {
		case m.Method == "" && len(m.ID) > 0: // response
			c.mu.Lock()
			ch := c.pending[string(m.ID)]
			delete(c.pending, string(m.ID))
			c.mu.Unlock()
			if ch != nil {
				ch <- m
			}
		case m.Method != "" && len(m.ID) > 0: // server -> client request
			if c.onRequest != nil {
				c.onRequest(m.ID, m.Method, m.Params)
			}
		case m.Method != "":
			if c.onNotify != nil {
				c.onNotify(m.Method, m.Params)
			}
		}
	}
}

func (c *rpcClient) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed, c.err = true, err
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
}

// alive reports whether the connection is usable.
func (c *rpcClient) alive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.closed
}

func (c *rpcClient) write(ctx context.Context, m rpcMessage) error {
	m.JSONRPC = "2.0"
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return c.conn.Write(wctx, websocket.MessageText, b)
}

// call sends a request and waits for its response. A JSON-RPC error is
// returned as *rpcError (definitive); a lost connection or timeout after the
// write is errConnLost (outcome unknown).
func (c *rpcClient) call(ctx context.Context, method string, params any, out any) error {
	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		raw = b
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("%w (before send)", errConnLost)
	}
	c.nextID++
	id := json.RawMessage(strconv.FormatInt(c.nextID, 10))
	ch := make(chan rpcMessage, 1)
	c.pending[string(id)] = ch
	c.mu.Unlock()
	if err := c.write(ctx, rpcMessage{ID: id, Method: method, Params: raw}); err != nil {
		c.mu.Lock()
		delete(c.pending, string(id))
		c.mu.Unlock()
		return fmt.Errorf("%w: write %s: %v", errConnLost, method, err)
	}
	select {
	case m, ok := <-ch:
		if !ok {
			return fmt.Errorf("%w during %s", errConnLost, method)
		}
		if m.Error != nil {
			return m.Error
		}
		if out != nil && len(m.Result) > 0 {
			return json.Unmarshal(m.Result, out)
		}
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, string(id))
		c.mu.Unlock()
		return fmt.Errorf("%w: %s: %v", errConnLost, method, ctx.Err())
	}
}

func (c *rpcClient) notify(ctx context.Context, method string, params any) error {
	var raw json.RawMessage
	if params != nil {
		raw, _ = json.Marshal(params)
	}
	return c.write(ctx, rpcMessage{Method: method, Params: raw})
}

// respond answers a server -> client request.
func (c *rpcClient) respond(ctx context.Context, id json.RawMessage, result any) error {
	b, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return c.write(ctx, rpcMessage{ID: id, Result: b})
}

func (c *rpcClient) close() {
	c.fail(errors.New("closed"))
	_ = c.conn.Close(websocket.StatusNormalClosure, "")
	c.cancel()
}
