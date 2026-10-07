package daemon

import (
	"context"
	"fmt"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/client"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/mock"
)

// A20 throttling at the node: Pyre's own limits are shrunk (a read-cache
// queue of 4, an outbox of 20) and background writers are slowed with
// sleeps, never with load. Interactive calls go over the real local socket.

func p95(d []time.Duration) time.Duration {
	if len(d) == 0 {
		return 0
	}
	s := slices.Clone(d)
	slices.Sort(s)
	return s[(len(s)*95+99)/100-1]
}

// timed runs fn from workers goroutines, n calls each, and returns every
// call's latency.
func timed(t *testing.T, workers, n int, fn func(w, i int) error) []time.Duration {
	t.Helper()
	var mu sync.Mutex
	var lat []time.Duration
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < n; i++ {
				s := time.Now()
				err := fn(w, i)
				d := time.Since(s)
				if err != nil {
					errs <- err
					return
				}
				mu.Lock()
				lat = append(lat, d)
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	return lat
}

func serveSocket(t *testing.T, n *node) (owner *client.Client, stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- n.d.ServeLocal(ctx) }()
	sock := n.d.SocketPath()
	waitFor(t, "socket", func() bool { _, err := os.Stat(sock); return err == nil })
	return socketClient(sock, ""), func() { cancel(); <-served }
}

