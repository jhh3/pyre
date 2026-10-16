package assemble

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/daemon"
	"github.com/jhh3/pyre/internal/runtime/mock"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
	"github.com/jhh3/pyre/internal/transport"
)

// slowReadAdapter is the mock adapter whose transcript read takes a fixed
// time (the cost of parsing a large transcript).
type slowReadAdapter struct {
	*mock.Adapter
	cost atomic.Int64 // nanoseconds
}

func (a *slowReadAdapter) ReadTranscript(ctx context.Context, h c.RunHandle, req c.ReadSessionRequest) (c.ReadSessionResponse, error) {
	time.Sleep(time.Duration(a.cost.Load()))
	return a.Adapter.ReadTranscript(ctx, h, req)
}

// TestReadAttachLatencyEndToEnd measures sessions.read and sessions.attach
// through the real hub, node transport and daemon, with reads costing what
// a 50 MB transcript cost before (180 ms) and after (0.1 ms) the tail
// cache, while a client polls reads like the TUI preview does. pyred runs
// hub commands one at a time, so an attach waits for the reads queued
// before it. Set PYRE_MEASURE_READ=1 to run it.
func TestReadAttachLatencyEndToEnd(t *testing.T) {
	if os.Getenv("PYRE_MEASURE_READ") == "" {
		t.Skip("set PYRE_MEASURE_READ=1 to measure")
	}
	base := t.TempDir()
	h, addr := serveHub(t, filepath.Join(base, "hub"), "127.0.0.1:0")
	defer h.stop()
	owner := c.WithPrincipal(context.Background(), c.Principal{Kind: c.PrincipalOwner, ID: "cli_synth-latency"})
	tk, err := h.st.Fleet.CreateEnrollment(owner, c.EnrollmentRequest{Mutation: c.Mutation{OperationID: c.NewID(c.PrefixOperation)}, Name: "synth-latency"})
	if err != nil {
		t.Fatal(err)
	}
	cred, err := h.st.Enrollment.RedeemNode(context.Background(), c.NodeEnrollExchange{NodeID: tk.NodeID, Secret: tk.Secret, Hostname: "synth-latency", Platform: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	state, err := os.MkdirTemp("/tmp", "pyre-latency-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(state) })
	db, err := sqlitex.Open(filepath.Join(state, "pyred.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ad := &slowReadAdapter{Adapter: mock.NewAdapter(mock.AdapterOptions{})}
	d, err := daemon.New(daemon.Config{StateDir: state, DB: db, NodeID: cred.NodeID, HeartbeatInterval: time.Hour,
		Adapters: []c.RuntimeAdapter{ad}, Poll: daemon.PollConfig{Interval: 200 * time.Millisecond, IdleMax: time.Second},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	tlsCfg, err := transport.PinnedTLSConfig(h.st.Cert.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	link, err := transport.NewNodeLink(transport.NodeLinkConfig{HubURL: "https://" + addr, NodeID: cred.NodeID, Token: cred.Token, TLS: tlsCfg, DB: db,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Hello: d.Hello, Handler: transport.CommandHandlerFunc(d.HandleCommand),
		Backoff: transport.BackoffConfig{Initial: 20 * time.Millisecond, Max: 200 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	d.SetReporter(link)
	ctx, cancel := context.WithCancel(context.Background())
	if err := d.Start(ctx); err != nil {
		t.Fatal(err)
	}
	linkDone := make(chan struct{})
	go func() { link.Run(ctx); close(linkDone) }()
	defer func() { cancel(); <-linkDone; d.Close() }()
	waitFor(t, "node connected", func() bool { return h.st.NodeServer.Connected(cred.NodeID) })
	var sids []string
	for i := 0; i < 3; i++ {
		pr, err := h.st.Hub.StartSession(owner, c.StartSessionRequest{Mutation: c.Mutation{OperationID: c.NewID(c.PrefixOperation)}, NodeID: cred.NodeID,
			Harness: c.HarnessMock, Cwd: state, Title: "synthetic latency"})
		if err != nil {
			t.Fatal(err)
		}
		sid := pr.Session.Session.SessionID
		waitFor(t, "idle", func() bool {
			v, err := h.st.Hub.GetSession(owner, c.GetRequest{ID: sid})
			return err == nil && v.Run != nil && v.Run.State == c.RunWaitingInput
		})
		sids = append(sids, sid)
	}
	pct := func(ds []time.Duration) string {
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		n := len(ds)
		return "p50=" + ds[n/2].Round(100*time.Microsecond).String() + " p90=" + ds[n*9/10].Round(100*time.Microsecond).String() + " max=" + ds[n-1].Round(100*time.Microsecond).String()
	}
	for _, cost := range []time.Duration{180 * time.Millisecond, 100 * time.Microsecond} {
		ad.cost.Store(int64(cost))
		// Background: previews of the three sessions, 3 reads/s.
		stop := make(chan struct{})
		var wg sync.WaitGroup
		var mu sync.Mutex
		var reads []time.Duration
		for _, sid := range sids {
			wg.Add(1)
			go func(sid string) {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					case <-time.After(time.Second):
					}
					t0 := time.Now()
					if _, err := h.st.Hub.ReadSession(owner, c.ReadSessionRequest{SessionID: sid, Tail: true, Limit: 30}); err != nil {
						t.Error(err)
					}
					mu.Lock()
					reads = append(reads, time.Since(t0))
					mu.Unlock()
				}
			}(sid)
		}
		var attaches []time.Duration
		for i := 0; i < 30; i++ {
			time.Sleep(time.Duration(97+i%7*31) * time.Millisecond)
			t0 := time.Now()
			if _, err := h.st.Hub.AttachSession(owner, c.AttachRequest{Mutation: c.Mutation{OperationID: c.NewID(c.PrefixOperation)}, SessionID: sids[i%len(sids)], ReadOnly: true}); err != nil {
				t.Fatal(err)
			}
			attaches = append(attaches, time.Since(t0))
		}
		close(stop)
		wg.Wait()
		t.Logf("node read cost %s: sessions.read n=%d %s; sessions.attach n=%d %s", cost, len(reads), pct(reads), len(attaches), pct(attaches))
	}
}
