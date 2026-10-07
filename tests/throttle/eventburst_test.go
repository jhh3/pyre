//go:build unix

// Package throttle holds A20 tests that shrink Pyre's own limits (stream
// write timeout, stream cap) instead of the machine's resources. Each test
// uses megabytes of memory and a few seconds; none of them loads the host.
package throttle

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/jhh3/pyre/internal/client"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/events"
	"github.com/jhh3/pyre/internal/httpapi"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// burstAPI serves the real event log; everything else is unused.
type burstAPI struct {
	contracts.API
	log *events.Log
}

func (a burstAPI) ListEvents(ctx context.Context, r contracts.ListEventsRequest) (contracts.ListEventsResponse, error) {
	return a.log.ListEvents(ctx, r)
}

func (a burstAPI) WhoAmI(ctx context.Context) (contracts.Principal, error) {
	p, _ := contracts.PrincipalFrom(ctx)
	return p, nil
}

func p95(d []time.Duration) time.Duration {
	s := slices.Clone(d)
	slices.Sort(s)
	return s[(len(s)*95+99)/100-1]
}

const token = "pyre_synthetic_owner_token_0001"

// stalledStream opens an event stream whose socket has a tiny receive
// buffer and never reads it: the server's writes block once a couple of
// hundred kilobytes are in flight.
func stalledStream(ctx context.Context, base string) (*websocket.Conn, error) {
	d := &net.Dialer{Control: func(_, _ string, rc syscall.RawConn) error {
		var serr error
		err := rc.Control(func(fd uintptr) { serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 2048) })
		if err != nil {
			return err
		}
		return serr
	}}
	hc := &http.Client{Transport: &http.Transport{DialContext: d.DialContext}}
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(base, "http")+contracts.PathEventStream+"?after_cursor=0",
		&websocket.DialOptions{HTTPClient: hc, HTTPHeader: http.Header{"Authorization": {"Bearer " + token}}})
	return c, err
}

