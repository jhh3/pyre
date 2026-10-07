package hive

// A fake Hive for hermetic tests. The test binary doubles as the `hive`
// launcher: TestMain dispatches to fakeHiveMain when FAKE_HIVE_MODE=1. A temp
// HOME is laid out like a real install (plugin cache copy, plugin.json,
// installed_plugins.json, hive-mem store with the real schema shape, state
// stamps). All data is synthetic.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

//go:embed testdata/store-schema.sql
var fakeSchema string

const fakeSHA = "0123456789abcdef0123456789abcdef01234567"

func TestMain(m *testing.M) {
	if os.Getenv("FAKE_HIVE_MODE") == "1" {
		os.Exit(fakeHiveMain(os.Args[1:]))
	}
	os.Exit(m.Run())
}

type fakeHome struct {
	t       *testing.T
	Home    string
	Plugin  string
	DB      string
	State   string
	Flags   string
	CallLog string
}

// newFakeHome lays out a synthetic Hive install. withCLI=false omits the
// launcher (index-only node); withDB=false omits the store.
func newFakeHome(t *testing.T, withCLI, withDB bool) *fakeHome {
	t.Helper()
	home := t.TempDir()
	if r, err := filepath.EvalSymlinks(home); err == nil {
		home = r
	}
	f := &fakeHome{t: t, Home: home,
		Plugin:  filepath.Join(home, ".claude", "plugins", "cache", "hive", "hive", "0.2.3"),
		DB:      filepath.Join(home, ".local", "share", storeDirName, "memory.db"),
		State:   filepath.Join(home, ".local", "state", "hive"),
		Flags:   filepath.Join(home, "fake-flags"),
		CallLog: filepath.Join(home, "fake-calls.log"),
	}
	must(t, os.MkdirAll(f.Flags, 0o755))
	must(t, os.MkdirAll(f.State, 0o755))
	if withCLI {
		exe, err := os.Executable()
		must(t, err)
		must(t, os.MkdirAll(filepath.Join(f.Plugin, "bin"), 0o755))
		must(t, os.MkdirAll(filepath.Join(f.Plugin, ".claude-plugin"), 0o755))
		must(t, os.WriteFile(filepath.Join(f.Plugin, ".claude-plugin", "plugin.json"), []byte(`{"name":"hive","version":"0.2.3"}`), 0o644))
		script := "#!/bin/sh\nFAKE_HIVE_MODE=1 exec '" + exe + "' \"$@\"\n"
		must(t, os.WriteFile(filepath.Join(f.Plugin, "bin", "hive"), []byte(script), 0o755))
		inst := fmt.Sprintf(`{"version":2,"plugins":{"hive@hive":[{"scope":"user","installPath":%q,"version":"0.2.3","gitCommitSha":%q}]}}`, f.Plugin, fakeSHA)
		must(t, os.WriteFile(filepath.Join(home, ".claude", "plugins", "installed_plugins.json"), []byte(inst), 0o644))
	}
	if withDB {
		f.seedDB()
	}
	must(t, os.WriteFile(filepath.Join(f.State, "sync.push.stamp"), []byte(fmt.Sprintf("%d.25", time.Now().Add(-10*time.Minute).Unix())), 0o644))
	must(t, os.WriteFile(filepath.Join(f.State, "tick.log"), []byte("tick ok\n"), 0o644))
	return f
}

func (f *fakeHome) env() Env {
	e := EnvFrom([]string{
		"HOME=" + f.Home,
		"PATH=/usr/bin:/bin",
		"FAKE_HIVE_FLAGS=" + f.Flags,
		"FAKE_HIVE_CALLLOG=" + f.CallLog,
	})
	e.DetectTimeout = 5 * time.Second
	e.QueryTimeout = 5 * time.Second
	e.WriteTimeout = 5 * time.Second
	return e
}

func (f *fakeHome) client() *Client {
	return New(f.env(), Options{StatusTTL: time.Hour})
}

