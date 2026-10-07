package resilience

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/jhh3/pyre/internal/client"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/transport"
	"github.com/jhh3/pyre/tests/resilience/faultnet"
)

// attemptLog records attempt timestamps.
type attemptLog struct {
	mu sync.Mutex
	at []time.Time
}

func (a *attemptLog) hit() {
	a.mu.Lock()
	a.at = append(a.at, time.Now())
	a.mu.Unlock()
}

func (a *attemptLog) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.at)
}

// gaps returns the intervals between consecutive attempts.
func (a *attemptLog) gaps() []time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []time.Duration
	for i := 1; i < len(a.at); i++ {
		out = append(out, a.at[i].Sub(a.at[i-1]))
	}
	return out
}

// maxAttemptsWithBackoff bounds attempts in window for exponential backoff
// from initial to max with jitter fraction j (delay d drawn from
// [d*(1-j), d]): the fastest possible schedule, plus slack.
func maxAttemptsWithBackoff(window, initial, max time.Duration, mult, j float64, slack int) int {
	n, elapsed, d := 1, time.Duration(0), float64(initial)
	for {
		step := time.Duration(d * (1 - j))
		if step <= 0 {
			step = time.Millisecond
		}
		elapsed += step
		if elapsed > window {
			return n + slack
		}
		n++
		d *= mult
		if d > float64(max) {
			d = float64(max)
		}
	}
}

// TestClientRetryBoundedUnderPersistentUnavailable: an always-unavailable
// hub costs exactly MaxAttempts requests per call, with backoff that honours
// Retry-After capped by RetryMax; non-retryable errors are not retried.
func TestClientRetryBoundedUnderPersistentUnavailable(t *testing.T) {
	var hits atomic.Int64
	var mode atomic.Value
	mode.Store("unavailable")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Count only our client's requests: local tools sometimes probe
		// loopback ports.
		if r.Header.Get("Authorization") == "Bearer tok" {
			hits.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		switch mode.Load() {
		case "unavailable":
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(contracts.ErrorBody{Error: contracts.Errorf(contracts.ErrUnavailable, "synthetic outage")})
		case "forbidden":
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(contracts.ErrorBody{Error: contracts.Errorf(contracts.ErrForbidden, "no")})
		case "bare502":
			w.WriteHeader(http.StatusBadGateway) // proxy page, no Pyre body
		}
	}))
	defer srv.Close()
	cl := client.New(srv.URL, "tok", client.Options{RetryBase: 10 * time.Millisecond, RetryMax: 100 * time.Millisecond, MaxAttempts: 5})
	ctx := context.Background()

	start := time.Now()
	_, err := cl.CreateProject(ctx, contracts.CreateProjectRequest{Name: "x"})
	took := time.Since(start)
	if !contracts.IsCode(err, contracts.ErrUnavailable) || hits.Load() != 5 {
		t.Fatalf("unavailable: err=%v hits=%d, want unavailable after exactly 5 attempts", err, hits.Load())
	}
	// Retry-After: 1 is capped at RetryMax (100ms) for 4 waits.
	if took > 2*time.Second {
		t.Fatalf("5 attempts took %v; Retry-After must be capped by RetryMax", took)
	}
	hits.Store(0)
	mode.Store("forbidden")
	if _, err := cl.CreateProject(ctx, contracts.CreateProjectRequest{Name: "x"}); !contracts.IsCode(err, contracts.ErrForbidden) || hits.Load() != 1 {
		t.Fatalf("forbidden: err=%v hits=%d, want 1 attempt", err, hits.Load())
	}
	hits.Store(0)
	mode.Store("bare502")
	// A bare 502 from a proxy maps to retryable unavailable: still bounded.
	_, err = cl.ListProjects(ctx, contracts.ListProjectsRequest{})
	if !contracts.IsCode(err, contracts.ErrUnavailable) || hits.Load() != 5 {
		t.Fatalf("bare 502: err=%v hits=%d, want unavailable after 5 attempts", err, hits.Load())
	}
	evidence(t, "persistent unavailable -> exactly 5 attempts in %v (Retry-After capped at RetryMax); forbidden -> 1 attempt; proxy 502 -> 5 attempts", took.Round(time.Millisecond))
}

// TestWatchEventsBackoffUnderPersistentStreamError: an event stream that
// accepts the WebSocket and immediately reports a retryable error must back
// off exponentially, not reconnect at the base rate forever.
func TestWatchEventsBackoffUnderPersistentStreamError(t *testing.T) {
	var log attemptLog
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != contracts.PathEventStream {
			http.NotFound(w, r)
			return
		}
		log.hit()
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		b, _ := json.Marshal(contracts.ErrorBody{Error: contracts.Errorf(contracts.ErrUnavailable, "event store unavailable")})
		_ = c.Write(r.Context(), websocket.MessageText, b)
		_ = c.Close(4500, "unavailable")
	}))
	defer srv.Close()
	base, max := 50*time.Millisecond, time.Second
	cl := client.New(srv.URL, "tok", client.Options{RetryBase: base, RetryMax: max})
	window := 3 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), window)
	defer cancel()
	_ = cl.WatchEvents(ctx, 0, func(contracts.Event) error { return nil })
	n := log.count()
	// client.backoff: full jitter in [d/2, d], d = base<<attempt capped at max.
	bound := maxAttemptsWithBackoff(window, base, max, 2, 0.5, 2)
	t.Logf("stream reconnects in %v: %d (exponential bound %d); gaps %v", window, n, bound, log.gaps())
	if n > bound {
		t.Fatalf("client.WatchEvents reconnected %d times in %v against a stream that always fails (bound %d)", n, window, bound)
	}
	evidence(t, "persistent stream error: %d reconnects in %v (bound %d)", n, window, bound)
}

