// Package wire is the termhost wire protocol and the small helpers the
// client (termhost) and the host process (termhost/hostproc) share. It
// must stay light: pyred links it on every hook invocation.
package wire

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Wire protocol (AF_UNIX stream socket in a private directory).
//
// Every connection starts with one JSON request line. A control request
// gets one JSON response line and the connection closes. An "attach"
// request gets a JSON response line; then both sides exchange frames:
//
//	type (1 byte) | length (uint32 big endian) | payload
//
// host -> client: FrameOutput (terminal bytes), FrameSize (cols, rows as
// two uint16: the session's size, sent on every change), FrameExit (the
// session is gone; payload is a reason).
// client -> host: FrameInput (bytes typed; dropped for read-only
// clients), FrameSize (the client's terminal size; applied only for a
// writable client without ignore-size).

// ProtocolVersion is bumped on incompatible wire changes.
const ProtocolVersion = 1

const (
	FrameOutput byte = 'o'
	FrameInput  byte = 'i'
	FrameSize   byte = 's'
	FrameExit   byte = 'x'
)

// Limits.
const (
	MaxRequestBytes = 16 << 20 // a paste can be long
	MaxFrameBytes   = 1 << 20
)

// Ops.
const (
	OpPing      = "ping"
	OpNew       = "new"
	OpRespawn   = "respawn"
	OpHas       = "has"
	OpList      = "list"
	OpClients   = "clients"
	OpCapture   = "capture"
	OpPaste     = "paste"
	OpKeys      = "keys"
	OpKill      = "kill"
	OpTerminate = "terminate"
	OpPane      = "pane"
	OpResize    = "resize"
	OpShutdown  = "shutdown"
	OpAttach    = "attach"
)

// Error codes.
const (
	CodeNoSession = "no_session"
	CodeInvalid   = "invalid"
	CodeConflict  = "conflict"
	CodeFailed    = "failed"
)

type Spec struct {
	Argv   []string          `json:"argv"`
	Cwd    string            `json:"cwd,omitempty"`
	Env    map[string]string `json:"env,omitempty"` // secrets: never logged
	Unset  []string          `json:"unset,omitempty"`
	Width  int               `json:"width,omitempty"`
	Height int               `json:"height,omitempty"`
}

type Request struct {
	V    int      `json:"v"`
	Op   string   `json:"op"`
	Name string   `json:"name,omitempty"`
	Spec *Spec    `json:"spec,omitempty"`
	Keys []string `json:"keys,omitempty"`
	Text []byte   `json:"text,omitempty"`

	History int  `json:"history,omitempty"`
	Escapes bool `json:"escapes,omitempty"`
	Join    bool `json:"join,omitempty"`

	Cols       int   `json:"cols,omitempty"`
	Rows       int   `json:"rows,omitempty"`
	ReadOnly   bool  `json:"read_only,omitempty"`
	IgnoreSize bool  `json:"ignore_size,omitempty"`
	PID        int   `json:"pid,omitempty"`
	GraceMS    int64 `json:"grace_ms,omitempty"`
}

type Client struct {
	ID       string `json:"id"`
	PID      int    `json:"pid,omitempty"`
	ReadOnly bool   `json:"read_only,omitempty"`
	Flags    string `json:"flags,omitempty"`
}

type Pane struct {
	ID      string `json:"id"`
	PID     int    `json:"pid"`
	Dead    bool   `json:"dead"`
	Exit    *int   `json:"exit,omitempty"`
	Command string `json:"command,omitempty"`
	Width   int    `json:"width"`
	Height  int    `json:"height"`
}

type Response struct {
	OK      bool     `json:"ok"`
	Code    string   `json:"code,omitempty"`
	Err     string   `json:"err,omitempty"`
	V       int      `json:"v,omitempty"`
	PID     int      `json:"pid,omitempty"`
	Backend string   `json:"backend,omitempty"`
	Exists  bool     `json:"exists,omitempty"`
	Names   []string `json:"names,omitempty"`
	Clients []Client `json:"clients,omitempty"`
	Pane    *Pane    `json:"pane,omitempty"`
	Text    string   `json:"text,omitempty"`
	Cols    int      `json:"cols,omitempty"`
	Rows    int      `json:"rows,omitempty"`
}

func ErrResponse(code string, err error) Response {
	return Response{Code: code, Err: err.Error()}
}

// ReadLine reads one JSON line of at most max bytes.
func ReadLine(r *bufio.Reader, max int, v any) error {
	var line []byte
	for {
		chunk, isPrefix, err := r.ReadLine()
		if err != nil {
			return err
		}
		line = append(line, chunk...)
		if len(line) > max {
			return errors.New("termhost: request too large")
		}
		if !isPrefix {
			break
		}
	}
	return json.Unmarshal(line, v)
}

func WriteLine(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

func WriteFrame(w io.Writer, typ byte, payload []byte) error {
	var hdr [5]byte
	hdr[0] = typ
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if len(payload) == 0 {
		_, err := w.Write(hdr[:])
		return err
	}
	// One write per frame keeps frames whole on a shared writer.
	buf := make([]byte, 0, 5+len(payload))
	buf = append(append(buf, hdr[:]...), payload...)
	_, err := w.Write(buf)
	return err
}

func ReadFrame(r io.Reader, buf []byte) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > MaxFrameBytes {
		return 0, nil, fmt.Errorf("termhost: frame of %d bytes exceeds the limit", n)
	}
	if int(n) > cap(buf) {
		buf = make([]byte, n)
	}
	buf = buf[:n]
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, nil, err
	}
	return hdr[0], buf, nil
}

func SizePayload(cols, rows int) []byte {
	var b [4]byte
	binary.BigEndian.PutUint16(b[:2], uint16(cols))
	binary.BigEndian.PutUint16(b[2:], uint16(rows))
	return b[:]
}

func ParseSize(b []byte) (cols, rows int, ok bool) {
	if len(b) != 4 {
		return 0, 0, false
	}
	return int(binary.BigEndian.Uint16(b[:2])), int(binary.BigEndian.Uint16(b[2:])), true
}

// ClampDims keeps a size usable.
func ClampDims(cols, rows int) (int, int) {
	return min(max(cols, 2), 1000), min(max(rows, 1), 1000)
}
