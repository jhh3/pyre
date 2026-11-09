package hive

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/projects"
	"github.com/jhh3/pyre/internal/projects/svcutil/svctest"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// indexedNotes are n synthetic notes the bridge already indexed.
func indexedNotes(n int) []contracts.Note {
	out := make([]contracts.Note, n)
	for i := range out {
		out[i] = contracts.Note{NoteID: "note_01SYNTHIDX" + strconv.Itoa(i), Kind: contracts.NoteNote, Title: "Synthetic indexed note " + strconv.Itoa(i),
			Body: "Synthetic body.", SyncState: SyncIndexed, HiveRef: "note:1:synthetic" + strconv.Itoa(i)}
	}
	return out
}

func (m *memNotes) counts() (calls, listed, filtered int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.listCalls, m.listed, m.filtered
}

func (m *memNotes) add(n contracts.Note) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.notes = append(m.notes, n)
}

// A steady-state pass reads only notes that still need work: one short
// filtered page instead of every note. A new note is picked up by the next
// pass; every FullScanEvery one cycle reads every note again.
func TestBridgeIncrementalScanReturnsOnlyChanges(t *testing.T) {
	f := newFakeHome(t, true, true)
	c := New(f.env(), Options{StatusTTL: time.Hour, StagingRoot: filepath.Join(f.Home, "staging")})
	store := &memNotes{notes: indexedNotes(45)}
	clk := &fakeClock{t: time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)}
	b, err := NewBridge(BridgeConfig{Store: store, Hive: c, Clock: clk, PageSize: 10, FullScanEvery: time.Hour})
	must(t, err)
	ctx := context.Background()

	// First pass: the full cycle (safety net) reads every note once.
	rep, err := b.SyncOnce(ctx)
	must(t, err)
	calls, listed, filtered := store.counts()
	if rep.Candidates != 0 || listed != 45 || filtered != 0 {
		t.Fatalf("first pass: %+v listed=%d filtered=%d", rep, listed, filtered)
	}

	// Steady state: one filtered call, nothing returned.
	for i := 0; i < 3; i++ {
		rep, err = b.SyncOnce(ctx)
		must(t, err)
	}
	c2, l2, f2 := store.counts()
	if c2-calls != 3 || l2 != listed || f2 != 3 || rep.Scanned != 0 {
		t.Fatalf("steady state: %d calls, %d notes listed, %d filtered (want 3 calls, 0 notes)", c2-calls, l2-listed, f2)
	}

	// A new note is the only thing the next pass reads, and it is published.
	store.add(contracts.Note{NoteID: "note_01SYNTHNEW", Kind: contracts.NoteDecision, Title: "Synthetic decision", Body: "Use metric gadgets.", SyncState: SyncPending})
	rep, err = b.SyncOnce(ctx)
	must(t, err)
	_, l3, _ := store.counts()
	if rep.Scanned != 1 || rep.Published != 1 || rep.Recorded != 1 || l3-l2 != 1 {
		t.Fatalf("new note pass: %+v listed %d", rep, l3-l2)
	}
	if got := store.get("note_01SYNTHNEW"); got.SyncState != SyncIndexed || got.HiveRef == "" {
		t.Fatalf("new note not recorded indexed: %+v", got)
	}
	rep, err = b.SyncOnce(ctx)
	must(t, err)
	if _, l4, _ := store.counts(); rep.Scanned != 0 || l4 != l3 {
		t.Fatalf("after indexing, the scan still returns notes: %+v", rep)
	}

	// The periodic full cycle reads everything again (an "indexed" record
	// without a ref is only visible to it).
	store.mu.Lock()
	store.notes[3].HiveRef = ""
	store.mu.Unlock()
	clk.Advance(2 * time.Hour)
	rep, err = b.SyncOnce(ctx)
	must(t, err)
	if rep.Scanned != 46 || rep.Candidates != 1 {
		t.Fatalf("full cycle: %+v", rep)
	}
}