// TestNodeLinkBackoffBounded: the node link's reconnect rate stays within
// its exponential schedule under each persistent failure shape.
func TestNodeLinkBackoffBounded(t *testing.T) {
	initial, max := 20*time.Millisecond, 200*time.Millisecond
	window := 2 * time.Second
	boundFor := func() int { return maxAttemptsWithBackoff(window, initial, max, 2, 0.5, 2) }

	run := func(t *testing.T, url string, mod func(*transport.NodeLinkConfig)) *testNode {
		node := newNode(t, "node_synthetic-backoff", "pyn_"+strings.Repeat("s", 43), func() string { return url }, newCountingHandler(), func(c *transport.NodeLinkConfig) {
			c.Backoff = transport.BackoffConfig{Initial: initial, Max: max}
			c.DialTimeout = time.Second
			if mod != nil {
				mod(c)
			}
		})
		node.start()
		time.Sleep(window)
		node.stop()
		return node
	}

	t.Run("http-503", func(t *testing.T) {
		var log attemptLog
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == contracts.PathNodeConnect {
				log.hit()
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(contracts.ErrorBody{Error: contracts.Errorf(contracts.ErrUnavailable, "down")})
		}))
		defer srv.Close()
		run(t, srv.URL, nil)
		if n := log.count(); n > boundFor() || n < 3 {
			t.Fatalf("503: %d attempts in %v, want 3..%d", n, window, boundFor())
		}
		evidence(t, "503: %d attempts in %v (bound %d)", log.count(), window, boundFor())
	})

	t.Run("connection-refused", func(t *testing.T) {
		px, err := faultnet.New("127.0.0.1:1")
		if err != nil {
			t.Fatal(err)
		}
		defer px.Close()
		px.SetMode(faultnet.ModeRefuse)
		run(t, "http://"+px.Addr(), nil)
		if n := px.Stats().Accepted; n > int64(boundFor()) || n < 3 {
			t.Fatalf("refused: %d attempts in %v, want 3..%d", n, window, boundFor())
		}
		evidence(t, "refused/reset: %d attempts in %v (bound %d)", px.Stats().Accepted, window, boundFor())
	})

	t.Run("flapping-after-welcome", func(t *testing.T) {
		// The hub accepts, welcomes, and the link dies immediately: backoff
		// must not reset (StableAfter) so flapping does not become a storm.
		ctx := ctxT(t)
		hub, _ := newHub(t, nil)
		nodeID, tok, err := hub.EnrollNode(ctx, "flap-node")
		if err != nil {
			t.Fatal(err)
		}
		px, err := faultnet.New(hub.Addr())
		if err != nil {
			t.Fatal(err)
		}
		defer px.Close()
		px.SetFrameFilter(func(f faultnet.Frame) faultnet.Action {
			if frameType(f) == "welcome" {
				return faultnet.Action{CutAfter: true}
			}
			return faultnet.Action{}
		})
		node := newNode(t, nodeID, tok, func() string { return "http://" + px.Addr() }, newCountingHandler(), func(c *transport.NodeLinkConfig) {
			c.Backoff = transport.BackoffConfig{Initial: initial, Max: max}
		})
		node.start()
		time.Sleep(window)
		node.stop()
		if n := px.Stats().Accepted; n > int64(boundFor()) || n < 3 {
			t.Fatalf("flapping: %d connections in %v, want 3..%d", n, window, boundFor())
		}
		evidence(t, "flapping after welcome: %d connections in %v (bound %d)", px.Stats().Accepted, window, boundFor())
	})

	t.Run("rejected-401", func(t *testing.T) {
		var log attemptLog
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == contracts.PathNodeConnect {
				log.hit()
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(contracts.ErrorBody{Error: contracts.Errorf(contracts.ErrUnauthorized, "credential revoked")})
		}))
		defer srv.Close()
		var last atomic.Value
		node := run(t, srv.URL, func(c *transport.NodeLinkConfig) {
			c.OnStateChange = func(s transport.LinkStatus) {
				if s.State == transport.LinkRejected {
					last.Store(s)
				}
			}
		})
		_ = node
		st, ok := last.Load().(transport.LinkStatus)
		if !ok {
			t.Fatal("link never entered the rejected state")
		}
		if st.NextRetry < max/2 {
			t.Fatalf("rejected link retries after %v, want the max backoff (>= %v)", st.NextRetry, max/2)
		}
		// Rejected links retry only at the max delay: at most window/(max/2)+1.
		if n := log.count(); n > int(window/(max/2))+2 {
			t.Fatalf("rejected: %d attempts in %v", n, window)
		}
		evidence(t, "401: state=rejected, %d attempts in %v at max backoff", log.count(), window)
	})
}