// Hub reads are cached by a background writer: a read never waits for the
// cache write (here slowed to 200 ms each), the writer's queue stays within
// its cap under a burst, and a queued value already answers offline reads.
func TestReadCacheWritesNeverDelayInteractiveReads(t *testing.T) {
	dir := shortDir(t)
	hub := &fakeHub{byOp: map[string]contracts.MessageView{}}
	n := startNode(t, dir, Config{Adapters: []contracts.RuntimeAdapter{mock.NewAdapter()}, Upstream: hub.client,
		OwnerToken: "pyre_owner_test_token_0000000000", OutboxFlush: time.Hour, ReadCacheQueue: 4})
	n.d.cache.mu.Lock()
	n.d.cache.hook = func() { time.Sleep(200 * time.Millisecond) }
	n.d.cache.mu.Unlock()
	owner, stop := serveSocket(t, n)
	defer stop()
	ctx := context.Background()

	// 4 callers x 50 distinct project reads (each one a cache write).
	lat := timed(t, 4, 50, func(w, i int) error {
		_, err := owner.GetProject(ctx, contracts.GetRequest{ID: fmt.Sprintf("prj_SYNTH%d_%03d", w, i)})
		return err
	})
	st := n.d.ReadCacheStats()
	t.Logf("200 socket reads with a 200 ms cache writer: p95=%s max=%s; cache queue max=%d/%d written=%d dropped=%d",
		p95(lat), slices.Max(lat), st.MaxPending, st.Queue, st.Written, st.Dropped)
	if got := p95(lat); got > 100*time.Millisecond {
		t.Errorf("interactive read p95 %s exceeds the 100 ms socket budget; reads wait for cache writes", got)
	}
	if st.MaxPending > 4 || st.Dropped == 0 {
		t.Errorf("cache queue not bounded as configured: %+v", st)
	}
	waitFor(t, "cache writer idle", func() bool { return n.d.ReadCacheStats().Pending == 0 })

	// A value still queued answers an offline read; once written, the
	// database copy does.
	fresh := contracts.GetRequest{ID: "prj_SYNTHFRESH"}
	if _, err := owner.GetProject(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	if n.d.ReadCacheStats().Pending == 0 {
		t.Fatal("the fresh value was written synchronously")
	}
	hub.down.Store(true)
	var notes []contracts.Freshness
	fctx := client.WithFreshnessSink(ctx, func(_ string, f contracts.Freshness) { notes = append(notes, f) })
	if p, err := owner.GetProject(fctx, fresh); err != nil || p.Name != "synthetic-project" || len(notes) != 1 || !notes[0].Stale {
		t.Fatalf("offline read of a queued value: %+v %v %+v", p, err, notes)
	}
	waitFor(t, "fresh value written", func() bool { return n.d.ReadCacheStats().Pending == 0 })
	if p, err := owner.GetProject(ctx, fresh); err != nil || p.Name != "synthetic-project" {
		t.Fatalf("offline read of the written value: %+v %v", p, err)
	}
}

// slowSends delays every hub SendMessage (a slow hub link).
type slowSends struct {
	contracts.API
	delay *atomic.Int64
}

func (s slowSends) SendMessage(ctx context.Context, r contracts.SendMessageRequest) (contracts.MessageView, error) {
	time.Sleep(time.Duration(s.delay.Load()))
	return s.API.SendMessage(ctx, r)
}

// The local outbox applies backpressure at its cap (capacity_exhausted,
// retryable; never a silent drop), drains in the background over a slow
// hub while interactive reads stay inside budget, and delivers every
// message exactly once.
func TestOutboxBackpressureAndBackgroundFlush(t *testing.T) {
	dir := shortDir(t)
	hub := &fakeHub{byOp: map[string]contracts.MessageView{}}
	var delay atomic.Int64
	n := startNode(t, dir, Config{Adapters: []contracts.RuntimeAdapter{mock.NewAdapter()},
		Upstream:   func(b string) contracts.API { return slowSends{API: hub.client(b), delay: &delay} },
		OwnerToken: "pyre_owner_test_token_0000000000", OutboxFlush: 50 * time.Millisecond, MaxOutbox: 20})
	owner, stop := serveSocket(t, n)
	defer stop()
	ctx := context.Background()
	send := func(i int) (contracts.MessageView, error) {
		return owner.SendMessage(ctx, contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: fmt.Sprintf("op_SYNTHBURST%02d", i)},
			Recipient: contracts.Address{Kind: contracts.AddrSession, ID: "ses_SYNTHPEER"}, Body: contracts.MessageBody{Text: fmt.Sprintf("synthetic %d", i)}})
	}

	hub.down.Store(true)
	var rejected []int
	for i := 0; i < 30; i++ {
		v, err := send(i)
		switch {
		case err == nil && v.Status.Storage == contracts.StoragePendingLocal:
		case contracts.IsCode(err, contracts.ErrCapacityExhausted) && client.Retryable(err):
			rejected = append(rejected, i)
		default:
			t.Fatalf("send %d during outage: %+v %v", i, v, err)
		}
	}
	if len(rejected) != 10 {
		t.Fatalf("outbox cap 20: %d of 30 sends refused, want 10", len(rejected))
	}

	// Hub back but slow (40 ms per message): the flush runs in the
	// background while interactive reads are measured.
	delay.Store(int64(40 * time.Millisecond))
	hub.down.Store(false)
	n.d.KickOutbox()
	start := time.Now()
	var lat []time.Duration
	for i := 0; len(hub.sends()) < 20; i++ {
		if time.Since(start) > 20*time.Second {
			t.Fatalf("outbox not flushed: %d of 20 stored", len(hub.sends()))
		}
		s := time.Now()
		var err error
		if i%2 == 0 {
			_, err = owner.ListSessions(ctx, contracts.ListSessionsRequest{})
		} else {
			_, err = owner.GetProject(ctx, contracts.GetRequest{ID: fmt.Sprintf("prj_SYNTHFLUSH%04d", i)})
		}
		lat = append(lat, time.Since(s))
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	flushed := time.Since(start)
	t.Logf("outbox flush of 20 over a 40 ms hub took %s; %d interactive reads during it: p95=%s max=%s",
		flushed.Round(time.Millisecond), len(lat), p95(lat), slices.Max(lat))
	if len(lat) < 20 {
		t.Errorf("only %d reads overlapped the flush", len(lat))
	}
	if got := p95(lat); got > 100*time.Millisecond {
		t.Errorf("interactive read p95 %s during the outbox flush exceeds the 100 ms budget", got)
	}

	// The refused sends are retried by their caller; nothing was lost or
	// duplicated.
	delay.Store(0)
	for _, i := range rejected {
		if _, err := send(i); err != nil {
			t.Fatalf("retry of refused send %d: %v", i, err)
		}
	}
	waitFor(t, "all 30 stored", func() bool { return len(hub.sends()) == 30 })
	seen := map[string]bool{}
	for _, s := range hub.sends() {
		if seen[s.req.OperationID] {
			t.Fatalf("duplicate delivery of %s", s.req.OperationID)
		}
		seen[s.req.OperationID] = true
	}
	if len(seen) != 30 {
		t.Fatalf("hub stored %d distinct messages, want 30", len(seen))
	}
}
