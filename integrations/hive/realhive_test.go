package hive

// Contract tests against a REAL Hive launcher. Skipped unless enabled:
//
//	PYRE_HIVE_REAL_BIN=/path/to/plugins/hive/bin/hive
//	    Runs Hive's real code against an ISOLATED temporary HOME (its own
//	    hive-mem store, state and staging dirs). The user's store, vault,
//	    hooks and crontab are never touched. Data is synthetic.
//
//	PYRE_HIVE_LIVE_PROBE=1
//	    Read-only probe of this node's real install: Detect plus one
//	    index-fallback and one CLI search for a synthetic term. Logs counts,
//	    timings and status fields only — never titles, bodies or names.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

func realEnv(t *testing.T) (Env, string) {
	bin := os.Getenv("PYRE_HIVE_REAL_BIN")
	if bin == "" {
		t.Skip("PYRE_HIVE_REAL_BIN not set")
	}
	home := t.TempDir()
	if r, err := filepath.EvalSymlinks(home); err == nil {
		home = r
	}
	env := EnvFrom([]string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "HIVE_MACHINE=pyre-contract-test", "LANG=C.UTF-8"})
	env.Bin = bin
	env.DetectTimeout, env.QueryTimeout, env.WriteTimeout = 15*time.Second, 30*time.Second, 60*time.Second
	return env, home
}

func TestRealHiveContract(t *testing.T) {
	env, home := realEnv(t)
	c := New(env, Options{StatusTTL: time.Hour, StagingRoot: filepath.Join(home, "staging")})
	ctx := context.Background()

	st := c.Refresh(ctx)
	t.Logf("detect: mode=%s backend=%s version=%s store=%q revision=%s detail=%q", st.Mode, st.Memory.Backend,
		st.Integration.Version, st.StoreVer, st.Integration.Revision, st.Memory.Detail)
	if st.Mode != ModeCLI {
		t.Fatalf("real hive CLI not usable: %+v", st)
	}

	pid := "prj_01PYRECONTRACT"
	notes := []contracts.Note{
		{NoteID: "note_01PYRECONTRACTA", Kind: contracts.NoteHandoff, ProjectID: &pid, SessionID: "ses_01PYRECONTRACT",
			Title: "Synthetic handoff: zorblax calibration halfway", Body: "Next: finish the zorblax table.\nÜnïcode survives.", CreatedAt: time.Now()},
		{NoteID: "note_01PYRECONTRACTB", Kind: contracts.NoteDecision, ProjectID: &pid,
			Title: "Synthetic decision: zorblax uses metric", Body: "Agreed metric for zorblax.", CreatedAt: time.Now()},
	}
	store := &memNotes{notes: notes}
	b, err := NewBridge(BridgeConfig{Store: store, Hive: c, StateFile: filepath.Join(home, "bridge.json")})
	must(t, err)
	start := time.Now()
	rep, err := b.SyncOnce(ctx)
	must(t, err)
	t.Logf("bridge pass 1: %+v in %s", rep, time.Since(start))
	if rep.Published != 2 || rep.Recorded != 2 {
		t.Fatalf("publish: %+v", rep)
	}
	refs := map[string]string{}
	for _, n := range notes {
		refs[n.NoteID] = store.get(n.NoteID).HiveRef
		if !strings.HasPrefix(refs[n.NoteID], "note:") {
			t.Fatalf("hive ref %q", refs[n.NoteID])
		}
	}
	// Crash replay: hub forgot, ledger lost -> no duplicate entries.
	store.resetSync()
	b2, err := NewBridge(BridgeConfig{Store: store, Hive: c})
	must(t, err)
	rep, err = b2.SyncOnce(ctx)
	must(t, err)
	t.Logf("bridge replay: %+v", rep)
	if rep.Published != 0 || rep.AlreadyInHive != 2 {
		t.Fatalf("replay duplicated: %+v", rep)
	}
	for id, ref := range refs {
		if store.get(id).HiveRef != ref {
			t.Fatalf("ref changed for %s", id)
		}
	}

	start = time.Now()
	res, err := c.Search(ctx, contracts.MemorySearchParams{Query: "zorblax calibration", ProjectID: pid})
	must(t, err)
	t.Logf("search: %d hits truncated=%v freshness=%+v in %s", len(res.Hits), res.Truncated, res.Freshness, time.Since(start))
	var hand *contracts.MemoryHit
	for i := range res.Hits {
		if strings.Contains(res.Hits[i].Source, "pyre_note=note_01PYRECONTRACTA") {
			hand = &res.Hits[i]
		}
	}
	if hand == nil || hand.Kind != "handoff" || hand.ProjectID != pid || hand.Authoritative || strings.Contains(hand.Title, "[pyre:") {
		t.Fatalf("handoff hit: %+v", res.Hits)
	}
	t.Logf("handoff hit: id=%s kind=%s source=%q", hand.ID, hand.Kind, hand.Source)

	h, err := c.Get(ctx, hand.ID)
	must(t, err)
	if !strings.Contains(h.Snippet, "zorblax table") || !strings.Contains(h.Snippet, "Ünïcode") {
		t.Fatalf("get body: %q", h.Snippet)
	}
	if _, err := c.Get(ctx, "note:0:doesnotexist"); !contracts.IsCode(err, contracts.ErrNotFound) {
		t.Fatalf("missing get: %v", err)
	}
	// Repo-key derivation matches Hive's own for the staging directory.
	e, ok, err := c.cliShow(ctx, hand.ID)
	must(t, err)
	if !ok || e.RepoKey != c.ProjectRepoKey(pid) {
		t.Fatalf("repo_key %q, Pyre computed %q", e.RepoKey, c.ProjectRepoKey(pid))
	}
	p := c.LaunchPointers(ctx, PointerRequest{ProjectID: pid, Timeout: 30 * time.Second})
	t.Logf("pointers: items=%d bytes=%d refs=%d", len(p.Items), p.Bytes, len(p.References))
	if len(p.Items) != 2 || len(p.References) != 2 {
		t.Fatalf("pointers: %+v", p)
	}
	// Degraded path over the same real store (created by the first note, so
	// re-detect rather than reuse the pre-write status).
	st = c.Refresh(ctx)
	if st.Memory.LastIndexAt == nil {
		t.Fatalf("no index freshness after writes: %+v", st)
	}
	idx, err := indexSearch(ctx, st.DBPath, "zorblax", "", "", 10)
	must(t, err)
	if len(idx) != 2 {
		t.Fatalf("read-only index search over the real store: %d", len(idx))
	}
}

