package digest

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

func agentByID(d Digest, id string) (Agent, bool) {
	for _, a := range d.Agents {
		if a.SessionID == id {
			return a, true
		}
	}
	return Agent{}, false
}

func TestBuildRanksNeedsYouThenDoneThenWorking(t *testing.T) {
	d := Build(SyntheticInput())
	var got []string
	for _, a := range d.Agents {
		got = append(got, a.SessionID+":"+string(a.Rank))
	}
	want := []string{
		"ses_synthA:needs_you", // actionable approval first
		"ses_synthB:needs_you", // blocked (failed run)
		"ses_synthH:done",      // finished 11:00
		"ses_synthC:done",      // finished 10:08
		"ses_synthD:working",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("order:\n got %v\nwant %v", got, want)
	}
	// Collapsed: the shell (observations only) and the archived session.
	if d.Quiet != 2 {
		t.Fatalf("quiet = %d, want 2", d.Quiet)
	}
	if _, ok := agentByID(d, "ses_synthF"); ok {
		t.Fatal("a session whose only event predates the cursor must not appear")
	}
	if len(d.Other) != 1 || d.Other[0].ID != "att_synthNode" {
		t.Fatalf("other = %+v, want the machine alert", d.Other)
	}
	if d.Count(RankNeedsYou) != 2 || d.Empty() {
		t.Fatalf("counts wrong: %+v", d)
	}
}

func TestBuildSummariesAndDeepLinks(t *testing.T) {
	d := Build(SyntheticInput())
	a, _ := agentByID(d, "ses_synthA")
	if a.Summary != "Approve: go test -bench ./..." || a.SummarySource != "attention" || a.Link != "pyre://approval/apr_synthA" {
		t.Fatalf("needs-you line: %+v", a)
	}
	if a.Seq != 41 || a.Turns != 0 || a.Tools != 3 || a.Replies != 1 {
		t.Fatalf("A counts: seq %d turns %d tools %d replies %d", a.Seq, a.Turns, a.Tools, a.Replies)
	}
	b, _ := agentByID(d, "ses_synthB")
	if b.Blocked != "failed: no eligible Codex account" || b.Summary != "Failed: no eligible Codex account" || b.Glyph() != "✗" {
		t.Fatalf("blocked line: %+v", b)
	}
	cc, _ := agentByID(d, "ses_synthC")
	if cc.Summary != "Updated the digest docs and added tests for ranking, e.g. needs-you first." {
		t.Fatalf("first sentence: %q", cc.Summary)
	}
	if cc.Link != "pyre://session/ses_synthC?run=run_synthC&seq=812" || cc.Seq != 812 {
		t.Fatalf("moment link: %q", cc.Link)
	}
	if cc.Turns != 2 || cc.Tools != 5 || cc.Replies != 1 || cc.Partial {
		t.Fatalf("C counts: %+v", cc)
	}
	if strings.Join(cc.Files, ",") != "docs/digest.md,internal/digest/digest_test.go" {
		t.Fatalf("files relative to cwd, deduplicated: %v", cc.Files)
	}
	if len(cc.Artifacts) != 1 || cc.Artifacts[0].Link != "pyre://artifact_version/ver_synthC2" || cc.Artifacts[0].Title != "synth digest spec v2" {
		t.Fatalf("artifacts: %+v", cc.Artifacts)
	}
	if len(cc.Docs) != 1 || cc.Docs[0].Link != "pyre://document_revision/rev_synthC" {
		t.Fatalf("docs: %+v", cc.Docs)
	}
	if len(cc.Messages) != 1 || cc.Messages[0].Direction != "sent" || cc.Messages[0].Link != "pyre://message/msg_synthCD" {
		t.Fatalf("messages: %+v", cc.Messages)
	}
	if f := cc.Facts(); f != "2 turns · 5 tools · 2 files · 1 artifact · 1 doc · 1 message" {
		t.Fatalf("facts: %q", f)
	}
	dd, _ := agentByID(d, "ses_synthD")
	if dd.Summary != "Indexing the synthetic vault: 3 of 7 shards done." || dd.Turns != 2 || !dd.Partial {
		t.Fatalf("working line: %+v", dd)
	}
	if strings.Join(dd.Files, ",") != "index/manifest.json,index/shard-3.idx" {
		t.Fatalf("codex file changes: %v", dd.Files)
	}
	if len(dd.Messages) != 1 || dd.Messages[0].Direction != "received" {
		t.Fatalf("received message: %+v", dd.Messages)
	}
	h, _ := agentByID(d, "ses_synthH")
	if h.Summary != "Finished" || h.SummarySource != "state" || h.Link != "pyre://session/ses_synthH" {
		t.Fatalf("state fallback: %+v", h)
	}
}

