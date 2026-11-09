package client

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/httpapi"
)

// The hub restarts while a call is in flight (it stored the mutation, then
// went away before answering), and the client also holds a pooled
// keep-alive connection to the old process. The long-lived client (like
// `pyre mcp` or pyred's proxy) reconnects to the new hub process on the
// same address, the retry carries the same operation id, and the hub's
// stored answer comes back: one effect, not two (iss_…).
func TestHubRestartMidCall(t *testing.T) {
	f := &fake{} // its stored map stands in for the hub database, which survives the restart
	api := httpapi.NewServer(f, httpapi.NewStaticTokens(map[string]c.Principal{"tok": owner}), httpapi.Options{})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	committed := make(chan struct{})
	var calls atomic.Int32
	old := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 2 {
			// The in-flight call: the hub commits it, then never answers
			// before it is stopped.
			api.ServeHTTP(httptest.NewRecorder(), r)
			close(committed)
			<-r.Context().Done()
			return
		}
		api.ServeHTTP(w, r)
	}))
	old.Listener.Close()
	old.Listener = ln
	old.Start()

	cl := New("http://"+addr, "tok", Options{RetryBase: 50 * time.Millisecond, RetryMax: 200 * time.Millisecond, Timeout: 10 * time.Second})
	ctx := context.Background()
	// Warm the pool: the client now holds a keep-alive connection to the old hub.
	if _, err := cl.WhoAmI(ctx); err != nil {
		t.Fatal(err)
	}

	type result struct {
		mv  c.MessageView
		err error
	}
	done := make(chan result, 1)
	go func() {
		mv, err := cl.SendMessage(ctx, c.SendMessageRequest{Recipient: c.Address{Kind: c.AddrOwner, ID: "owner"}, Body: c.MessageBody{Text: "across a restart"}})
		done <- result{mv, err}
	}()
	select {
	case <-committed:
	case <-time.After(10 * time.Second):
		t.Fatal("the call never reached the old hub")
	}

	// Restart: the old process goes away with its connections; a new one
	// listens on the same address.
	old.CloseClientConnections()
	old.Close()
	ln2, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("relisten on %s: %v", addr, err)
	}
	restarted := &http.Server{Handler: api}
	go restarted.Serve(ln2)
	t.Cleanup(func() { restarted.Close() })

	var res result
	select {
	case res = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the call hung across the hub restart")
	}
	if res.err != nil {
		t.Fatalf("call across the restart: %v", res.err)
	}
	f.mu.Lock()
	ops, stored := append([]string(nil), f.sendOps...), len(f.stored)
	f.mu.Unlock()
	if len(ops) < 2 || ops[0] == "" || ops[len(ops)-1] != ops[0] || stored != 1 || res.mv.Envelope.OperationID != ops[0] {
		t.Fatalf("want one stored message replayed under one operation id: ops=%v stored=%d got=%s", ops, stored, res.mv.Envelope.OperationID)
	}

	// Later calls on the same long-lived client keep working.
	if _, err := cl.WhoAmI(ctx); err != nil {
		t.Fatalf("call after the restart: %v", err)
	}
}