func (f *fakeHome) flag(name string, on bool) {
	p := filepath.Join(f.Flags, name)
	if on {
		must(f.t, os.WriteFile(p, []byte("1"), 0o644))
	} else {
		_ = os.Remove(p)
	}
}

func (f *fakeHome) calls(verb string) int {
	b, _ := os.ReadFile(f.CallLog)
	n := 0
	for _, ln := range strings.Split(string(b), "\n") {
		if ln == verb {
			n++
		}
	}
	return n
}

func (f *fakeHome) seedDB() {
	t := f.t
	must(t, os.MkdirAll(filepath.Dir(f.DB), 0o700))
	db, err := sql.Open("sqlite", "file:"+f.DB+"?_pragma=journal_mode(WAL)")
	must(t, err)
	defer db.Close()
	_, err = db.Exec(fakeSchema)
	must(t, err)
	now := time.Now().Unix()
	exec := func(q string, args ...any) {
		_, err := db.Exec(q, args...)
		must(t, err)
	}
	exec(`INSERT INTO meta(k,v) VALUES('schema_version','2')`)
	exec(`INSERT INTO entries(uid,source_bytes,origin,repo_key,kind,ts,title,outcome) VALUES(?,?,?,?,?,?,?,?)`,
		"11111111-aaaa-4bbb-8ccc-000000000001", 4096, "live", "synthetic/widgets", "session", now-3600,
		"Synthetic session about widget frobnication", "Frobnicated the synthetic widget. Tests pass.")
	exec(`INSERT INTO entries(uid,origin,repo_key,kind,ts,title,decided) VALUES(?,?,?,?,?,?,?)`,
		"lgc:obs:900001", "legacy-note", "synthetic/widgets", "decision", now-86400*3,
		"Chose sqlite for the widget cache", "sqlite over files for the synthetic widget cache")
	exec(`INSERT INTO entries(uid,origin,repo_key,kind,ts,title,outcome,capture_mode) VALUES(?,?,?,?,?,?,?,?)`,
		"note:1700000000:abcdefabcdef", "noted", "synthetic/widgets", "decision", now-7200,
		"Explicit widget rollout plan", "Explicit widget rollout plan: canary first", "noted")
	exec(`INSERT INTO entries(uid,source_bytes,origin,repo_key,kind,ts,title,superseded) VALUES(?,?,?,?,?,?,?,?)`,
		"11111111-aaaa-4bbb-8ccc-000000000001", 1024, "live", "synthetic/widgets", "session", now-7200,
		"Older superseded widget capture", 1)
	exec(`INSERT INTO hive_sessions(session_id,harness,machine,transcript_path,cwd,started_at,last_activity) VALUES(?,?,?,?,?,?,?)`,
		"11111111-aaaa-4bbb-8ccc-000000000001", "claude-code", "fakebox", "/synthetic/transcripts/one.jsonl", "/synthetic/widgets", now-4000, now-3600)
	exec(`INSERT INTO hive_jobs(kind,state,created_at,updated_at) VALUES('enrich_close','pending',?,?),('title','running',?,?),('title','done',?,?)`, now, now, now, now, now, now)
	exec(`INSERT INTO hive_lane_cursors(lane_path,byte_off,updated_at) VALUES('mem/lanes/otherbox/2026-10.ndjson',100,?)`, now-60)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// ---- fake launcher ----

func fakeFlag(name string) bool {
	_, err := os.Stat(filepath.Join(os.Getenv("FAKE_HIVE_FLAGS"), name))
	return err == nil
}

func fakeHiveMain(args []string) int {
	if p := os.Getenv("FAKE_HIVE_CALLLOG"); p != "" && len(args) > 0 {
		if fh, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			fmt.Fprintln(fh, args[0])
			fh.Close()
		}
	}
	if fakeFlag("sleep") {
		time.Sleep(30 * time.Second)
	}
	if fakeFlag("broken") {
		fmt.Fprintln(os.Stderr, "Traceback: synthetic breakage")
		return 2
	}
	if len(args) == 0 {
		return 0
	}
	home := os.Getenv("HOME")
	dbPath := filepath.Join(home, ".local", "share", storeDirName, "memory.db")
	switch args[0] {
	case "mem":
		if len(args) > 1 && args[1] == "version" {
			fmt.Println("hive-mem 0.4.5-fake (schema 2)")
			return 0
		}
	case "search":
		if fakeFlag("fail-search") {
			fmt.Fprintln(os.Stderr, "synthetic search failure")
			return 2
		}
		return fakeSearch(dbPath, args[1:])
	case "show":
		return fakeShow(dbPath, args[1:])
	case "timeline":
		return fakeSearch(dbPath, append([]string{"--recent"}, args[1:]...))
	case "note":
		if fakeFlag("fail-note") {
			fmt.Fprintln(os.Stderr, "note: write failed (synthetic)")
			return 1
		}
		return fakeNote(dbPath, args[1:])
	}
	fmt.Fprintf(os.Stderr, "hive: unknown command '%s'\n", args[0])
	return 2
}

