package mock_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/client"
	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/httpapi"
	"github.com/jhh3/pyre/internal/runtime/mock"
)

// TestHTTPRoundTrip drives the mock through httpapi with the real client.
func TestHTTPRoundTrip(t *testing.T) {
	b := mock.New(mock.Options{Clock: mock.NewFakeClock(mock.FixtureEpoch)})
	owner := c.Principal{Kind: c.PrincipalOwner, ID: "cli_synth-laptop"}
	srv := httptest.NewServer(httpapi.NewServer(b, httpapi.NewStaticTokens(map[string]c.Principal{"dev-token": owner}), httpapi.Options{StreamWait: time.Second}))
	defer srv.Close()
	cl := client.New(srv.URL, "dev-token", client.Options{RetryBase: time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	h, err := cl.Hello(ctx, c.HelloRequest{Client: "test"})
	if err != nil || !h.Mock {
		t.Fatalf("hello %+v %v", h, err)
	}
	sessions, err := client.All(ctx, func(ctx context.Context, cur string) (c.Page[c.SessionView], error) {
		return cl.ListSessions(ctx, c.ListSessionsRequest{PageRequest: c.PageRequest{Cursor: cur, Limit: 3}})
	})
	if err != nil || len(sessions) < 8 {
		t.Fatalf("paged sessions %d %v", len(sessions), err)
	}

	data := bytes.Repeat([]byte("synthetic artifact bytes\n"), 2000)
	rec, err := cl.Publish(ctx, bytes.NewReader(data), client.PublishMeta{Title: "report", Filename: "report.txt", MediaType: "text/plain", ChunkSize: 7000})
	if err != nil {
		t.Fatal(err)
	}
	rc, ver, err := cl.OpenContent(ctx, rec.Version.VersionID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || !bytes.Equal(got, data) || ver.Digest != rec.Version.Digest {
		t.Fatalf("download mismatch: %v", err)
	}
	r2, err := cl.OpenContentRange(ctx, rec.Version.VersionID, 25, 25)
	if err != nil {
		t.Fatal(err)
	}
	part, _ := io.ReadAll(r2)
	r2.Close()
	if string(part) != "synthetic artifact bytes\n" {
		t.Fatalf("range %q", part)
	}
	up, err := cl.GetUpload(ctx, c.GetRequest{ID: "upl_missing"})
	if !c.IsCode(err, c.ErrNotFound) {
		t.Fatalf("get upload missing: %+v %v", up, err)
	}

	// Event stream sees new events and stops on a callback error.
	head := b.HeadCursor()
	go func() {
		time.Sleep(100 * time.Millisecond)
		b.CreateProject(c.WithPrincipal(context.Background(), owner), c.CreateProjectRequest{Mutation: c.Mutation{OperationID: "op_watch"}, Name: "watched"})
	}()
	stop := errors.New("stop")
	var seen c.Event
	err = cl.WatchEvents(ctx, head, func(e c.Event) error {
		if e.Kind == c.EvProjectChanged {
			seen = e
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) || seen.Cursor <= head {
		t.Fatalf("watch: %v %+v", err, seen)
	}
	b.TrimEvents(0)
	if err := cl.WatchEvents(ctx, 0, func(c.Event) error { return nil }); !c.IsCode(err, c.ErrResetRequired) {
		t.Fatalf("want reset_required, got %v", err)
	}
}
