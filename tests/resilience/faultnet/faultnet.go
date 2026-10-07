// Package faultnet is a small TCP fault-injection proxy for resilience
// tests. It sits between a client and an upstream server on 127.0.0.1 and
// can:
//
//   - pass traffic through unchanged (ModePass);
//   - partition: accept connections but forward nothing, so peers see
//     silence rather than a reset (ModeBlackhole) — heal with Heal, which
//     also cuts the stale connections;
//   - refuse: accept and immediately close new connections (ModeRefuse);
//   - delay every forwarded chunk or frame (SetDelay);
//   - cut every live connection at once (CutAll);
//   - in WebSocket mode (SetFrameFilter), relay the HTTP upgrade and then
//     parse RFC 6455 frames in both directions so a filter can drop,
//     duplicate, delay or cut on individual frames (payloads are unmasked
//     for inspection; the original bytes are forwarded).
//
// TLS cannot be inspected, so frame-level faults need a plaintext (ws://)
// hub; byte-level faults work for TLS too.
package faultnet

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Mode is the proxy's connection policy.
type Mode int

const (
	ModePass Mode = iota
	ModeBlackhole
	ModeRefuse
)

// Direction of a forwarded chunk or frame.
type Direction int

const (
	ClientToServer Direction = iota
	ServerToClient
)

func (d Direction) String() string {
	if d == ClientToServer {
		return "c2s"
	}
	return "s2c"
}

// Frame is one parsed WebSocket frame.
type Frame struct {
	Dir     Direction
	Fin     bool
	Opcode  byte   // 1 text, 2 binary, 8 close, 9 ping, 10 pong, 0 continuation
	Payload []byte // unmasked copy
	raw     []byte
}

// IsData reports whether the frame carries application data.
func (f Frame) IsData() bool { return f.Opcode == 1 || f.Opcode == 2 || f.Opcode == 0 }

// Action is a FrameFilter's decision.
type Action struct {
	Drop      bool
	Duplicate bool          // forward the frame twice
	Delay     time.Duration // sleep before forwarding (blocks that direction)
	// CutAfter closes this connection after the frame is handled.
	CutAfter bool
}

// FrameFilter decides what to do with each frame. It is called from the
// per-direction relay goroutines and must be safe for concurrent use.
type FrameFilter func(f Frame) Action

// Stats counts proxy activity.
type Stats struct {
	Accepted    int64 // connection attempts accepted by the listener
	Forwarded   int64 // chunks/frames forwarded
	Dropped     int64
	Duplicated  int64
	Cut         int64 // connections closed by CutAll/CutAfter/heal
	BytesC2S    int64
	BytesS2C    int64
	Refused     int64
	Blackholed  int64
	FramesC2S   int64
	FramesS2C   int64
	ActiveConns int64
}

// Proxy is a running fault proxy.
type Proxy struct {
	ln       net.Listener
	upstream string

	mu     sync.Mutex
	mode   Mode
	delay  time.Duration
	filter FrameFilter
	conns  map[*pair]struct{}
	closed bool

	accepted, forwarded, dropped, duplicated, cut atomic.Int64
	bytesC2S, bytesS2C, refused, blackholed       atomic.Int64
	framesC2S, framesS2C                          atomic.Int64

	wg sync.WaitGroup
}

type pair struct {
	client, server net.Conn
	once           sync.Once
}

func (p *pair) close() {
	p.once.Do(func() {
		if tc, ok := p.client.(*net.TCPConn); ok {
			_ = tc.SetLinger(0) // RST, like a dropped route
		}
		_ = p.client.Close()
		if p.server != nil {
			if tc, ok := p.server.(*net.TCPConn); ok {
				_ = tc.SetLinger(0)
			}
			_ = p.server.Close()
		}
	})
}

// New listens on 127.0.0.1:0 and forwards to upstream ("host:port").
func New(upstream string) (*Proxy, error) {
	return Listen("127.0.0.1:0", upstream)
}

// Listen is New with an explicit listen address (e.g. to keep a stable
// port across a simulated restart).
func Listen(addr, upstream string) (*Proxy, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	p := &Proxy{ln: ln, upstream: upstream, conns: map[*pair]struct{}{}}
	p.wg.Add(1)
	go p.acceptLoop()
	return p, nil
}

