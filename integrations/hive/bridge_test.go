package hive

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

func synthNotes() []contracts.Note {
	pid := "prj_01SYNTHPROJ"
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	return []contracts.Note{
		{NoteID: "note_01SYNTHA", Kind: contracts.NoteNote, Title: "Synthetic gadget note", Body: "The gadget calibrates at boot.", CreatedAt: now, SyncState: SyncPending},
		{NoteID: "note_01SYNTHB", Kind: contracts.NoteHandoff, ProjectID: &pid, SessionID: "ses_01SYNTH", Title: "Handoff: gadget calibration half done", Body: "Next: finish the calibration table.", CreatedAt: now, SyncState: SyncPending},
		{NoteID: "note_01SYNTHC", Kind: contracts.NoteDecision, ProjectID: &pid, Title: "Decision: gadgets use metric units", Body: "Agreed metric.", CreatedAt: now},
	}
}

func newTestBridge(t *testing.T, c Publisher, store NoteStore, clk contracts.Clock, state string) *Bridge {
	t.Helper()
	b, err := NewBridge(BridgeConfig{Store: store, Hive: c, Clock: clk, StateFile: state, PageSize: 2,
		InitialBackoff: time.Second, MaxBackoff: 8 * time.Second, MaxAttempts: 2})
	must(t, err)
	return b
}

func TestBridgePublishIdempotent(t *testing.T) {
	f := newFakeHome(t, true, true)
	c := New(f.env(), Options{StatusTTL: time.Hour, StagingRoot: filepath.Join(f.Home, "staging")})
	store := &memNotes{notes: synthNotes()}
	clk := &fakeClock{t: time.Now()}
	b := newTestBridge(t, c, store, clk, filepath.Join(f.Home, "bridge.json"))

	rep, err := b.SyncOnce(context.Background())
	must(t, err)
	if rep.Published != 3 || rep.Recorded != 3 || rep.Failed != 0 {
		t.Fatalf("first pass: %+v", rep)
	}
	if n := f.calls("note"); n != 3 {
		t.Fatalf("hive note called %d times, want 3", n)
	}
	for _, n := range synthNotes() {
		got := store.get(n.NoteID)
		if got.SyncState != SyncIndexed || !strings.HasPrefix(got.HiveRef, "note:") {
			t.Fatalf("hub record for %s: %+v", n.NoteID, got)
		}
	}
	if b.Pending() != 0 {
		t.Fatalf("pending %d", b.Pending())
	}

	// Second pass: nothing to do, no writes.
	rep, err = b.SyncOnce(context.Background())
	must(t, err)
	if rep.Published != 0 || f.calls("note") != 3 {
		t.Fatalf("second pass wrote again: %+v calls=%d", rep, f.calls("note"))
	}

	// Crash after the Hive write but before the hub recorded it, with the
	// ledger lost too: the marker lookup prevents a duplicate.
	refs := map[string]string{}
	for _, n := range synthNotes() {
		refs[n.NoteID] = store.get(n.NoteID).HiveRef
	}
	store.resetSync()
	b2 := newTestBridge(t, c, store, clk, "")
	rep, err = b2.SyncOnce(context.Background())
	must(t, err)
	if rep.Published != 0 || rep.AlreadyInHive != 3 || rep.Recorded != 3 || f.calls("note") != 3 {
		t.Fatalf("replay after crash: %+v calls=%d", rep, f.calls("note"))
	}
	for id, ref := range refs {
		if store.get(id).HiveRef != ref {
			t.Fatalf("ref changed for %s", id)
		}
	}
	// Deterministic op ids: the replayed hub updates reuse the same ids.
	seen := map[string]int{}
	for _, u := range store.updates {
		seen[u.OperationID]++
		if !contracts.ValidID(u.OperationID, contracts.PrefixOperation) {
			t.Fatalf("invalid op id %q", u.OperationID)
		}
	}
	for op, n := range seen {
		if n != 2 {
			t.Fatalf("op %s used %d times, want 2 (same logical update replayed)", op, n)
		}
	}

	// Published notes are now searchable, mapped back to the hub note.
	res, err := c.Search(context.Background(), contracts.MemorySearchParams{Query: "calibration", ProjectID: "prj_01SYNTHPROJ"})
	must(t, err)
	var handoff *contracts.MemoryHit
	for i := range res.Hits {
		if strings.Contains(res.Hits[i].Source, "pyre_note=note_01SYNTHB") {
			handoff = &res.Hits[i]
		}
	}
	if handoff == nil || handoff.Kind != "handoff" || handoff.ProjectID != "prj_01SYNTHPROJ" || handoff.Authoritative ||
		strings.Contains(handoff.Title, "[pyre:") {
		t.Fatalf("handoff hit: %+v", res.Hits)
	}
	// Launch pointers for the project find its bridged notes without a query.
	p := c.LaunchPointers(context.Background(), PointerRequest{ProjectID: "prj_01SYNTHPROJ", Timeout: 20 * time.Second})
	if len(p.Items) != 2 || len(p.References) != 2 || p.References[0].Kind != contracts.KindNote {
		t.Fatalf("project pointers: %+v", p)
	}
}

