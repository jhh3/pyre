package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/mock"
)

// rc126: `pyre note show <id>` and `pyre notes show <id>` print one note.
func TestNoteShow(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	for _, cmd := range []string{"note", "notes"} {
		r := run(t, e, nil, cmd, "show", "note_synth-handoff-1")
		expectCode(t, r, 0)
		for _, want := range []string{"handoff note_synth-handoff-1 \"Handoff: api work\"", "author:  system pyre", "session: ses_synth-fbx", "created: ", "Synthetic handoff: branch api-client"} {
			if !strings.Contains(r.stdout, want) {
				t.Fatalf("%s show: missing %q\n%s", cmd, want, r)
			}
		}
		r = run(t, e, nil, "--json", cmd, "show", "note_synth-handoff-1")
		expectCode(t, r, 0)
		n := mustJSON[c.Note](t, r)
		if n.NoteID != "note_synth-handoff-1" || n.Kind != c.NoteHandoff || !strings.HasPrefix(n.Body, "Synthetic handoff") || n.CreatedAt.IsZero() {
			t.Fatalf("%s show --json: %+v", cmd, n)
		}
	}
	r := run(t, e, nil, "note", "show", "note_synth-missing")
	expectCode(t, r, ExitNotFound)
	expectCode(t, run(t, e, nil, "note", "show", "ses_synth-b"), ExitUsage)
}

// rc126: `pyre note <note_id>` with no body must not create an empty note
// titled with the ID.
func TestNoteRefusesIDTitleWithoutBody(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	id := c.NewID(c.PrefixNote)
	r := run(t, e, nil, "note", id)
	expectCode(t, r, ExitUsage)
	if !strings.Contains(r.stderr, "to read a note use: pyre note show <id>") {
		t.Fatalf("guard message: %s", r)
	}
	r = run(t, e, nil, "--json", "notes", "--query", id)
	expectCode(t, r, 0)
	if p := mustJSON[c.Page[c.Note]](t, r); len(p.Items) != 0 {
		t.Fatalf("the refused note was created: %+v", p.Items)
	}
	// With a body it is an ordinary note; an ordinary title needs no body.
	expectCode(t, run(t, e, nil, "note", id, "--body", "about this note"), 0)
	expectCode(t, run(t, e, nil, "note", "plain title"), 0)
}

// rc127: `pyre note ls` created an empty note titled "ls"; it now lists
// notes, and other bodyless command words are refused.
func TestNoteLsListsAndCommandWordsRefused(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	for _, args := range [][]string{{"note", "ls"}, {"note", "list"}} {
		r := run(t, e, nil, append([]string{"--json"}, args...)...)
		expectCode(t, r, 0)
		if p := mustJSON[c.Page[c.Note]](t, r); len(p.Items) == 0 {
			t.Fatalf("%v listed nothing: %s", args, r)
		}
	}
	for _, w := range []string{"search", "RM", "help "} {
		r := run(t, e, nil, "note", w)
		expectCode(t, r, ExitUsage)
		if !strings.Contains(r.stderr, "pyre note ls") {
			t.Fatalf("note %q guard message: %s", w, r)
		}
	}
	expectCode(t, run(t, e, nil, "note", "search", "--body", "a note about search"), 0)
}

func TestGetNoteMCPTool(t *testing.T) {
	b := mock.New(mock.Options{Clock: mock.NewOffsetClock(mock.FixtureEpoch), DefaultPrincipal: &testOwner})
	ops := &Ops{API: b, PollInterval: 10 * time.Millisecond, Self: Identity{SessionID: "ses_synth-b", NodeID: "node_synth-mac"}}
	var get, create MCPTool
	for _, tl := range MCPTools() {
		switch tl.Name {
		case "get_note":
			get = tl
		case "create_note":
			create = tl
		}
	}
	if get.run == nil || !get.Annotations.ReadOnlyHint {
		t.Fatalf("get_note not registered read-only: %+v", get)
	}
	ctx := context.Background()
	v, err := get.run(ctx, ops, json.RawMessage(`{"note_id":"note_synth-handoff-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if n := v.(c.Note); n.NoteID != "note_synth-handoff-1" || !strings.HasPrefix(n.Body, "Synthetic handoff") {
		t.Fatalf("get_note: %+v", n)
	}
	if _, err := get.run(ctx, ops, json.RawMessage(`{"note_id":"note_synth-missing"}`)); c.AsError(err).Code != c.ErrNotFound {
		t.Fatalf("get_note missing: %v", err)
	}
	if _, err := get.run(ctx, ops, json.RawMessage(`{"note_id":"msg_synth-x"}`)); err == nil {
		t.Fatal("get_note must reject a non-note ID")
	}
	if _, err := create.run(ctx, ops, json.RawMessage(`{"title":"note_synth-handoff-1","body":""}`)); err == nil || !strings.Contains(err.Error(), "pyre note show") {
		t.Fatalf("create_note with a note-ID title and no body: %v", err)
	}
}
