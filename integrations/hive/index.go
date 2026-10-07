package hive

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	_ "modernc.org/sqlite" // pure-Go driver (already a Pyre dependency)
)

// The index is Hive-owned. Pyre opens it strictly read-only (mode=ro plus
// query_only) — the same way hive's own `ask` hydration does — so a Pyre bug
// can never write to, migrate or create the user's memory store.

func openIndexRO(path string) (*sql.DB, error) {
	if !fileExists(path) {
		return nil, errors.New("hive index not found")
	}
	esc := strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(path)
	db, err := sql.Open("sqlite", "file:"+esc+"?mode=ro&_pragma=busy_timeout(2000)&_pragma=query_only(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// indexStats are cheap, bounded freshness/backlog reads.
type indexStats struct {
	PendingJobs    int
	LastLaneImport *time.Time
	HasEntries     bool
}

func readIndexStats(ctx context.Context, path string) (indexStats, error) {
	var st indexStats
	db, err := openIndexRO(path)
	if err != nil {
		return st, err
	}
	defer db.Close()
	var one int
	if err := db.QueryRowContext(ctx, "SELECT 1 FROM sqlite_master WHERE type='table' AND name='entries'").Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return st, errors.New("hive index has no entries table")
		}
		return st, err
	}
	st.HasEntries = true
	// hive_* tables are absent before `hive` first touches the store; that is
	// not an error.
	_ = db.QueryRowContext(ctx, "SELECT count(*) FROM hive_jobs WHERE state IN ('pending','running')").Scan(&st.PendingJobs)
	var maxUpd sql.NullInt64
	if err := db.QueryRowContext(ctx, "SELECT max(updated_at) FROM hive_lane_cursors").Scan(&maxUpd); err == nil && maxUpd.Valid && maxUpd.Int64 > 0 {
		t := time.Unix(maxUpd.Int64, 0).UTC()
		st.LastLaneImport = &t
	}
	return st, nil
}

// sessionProv is provenance for live-captured entries, whose uid is the
// harness session id.
type sessionProv struct {
	Machine, Harness, TranscriptPath, Cwd, StreamSlug string
}

func lookupSessions(ctx context.Context, path string, ids []string) map[string]sessionProv {
	out := map[string]sessionProv{}
	if len(ids) == 0 {
		return out
	}
	db, err := openIndexRO(path)
	if err != nil {
		return out
	}
	defer db.Close()
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	q := "SELECT session_id, harness, machine, transcript_path, cwd, stream_slug FROM hive_sessions WHERE session_id IN (?" +
		strings.Repeat(",?", len(ids)-1) + ") ORDER BY last_activity DESC"
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var p sessionProv
		if rows.Scan(&id, &p.Harness, &p.Machine, &p.TranscriptPath, &p.Cwd, &p.StreamSlug) == nil {
			if _, seen := out[id]; !seen {
				out[id] = p
			}
		}
	}
	return out
}

// buildMatch mirrors hive-mem's jm_query_build (lib/query.sh): FTS specials
// and control bytes become separators, terms are de-duplicated
// case-insensitively, long single-case alphabetic terms get a prefix
// alternative, everything is double-quoted, joined with OR and capped at 200
// characters on a whole-term boundary.
func buildMatch(raw string) string {
	cleaned := strings.Map(func(r rune) rune {
		if strings.ContainsRune("\"*():^-", r) || r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, raw)
	var out string
	seen := map[string]bool{}
	n := 0
	for _, t := range strings.Fields(cleaned) {
		if n >= 16 {
			break
		}
		low := strings.ToLower(t)
		if seen[low] {
			continue
		}
		piece := `"` + t + `"`
		if utf8.RuneCountInString(t) >= 5 && isASCIIAlpha(t) && (t == low || t == strings.ToUpper(t)) {
			piece = `("` + t + `" OR "` + t + `"*)`
		}
		cand := piece
		if out != "" {
			cand = out + " OR " + piece
		}
		if utf8.RuneCountInString(cand) > 200 {
			break
		}
		out = cand
		seen[low] = true
		n++
	}
	return out
}

func isASCIIAlpha(s string) bool {
	for _, r := range s {
		if r > unicode.MaxASCII || !unicode.IsLetter(r) {
			return false
		}
	}
	return s != ""
}

// indexSearch is the degraded local path used when the CLI is unavailable:
// BM25 with hive-mem's column weights, scaled by durability, current rows
// only. It is lexical-only and does not reproduce Hive's recency/scope
// rescoring; results are labelled accordingly.
func indexSearch(ctx context.Context, path, query, kind, repoKey string, limit int) ([]entry, error) {
	match := buildMatch(query)
	if match == "" {
		return nil, nil
	}
	db, err := openIndexRO(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	q := `SELECT e.id, e.uid, e.ts, e.kind, e.repo_key, e.worktree, e.origin, e.title,
	        CASE WHEN e.next_steps <> '' THEN 'next: ' || e.next_steps
	             WHEN e.outcome <> '' THEN substr(e.outcome, 1, 400) ELSE '' END
	      FROM entries_fts f JOIN entries e ON e.id = f.rowid
	     WHERE entries_fts MATCH ? AND e.superseded = 0
	       AND (? = '' OR e.kind = ?) AND (? = '' OR e.repo_key = ?)
	     ORDER BY bm25(entries_fts, 8.0, 2.0, 2.0, 3.0, 3.0, 2.5, 1.5, 2.0, 1.0) * e.durability, e.ts DESC
	     LIMIT ?`
	rows, err := db.QueryContext(ctx, q, match, kind, kind, repoKey, repoKey, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []entry
	seen := map[string]bool{}
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.ID, &e.UID, &e.TS, &e.Kind, &e.RepoKey, &e.Worktree, &e.Origin, &e.Title, &e.Cont); err != nil {
			return nil, err
		}
		if seen[e.UID] {
			continue
		}
		seen[e.UID] = true
		out = append(out, e)
	}
	return out, rows.Err()
}

// indexGet mirrors `hive-mem show --json` read-only.
func indexGet(ctx context.Context, path, id string) (entry, bool, error) {
	db, err := openIndexRO(path)
	if err != nil {
		return entry{}, false, err
	}
	defer db.Close()
	var e entry
	err = db.QueryRowContext(ctx, `SELECT id, uid, ts, kind, repo_key, worktree, branch, repo_path, origin, title,
	        request, outcome, learned, decided, next_steps, superseded
	      FROM entries WHERE uid = ? OR CAST(id AS TEXT) = ?
	     ORDER BY superseded ASC, source_bytes DESC, id DESC LIMIT 1`, id, id).
		Scan(&e.ID, &e.UID, &e.TS, &e.Kind, &e.RepoKey, &e.Worktree, &e.Branch, &e.RepoPath, &e.Origin, &e.Title,
			&e.Request, &e.Outcome, &e.Learned, &e.Decided, &e.NextSteps, &e.Superseded)
	if errors.Is(err, sql.ErrNoRows) {
		return entry{}, false, nil
	}
	if err != nil {
		return entry{}, false, err
	}
	return e, true, nil
}
