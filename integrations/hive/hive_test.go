package hive

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

func TestDetectAbsent(t *testing.T) {
	home := t.TempDir()
	env := EnvFrom([]string{"HOME=" + home, "PATH=" + home})
	start := time.Now()
	st := Detect(context.Background(), env)
	if time.Since(start) > 2*time.Second {
		t.Fatalf("detect of an absent hive took %s", time.Since(start))
	}
	if st.Mode != ModeUnavailable || st.Memory.Backend != "unavailable" || st.Integration.Present {
		t.Fatalf("absent hive: %+v", st)
	}
	if !strings.Contains(st.Memory.Detail, "not installed") || !strings.Contains(st.Memory.Detail, "launches are unaffected") {
		t.Fatalf("detail not explanatory: %q", st.Memory.Detail)
	}
	if st.Integration.Health != "unavailable" {
		t.Fatalf("health %q", st.Integration.Health)
	}
}

func TestDetectFakeInstall(t *testing.T) {
	f := newFakeHome(t, true, true)
	st := Detect(context.Background(), f.env())
	if st.Mode != ModeCLI || st.Memory.Backend != "hive" {
		t.Fatalf("mode %s backend %s detail %q", st.Mode, st.Memory.Backend, st.Memory.Detail)
	}
	if st.Integration.Version != "0.2.3" || st.Memory.Version != "0.2.3" || st.Integration.Revision != fakeSHA {
		t.Fatalf("version/revision: %+v", st.Integration)
	}
	if st.Integration.Path != f.Plugin || st.Integration.Dirty {
		t.Fatalf("path/dirty: %+v", st.Integration)
	}
	if st.StoreVer != "hive-mem 0.4.5-fake (schema 2)" {
		t.Fatalf("store version %q", st.StoreVer)
	}
	if st.Memory.PendingItems != 2 {
		t.Fatalf("pending = %d, want 2 (pending+running jobs)", st.Memory.PendingItems)
	}
	if st.Memory.LastSyncAt == nil || st.Memory.LastIndexAt == nil || st.LastTickAt == nil {
		t.Fatalf("freshness missing: %+v", st)
	}
	if st.Stale || st.Integration.Health != "ok" {
		t.Fatalf("fresh install reported stale/unhealthy: %+v", st)
	}
}

func TestDetectStaleAndBreakers(t *testing.T) {
	f := newFakeHome(t, true, true)
	old := time.Now().Add(-2 * time.Hour)
	must(t, os.Chtimes(filepath.Join(f.State, "tick.log"), old, old))
	must(t, os.WriteFile(filepath.Join(f.State, "breaker.session_end"), []byte("3"), 0o644))
	must(t, os.WriteFile(filepath.Join(f.State, "breaker.session_end.notified"), []byte("1"), 0o644))
	must(t, os.WriteFile(filepath.Join(f.State, "breaker.stop"), []byte("1"), 0o644))
	env := f.env()
	env.Vars = append(env.Vars, "HIVE_DISABLE=1")
	st := Detect(context.Background(), env)
	if !st.Stale || !strings.Contains(st.StaleReason, "last hive tick") {
		t.Fatalf("stale not detected: %+v", st)
	}
	if len(st.Breakers) != 1 || st.Breakers[0] != "session_end" {
		t.Fatalf("breakers %v", st.Breakers)
	}
	if !st.Disabled || st.Integration.Health != "degraded" {
		t.Fatalf("disabled/health: %+v", st)
	}
	for _, want := range []string{"stale", "session_end", "HIVE_DISABLE"} {
		if !strings.Contains(st.Memory.Detail, want) {
			t.Fatalf("detail %q missing %q", st.Memory.Detail, want)
		}
	}
}

func TestDetectIndexOnlyWhenCLIBroken(t *testing.T) {
	f := newFakeHome(t, true, true)
	f.flag("broken", true)
	st := Detect(context.Background(), f.env())
	if st.Mode != ModeIndex || st.Memory.Backend != "hive" || st.Integration.Health != "degraded" {
		t.Fatalf("broken CLI: %+v", st)
	}
	if !strings.Contains(st.Memory.Detail, "read-only local index") {
		t.Fatalf("detail %q", st.Memory.Detail)
	}
}