// Addr is the proxy's listen address.
func (p *Proxy) Addr() string { return p.ln.Addr().String() }

// SetUpstream changes the target for new connections.
func (p *Proxy) SetUpstream(addr string) {
	p.mu.Lock()
	p.upstream = addr
	p.mu.Unlock()
}

// SetMode changes the policy. Switching to ModeBlackhole also silences
// existing connections (their bytes are discarded from then on).
func (p *Proxy) SetMode(m Mode) {
	p.mu.Lock()
	p.mode = m
	p.mu.Unlock()
}

// Partition is SetMode(ModeBlackhole).
func (p *Proxy) Partition() { p.SetMode(ModeBlackhole) }

// Heal ends a partition: stale connections (whose streams lost bytes) are
// cut and new connections pass.
func (p *Proxy) Heal() {
	p.SetMode(ModePass)
	p.CutAll()
}

// SetDelay delays every forwarded chunk/frame by d.
func (p *Proxy) SetDelay(d time.Duration) {
	p.mu.Lock()
	p.delay = d
	p.mu.Unlock()
}

// SetFrameFilter enables WebSocket frame mode for NEW connections (nil
// returns to byte mode).
func (p *Proxy) SetFrameFilter(f FrameFilter) {
	p.mu.Lock()
	p.filter = f
	p.mu.Unlock()
}

// CutAll closes every live connection with a reset.
func (p *Proxy) CutAll() int {
	p.mu.Lock()
	ps := make([]*pair, 0, len(p.conns))
	for c := range p.conns {
		ps = append(ps, c)
	}
	p.mu.Unlock()
	for _, c := range ps {
		c.close()
		p.cut.Add(1)
	}
	return len(ps)
}

// Stats returns counters.
func (p *Proxy) Stats() Stats {
	p.mu.Lock()
	active := int64(len(p.conns))
	p.mu.Unlock()
	return Stats{
		Accepted: p.accepted.Load(), Forwarded: p.forwarded.Load(), Dropped: p.dropped.Load(),
		Duplicated: p.duplicated.Load(), Cut: p.cut.Load(), BytesC2S: p.bytesC2S.Load(), BytesS2C: p.bytesS2C.Load(),
		Refused: p.refused.Load(), Blackholed: p.blackholed.Load(), FramesC2S: p.framesC2S.Load(), FramesS2C: p.framesS2C.Load(),
		ActiveConns: active,
	}
}

// Close stops the listener and cuts every connection.
func (p *Proxy) Close() error {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	err := p.ln.Close()
	p.CutAll()
	p.wg.Wait()
	return err
}

func (p *Proxy) state() (Mode, time.Duration, FrameFilter, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.mode, p.delay, p.filter, p.upstream
}

func (p *Proxy) acceptLoop() {
	defer p.wg.Done()
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.accepted.Add(1)
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.handle(c)
		}()
	}
}

func (p *Proxy) track(pr *pair) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	p.conns[pr] = struct{}{}
	return true
}

func (p *Proxy) untrack(pr *pair) {
	p.mu.Lock()
	delete(p.conns, pr)
	p.mu.Unlock()
}

func (p *Proxy) handle(c net.Conn) {
	mode, _, filter, upstream := p.state()
	pr := &pair{client: c}
	if !p.track(pr) {
		pr.close()
		return
	}
	defer p.untrack(pr)
	defer pr.close()
	switch mode {
	case ModeRefuse:
		p.refused.Add(1)
		return
	case ModeBlackhole:
		p.blackholed.Add(1)
		_, _ = io.Copy(io.Discard, c) // hold until the peer or CutAll closes it
		return
	}
	s, err := net.DialTimeout("tcp", upstream, 5*time.Second)
	if err != nil {
		return
	}
	pr.server = s
	var wg sync.WaitGroup
	wg.Add(2)
	if filter != nil {
		cr, sr := bufio.NewReader(c), bufio.NewReader(s)
		upgraded := make(chan bool, 1)
		go func() {
			defer wg.Done()
			defer pr.close()
			if err := relayHeader(cr, s); err != nil {
				return
			}
			if !<-upgraded {
				p.copyBytes(s, cr, ClientToServer)
				return
			}
			p.relayFrames(pr, cr, s, ClientToServer, filter)
		}()
		go func() {
			defer wg.Done()
			defer pr.close()
			hdr, err := readHeader(sr)
			if err != nil {
				upgraded <- false
				return
			}
			ok := bytes.HasPrefix(hdr, []byte("HTTP/1.1 101"))
			if _, err := c.Write(hdr); err != nil {
				upgraded <- false
				return
			}
			upgraded <- ok
			if !ok {
				p.copyBytes(c, sr, ServerToClient)
				return
			}
			p.relayFrames(pr, sr, c, ServerToClient, filter)
		}()
	} else {
		go func() { defer wg.Done(); defer pr.close(); p.copyBytes(s, c, ClientToServer) }()
		go func() { defer wg.Done(); defer pr.close(); p.copyBytes(c, s, ServerToClient) }()
	}
	wg.Wait()
}