func TestBuildIgnoresOldAttentionAndCollapsesNoise(t *testing.T) {
	in := SyntheticInput()
	d := Build(in)
	if _, ok := agentByID(d, "ses_synthE"); ok {
		t.Fatal("observation-only shell must be collapsed")
	}
	// Add a real transcript turn to the shell: it is no longer noise.
	in.Transcripts["ses_synthE"] = c.ReadSessionResponse{Entries: []c.TranscriptEntry{
		{Seq: 1, RunID: "run_synthE", At: SyntheticNow.Add(-time.Hour), Kind: "user", Text: "make"}}}
	d = Build(in)
	if e, ok := agentByID(d, "ses_synthE"); !ok || e.Rank != RankWorking {
		t.Fatalf("shell with a turn: %+v %v", e, ok)
	}
}

func TestBuildEmpty(t *testing.T) {
	in := SyntheticInput()
	in.Events, in.Attention, in.Transcripts = nil, nil, nil
	d := Build(in)
	if !d.Empty() || len(d.Agents) != 0 {
		t.Fatalf("want empty digest, got %+v", d.Agents)
	}
	var b bytes.Buffer
	WriteText(&b, d, SyntheticNow)
	if !strings.Contains(b.String(), "Nothing happened") {
		t.Fatal(b.String())
	}
}

