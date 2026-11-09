package hive

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/client"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/httpapi"
	"github.com/jhh3/pyre/internal/projects"
	"github.com/jhh3/pyre/internal/projects/svcutil/svctest"
)

type notesHub struct {
	contracts.API
	s *projects.Store
}

func (h notesHub) ListNotes(ctx context.Context, r contracts.ListNotesRequest) (contracts.Page[contracts.Note], error) {
	return h.s.ListNotes(ctx, r)
}
func (h notesHub) UpdateNoteSync(ctx context.Context, r contracts.UpdateNoteSyncRequest) (contracts.Note, error) {
	return h.s.UpdateNoteSync(ctx, r)
}
func (h notesHub) GetNote(ctx context.Context, r contracts.GetRequest) (contracts.Note, error) {
	return h.s.GetNote(ctx, r)
}

type wireCount struct{ calls, bytes atomic.Int64 }

type countW struct {
	http.ResponseWriter
	n int64
}

func (w *countW) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	w.n += int64(n)
	return n, err
}

func TestMeasureNotesTraffic(t *testing.T) {
	if os.Getenv("PYRE_MEASURE") == "" {
		t.Skip("measurement harness")
	}
	db := svctest.OpenDB(t)
	store, err := projects.New(context.Background(), db, svctest.NewSink(t, db), projects.Options{})
	must(t, err)
	body := strings.Repeat("Synthetic note body about gadget calibration and boot order. ", 24)
	for i := 0; i < 415; i++ {
		_, err := store.CreateNote(svctest.Owner(), contracts.CreateNoteRequest{Mutation: svctest.Op(), Kind: contracts.NoteNote, Title: fmt.Sprintf("Synthetic note %d", i), Body: body})
		must(t, err)
	}
	f := newFakeHome(t, true, true)
	hv := New(f.env(), Options{StatusTTL: time.Hour, StagingRoot: filepath.Join(f.Home, "staging")})
	node := contracts.Principal{Kind: contracts.PrincipalNode, ID: "node_synth"}
	bctx := contracts.WithPrincipal(context.Background(), node)
	b0, err := NewBridge(BridgeConfig{Store: store, Hive: hv, BatchSize: 1000, MaxScanPages: 100})
	must(t, err)
	for i := 0; i < 3; i++ {
		_, err = b0.SyncOnce(bctx)
		must(t, err)
	}

	run := func(name string, opts httpapi.Options, cfg BridgeConfig, passes int, step time.Duration, hours float64) {
		w := &wireCount{}
		inner := httpapi.NewServer(notesHub{s: store}, httpapi.NewStaticTokens(map[string]contracts.Principal{"tok": node}), opts)
		srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			c := &countW{ResponseWriter: rw}
			inner.ServeHTTP(c, r)
			if strings.HasSuffix(r.URL.Path, "notes.list") {
				w.calls.Add(1)
				w.bytes.Add(c.n)
			}
		}))
		defer srv.Close()
		clk := &fakeClock{t: time.Now()}
		cfg.Store, cfg.Hive, cfg.Clock = client.New(srv.URL, "tok", client.Options{}), hv, clk
		b, err := NewBridge(cfg)
		must(t, err)
		for i := 0; i < passes; i++ {
			_, err := b.SyncOnce(bctx)
			must(t, err)
			clk.Advance(step)
		}
		fmt.Printf("MEASURE notes.list %-30s per node per hour: %6.1f calls %10.0f bytes\n", name, float64(w.calls.Load())/hours, float64(w.bytes.Load())/hours)
	}
	run("before (30s, full scans, plain)", httpapi.Options{}, BridgeConfig{FullScanEvery: time.Nanosecond}, 120, 30*time.Second, 1)
	run("after (idle, filtered, gzip)", httpapi.Options{Compress: true, ETags: true}, BridgeConfig{}, 36, 10*time.Minute, 6)
}