// The scan cursor is the hub's keyset position: it survives a hub restart
// (and a bridge restart, from the state file). A cursor the hub refuses
// starts the cycle over instead of failing every pass.
func TestBridgeCursorAcrossHubRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	openStore := func() (*projects.Store, func()) {
		db, err := sqlitex.Open(path)
		must(t, err)
		s, err := projects.New(context.Background(), db, svctest.NewSink(t, db), projects.Options{})
		must(t, err)
		return s, func() { db.Close() }
	}
	store, closeHub := openStore()
	owner := svctest.Owner()
	var ids []string
	for i := 0; i < 5; i++ {
		n, err := store.CreateNote(owner, contracts.CreateNoteRequest{Mutation: svctest.Op(), Kind: contracts.NoteNote, Title: "Synthetic note " + strconv.Itoa(i), Body: "Synthetic body " + strconv.Itoa(i)})
		must(t, err)
		ids = append(ids, n.NoteID)
	}
	f := newFakeHome(t, true, true)
	c := New(f.env(), Options{StatusTTL: time.Hour, StagingRoot: filepath.Join(f.Home, "staging")})
	bctx := contracts.WithPrincipal(context.Background(), contracts.Principal{Kind: contracts.PrincipalNode, ID: "node_synth"})
	state := filepath.Join(f.Home, "bridge.json")
	cfg := BridgeConfig{Store: store, Hive: c, PageSize: 2, BatchSize: 2, MaxScanPages: 1, StateFile: state}
	b, err := NewBridge(cfg)
	must(t, err)
	rep, err := b.SyncOnce(bctx)
	must(t, err)
	if rep.Published != 2 {
		t.Fatalf("first pass: %+v", rep)
	}
	cursor := readCursor(t, state)
	if cursor == "" {
		t.Fatal("no cursor persisted mid-cycle")
	}

	// Hub and bridge restart; the persisted cursor resumes the cycle.
	closeHub()
	store, closeHub = openStore()
	defer closeHub()
	cfg.Store = store
	b, err = NewBridge(cfg)
	must(t, err)
	for i := 0; i < 4; i++ {
		_, err = b.SyncOnce(bctx)
		must(t, err)
	}
	for _, id := range ids {
		n, err := store.GetNote(owner, contracts.GetRequest{ID: id})
		must(t, err)
		if n.SyncState != SyncIndexed {
			t.Fatalf("note %s not indexed after restart: %+v", id, n)
		}
	}
	if f.calls("note") != 5 {
		t.Fatalf("hive note called %d times, want 5", f.calls("note"))
	}

	// A cursor the hub cannot decode: the pass starts over, no error.
	writeCursor(t, state, "!!not-a-cursor!!")
	b, err = NewBridge(cfg)
	must(t, err)
	n, err := store.CreateNote(owner, contracts.CreateNoteRequest{Mutation: svctest.Op(), Kind: contracts.NoteNote, Title: "Synthetic late note", Body: "Synthetic late body"})
	must(t, err)
	rep, err = b.SyncOnce(bctx)
	if err != nil || rep.HubErrors != 0 {
		t.Fatalf("bad cursor pass: %v %+v", err, rep)
	}
	for i := 0; i < 3; i++ {
		_, err = b.SyncOnce(bctx)
		must(t, err)
	}
	if got, _ := store.GetNote(owner, contracts.GetRequest{ID: n.NoteID}); got.SyncState != SyncIndexed {
		t.Fatalf("note after a refused cursor: %+v", got)
	}
}

func readCursor(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	must(t, err)
	var l ledger
	must(t, json.Unmarshal(b, &l))
	return l.Cursor
}

func writeCursor(t *testing.T, path, cursor string) {
	t.Helper()
	b, err := os.ReadFile(path)
	must(t, err)
	var l ledger
	must(t, json.Unmarshal(b, &l))
	l.Cursor = cursor
	b, err = json.Marshal(l)
	must(t, err)
	must(t, os.WriteFile(path, b, 0o600))
}

// A hub that predates the sync_states filter refuses the field; the bridge
// then scans every note, as before, and keeps working.
func TestBridgeOldHubWithoutFilter(t *testing.T) {
	f := newFakeHome(t, true, true)
	c := New(f.env(), Options{StatusTTL: time.Hour, StagingRoot: filepath.Join(f.Home, "staging")})
	store := &memNotes{notes: append(indexedNotes(5), synthNotes()...), noFilter: true}
	clk := &fakeClock{t: time.Now()}
	b, err := NewBridge(BridgeConfig{Store: store, Hive: c, Clock: clk, PageSize: 3})
	must(t, err)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		rep, err := b.SyncOnce(ctx)
		if err != nil || rep.HubErrors != 0 {
			t.Fatalf("pass %d against an old hub: %v %+v", i, err, rep)
		}
	}
	for _, n := range synthNotes() {
		if got := store.get(n.NoteID); got.SyncState != SyncIndexed {
			t.Fatalf("note %s not indexed via an old hub: %+v", n.NoteID, got)
		}
	}
	// The old hub never answered a filtered read.
	if _, _, filtered := store.counts(); filtered != 0 {
		t.Fatalf("filtered calls accepted by an old hub: %d", filtered)
	}
	if !b.noFilter {
		t.Fatal("bridge did not latch the unfiltered scan")
	}
}

// Run backs off while passes find nothing and returns to Interval (at once)
// on Notify.
func TestBridgeRunIdleBackoff(t *testing.T) {
	b, err := NewBridge(BridgeConfig{Store: &memNotes{}, Hive: &stubHive{}, Interval: time.Second, MaxIdleInterval: 8 * time.Second})
	must(t, err)
	var got []time.Duration
	for i := 0; i <= 5; i++ {
		got = append(got, b.idleInterval(i))
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second, 8 * time.Second}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("idle intervals %v, want %v", got, want)
		}
	}

	store := &memNotes{notes: indexedNotes(3)}
	b, err = NewBridge(BridgeConfig{Store: store, Hive: &stubHive{}, Interval: 10 * time.Millisecond, MaxIdleInterval: 400 * time.Millisecond, MinPassGap: -1})
	must(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = b.Run(ctx) }()
	time.Sleep(time.Second)
	idlePasses := b.Passes()
	// Without backoff a 10ms period would give ~100 passes in a second;
	// doubling to 400ms gives about 8.
	if idlePasses < 3 || idlePasses > 15 {
		t.Fatalf("%d passes in 1s of idle (want a backed-off handful)", idlePasses)
	}
	b.Notify()
	deadline := time.Now().Add(200 * time.Millisecond)
	for b.Passes() == idlePasses && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if b.Passes() == idlePasses {
		t.Fatal("Notify did not start a pass promptly during idle backoff")
	}
}

// stubHive is an available Hive that already holds every note.
type stubHive struct{}

func (stubHive) Available(context.Context) (bool, string) { return true, "" }
func (stubHive) FindNote(_ context.Context, id string) (string, bool, error) {
	return "note:1:" + id, true, nil
}
func (stubHive) PublishNote(_ context.Context, n contracts.Note) (string, error) {
	return "note:1:" + n.NoteID, nil
}