func fakeDB(path string) *sql.DB {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		panic(err)
	}
	return db
}

func fakeSearch(dbPath string, args []string) int {
	limit, kind, repo, recent := 20, "", "", false
	var terms []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json", "--all-repos":
		case "--recent":
			recent = true
		case "--limit":
			i++
			limit, _ = strconv.Atoi(args[i])
		case "--kind":
			i++
			kind = args[i]
		case "--repo":
			i++
			repo = args[i]
		case "--":
			terms = append(terms, args[i+1:]...)
			i = len(args)
		default:
			terms = append(terms, args[i])
		}
	}
	db := fakeDB(dbPath)
	defer db.Close()
	var rows *sql.Rows
	var err error
	cols := `e.id, e.uid, e.ts, e.kind, e.repo_key, e.worktree, e.origin, e.title,
	  CASE WHEN e.next_steps <> '' THEN 'next: ' || e.next_steps WHEN e.outcome <> '' THEN substr(e.outcome,1,400) ELSE '' END`
	if recent {
		rows, err = db.Query(`SELECT `+cols+` FROM entries e WHERE e.superseded=0 AND (?='' OR e.repo_key=?) ORDER BY e.ts DESC LIMIT ?`, repo, repo, limit)
	} else {
		match := buildMatch(strings.Join(terms, " "))
		if match == "" {
			fmt.Fprintln(os.Stderr, "no results")
			return 1
		}
		rows, err = db.Query(`SELECT `+cols+` FROM entries_fts f JOIN entries e ON e.id=f.rowid
		  WHERE entries_fts MATCH ? AND e.superseded=0 AND (?='' OR e.kind=?) AND (?='' OR e.repo_key=?)
		  ORDER BY bm25(entries_fts) LIMIT ?`, match, kind, kind, repo, repo, limit)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.ID, &e.UID, &e.TS, &e.Kind, &e.RepoKey, &e.Worktree, &e.Origin, &e.Title, &e.Cont); err != nil {
			return 2
		}
		out := map[string]any{"id": e.ID, "uid": e.UID, "ts": e.TS, "date": time.Unix(e.TS, 0).UTC().Format("2006-01-02"),
			"kind": e.Kind, "repo_key": e.RepoKey, "worktree": e.Worktree, "origin": e.Origin, "ephemeral": 0,
			"recurrence": 1, "score": 1.0, "title": e.Title, "cont": e.Cont}
		b, _ := json.Marshal(out)
		fmt.Println(string(b))
		n++
	}
	if n == 0 {
		fmt.Fprintln(os.Stderr, "no results")
		return 1
	}
	return 0
}

func fakeShow(dbPath string, args []string) int {
	id := ""
	for _, a := range args {
		if a != "--json" {
			id = a
		}
	}
	e, ok, err := indexGet(context.Background(), dbPath, id)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if !ok {
		fmt.Fprintf(os.Stderr, "hive-mem: no entry %s\n", id)
		return 1
	}
	b, _ := json.Marshal(map[string]any{"id": e.ID, "uid": e.UID, "ts": e.TS, "kind": e.Kind, "repo_key": e.RepoKey,
		"worktree": e.Worktree, "branch": e.Branch, "repo_path": e.RepoPath, "origin": e.Origin, "title": e.Title,
		"request": e.Request, "outcome": e.Outcome, "learned": e.Learned, "decided": e.Decided, "next_steps": e.NextSteps,
		"superseded": e.Superseded, "source_bytes": 0})
	fmt.Println(string(b))
	return 0
}

