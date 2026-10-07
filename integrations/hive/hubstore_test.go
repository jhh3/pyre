package hive

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/projects"
	"github.com/jhh3/pyre/internal/projects/svcutil/svctest"
)

// The bridge against the real hub note store (internal/projects): real
// idempotent mutations, real cursors, real UpdateNoteSync validation.
func TestBridgeWithRealProjectsStore(t *testing.T) {
	db := svctest.OpenDB(t)
	store, err := projects.New(context.Background(), db, svctest.NewSink(t, db), projects.Options{})
	must(t, err)
	owner := svctest.Owner()
	p, err := store.CreateProject(owner, contracts.CreateProjectRequest{Mutation: svctest.Op(), Name: "Synthetic gadgets", Brief: "# Brief\n\nShip gadgets.\n"})
	must(t, err)
	sess := svctest.Session("ses_synth-worker")
	var ids []string
	for i, title := range []string{"Handoff: gadget calibration half done", "Decision: gadgets use metric", "Note: gadget boot order"} {
		kind := []contracts.NoteKind{contracts.NoteHandoff, contracts.NoteDecision, contracts.NoteNote}[i]
		n, err := store.CreateNote(sess, contracts.CreateNoteRequest{Mutation: svctest.Op(), Kind: kind, ProjectID: &p.ProjectID, Title: title, Body: "Synthetic body " + title})
		must(t, err)
		ids = append(ids, n.NoteID)
	}
	empty, err := store.CreateNote(owner, contracts.CreateNoteRequest{Mutation: svctest.Op(), Kind: contracts.NoteNote, Title: "Standalone note without body"})
	must(t, err)

	f := newFakeHome(t, true, true)
	c := New(f.env(), Options{StatusTTL: time.Hour, StagingRoot: filepath.Join(f.Home, "staging")})
	bctx := contracts.WithPrincipal(context.Background(), contracts.Principal{Kind: contracts.PrincipalNode, ID: "node_synth"})
	b, err := NewBridge(BridgeConfig{Store: store, Hive: c, PageSize: 2, StateFile: filepath.Join(f.Home, "bridge.json")})
	must(t, err)
	rep, err := b.SyncOnce(bctx)
	must(t, err)
	if rep.Published != 4 || rep.Recorded != 4 || rep.HubErrors != 0 {
		t.Fatalf("real store pass: %+v", rep)
	}
	for _, id := range append(ids, empty.NoteID) {
		n, err := store.GetNote(owner, contracts.GetRequest{ID: id})
		must(t, err)
		if n.SyncState != projects.SyncIndexed || !strings.HasPrefix(n.HiveRef, "note:") {
			t.Fatalf("hub note %s: %+v", id, n)
		}
	}
	rep, err = b.SyncOnce(bctx)
	must(t, err)
	if rep.Published != 0 || rep.Candidates != 0 || f.calls("note") != 4 {
		t.Fatalf("second pass: %+v calls=%d", rep, f.calls("note"))
	}

	// Hub launch context + Hive pointers: hub-listed notes are not repeated.
	lc, err := store.GetLaunchContext(owner, contracts.ContextRequest{ProjectID: &p.ProjectID})
	must(t, err)
	before := len(lc.References)
	ptrs := c.LaunchPointers(context.Background(), PointerRequest{ProjectID: p.ProjectID, Timeout: 20 * time.Second})
	if len(ptrs.Items) != 3 {
		t.Fatalf("pointers: %+v", ptrs)
	}
	AppendPointers(&lc, ptrs, 8<<10)
	seen := map[string]int{}
	for _, r := range lc.References {
		seen[r.ID]++
	}
	for id, n := range seen {
		if n > 1 {
			t.Fatalf("reference %s duplicated in launch context", id)
		}
	}
	if lc.MemoryFreshness == nil || lc.MemoryFreshness.Source != "hive:cli" || lc.Bytes != len(lc.Text) || len(lc.Text) > 8<<10 {
		t.Fatalf("launch context: refs %d->%d freshness=%+v", before, len(lc.References), lc.MemoryFreshness)
	}
}

func TestBridgeFetchesBodyForPartialNotes(t *testing.T) {
	f := newFakeHome(t, true, true)
	c := New(f.env(), Options{StatusTTL: time.Hour, StagingRoot: filepath.Join(f.Home, "staging")})
	full := synthNotes()[0]
	partial := full
	partial.Body = "" // as carried by note.created events
	store := &getterNotes{memNotes: memNotes{notes: []contracts.Note{partial}}, full: map[string]contracts.Note{full.NoteID: full}}
	b, err := NewBridge(BridgeConfig{Store: store, Hive: c})
	must(t, err)
	rep, err := b.SyncOnce(context.Background())
	must(t, err)
	if rep.Published != 1 || store.gets != 1 {
		t.Fatalf("partial note: %+v gets=%d", rep, store.gets)
	}
	h, err := c.Get(context.Background(), store.get(full.NoteID).HiveRef)
	must(t, err)
	if !strings.Contains(h.Snippet, "calibrates at boot") {
		t.Fatalf("body not published: %q", h.Snippet)
	}
}

type getterNotes struct {
	memNotes
	full map[string]contracts.Note
	gets int
}

func (g *getterNotes) GetNote(_ context.Context, req contracts.GetRequest) (contracts.Note, error) {
	g.gets++
	if n, ok := g.full[req.ID]; ok {
		return n, nil
	}
	return contracts.Note{}, contracts.Errorf(contracts.ErrNotFound, "no note")
}