func TestBridgeHiveUnavailableLeavesNotesPending(t *testing.T) {
	home := t.TempDir()
	c := New(EnvFrom([]string{"HOME=" + home, "PATH=" + home}), Options{StatusTTL: time.Hour})
	store := &memNotes{notes: synthNotes()}
	clk := &fakeClock{t: time.Now()}
	b := newTestBridge(t, c, store, clk, "")
	rep, err := b.SyncOnce(context.Background())
	if err != nil || rep.Skipped != "hive_unavailable" || !strings.Contains(rep.HiveUnavailable, "not installed") {
		t.Fatalf("unavailable pass: %v %+v", err, rep)
	}
	if len(store.updates) != 0 || store.listCalls != 0 {
		t.Fatalf("touched hub while hive absent: %+v", store.updates)
	}
	rep, _ = b.SyncOnce(context.Background())
	if rep.Skipped != "backoff" {
		t.Fatalf("expected backoff, got %+v", rep)
	}
	clk.Advance(2 * time.Second)
	rep, _ = b.SyncOnce(context.Background())
	if rep.Skipped != "hive_unavailable" || rep.NextAttemptAt.Sub(clk.Now()) != 2*time.Second {
		t.Fatalf("backoff did not grow: %+v", rep)
	}
}

// Hive installed but not writable (index-only): notes written meanwhile are
// counted as pending although the bridge never attempted them.
func TestBridgePendingCountsUnattemptedNotes(t *testing.T) {
	f := newFakeHome(t, false, true)
	c := New(f.env(), Options{StatusTTL: time.Hour})
	b := newTestBridge(t, c, &memNotes{notes: synthNotes()}, &fakeClock{t: time.Now()}, "")
	c.SetPendingFunc(b.Pending)
	rep, err := b.SyncOnce(context.Background())
	if err != nil || rep.Skipped != "hive_unavailable" {
		t.Fatalf("pass: %v %+v", err, rep)
	}
	if got := b.Pending(); got != 3 {
		t.Fatalf("pending = %d, want 3 (none attempted, none indexed)", got)
	}
	if st := c.Status(context.Background()); st.Memory.PendingItems < 3 {
		t.Fatalf("memory status pending_items = %d", st.Memory.PendingItems)
	}
}

