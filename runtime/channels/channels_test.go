package channels

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeClient drives the server like Claude Code's MCP client would.
type fakeClient struct {
	t   *testing.T
	w   io.WriteCloser
	out chan map[string]any
}

func startServer(t *testing.T, dir string) (*fakeClient, chan error) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	s := &Server{Spool: filepath.Join(dir, "spool"), Events: filepath.Join(dir, "events.jsonl"), In: inR, Out: outW, Poll: 10 * time.Millisecond}
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { done <- s.Run(ctx); outW.Close() }()
	c := &fakeClient{t: t, w: inW, out: make(chan map[string]any, 16)}
	go func() {
		sc := bufio.NewScanner(outR)
		for sc.Scan() {
			var m map[string]any
			if json.Unmarshal(sc.Bytes(), &m) == nil {
				c.out <- m
			}
		}
		close(c.out)
	}()
	return c, done
}

func (c *fakeClient) send(v map[string]any) {
	v["jsonrpc"] = "2.0"
	b, _ := json.Marshal(v)
	c.w.Write(append(b, '\n'))
}

func (c *fakeClient) next() map[string]any {
	c.t.Helper()
	select {
	case m, ok := <-c.out:
		if !ok {
			c.t.Fatal("server closed output")
		}
		return m
	case <-time.After(3 * time.Second):
		c.t.Fatal("timed out waiting for server output")
	}
	return nil
}

func TestChannelHandshakePushAndAck(t *testing.T) {
	dir := t.TempDir()
	c, done := startServer(t, dir)
	c.send(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": "2025-06-18"}})
	init := c.next()
	res := init["result"].(map[string]any)
	caps := res["capabilities"].(map[string]any)["experimental"].(map[string]any)
	if _, ok := caps["claude/channel"]; !ok || res["protocolVersion"] != "2025-06-18" {
		t.Fatalf("initialize = %v", init)
	}
	c.send(map[string]any{"method": "notifications/initialized"})
	c.send(map[string]any{"id": 2, "method": "tools/list"})
	tl := c.next()
	tools := tl["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "pyre_ack" {
		t.Fatalf("tools = %v", tl)
	}

	spool := filepath.Join(dir, "spool")
	m := Message{MessageID: "msg_1", Content: "[pyre-msg:msg_1] from session:ses_a:\nhello", Sender: "session:ses_a"}
	if err := Enqueue(spool, m); err != nil {
		t.Fatal(err)
	}
	if err := Enqueue(spool, m); err != nil { // idempotent
		t.Fatal(err)
	}
	n := c.next()
	if n["method"] != "notifications/claude/channel" {
		t.Fatalf("notification = %v", n)
	}
	p := n["params"].(map[string]any)
	if p["content"] != m.Content || p["meta"].(map[string]any)["message_id"] != "msg_1" {
		t.Fatalf("params = %v", p)
	}
	c.send(map[string]any{"id": 3, "method": "tools/call", "params": map[string]any{"name": "pyre_ack", "arguments": map[string]any{"message_id": "msg_1"}}})
	if r := c.next(); r["result"] == nil {
		t.Fatalf("ack reply = %v", r)
	}
	// Already pushed: never queued again.
	if err := Enqueue(spool, m); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(spool, "msg_1.json")); !os.IsNotExist(err) {
		t.Fatal("pushed message re-queued")
	}
	evs, _ := ReadEvents(filepath.Join(dir, "events.jsonl"))
	if len(evs) != 2 || evs[0].Kind != "pushed" || evs[1].Kind != "ack" || evs[1].MessageID != "msg_1" {
		t.Fatalf("events = %+v", evs)
	}
	c.send(map[string]any{"id": 4, "method": "bogus"})
	if r := c.next(); r["error"] == nil {
		t.Fatalf("unknown method = %v", r)
	}
	c.w.Close()
	<-done
}

func TestPushingAtStartupIsNotResent(t *testing.T) {
	dir := t.TempDir()
	spool := filepath.Join(dir, "spool")
	os.MkdirAll(spool, 0o700)
	b, _ := json.Marshal(Message{MessageID: "msg_x", Content: "x"})
	os.WriteFile(filepath.Join(spool, "msg_x.pushing"), b, 0o600) // crashed mid-push
	c, _ := startServer(t, dir)
	c.send(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{}})
	c.next()
	c.send(map[string]any{"method": "notifications/initialized"})
	Enqueue(spool, Message{MessageID: "msg_x", Content: "x"}) // refused silently: already claimed
	Enqueue(spool, Message{MessageID: "msg_y", Content: "y"})
	n := c.next()
	if n["params"].(map[string]any)["meta"].(map[string]any)["message_id"] != "msg_y" {
		t.Fatalf("expected only msg_y, got %v", n)
	}
	select {
	case m := <-c.out:
		t.Fatalf("unexpected extra output %v", m)
	case <-time.After(100 * time.Millisecond):
	}
	if err := Enqueue(spool, Message{MessageID: "../evil", Content: "x"}); err == nil {
		t.Fatal("path traversal id accepted")
	}
}