func TestLiveProbeReadOnly(t *testing.T) {
	if os.Getenv("PYRE_HIVE_LIVE_PROBE") != "1" {
		t.Skip("PYRE_HIVE_LIVE_PROBE not set")
	}
	env := EnvFromOS()
	env.DetectTimeout = 10 * time.Second
	ctx := context.Background()
	start := time.Now()
	st := Detect(ctx, env)
	t.Logf("detect %s: mode=%s backend=%s version=%s store=%q revision=%.12s dirty=%v health=%s pending=%d last_sync=%v last_index=%v last_tick=%v stale=%v (%s) breakers=%v",
		time.Since(start).Round(time.Millisecond), st.Mode, st.Memory.Backend, st.Integration.Version, st.StoreVer,
		st.Integration.Revision, st.Integration.Dirty, st.Integration.Health, st.Memory.PendingItems,
		st.Memory.LastSyncAt != nil, st.Memory.LastIndexAt != nil, st.LastTickAt != nil, st.Stale, st.StaleReason, st.Breakers)
	if st.Mode == ModeUnavailable {
		t.Logf("detail: %s", st.Memory.Detail)
		return
	}
	for _, q := range []string{"zzpyreprobeqq", "test"} {
		start = time.Now()
		hits, err := indexSearch(ctx, st.DBPath, q, "", "", 20)
		t.Logf("index-ro search(%s): hits=%d err=%v in %s", q, len(hits), err, time.Since(start).Round(time.Millisecond))
	}
	c := New(env, Options{StatusTTL: time.Hour})
	start = time.Now()
	res, err := c.Search(ctx, contracts.MemorySearchParams{Query: "test", Limit: 10})
	auth := 0
	kinds := map[string]int{}
	withMachine := 0
	for _, h := range res.Hits {
		kinds[h.Kind]++
		if h.Authoritative {
			auth++
		}
		if strings.Contains(h.Source, "machine=") {
			withMachine++
		}
	}
	t.Logf("cli search: hits=%d truncated=%v kinds=%v authoritative=%d with_machine=%d source=%s stale=%v err=%v in %s",
		len(res.Hits), res.Truncated, kinds, auth, withMachine, res.Freshness.Source, res.Freshness.Stale, err, time.Since(start).Round(time.Millisecond))
}