func (p *Proxy) countBytes(dir Direction, n int) {
	if dir == ClientToServer {
		p.bytesC2S.Add(int64(n))
	} else {
		p.bytesS2C.Add(int64(n))
	}
}

// copyBytes forwards chunks, honouring blackhole (discard) and delay.
func (p *Proxy) copyBytes(dst io.Writer, src io.Reader, dir Direction) {
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			mode, delay, _, _ := p.state()
			if mode == ModeBlackhole {
				p.dropped.Add(1)
			} else {
				if delay > 0 {
					time.Sleep(delay)
				}
				if _, werr := dst.Write(buf[:n]); werr != nil {
					return
				}
				p.forwarded.Add(1)
				p.countBytes(dir, n)
			}
		}
		if err != nil {
			return
		}
	}
}

func readHeader(r *bufio.Reader) ([]byte, error) {
	var hdr []byte
	for {
		line, err := r.ReadSlice('\n')
		hdr = append(hdr, line...)
		if err != nil {
			return hdr, err
		}
		if len(hdr) > 64<<10 {
			return hdr, errors.New("faultnet: header too large")
		}
		if bytes.Equal(line, []byte("\r\n")) || bytes.Equal(line, []byte("\n")) {
			return hdr, nil
		}
	}
}

func relayHeader(r *bufio.Reader, w io.Writer) error {
	hdr, err := readHeader(r)
	if err != nil {
		return err
	}
	_, err = w.Write(hdr)
	return err
}

func readFrame(r *bufio.Reader, dir Direction) (Frame, error) {
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return Frame{}, err
	}
	raw := append([]byte(nil), h[:]...)
	f := Frame{Dir: dir, Fin: h[0]&0x80 != 0, Opcode: h[0] & 0x0f}
	masked := h[1]&0x80 != 0
	n := uint64(h[1] & 0x7f)
	switch n {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return f, err
		}
		raw = append(raw, b[:]...)
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return f, err
		}
		raw = append(raw, b[:]...)
		n = binary.BigEndian.Uint64(b[:])
	}
	if n > 64<<20 {
		return f, errors.New("faultnet: frame too large")
	}
	var key [4]byte
	if masked {
		if _, err := io.ReadFull(r, key[:]); err != nil {
			return f, err
		}
		raw = append(raw, key[:]...)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return f, err
	}
	raw = append(raw, payload...)
	if masked {
		for i := range payload {
			payload[i] ^= key[i%4]
		}
	}
	f.Payload = payload
	f.raw = raw
	return f, nil
}

func (p *Proxy) relayFrames(pr *pair, r *bufio.Reader, w io.Writer, dir Direction, filter FrameFilter) {
	for {
		f, err := readFrame(r, dir)
		if err != nil {
			return
		}
		if dir == ClientToServer {
			p.framesC2S.Add(1)
		} else {
			p.framesS2C.Add(1)
		}
		mode, delay, _, _ := p.state()
		if mode == ModeBlackhole {
			p.dropped.Add(1)
			continue
		}
		act := filter(f)
		if d := delay + act.Delay; d > 0 {
			time.Sleep(d)
		}
		if act.Drop {
			p.dropped.Add(1)
		} else {
			times := 1
			if act.Duplicate {
				times = 2
				p.duplicated.Add(1)
			}
			for i := 0; i < times; i++ {
				if _, err := w.Write(f.raw); err != nil {
					return
				}
				p.forwarded.Add(1)
				p.countBytes(dir, len(f.raw))
			}
		}
		if act.CutAfter {
			p.cut.Add(1)
			pr.close()
			return
		}
	}
}
