package assemble

import (
	"bytes"
	"context"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/client"
	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/hub"
)

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// Closing the hub with an open event stream ends the stream cleanly
// before the database closes (no "sql: database is closed" internal error).
func TestCloseDrainsEventStreams(t *testing.T) {
	logs := &lockedBuf{}
	st, err := Open(context.Background(), Config{DataDir: filepath.Join(t.TempDir(), "hub"), Logger: slog.New(slog.NewTextHandler(logs, nil)),
		Hub: hub.Options{PollInterval: 20 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	st.Start(context.Background())
	tok, err := st.Auth.Issue(context.Background(), c.PrincipalOwner, "cli_synth-stream", "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(st.Handler)
	defer srv.Close()
	cl := client.New(srv.URL, tok, client.Options{RetryBase: 10 * time.Millisecond, RetryMax: 50 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- cl.WatchEvents(ctx, 0, func(c.Event) error {
			select {
			case got <- struct{}{}:
			default:
			}
			return nil
		})
	}()
	// Make sure the stream is live (one event delivered).
	if _, err := st.Hub.CreateProject(c.WithPrincipal(context.Background(), c.Principal{Kind: c.PrincipalOwner, ID: "cli_synth-stream"}),
		c.CreateProjectRequest{Mutation: c.Mutation{OperationID: c.NewID(c.PrefixOperation)}, Name: "synthetic-stream"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("stream delivered nothing")
	}
	time.Sleep(100 * time.Millisecond) // the stream is now long-polling the DB
	// Like pyre-hub serve: stop accepting connections (the hijacked stream
	// stays open), then close the stack.
	srv.Listener.Close()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	cancel()
	<-done
	if l := logs.String(); strings.Contains(l, "database is closed") || strings.Contains(l, "internal error") {
		t.Fatalf("shutdown with an open stream logged an internal error:\n%s", l)
	}
}