// The shared fixture: tests/fixtures/digest/{input,expected}.json. The
// Android port reads the same files (DigestBuilderTest.kt).
func TestSharedFixture(t *testing.T) {
	dir := filepath.Join("..", "..", "tests", "fixtures", "digest")
	in := SyntheticInput()
	d := Build(in)
	inB, _ := json.MarshalIndent(fixtureInput(in), "", "  ")
	outB, _ := json.MarshalIndent(d, "", "  ")
	if os.Getenv("PYRE_UPDATE_GOLDEN") == "1" {
		_ = os.MkdirAll(dir, 0o755)
		if err := os.WriteFile(filepath.Join(dir, "input.json"), append(inB, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "expected.json"), append(outB, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for name, want := range map[string][]byte{"input.json": inB, "expected.json": outB} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%v (regenerate with PYRE_UPDATE_GOLDEN=1)", err)
		}
		if !bytes.Equal(bytes.TrimSpace(got), bytes.TrimSpace(want)) {
			t.Fatalf("tests/fixtures/digest/%s is stale; regenerate with PYRE_UPDATE_GOLDEN=1 go test ./internal/digest", name)
		}
	}
	// The fixture decodes back into the same digest.
	var fi fixtureIn
	if err := json.Unmarshal(inB, &fi); err != nil {
		t.Fatal(err)
	}
	again, _ := json.MarshalIndent(Build(fi.input()), "", "  ")
	if !bytes.Equal(again, outB) {
		t.Fatal("fixture round trip changed the digest")
	}
}

// fixtureIn is Input in a JSON shape other clients can read.
type fixtureIn struct {
	Since       time.Time                        `json:"since"`
	SinceCursor int64                            `json:"since_cursor"`
	HeadCursor  int64                            `json:"head_cursor"`
	Now         time.Time                        `json:"now"`
	SinceLabel  string                           `json:"since_label"`
	Events      []c.Event                        `json:"events"`
	Sessions    []c.SessionView                  `json:"sessions"`
	Attention   []c.AttentionItem                `json:"attention"`
	Transcripts map[string]c.ReadSessionResponse `json:"transcripts"`
}

func fixtureInput(in Input) fixtureIn {
	return fixtureIn{in.Since, in.SinceCursor, in.HeadCursor, in.Now, in.SinceLabel, in.Events, in.Sessions, in.Attention, in.Transcripts}
}

func (f fixtureIn) input() Input {
	return Input{Since: f.Since, SinceCursor: f.SinceCursor, HeadCursor: f.HeadCursor, Now: f.Now, SinceLabel: f.SinceLabel,
		Events: f.Events, Sessions: f.Sessions, Attention: f.Attention, Transcripts: f.Transcripts}
}

func TestParseLink(t *testing.T) {
	l, ok := ParseLink(SessionLink("ses_x", "run_y", 42))
	if !ok || l.Kind != c.KindSession || l.ID != "ses_x" || l.RunID != "run_y" || l.Seq != 42 {
		t.Fatalf("%+v %v", l, ok)
	}
	l, ok = ParseLink("pyre://artifact_version/ver_1")
	if !ok || l.Kind != c.KindArtifactVersion || l.ID != "ver_1" || l.Seq != 0 {
		t.Fatalf("%+v", l)
	}
	for _, bad := range []string{"", "https://x/y", "pyre://session/", "pyre://session/a/b"} {
		if _, ok := ParseLink(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestFirstSentence(t *testing.T) {
	cases := map[string]string{
		"Done. Refactored the parser and fixed the flaky test.":  "Done. Refactored the parser and fixed the flaky test.",
		"## Plan\n\n1. Read main.go first. Then edit.":           "Read main.go first.",
		"```go\nx := 1\n```\nFixed it in v1.2 today! More later": "Fixed it in v1.2 today!",
		"- **Bold** bullet: details follow":                      "Bold bullet: details follow",
		"":                                                       "",
		"Here is what changed:\n- a\n- b":                        "Here is what changed",
	}
	for in, want := range cases {
		if got := firstSentence(in, 160); got != want {
			t.Errorf("firstSentence(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("word ", 60)
	if got := firstSentence(long, 40); len([]rune(got)) > 40 || !strings.HasSuffix(got, "…") {
		t.Errorf("clip: %q", got)
	}
}

func TestToolCalls(t *testing.T) {
	n, f := toolCalls("Read: /a.go; Edit: /b.go; Bash: go test")
	if n != 3 || len(f) != 1 || f[0] != "/b.go" {
		t.Fatalf("%d %v", n, f)
	}
	n, f = toolCalls("file change: x.go, y.go")
	if n != 1 || len(f) != 2 {
		t.Fatalf("%d %v", n, f)
	}
	if n, _ := toolCalls("command: ls (exit 0)"); n != 1 {
		t.Fatal(n)
	}
}

func TestCutoffAndLastSeen(t *testing.T) {
	dir := t.TempDir()
	p := StateFile(filepath.Join(dir, "client.json"))
	now := SyntheticNow
	o, err := Cutoff("last", now, p)
	if err != nil || o.AfterCursor != 0 || !o.Since.Equal(now.Add(-DefaultWindow)) || o.Label != "last 12h" {
		t.Fatalf("no mark: %+v %v", o, err)
	}
	if err := SaveLastSeen(p, LastSeen{Cursor: 500, At: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := SaveLastSeen(p, LastSeen{Cursor: 400, At: now}); err != nil { // older cursor: ignored
		t.Fatal(err)
	}
	o, _ = Cutoff("", now, p)
	if o.AfterCursor != 500 || !o.Since.Equal(now.Add(-time.Hour)) || o.Label != "last seen" {
		t.Fatalf("mark: %+v", o)
	}
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode: %v %v", fi.Mode(), err)
	}
	o, err = Cutoff("1d6h", now, p)
	if err != nil || !o.Since.Equal(now.Add(-30*time.Hour)) || o.AfterCursor != 0 {
		t.Fatalf("duration: %+v %v", o, err)
	}
	if _, err := Cutoff("soon", now, p); err == nil {
		t.Fatal("bad duration accepted")
	}
}

// fakeSource serves a synthetic event log with retention and paging.
// Fetch reads transcripts concurrently, so reads is guarded by mu.
type fakeSource struct {
	in      Input
	oldest  int64
	mu      sync.Mutex
	reads   []string
	limitOK bool
}

func (f *fakeSource) ListEvents(_ context.Context, req c.ListEventsRequest) (c.ListEventsResponse, error) {
	head := f.in.HeadCursor
	if req.AfterCursor > head || req.AfterCursor < f.oldest-1 {
		return c.ListEventsResponse{}, c.Errorf(c.ErrResetRequired, "stale").WithDetail("oldest_cursor", f.oldest).WithDetail("snapshot_cursor", head)
	}
	if req.Limit > 1 {
		f.limitOK = true
	}
	kinds := map[c.EventKind]bool{}
	for _, k := range req.Kinds {
		kinds[k] = true
	}
	resp := c.ListEventsResponse{HeadCursor: head, NextCursor: req.AfterCursor}
	for _, ev := range f.in.Events {
		if ev.Cursor <= req.AfterCursor || ev.Cursor < f.oldest || (len(kinds) > 0 && !kinds[ev.Kind]) {
			continue
		}
		resp.Events = append(resp.Events, ev)
		resp.NextCursor = ev.Cursor
		if len(resp.Events) >= max(1, min(req.Limit, 7)) { // small pages exercise paging
			break
		}
	}
	return resp, nil
}

func (f *fakeSource) ListSessions(context.Context, c.ListSessionsRequest) (c.Page[c.SessionView], error) {
	return c.Page[c.SessionView]{Items: f.in.Sessions}, nil
}

func (f *fakeSource) ListAttention(context.Context, c.ListAttentionRequest) (c.Page[c.AttentionItem], error) {
	return c.Page[c.AttentionItem]{Items: f.in.Attention}, nil
}

func (f *fakeSource) ReadSession(_ context.Context, req c.ReadSessionRequest) (c.ReadSessionResponse, error) {
	f.mu.Lock()
	f.reads = append(f.reads, req.SessionID)
	f.mu.Unlock()
	if req.SessionID == "ses_synthB" {
		return c.ReadSessionResponse{}, c.Errorf(c.ErrUnavailable, "synth-linux is offline")
	}
	return f.in.Transcripts[req.SessionID], nil
}

func TestFetchFromCursorAndFromTime(t *testing.T) {
	in := SyntheticInput()
	src := &fakeSource{in: in, oldest: 90}
	want := Build(in)

	got, err := Fetch(context.Background(), src, Options{AfterCursor: 100, Since: in.Since, Now: in.Now, Label: "last seen"})
	if err != nil {
		t.Fatal(err)
	}
	if order(got) != order(want) || got.Quiet != want.Quiet || got.HeadCursor != in.HeadCursor {
		t.Fatalf("from cursor:\n got %s q%d\nwant %s q%d", order(got), got.Quiet, order(want), want.Quiet)
	}
	if !src.limitOK {
		t.Fatal("events were not paged")
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "synth-flaky-ci") {
		t.Fatalf("offline transcript should be one warning: %v", got.Warnings)
	}

	// Time cutoff: binary search finds cursor 100 (the cursor-90 event is
	// before 09:00 and the first later event is at 09:10).
	src2 := &fakeSource{in: in, oldest: 90}
	got2, err := Fetch(context.Background(), src2, Options{Since: in.Since, Now: in.Now})
	if err != nil {
		t.Fatal(err)
	}
	if got2.SinceCursor != 90 || order(got2) != order(want) {
		t.Fatalf("from time: since cursor %d, order %s", got2.SinceCursor, order(got2))
	}

	// A cursor older than retention: truncated, still a digest.
	src3 := &fakeSource{in: in, oldest: 120}
	got3, err := Fetch(context.Background(), src3, Options{AfterCursor: 100, Since: in.Since, Now: in.Now})
	if err != nil || !got3.Truncated {
		t.Fatalf("retention: %v truncated=%v", err, got3.Truncated)
	}
	// A cursor from the future (another hub): falls back to time.
	got4, err := Fetch(context.Background(), &fakeSource{in: in, oldest: 90}, Options{AfterCursor: 1 << 40, Since: in.Since, Now: in.Now})
	if err != nil || order(got4) != order(want) {
		t.Fatalf("future cursor: %v %s", err, order(got4))
	}
}

func order(d Digest) string {
	var s []string
	for _, a := range d.Agents {
		s = append(s, a.SessionID)
	}
	return strings.Join(s, ",")
}

func TestSummarizeHookIsOptional(t *testing.T) {
	d := Build(SyntheticInput())
	same, err := Summarize(context.Background(), d, SyntheticNow, nil)
	if err != nil || same.Summary != "" {
		t.Fatal("nil summarizer must leave the digest unchanged")
	}
	var prompt string
	got, err := Summarize(context.Background(), d, SyntheticNow, func(_ context.Context, p string) (string, error) {
		prompt = p
		return "synthetic summary", nil
	})
	if err != nil || got.Summary != "synthetic summary" {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "synth-api-refactor") || strings.Contains(prompt, "Earlier synthetic message") {
		t.Fatalf("prompt must carry the digest only:\n%s", prompt)
	}
	if _, err := Summarize(context.Background(), d, SyntheticNow, CommandSummarizer([]string{"false"}, time.Second)); err == nil {
		t.Fatal("failing command must report an error")
	}
	out, err := CommandSummarizer([]string{"cat"}, 5*time.Second)(context.Background(), "echo me")
	if err != nil || out != "echo me" {
		t.Fatalf("command summarizer: %q %v", out, err)
	}
}

func TestWriteText(t *testing.T) {
	var b bytes.Buffer
	d := Build(SyntheticInput())
	WriteText(&b, d, SyntheticNow)
	s := b.String()
	for _, want := range []string{
		"While you were away · since ",
		"5 agents, 2 need you",
		"Needs you\n  ! synth-api-refactor",
		"  ✗ synth-flaky-ci",
		"Done\n  ✓ synth-codex-check",
		"Still working\n  ● synth-search-index",
		"pyre://session/ses_synthC?run=run_synthC&seq=812",
		"artifact: synth digest spec v2  pyre://artifact_version/ver_synthC2",
		"files: docs/digest.md, internal/digest/digest_test.go",
		"Also needs you\n  ! synth-linux was offline for 12 minutes  pyre://node/node_synthLinux",
		"2 quiet sessions collapsed",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	if i, j := strings.Index(s, "Needs you"), strings.Index(s, "Still working"); i < 0 || j < i {
		t.Error("sections out of order")
	}
}
