package tui_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/digest"
	"github.com/jhh3/pyre/internal/runtime/mock"
	"github.com/jhh3/pyre/internal/tui"
	"github.com/jhh3/pyre/internal/tui/tuitest"
)

// startDigest runs the TUI over the synthetic digest fixture with a state
// directory whose last-seen mark is the fixture's cutoff (cursor 100).
func startDigest(t *testing.T, w, ht int, dir string, in digest.Input) *tuitest.Harness {
	t.Helper()
	if dir == "" {
		dir = t.TempDir()
	}
	state := filepath.Join(dir, "tui-state.json")
	if _, err := os.Stat(state); err != nil {
		if err := os.WriteFile(state, []byte(`{"tips_dismissed":true}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := digest.SaveLastSeen(filepath.Join(dir, "digest-state.json"), digest.LastSeen{Cursor: in.SinceCursor, At: in.Since}); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := tuitest.NewMock(t)
	api := &digest.FixtureAPI{API: b, In: in}
	opts := tuitest.Options()
	opts.Clock = mock.NewFakeClock(digest.SyntheticNow)
	opts.StateFile = state
	h := tuitest.New(t, api, opts, w, ht)
	h.WaitFor("digest", 5*time.Second, func() bool { return h.M.DigestLoaded() && h.M.Loaded("sessions") && h.M.Loaded("attention") })
	h.Settle()
	return h
}

func lineIndex(view, s string) int {
	for i, l := range strings.Split(view, "\n") {
		if strings.Contains(l, s) {
			return i
		}
	}
	return -1
}

// openDigest opens the digest view (<leader>w; :digest does the same).
func openDigest(h *tuitest.Harness) {
	h.Key("space", "w")
	h.Settle()
}

func TestDigestPanelOnLaunch(t *testing.T) {
	h := startDigest(t, 200, 55, "", digest.SyntheticInput())
	if !h.Contains("While you were away: 5 agents, 2 need you — :digest shows it") {
		t.Fatalf("launch toast missing:\n%s", h.View())
	}
	if strings.Contains(h.View(), "› Inbox") {
		t.Fatalf("the digest opens at launch, not the Inbox (W-inbox):\n%s", h.View())
	}
	openDigest(h)
	v := h.View()
	if h.M.ActiveView() != "While you were away" {
		t.Fatalf("view = %s", h.M.ActiveView())
	}
	if !strings.Contains(v, "While you were away · since ") || !strings.Contains(v, "5 agents, 2 need you") || !strings.Contains(v, "esc mark seen") {
		t.Fatalf("panel header:\n%s", v)
	}
	if strings.Contains(v, ":digest shows it") {
		t.Fatal("the launch toast should clear once the digest is on screen")
	}
	// Ranked: needs you, blocked, done (newest first), still working; then
	// the collapsed line.
	order := []string{"! synth-api-refactor", "✗ synth-flaky-ci", "✓ synth-codex-check", "✓ synth-docs-pass", "● synth-search-index", "2 quiet sessions"}
	last := -1
	for _, s := range order {
		i := lineIndex(v, s)
		if i <= last {
			t.Fatalf("%q at line %d, want after %d:\n%s", s, i, last, v)
		}
		last = i
	}
	if !strings.Contains(v, "› ! synth-api-refactor") || !h.M.DigestFocused() {
		t.Fatalf("the first line should hold the cursor:\n%s", v)
	}
	if !strings.Contains(v, "synth-linux was offline for 12 minutes") {
		t.Fatal("non-session items need a place in the panel")
	}
	// Detail pane: the selected line's deep link and what enter does.
	if !strings.Contains(v, "enter open the session (a answers)") || !strings.Contains(v, "pyre://approval/apr_synthA") {
		t.Fatalf("detail for the needs-you line:\n%s", v)
	}
	for _, want := range []string{"↑↓ select", "enter open", "r transcript", "esc mark seen"} {
		if !strings.Contains(v[strings.LastIndex(v, "\n"):], want) {
			t.Errorf("footer misses %q", want)
		}
	}
	golden(t, "digest-200x55", v)
}

func TestDigestPanelNarrow(t *testing.T) {
	h := startDigest(t, 120, 34, "", digest.SyntheticInput())
	h.Key(":")
	h.Type("digest")
	h.Key("enter")
	h.Settle()
	v := h.View()
	if !strings.Contains(v, "› ! synth-api-refactor") || h.M.ActiveView() != "While you were away" {
		t.Fatalf("120x34 digest via :digest:\n%s", v)
	}
	golden(t, "digest-120x34", v)
}

func TestDigestPanelScrollsManyAgents(t *testing.T) {
	in := digest.SyntheticInput()
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("ses_synthW%02d", i)
		in.Sessions = append(in.Sessions, contracts.SessionView{
			Session:  contracts.Session{SessionID: id, Title: fmt.Sprintf("synth-worker-%02d", i), Harness: contracts.HarnessClaude},
			NodeName: "synth-mac",
			Run:      &contracts.Run{RunID: "run_" + id, SessionID: id, NodeID: "node_synth-mac", State: contracts.RunRunning, Capabilities: []string{contracts.CapTranscript}},
		})
		in.Transcripts[id] = contracts.ReadSessionResponse{Entries: []contracts.TranscriptEntry{
			{Seq: 1, RunID: "run_" + id, At: in.Since.Add(time.Duration(i+1) * time.Minute), Kind: "assistant", Text: fmt.Sprintf("Synthetic worker %02d is busy.", i)}}}
	}
	h := startDigest(t, 160, 24, "", in)
	openDigest(h)
	v := h.View()
	if !strings.Contains(v, "17 agents, 2 need you") || !strings.Contains(v, "more agents (j/k)") {
		t.Fatalf("a long digest scrolls inside a bounded panel:\n%s", v)
	}
	h.Key("end") // G opens the Grid; end is nav.bottom
	if v := h.View(); !strings.Contains(v, "› ● synth-worker-00") {
		t.Fatalf("end reaches the last (oldest working) line:\n%s", v)
	}
}

func TestDigestNavigationAndDetail(t *testing.T) {
	h := startDigest(t, 200, 55, "", digest.SyntheticInput())
	openDigest(h)
	h.Key("j", "j", "j")
	h.Settle()
	v := h.View()
	if !strings.Contains(v, "› ✓ synth-docs-pass") {
		t.Fatalf("cursor on synth-docs-pass:\n%s", v)
	}
	for _, want := range []string{"Activity   2 turns · 5 tool calls · 1 reply", "Files      docs/digest.md", "internal/digest/digest_test.go",
		"Artifacts  synth digest spec v2", "Docs       synth brief: digest section", "Messages   → Synthetic: the docs are ready for review.",
		"enter open the transcript at this moment", "pyre://session/ses_synthC?run=run_synthC&seq=812"} {
		if !strings.Contains(v, want) {
			t.Errorf("detail misses %q", want)
		}
	}
	// The cursor stops at the last line.
	h.Key("j", "j", "j")
	if !h.M.DigestFocused() || !strings.Contains(h.View(), "› ● synth-search-index") {
		t.Fatalf("down stops on the last line:\n%s", h.View())
	}
	// Session keys mean nothing on a digest line.
	h.Key("d")
	if !h.M.DigestFocused() {
		t.Fatal("d must not leave the digest")
	}
}

func TestDigestEnterJumpsToTheMoment(t *testing.T) {
	in := digest.SyntheticInput()
	// A long transcript: the moment (seq 812) is far below the top.
	tr := in.Transcripts["ses_synthC"]
	var early []contracts.TranscriptEntry
	for i := 0; i < 120; i++ {
		early = append(early, contracts.TranscriptEntry{Seq: int64(600 + i), RunID: "run_synthC", At: in.Since.Add(-time.Hour),
			Kind: "assistant", Text: fmt.Sprintf("Synthetic filler line %03d.", i)})
	}
	tr.Entries = append(early, tr.Entries...)
	in.Transcripts["ses_synthC"] = tr
	h := startDigest(t, 160, 44, "", in)
	openDigest(h)
	h.Key("j", "j", "j", "enter")
	h.WaitFor("transcript", 3*time.Second, func() bool { return h.M.Loaded("transcript:ses_synthC") })
	h.Settle()
	v := h.View()
	if h.M.ActiveView() != "Transcript" {
		t.Fatalf("enter should open the transcript, got %s:\n%s", h.M.ActiveView(), v)
	}
	if !strings.Contains(v, "Updated the digest docs") || strings.Contains(v, "Synthetic filler line 000") {
		t.Fatalf("the reader should open at the moment, not the top:\n%s", v)
	}
	if !strings.Contains(v, "at the moment of: Updated the digest docs") {
		t.Fatalf("toast should name the moment:\n%s", v)
	}
	// esc returns to the digest with its cursor intact.
	h.Key("esc")
	if h.M.ActiveView() != "While you were away" || !strings.Contains(h.View(), "› ✓ synth-docs-pass") {
		t.Fatalf("back to the digest:\n%s", h.View())
	}
}

func TestDigestEnterOnNeedsYouOpensTheSession(t *testing.T) {
	h := startDigest(t, 200, 55, "", digest.SyntheticInput())
	openDigest(h)
	h.Key("enter")
	h.Settle()
	if h.M.ActiveView() != "Sessions" || h.M.SelectedSession() != "ses_synthA" {
		t.Fatalf("enter on a needs-you line should open its session: %s sel=%q", h.M.ActiveView(), h.M.SelectedSession())
	}
	// A blocked line (failed run, no attention item) opens the transcript.
	openDigest(h)
	h.Key("home", "j")
	if !strings.Contains(h.View(), "enter open the transcript") {
		t.Fatalf("an ended blocked session (hidden from the live list) opens its transcript:\n%s", h.View())
	}
	h.Key("enter")
	h.Settle()
	if h.M.ActiveView() != "Transcript" || !strings.Contains(h.View(), "synth-flaky-ci") {
		t.Fatalf("blocked line should open its transcript: %s\n%s", h.M.ActiveView(), h.View())
	}
	h.Key("esc")
	h.Key("end")
	if !strings.Contains(h.View(), "› ● synth-search-index") {
		t.Fatalf("end: %s", h.View())
	}
}

func TestDigestDismissMarksSeen(t *testing.T) {
	dir := t.TempDir()
	in := digest.SyntheticInput()
	h := startDigest(t, 200, 55, dir, in)
	openDigest(h)
	h.Key("esc")
	h.Settle()
	if strings.Contains(h.View(), "While you were away ·") || h.M.ActiveView() != "Sessions" {
		t.Fatalf("esc should mark seen and go back:\n%s", h.View())
	}
	ls, ok := digest.LoadLastSeen(filepath.Join(dir, "digest-state.json"))
	if !ok || ls.Cursor != in.HeadCursor {
		t.Fatalf("mark = %+v, want cursor %d", ls, in.HeadCursor)
	}
	h.Close()
	// Next launch: nothing new since the mark, no digest and no toast.
	h2 := startDigest(t, 200, 55, dir, in)
	if h2.Contains("While you were away") {
		t.Fatalf("nothing new: no toast:\n%s", h2.View())
	}
	openDigest(h2)
	if h2.Contains("While you were away ·") || !h2.Contains("nothing happened since you last looked") {
		t.Fatalf("nothing new: no digest:\n%s", h2.View())
	}
}

func TestDigestQuitMarksSeenOnlyWhenShown(t *testing.T) {
	in := digest.SyntheticInput()
	// Never opened the digest: the mark stays, so it is still there next time.
	dir := t.TempDir()
	h := startDigest(t, 200, 55, dir, in)
	h.Close()
	if ls, _ := digest.LoadLastSeen(filepath.Join(dir, "digest-state.json")); ls.Cursor != in.SinceCursor {
		t.Fatalf("unseen digest must not move the mark: %+v", ls)
	}
	// Looked at it, then quit: marked seen.
	dir2 := t.TempDir()
	h2 := startDigest(t, 200, 55, dir2, in)
	openDigest(h2)
	h2.Close()
	if ls, _ := digest.LoadLastSeen(filepath.Join(dir2, "digest-state.json")); ls.Cursor != in.HeadCursor {
		t.Fatalf("seen digest should move the mark on quit: %+v", ls)
	}
}

func TestDigestOffWithoutStateFile(t *testing.T) {
	h, _ := start(t, 200, 55, nil, func(o *tui.Options) { o.StateFile = "" })
	openDigest(h)
	if h.Contains("While you were away ·") || !h.Contains("no digest here") {
		t.Fatalf("no state file (embedders, tests): no digest:\n%s", h.View())
	}
}