var fakeNoteMu sync.Mutex

func fakeNote(dbPath string, args []string) int {
	kind, text := "note", ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--kind":
			i++
			kind = args[i]
		case "--":
			text = strings.Join(args[i+1:], " ")
			i = len(args)
		default:
			text = strings.TrimSpace(text + " " + args[i])
		}
	}
	if text == "" {
		fmt.Fprintln(os.Stderr, "note: the note text is required")
		return 2
	}
	cwd, _ := os.Getwd()
	if r, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = r
	}
	sum := sha256.Sum256([]byte(cwd))
	rk := "dir:" + filepath.Base(cwd) + "@" + hex.EncodeToString(sum[:])[:8]
	if fakeFlag("hide-note") {
		text = strings.Replace(text, "[pyre:", "[pyre-mangled:", 1)
	}
	title := truncBytes(flatten(text), 200)
	outcome := ""
	if flatten(text) != title {
		outcome = text
	}
	now := time.Now().Unix()
	h := sha256.Sum256([]byte(rk + "|" + title))
	uid := fmt.Sprintf("note:%d:%s", now, hex.EncodeToString(h[:])[:12])
	fakeNoteMu.Lock()
	defer fakeNoteMu.Unlock()
	db := fakeDB(dbPath)
	defer db.Close()
	if _, err := db.Exec(`INSERT OR IGNORE INTO entries(uid,origin,repo_key,kind,ts,title,outcome,capture_mode,repo_path) VALUES(?,?,?,?,?,?,?,?,?)`,
		uid, "noted", rk, kind, now, title, outcome, "noted", cwd); err != nil {
		fmt.Fprintln(os.Stderr, "note: write failed:", err)
		return 1
	}
	fmt.Printf("noted  %s  [%s]  %s\n", rk, kind, title)
	return 0
}

// ---- in-memory hub note store ----

type memNotes struct {
	mu        sync.Mutex
	notes     []contracts.Note
	updates   []contracts.UpdateNoteSyncRequest
	failNext  int
	listCalls int
}

func (m *memNotes) ListNotes(_ context.Context, req contracts.ListNotesRequest) (contracts.Page[contracts.Note], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.listCalls++
	start := 0
	if req.Cursor != "" {
		start, _ = strconv.Atoi(req.Cursor)
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 50
	}
	end := min(start+limit, len(m.notes))
	pg := contracts.Page[contracts.Note]{Items: append([]contracts.Note(nil), m.notes[start:end]...)}
	if end < len(m.notes) {
		pg.NextCursor = strconv.Itoa(end)
	}
	return pg, nil
}

func (m *memNotes) UpdateNoteSync(_ context.Context, req contracts.UpdateNoteSyncRequest) (contracts.Note, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failNext > 0 {
		m.failNext--
		return contracts.Note{}, contracts.Errorf(contracts.ErrUnavailable, "synthetic hub outage")
	}
	m.updates = append(m.updates, req)
	for i := range m.notes {
		if m.notes[i].NoteID == req.NoteID {
			m.notes[i].SyncState = req.SyncState
			if req.HiveRef != "" {
				m.notes[i].HiveRef = req.HiveRef
			}
			return m.notes[i], nil
		}
	}
	return contracts.Note{}, contracts.Errorf(contracts.ErrNotFound, "no note")
}

func (m *memNotes) get(id string) contracts.Note {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, n := range m.notes {
		if n.NoteID == id {
			return n
		}
	}
	return contracts.Note{}
}

func (m *memNotes) resetSync() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.notes {
		m.notes[i].SyncState, m.notes[i].HiveRef = SyncPending, ""
	}
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}