// A burst of 5000 small events is fanned out to 8 live subscribers in
// order while interactive calls stay inside budget; a stalled subscriber
// is disconnected after the (shrunk) write timeout instead of buffering;
// the stream cap refuses extra streams; memory and goroutines stay flat.
func TestEventBurstFanOutKeepsInteractiveCallsResponsive(t *testing.T) {
	const nEvents, nSubs = 5000, 8
	const maxStreams = nSubs + 1
	db, err := sqlitex.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	elog, err := events.Open(ctx, db, events.Options{})
	if err != nil {
		t.Fatal(err)
	}
	owner := contracts.Principal{Kind: contracts.PrincipalOwner, ID: "cli_synthetic"}
	sv := httpapi.New(burstAPI{log: elog}, httpapi.NewStaticTokens(map[string]contracts.Principal{token: owner}),
		httpapi.Options{StreamWait: time.Second, StreamWriteTimeout: 300 * time.Millisecond, MaxStreams: maxStreams})
	srv := httptest.NewServer(sv)
	defer srv.Close()
	cl := client.New(srv.URL, token, client.Options{MaxAttempts: 1, Timeout: 5 * time.Second})

	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	heapBase := ms.HeapAlloc
	goBase := runtime.NumGoroutine()

	// Subscribers: 8 live ones (the production client), one stalled.
	var subWG sync.WaitGroup
	got := make([]atomic.Int64, nSubs)
	orderErr := make(chan error, nSubs)
	sctx, stopSubs := context.WithCancel(ctx)
	for i := 0; i < nSubs; i++ {
		subWG.Add(1)
		go func(i int) {
			defer subWG.Done()
			last := int64(0)
			_ = cl.WatchEvents(sctx, 0, func(ev contracts.Event) error {
				if ev.Cursor != last+1 {
					orderErr <- fmt.Errorf("subscriber %d: cursor %d after %d", i, ev.Cursor, last)
					return context.Canceled
				}
				last = ev.Cursor
				got[i].Add(1)
				return nil
			})
		}(i)
	}
	stalled, err := stalledStream(ctx, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer stalled.CloseNow()
	waitUntil(t, 5*time.Second, "all streams open", func() bool { return sv.OpenStreams() == maxStreams })

	// The cap: a further stream is refused (retryable capacity_exhausted).
	resp, err := http.DefaultClient.Do(mustReq(t, srv.URL+contracts.PathEventStream))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("stream beyond MaxStreams: HTTP %d, want 429", resp.StatusCode)
	}

	// Interactive callers run throughout the burst.
	var lat []time.Duration
	var latMu sync.Mutex
	ictx, stopInteractive := context.WithCancel(ctx)
	var iwg sync.WaitGroup
	for w := 0; w < 2; w++ {
		iwg.Add(1)
		go func(w int) {
			defer iwg.Done()
			for ictx.Err() == nil {
				s := time.Now()
				var err error
				if w == 0 {
					_, err = cl.WhoAmI(ictx)
				} else {
					_, err = cl.ListEvents(ictx, contracts.ListEventsRequest{Limit: 1})
				}
				if err != nil {
					if ictx.Err() == nil {
						t.Errorf("interactive call: %v", err)
					}
					return
				}
				latMu.Lock()
				lat = append(lat, time.Since(s))
				latMu.Unlock()
				time.Sleep(2 * time.Millisecond)
			}
		}(w)
	}
	var heapPeak atomic.Uint64
	sampleDone := make(chan struct{})
	go func() {
		defer close(sampleDone)
		for ictx.Err() == nil {
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			if m.HeapAlloc > heapPeak.Load() {
				heapPeak.Store(m.HeapAlloc)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	// The burst: 5000 events in 100 transactions of 50, as fast as the
	// database commits them.
	pad := strings.Repeat("s", 200)
	start := time.Now()
	for b := 0; b < nEvents/50; b++ {
		err := sqlitex.Tx(ctx, db, func(tx *sql.Tx) error {
			for j := 0; j < 50; j++ {
				p, _ := json.Marshal(map[string]any{"n": b*50 + j, "pad": pad})
				if _, err := elog.AppendTx(ctx, tx, contracts.Event{Kind: contracts.EvMessageStored,
					Object: contracts.ObjectRef{Kind: contracts.KindSession, ID: "ses_SYNTHBURST"}, Payload: p}); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		elog.Notify()
	}
	appended := time.Since(start)
	waitUntil(t, 90*time.Second, "every live subscriber received the burst", func() bool {
		for i := range got {
			if got[i].Load() < nEvents {
				return false
			}
		}
		return true
	})
	fanned := time.Since(start)
	// The stalled stream was cut by the server (write timeout), not kept.
	waitUntil(t, 5*time.Second, "stalled stream disconnected", func() bool { return sv.OpenStreams() == nSubs })
	stopInteractive()
	iwg.Wait()
	<-sampleDone
	select {
	case err := <-orderErr:
		t.Fatal(err)
	default:
	}

	peak := heapPeak.Load()
	growth := int64(peak) - int64(heapBase)
	t.Logf("%d events appended in %s, fanned out to %d subscribers in %s; %d interactive calls p95=%s max=%s; heap base %.1f MiB peak %.1f MiB",
		nEvents, appended.Round(time.Millisecond), nSubs, fanned.Round(time.Millisecond), len(lat), p95(lat), slices.Max(lat),
		float64(heapBase)/(1<<20), float64(peak)/(1<<20))
	// The budget is 100 ms. Under the race detector (CI runs every package
	// with -race, in parallel, on shared runners) the whole process runs
	// several times slower; the GitHub Ubuntu runner measured p95 123 ms
	// with the burst itself taking 18 s instead of under 1 s. The test
	// still catches a stalled or starved API there (p95 of seconds).
	budget := 100 * time.Millisecond
	if raceEnabled {
		budget = 500 * time.Millisecond
	}
	if got := p95(lat); got > budget {
		t.Errorf("interactive p95 %s during the event burst exceeds the %s budget", got, budget)
	}
	if len(lat) < 20 {
		t.Errorf("only %d interactive calls overlapped the burst", len(lat))
	}
	if growth > 32<<20 {
		t.Errorf("heap grew %.1f MiB during the burst (stalled subscriber buffered?)", float64(growth)/(1<<20))
	}

	// A new stream fits again now that the stalled one is gone.
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+contracts.PathEventStream+"?after_cursor="+fmt.Sprint(nEvents),
		&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + token}}})
	if err != nil {
		t.Fatalf("stream after the stalled one was dropped: %v", err)
	}
	c.CloseNow()

	stopSubs()
	subWG.Wait()
	waitUntil(t, 5*time.Second, "streams closed", func() bool { return sv.OpenStreams() == 0 })
	srv.CloseClientConnections()
	waitUntil(t, 5*time.Second, "goroutines back to baseline", func() bool { return runtime.NumGoroutine() <= goBase+4 })
}

func mustReq(t *testing.T, url string) *http.Request {
	t.Helper()
	r, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Upgrade", "websocket")
	r.Header.Set("Sec-WebSocket-Version", "13")
	r.Header.Set("Sec-WebSocket-Key", "c3ludGhldGljLWtleS0wMDE=")
	return r
}

func waitUntil(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	end := time.Now().Add(d)
	for !cond() {
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
