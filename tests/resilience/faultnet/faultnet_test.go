package faultnet

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func echoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	return ln.Addr().String()
}

func roundTrip(addr, msg string, timeout time.Duration) (string, error) {
	c, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return "", err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(timeout))
	if _, err := c.Write([]byte(msg + "\n")); err != nil {
		return "", err
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	return strings.TrimSpace(line), err
}

func TestModes(t *testing.T) {
	p, err := New(echoServer(t))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if got, err := roundTrip(p.Addr(), "hello", time.Second); err != nil || got != "hello" {
		t.Fatalf("pass: %q %v", got, err)
	}
	p.SetMode(ModeRefuse)
	if _, err := roundTrip(p.Addr(), "x", time.Second); err == nil {
		t.Fatal("refuse mode answered")
	}
	p.Partition()
	start := time.Now()
	if _, err := roundTrip(p.Addr(), "x", 300*time.Millisecond); err == nil || time.Since(start) < 250*time.Millisecond {
		t.Fatalf("blackhole should time out silently, err=%v after %v", err, time.Since(start))
	}
	p.Heal()
	p.SetDelay(100 * time.Millisecond)
	start = time.Now()
	if got, err := roundTrip(p.Addr(), "slow", 2*time.Second); err != nil || got != "slow" || time.Since(start) < 200*time.Millisecond {
		t.Fatalf("delay: %q %v %v", got, err, time.Since(start))
	}
	st := p.Stats()
	// >= rather than ==: local tools occasionally probe loopback ports.
	if st.Accepted < 4 || st.Refused < 1 || st.Blackholed < 1 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestCutAll(t *testing.T) {
	p, err := New(echoServer(t))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	c, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	time.Sleep(50 * time.Millisecond)
	if n := p.CutAll(); n != 1 {
		t.Fatalf("cut %d", n)
	}
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("cut connection still readable")
	}
}

func TestWebSocketFrames(t *testing.T) {
	var serverGot atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
		if err != nil {
			return
		}
		defer c.CloseNow()
		for {
			_, b, err := c.Read(r.Context())
			if err != nil {
				return
			}
			serverGot.Add(1)
			_ = c.Write(r.Context(), websocket.MessageText, append([]byte("echo:"), b...))
		}
	}))
	defer srv.Close()
	p, err := New(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	var sawC2S atomic.Int64
	p.SetFrameFilter(func(f Frame) Action {
		if f.Opcode != 1 {
			return Action{}
		}
		if f.Dir == ClientToServer {
			sawC2S.Add(1)
			if strings.Contains(string(f.Payload), "dup") {
				return Action{Duplicate: true}
			}
			if strings.Contains(string(f.Payload), "drop") {
				return Action{Drop: true}
			}
		}
		return Action{}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws://"+p.Addr()+"/", &websocket.DialOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	for _, m := range []string{"a", "drop-me", "dup-me", "b"} {
		if err := c.Write(ctx, websocket.MessageText, []byte(m)); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	for len(got) < 4 {
		_, b, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("read after %q: %v", got, err)
		}
		got = append(got, string(b))
	}
	want := "echo:a,echo:dup-me,echo:dup-me,echo:b"
	if strings.Join(got, ",") != want {
		t.Fatalf("got %v want %s", got, want)
	}
	if sawC2S.Load() != 4 || serverGot.Load() != 4 {
		t.Fatalf("filter saw %d frames, server got %d", sawC2S.Load(), serverGot.Load())
	}
}