func TestDetectTimeoutBounded(t *testing.T) {
	f := newFakeHome(t, true, true)
	f.flag("sleep", true)
	env := f.env()
	env.DetectTimeout = 300 * time.Millisecond
	start := time.Now()
	st := Detect(context.Background(), env)
	if el := time.Since(start); el > 4*time.Second {
		t.Fatalf("detect not bounded: %s", el)
	}
	if st.Mode == ModeCLI {
		t.Fatalf("hung CLI reported usable")
	}
}

func TestSearchCLIMapping(t *testing.T) {
	f := newFakeHome(t, true, true)
	c := f.client()
	res, err := c.Search(context.Background(), contracts.MemorySearchParams{Query: "widget", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if res.Freshness.Source != "hive:cli" || res.Freshness.Stale || res.Truncated {
		t.Fatalf("freshness/truncation: %+v %v", res.Freshness, res.Truncated)
	}
	byID := map[string]contracts.MemoryHit{}
	for _, h := range res.Hits {
		byID[h.ID] = h
		if h.Authoritative {
			t.Fatalf("hive hit marked authoritative: %+v", h)
		}
	}
	live := byID["11111111-aaaa-4bbb-8ccc-000000000001"]
	if live.Kind != "transcript" || live.SessionRef == "" {
		t.Fatalf("live hit: %+v", live)
	}
	for _, want := range []string{"machine=fakebox", "session=11111111", "path=/synthetic/transcripts/one.jsonl", "repo=synthetic/widgets"} {
		if !strings.Contains(live.Source, want) {
			t.Fatalf("source %q missing %q", live.Source, want)
		}
	}
	if strings.Contains(live.Title, "superseded") {
		t.Fatal("superseded row returned")
	}
	if got := byID["lgc:obs:900001"].Kind; got != "transcript" {
		t.Fatalf("transcript-derived decision mapped to %q (must not look like a project decision)", got)
	}
	if got := byID["note:1700000000:abcdefabcdef"].Kind; got != "decision" {
		t.Fatalf("explicit noted decision kind %q", got)
	}

	res, err = c.Search(context.Background(), contracts.MemorySearchParams{Query: "widget", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 1 || !res.Truncated {
		t.Fatalf("limit 1: %d hits truncated=%v", len(res.Hits), res.Truncated)
	}
	if _, err := c.Search(context.Background(), contracts.MemorySearchParams{Query: "  "}); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("empty query: %v", err)
	}
	res, err = c.Search(context.Background(), contracts.MemorySearchParams{Query: "zzzunmatchedzzz"})
	if err != nil || len(res.Hits) != 0 || res.Hits == nil {
		t.Fatalf("no-hit search: %v %+v", err, res)
	}
}

// Search freshness follows the newest index write even while the detection
// result is cached (e.g. a note the bridge published a moment ago).
func TestSearchFreshnessFollowsNewestWrite(t *testing.T) {
	f := newFakeHome(t, true, true)
	c := New(f.env(), Options{StatusTTL: time.Hour})
	ctx := context.Background()
	if _, err := c.Search(ctx, contracts.MemorySearchParams{Query: "widget"}); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(90 * time.Second).Truncate(time.Second)
	must(t, os.Chtimes(f.DB, later, later))
	res, err := c.Search(ctx, contracts.MemorySearchParams{Query: "widget"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Freshness.ObservedAt.Equal(later.UTC()) {
		t.Fatalf("freshness %s, want the newest write %s", res.Freshness.ObservedAt, later.UTC())
	}
	if st := c.Status(ctx); st.Memory.LastIndexAt == nil || !st.Memory.LastIndexAt.Equal(later.UTC()) {
		t.Fatalf("status last_index_at %v", st.Memory.LastIndexAt)
	}
}

func TestSearchIndexFallbackWhenCLIBroken(t *testing.T) {
	f := newFakeHome(t, true, true)
	f.flag("broken", true)
	c := f.client()
	res, err := c.Search(context.Background(), contracts.MemorySearchParams{Query: "widget cache"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Freshness.Source != "hive:index-ro" || !res.Freshness.Stale || !strings.Contains(res.Freshness.Reason, "read-only local index") {
		t.Fatalf("fallback freshness: %+v", res.Freshness)
	}
	if len(res.Hits) == 0 || res.Hits[0].ID != "lgc:obs:900001" {
		t.Fatalf("fallback hits: %+v", res.Hits)
	}
}

func TestSearchFallbackWhenCLIFailsAfterDetect(t *testing.T) {
	f := newFakeHome(t, true, true)
	c := New(f.env(), Options{StatusTTL: time.Hour})
	if c.Status(context.Background()).Mode != ModeCLI {
		t.Fatal("expected cli")
	}
	f.flag("fail-search", true)
	res, err := c.Search(context.Background(), contracts.MemorySearchParams{Query: "frobnication"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Freshness.Source != "hive:index-ro" || !strings.Contains(res.Freshness.Reason, "hive CLI failed") || len(res.Hits) != 1 {
		t.Fatalf("runtime fallback: %+v", res)
	}
}

func TestSearchTimeoutFallsBack(t *testing.T) {
	f := newFakeHome(t, true, true)
	env := f.env()
	env.QueryTimeout = 300 * time.Millisecond
	c := New(env, Options{StatusTTL: time.Hour})
	_ = c.Status(context.Background())
	f.flag("sleep", true)
	start := time.Now()
	res, err := c.Search(context.Background(), contracts.MemorySearchParams{Query: "widget"})
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("search not bounded: %s", el)
	}
	if err != nil || res.Freshness.Source != "hive:index-ro" {
		t.Fatalf("timeout fallback: %v %+v", err, res.Freshness)
	}
}

func TestSearchUnavailable(t *testing.T) {
	home := t.TempDir()
	c := New(EnvFrom([]string{"HOME=" + home, "PATH=" + home}), Options{})
	_, err := c.Search(context.Background(), contracts.MemorySearchParams{Query: "x"})
	if !contracts.IsCode(err, contracts.ErrUnavailable) || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("want unavailable, got %v", err)
	}
}

func TestSearchProjectBoost(t *testing.T) {
	f := newFakeHome(t, true, true)
	c := New(f.env(), Options{StatusTTL: time.Hour, ProjectRepoKeys: func(p string) []string {
		if p == "prj_SYNTH" {
			return []string{"synthetic/widgets"}
		}
		return nil
	}})
	res, err := c.Search(context.Background(), contracts.MemorySearchParams{Query: "widget", ProjectID: "prj_SYNTH"})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range res.Hits {
		if h.ProjectID != "prj_SYNTH" {
			t.Fatalf("project repo hit not tagged: %+v", h)
		}
	}
}

func TestGet(t *testing.T) {
	f := newFakeHome(t, true, true)
	c := f.client()
	h, err := c.Get(context.Background(), "lgc:obs:900001")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.Snippet, "decided: sqlite over files") || h.Authoritative || h.Kind != "transcript" {
		t.Fatalf("get: %+v", h)
	}
	if _, err := c.Get(context.Background(), "nope:1"); !contracts.IsCode(err, contracts.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := c.Get(context.Background(), "--json"); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("flag-like id: %v", err)
	}
	f.flag("broken", true)
	h, err = f.client().Get(context.Background(), "11111111-aaaa-4bbb-8ccc-000000000001")
	if err != nil || !strings.Contains(h.Snippet, "Frobnicated") || !strings.Contains(h.Source, "machine=fakebox") {
		t.Fatalf("index get: %v %+v", err, h)
	}
}

func TestActions(t *testing.T) {
	f := newFakeHome(t, true, true)
	c := f.client()
	hs := c.Handlers()
	for _, a := range []string{contracts.ActionMemorySearch, contracts.ActionMemoryGet, contracts.ActionMemoryStatus} {
		if hs[a] == nil {
			t.Fatalf("missing handler %s", a)
		}
	}
	out, err := hs[contracts.ActionMemorySearch](context.Background(), json.RawMessage(`{"query":"widget","limit":2}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	var res contracts.MemorySearchResult
	must(t, json.Unmarshal(b, &res))
	if len(res.Hits) != 2 {
		t.Fatalf("action search: %s", b)
	}
	if _, err := hs[contracts.ActionMemoryGet](context.Background(), json.RawMessage(`{}`)); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("get without id: %v", err)
	}
	if _, err := hs[contracts.ActionMemorySearch](context.Background(), json.RawMessage(`{"query":`)); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("bad json: %v", err)
	}
	st, err := hs[contracts.ActionMemoryStatus](context.Background(), nil)
	if err != nil || st.(contracts.MemoryStatus).Backend != "hive" {
		t.Fatalf("status: %v %+v", err, st)
	}
	// Absent hive: status still answers, without error.
	home := t.TempDir()
	absent := New(EnvFrom([]string{"HOME=" + home, "PATH=" + home}), Options{})
	st, err = absent.StatusAction(context.Background(), json.RawMessage(`{}`))
	if err != nil || st.(contracts.MemoryStatus).Backend != "unavailable" {
		t.Fatalf("absent status: %v %+v", err, st)
	}
}

func TestBuildMatchMirrorsJhh3Mem(t *testing.T) {
	cases := map[string]string{
		"widget":                `("widget" OR "widget"*)`,
		"Widget":                `"Widget"`,
		"cache cache CACHE":     `("cache" OR "cache"*)`,
		"note_01ABC":            `"note_01ABC"`,
		`a "b" (c)`:             `"a" OR "b" OR "c"`,
		"well-known":            `"well" OR ("known" OR "known"*)`,
		"\x01\x02":              ``,
		"ünïcode":               `"ünïcode"`,
		"pyre:note_01ABC thing": `"pyre" OR "note_01ABC" OR ("thing" OR "thing"*)`,
	}
	for in, want := range cases {
		if got := buildMatch(in); got != want {
			t.Errorf("buildMatch(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("abcdefghij ", 40)
	if got := buildMatch(long); len(got) > 200 {
		t.Fatalf("match not capped: %d", len(got))
	}
}

func TestLaunchPointersAndAppend(t *testing.T) {
	f := newFakeHome(t, true, true)
	c := f.client()
	p := c.LaunchPointers(context.Background(), PointerRequest{Query: "widget", MaxItems: 2, MaxBytes: 400, Timeout: 20 * time.Second})
	if len(p.Items) != 2 || !p.Truncated || p.Bytes > 400 || p.Freshness == nil || p.Freshness.Source != "hive:cli" {
		t.Fatalf("pointers: %+v", p)
	}
	if !strings.Contains(p.Text, "[hive:") || strings.Contains(p.Text, "Frobnicated the synthetic widget") {
		t.Fatalf("pointers must be titles+refs, not bodies: %q", p.Text)
	}
	lc := contracts.LaunchContext{Text: "# Brief\nShip the synthetic widget.\n", References: []contracts.ObjectRef{}}
	lc.Bytes = len(lc.Text)
	AppendPointers(&lc, p, 8<<10)
	if !strings.Contains(lc.Text, "Memory pointers") || lc.MemoryFreshness == nil || lc.Bytes != len(lc.Text) {
		t.Fatalf("append: %+v", lc)
	}
	tight := contracts.LaunchContext{Text: strings.Repeat("x", 100)}
	AppendPointers(&tight, p, 120)
	if len(tight.Text) > 120 || !tight.Truncated {
		t.Fatalf("append not bounded: %d", len(tight.Text))
	}

	// No query and no project: nothing global is injected.
	if p := c.LaunchPointers(context.Background(), PointerRequest{}); len(p.Items) != 0 || p.Text != "" {
		t.Fatalf("global dump: %+v", p)
	}
	// Absent hive: empty, stale freshness, no error, fast.
	home := t.TempDir()
	absent := New(EnvFrom([]string{"HOME=" + home, "PATH=" + home}), Options{})
	start := time.Now()
	p = absent.LaunchPointers(context.Background(), PointerRequest{Query: "widget"})
	if p.Text != "" || p.Freshness == nil || !p.Freshness.Stale || time.Since(start) > 2*time.Second {
		t.Fatalf("absent pointers: %+v", p)
	}
}

func TestLaunchPointersTimeout(t *testing.T) {
	f := newFakeHome(t, true, false) // CLI only, no index to fall back to
	c := New(f.env(), Options{StatusTTL: time.Hour})
	_ = c.Status(context.Background())
	f.flag("sleep", true)
	start := time.Now()
	p := c.LaunchPointers(context.Background(), PointerRequest{Query: "widget", Timeout: 300 * time.Millisecond})
	if el := time.Since(start); el > 4*time.Second {
		t.Fatalf("launch pointers blocked %s", el)
	}
	if p.Text != "" || p.Freshness == nil || !p.Freshness.Stale {
		t.Fatalf("timeout pointers: %+v", p)
	}
}

func TestNoteTextMarker(t *testing.T) {
	pid := "prj_01SYNTHETICPROJECT0000000"
	n := contracts.Note{NoteID: "note_01SYNTHETICNOTE00000000000", Kind: contracts.NoteHandoff, ProjectID: &pid,
		Title: strings.Repeat("Long synthetic handoff title ", 20), Body: strings.Repeat("ü", 5000), SessionID: "ses_01SYNTH",
		PromotedTo: "rev_01SYNTH"}
	txt := NoteText(n)
	if len(txt) > maxNoteBytes {
		t.Fatalf("note text %d bytes", len(txt))
	}
	m, ok := parseMarker(truncBytes(flatten(txt), 200))
	if !ok || m.NoteID != n.NoteID || m.Kind != "handoff" || m.ProjectID != pid || !m.Promoted {
		t.Fatalf("marker not in first 200 bytes / wrong: %+v %v", m, ok)
	}
	if !strings.Contains(txt, "[truncated; full text in the Pyre hub note]") {
		t.Fatal("truncation not marked")
	}
	odd := contracts.Note{NoteID: "note_X", Kind: contracts.NoteKind("future_kind"), Title: "t"}
	if m, _ := parseMarker(NoteText(odd)); m.Kind != "note" {
		t.Fatalf("unknown note kind must degrade to note, got %q", m.Kind)
	}
}

// TestSearchAttributesPyreSessions: a transcript Hive captured from a
// harness Pyre launched is attributed to the Pyre session (SessionRef, like
// hub notes), its run, and the session's project; other sessions keep Hive's
// native reference.
func TestSearchAttributesPyreSessions(t *testing.T) {
	f := newFakeHome(t, true, true)
	c := f.client()
	const native = "11111111-aaaa-4bbb-8ccc-000000000001"
	var asked []string
	c.SetNativeResolver(func(_ context.Context, id string) (NativeSession, bool) {
		asked = append(asked, id)
		if id == native {
			return NativeSession{SessionID: "ses_synth-a10", RunID: "run_synth-a10", ProjectID: "prj_synth-a10"}, true
		}
		return NativeSession{}, false
	})
	res, err := c.Search(context.Background(), contracts.MemorySearchParams{Query: "widget", Limit: 10, ProjectID: "prj_synth-a10"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) == 0 || res.Hits[0].ID != native {
		t.Fatalf("the Pyre project's transcript should rank first: %+v", res.Hits)
	}
	h := res.Hits[0]
	if h.SessionRef != "ses_synth-a10" || h.ProjectID != "prj_synth-a10" || h.Kind != "transcript" || h.Authoritative {
		t.Fatalf("attributed hit: %+v", h)
	}
	for _, want := range []string{"session=" + native, "pyre_session=ses_synth-a10", "pyre_run=run_synth-a10", "machine=fakebox"} {
		if !strings.Contains(h.Source, want) {
			t.Fatalf("source %q missing %q", h.Source, want)
		}
	}
	for _, o := range res.Hits[1:] {
		if strings.Contains(o.Source, "pyre_session=") {
			t.Fatalf("non-Pyre hit attributed: %+v", o)
		}
		if o.Kind != "transcript" && len(asked) > 0 && o.SessionRef != "" {
			t.Fatalf("only transcripts are resolved: %+v", o)
		}
	}
	g, err := c.Get(context.Background(), native)
	if err != nil || g.SessionRef != "ses_synth-a10" || !strings.Contains(g.Source, "pyre_session=ses_synth-a10") {
		t.Fatalf("get: %v %+v", err, g)
	}
}