func TestBridgeWriteFailuresBackOffThenRecover(t *testing.T) {
	f := newFakeHome(t, true, true)
	c := New(f.env(), Options{StatusTTL: time.Hour, StagingRoot: filepath.Join(f.Home, "staging")})
	store := &memNotes{notes: synthNotes()[:1]}
	clk := &fakeClock{t: time.Now()}
	b := newTestBridge(t, c, store, clk, "")
	f.flag("fail-note", true)

	rep, _ := b.SyncOnce(context.Background())
	if rep.Failed != 1 || store.get("note_01SYNTHA").SyncState != SyncPending {
		t.Fatalf("first failure: %+v state=%q", rep, store.get("note_01SYNTHA").SyncState)
	}
	rep, _ = b.SyncOnce(context.Background()) // within per-note backoff
	if rep.Candidates != 0 {
		t.Fatalf("retried inside backoff: %+v", rep)
	}
	clk.Advance(2 * time.Second)
	rep, _ = b.SyncOnce(context.Background())
	if rep.Failed != 1 || store.get("note_01SYNTHA").SyncState != SyncFailed {
		t.Fatalf("after MaxAttempts: %+v state=%q", rep, store.get("note_01SYNTHA").SyncState)
	}
	f.flag("fail-note", false)
	clk.Advance(time.Minute)
	rep, _ = b.SyncOnce(context.Background())
	if rep.Published != 1 || store.get("note_01SYNTHA").SyncState != SyncIndexed {
		t.Fatalf("recovery: %+v", rep)
	}
}

func TestBridgeWrittenButUnverifiedNeverRewrites(t *testing.T) {
	f := newFakeHome(t, true, true)
	c := New(f.env(), Options{StatusTTL: time.Hour, StagingRoot: filepath.Join(f.Home, "staging")})
	store := &memNotes{notes: synthNotes()[:1]}
	clk := &fakeClock{t: time.Now()}
	state := filepath.Join(f.Home, "bridge.json")
	b := newTestBridge(t, c, store, clk, state)
	f.flag("hide-note", true)
	rep, _ := b.SyncOnce(context.Background())
	if rep.Failed != 1 || f.calls("note") != 1 {
		t.Fatalf("hidden write: %+v", rep)
	}
	// A restarted bridge reads the ledger and does not write again.
	b2 := newTestBridge(t, c, store, clk, state)
	for i := 0; i < 4; i++ {
		clk.Advance(time.Minute)
		_, _ = b2.SyncOnce(context.Background())
	}
	if f.calls("note") != 1 {
		t.Fatalf("rewrote an already-written note: %d calls", f.calls("note"))
	}
	if store.get("note_01SYNTHA").SyncState != SyncFailed {
		t.Fatalf("unverifiable write should surface as failed, got %q", store.get("note_01SYNTHA").SyncState)
	}
}

func TestBridgeHubOutageKeepsRef(t *testing.T) {
	f := newFakeHome(t, true, true)
	c := New(f.env(), Options{StatusTTL: time.Hour, StagingRoot: filepath.Join(f.Home, "staging")})
	store := &memNotes{notes: synthNotes()[:1], failNext: 1}
	clk := &fakeClock{t: time.Now()}
	state := filepath.Join(f.Home, "bridge.json")
	b := newTestBridge(t, c, store, clk, state)
	rep, _ := b.SyncOnce(context.Background())
	if rep.Published != 1 || rep.HubErrors != 1 || b.Pending() != 1 {
		t.Fatalf("hub outage: %+v pending=%d", rep, b.Pending())
	}
	b2 := newTestBridge(t, c, store, clk, state)
	rep, _ = b2.SyncOnce(context.Background())
	if rep.Recorded != 1 || rep.Published != 0 || f.calls("note") != 1 || f.calls("search") != 2 {
		// search: one marker lookup before the write, one verification after;
		// the retry uses the ledgered ref and needs neither.
		t.Fatalf("retry after hub outage: %+v notes=%d searches=%d", rep, f.calls("note"), f.calls("search"))
	}
}

func TestBridgeBatchBound(t *testing.T) {
	f := newFakeHome(t, true, true)
	c := New(f.env(), Options{StatusTTL: time.Hour, StagingRoot: filepath.Join(f.Home, "staging")})
	store := &memNotes{notes: synthNotes()}
	b, err := NewBridge(BridgeConfig{Store: store, Hive: c, BatchSize: 1, PageSize: 10})
	must(t, err)
	rep, _ := b.SyncOnce(context.Background())
	if rep.Published != 1 || rep.Deferred != 2 {
		t.Fatalf("batch bound: %+v", rep)
	}
}
