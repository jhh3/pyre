package search

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// Migrations is the "search" namespace in the node database: one row per
// indexed transcript file (byte offset and running sequence number, so
// indexing is incremental) and the FTS5 table of entries.
var Migrations = []sqlitex.Migration{{Namespace: "search", Version: 1, Name: "transcript_fts", SQL: `
CREATE TABLE search_files (
	path       TEXT PRIMARY KEY,
	run_id     TEXT NOT NULL,
	session_id TEXT NOT NULL,
	harness    TEXT NOT NULL,
	title      TEXT NOT NULL DEFAULT '',
	format     TEXT NOT NULL,
	offset     INTEGER NOT NULL DEFAULT 0,
	seq        INTEGER NOT NULL DEFAULT 0,
	size       INTEGER NOT NULL DEFAULT 0,
	entries    INTEGER NOT NULL DEFAULT 0,
	updated_at TEXT NOT NULL
);
CREATE INDEX search_files_run ON search_files(run_id);
CREATE INDEX search_files_session ON search_files(session_id);
CREATE VIRTUAL TABLE search_fts USING fts5(
	text,
	path UNINDEXED,
	kind UNINDEXED,
	seq UNINDEXED,
	at UNINDEXED,
	tokenize = 'porter unicode61'
);
`}}

// Source is one transcript file of a run.
type Source struct {
	RunID     string
	SessionID string
	Harness   contracts.Harness
	Title     string
	Path      string
	Format    string
}

// Limits bound indexing and queries.
type Limits struct {
	ChunkBytes   int64         // bytes read per file per pass (4 MiB)
	MaxLineBytes int           // longer lines are skipped (16 MiB)
	DefaultLimit int           // hits (50)
	MaxLimit     int           // hits (200)
	Budget       time.Duration // default query budget (1.5s)
	MaxBudget    time.Duration // (10s)
}

func (l *Limits) defaults() {
	if l.ChunkBytes <= 0 {
		l.ChunkBytes = 4 << 20
	}
	if l.MaxLineBytes <= 0 {
		l.MaxLineBytes = 16 << 20
	}
	if l.DefaultLimit <= 0 {
		l.DefaultLimit = 50
	}
	if l.MaxLimit <= 0 {
		l.MaxLimit = 200
	}
	if l.Budget <= 0 {
		l.Budget = 1500 * time.Millisecond
	}
	if l.MaxBudget <= 0 {
		l.MaxBudget = 10 * time.Second
	}
}

// Index is a node's transcript index. Indexing is serialized (one writer);
// queries run concurrently.
type Index struct {
	db     *sql.DB
	nodeID string
	lim    Limits
	now    func() time.Time
	mu     sync.Mutex // serializes Update passes
}

// Open migrates the search namespace on db.
func Open(ctx context.Context, db *sql.DB, nodeID string, lim Limits) (*Index, error) {
	if err := sqlitex.Migrate(ctx, db, Migrations...); err != nil {
		return nil, err
	}
	lim.defaults()
	return &Index{db: db, nodeID: nodeID, lim: lim, now: func() time.Time { return time.Now().UTC() }}, nil
}

// SetClock replaces the clock (tests).
func (ix *Index) SetClock(now func() time.Time) { ix.now = now }

// Stats are an update pass's results.
type Stats struct {
	Files   int // known files
	Indexed int // entries added this pass
	Pending int // files with unread bytes left (budget or chunk)
	Partial bool
}

// Update brings every source up to date, reading at most ChunkBytes per
// file per round, until done or ctx expires. Files that shrank or were
// replaced are re-indexed from the start.
func (ix *Index) Update(ctx context.Context, sources []Source) (Stats, error) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	return ix.update(ctx, sources)
}

// TryUpdate is Update unless another pass is running (busy=true: nothing
// done; the running pass brings the index up to date).
func (ix *Index) TryUpdate(ctx context.Context, sources []Source) (st Stats, busy bool, err error) {
	if !ix.mu.TryLock() {
		return Stats{Files: len(sources)}, true, nil
	}
	defer ix.mu.Unlock()
	st, err = ix.update(ctx, sources)
	return st, false, err
}

func (ix *Index) update(ctx context.Context, sources []Source) (Stats, error) {
	st := Stats{Files: len(sources)}
	todo := sources
	for len(todo) > 0 {
		var more []Source
		for _, s := range todo {
			if ctx.Err() != nil {
				st.Partial = true
				st.Pending += len(todo)
				return st, nil
			}
			n, left, err := ix.updateFile(ctx, s)
			st.Indexed += n
			if err != nil {
				if ctx.Err() != nil {
					st.Partial = true
					st.Pending++
					continue
				}
				return st, fmt.Errorf("index %s: %w", s.RunID, err)
			}
			if left {
				more = append(more, s)
			}
		}
		todo = more
		if len(todo) > 0 && ctx.Err() != nil {
			st.Partial = true
			st.Pending = len(todo)
			return st, nil
		}
	}
	return st, nil
}

type fileRow struct {
	offset, seq, size int64
	title             string
	found             bool
}

func (ix *Index) row(ctx context.Context, path string) (fileRow, error) {
	var r fileRow
	err := ix.db.QueryRowContext(ctx, `SELECT offset, seq, size, title FROM search_files WHERE path=?`, path).Scan(&r.offset, &r.seq, &r.size, &r.title)
	if errors.Is(err, sql.ErrNoRows) {
		return r, nil
	}
	r.found = err == nil
	return r, err
}

// updateFile indexes up to ChunkBytes of new complete lines; left reports
// unread bytes remaining.
func (ix *Index) updateFile(ctx context.Context, s Source) (int, bool, error) {
	if s.Path == "" {
		return 0, false, nil
	}
	fi, err := os.Stat(s.Path)
	if err != nil {
		return 0, false, nil // not written yet, or gone: keep what is indexed
	}
	r, err := ix.row(ctx, s.Path)
	if err != nil {
		return 0, false, err
	}
	reset := r.found && fi.Size() < r.offset
	if r.found && !reset && fi.Size() == r.offset {
		if r.title != s.Title && s.Title != "" {
			_, err := ix.db.ExecContext(ctx, `UPDATE search_files SET title=? WHERE path=?`, s.Title, s.Path)
			return 0, false, err
		}
		return 0, false, nil
	}
	if reset {
		r.offset, r.seq = 0, 0
	}
	f, err := os.Open(s.Path)
	if err != nil {
		return 0, false, nil
	}
	defer f.Close()
	if _, err := f.Seek(r.offset, io.SeekStart); err != nil {
		return 0, false, err
	}
	p := newParser(s.Format, r.seq)
	var entries []Entry
	br := bufio.NewReaderSize(f, 256<<10)
	read, chunked := int64(0), false
	for {
		if read >= ix.lim.ChunkBytes {
			chunked = true
			break
		}
		line, err := br.ReadBytes('\n')
		if err != nil { // EOF: a trailing partial line waits for its newline
			break
		}
		read += int64(len(line))
		if len(line) > ix.lim.MaxLineBytes {
			p.skip() // counted like the adapter would, not indexed
			continue
		}
		if e, ok := p.line(line[:len(line)-1]); ok {
			entries = append(entries, e)
		}
	}
	off := r.offset + read
	now := ix.now()
	err = sqlitex.Tx(ctx, ix.db, func(tx *sql.Tx) error {
		if reset {
			if _, err := tx.ExecContext(ctx, `DELETE FROM search_fts WHERE path=?`, s.Path); err != nil {
				return err
			}
		}
		if len(entries) > 0 {
			stmt, err := tx.PrepareContext(ctx, `INSERT INTO search_fts(text, path, kind, seq, at) VALUES(?,?,?,?,?)`)
			if err != nil {
				return err
			}
			defer stmt.Close()
			for _, e := range entries {
				at := e.At
				if at.IsZero() {
					at = fi.ModTime()
				}
				if _, err := stmt.ExecContext(ctx, e.Text, s.Path, e.Kind, e.Seq, at.Unix()); err != nil {
					return err
				}
			}
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO search_files(path, run_id, session_id, harness, title, format, offset, seq, size, entries, updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(path) DO UPDATE SET run_id=excluded.run_id, session_id=excluded.session_id, harness=excluded.harness,
				title=CASE WHEN excluded.title='' THEN search_files.title ELSE excluded.title END, format=excluded.format,
				offset=excluded.offset, seq=excluded.seq, size=excluded.size,
				entries=CASE WHEN ? THEN excluded.entries ELSE search_files.entries + excluded.entries END, updated_at=excluded.updated_at`,
			s.Path, s.RunID, s.SessionID, string(s.Harness), s.Title, s.Format, off, p.seq(), fi.Size(), len(entries), sqlitex.FormatTime(now), reset)
		return err
	})
	if err != nil {
		return 0, false, err
	}
	return len(entries), chunked && off < fi.Size(), nil
}

// Query is a node-local search.
type Query struct {
	Text       string
	Since      *time.Time
	SessionIDs []string
	Limit      int
	Budget     time.Duration
}

// Search runs an FTS query ranked by BM25 relevance weighted by recency.
// Snippets mark matches with contracts.SearchMarkOpen/Close.
func (ix *Index) Search(ctx context.Context, q Query) (contracts.TranscriptSearchResult, error) {
	start := time.Now()
	res := contracts.TranscriptSearchResult{Hits: []contracts.TranscriptHit{}, Freshness: contracts.Freshness{ObservedAt: ix.now(), Source: "node:" + ix.nodeID}}
	match := FTSQuery(q.Text)
	if match == "" {
		return res, contracts.Errorf(contracts.ErrInvalid, "a search query with at least one word is required")
	}
	limit := q.Limit
	if limit <= 0 {
		limit = ix.lim.DefaultLimit
	}
	limit = min(limit, ix.lim.MaxLimit)
	budget := q.Budget
	if budget <= 0 {
		budget = ix.lim.Budget
	}
	budget = min(budget, ix.lim.MaxBudget)
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	now := ix.now().Unix()
	where := []string{"search_fts MATCH ?"}
	args := []any{match}
	if q.Since != nil {
		where = append(where, "f.at >= ?")
		args = append(args, q.Since.Unix())
	}
	if len(q.SessionIDs) > 0 {
		where = append(where, "s.session_id IN (?"+strings.Repeat(",?", len(q.SessionIDs)-1)+")")
		for _, id := range q.SessionIDs {
			args = append(args, id)
		}
	}
	// Recency: a month-old match weighs half a fresh one of equal BM25.
	args = append(args, now, limit+1)
	rows, err := ix.db.QueryContext(ctx, `
		SELECT s.run_id, s.session_id, s.harness, s.title, f.kind, f.seq, f.at,
			snippet(search_fts, 0, '`+contracts.SearchMarkOpen+`', '`+contracts.SearchMarkClose+`', '…', 24),
			bm25(search_fts)
		FROM search_fts f JOIN search_files s ON s.path = f.path
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY bm25(search_fts) / (1.0 + MAX(0, ? - f.at) / 2592000.0)
		LIMIT ?`, args...)
	if err != nil {
		if ctx.Err() != nil {
			res.Partial = true
			res.TookMS = time.Since(start).Milliseconds()
			return res, nil
		}
		return res, fmt.Errorf("search: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var h contracts.TranscriptHit
		var harness string
		var at int64
		var bm float64
		if err := rows.Scan(&h.RunID, &h.SessionID, &harness, &h.Title, &h.Kind, &h.Seq, &at, &h.Snippet, &bm); err != nil {
			return res, err
		}
		if len(res.Hits) == limit {
			res.Truncated = true
			break
		}
		h.NodeID, h.Harness, h.At, h.Score = ix.nodeID, contracts.Harness(harness), time.Unix(at, 0).UTC(), -bm
		h.Snippet = strings.Join(strings.Fields(h.Snippet), " ")
		res.Hits = append(res.Hits, h)
	}
	if err := rows.Err(); err != nil {
		if ctx.Err() == nil {
			return res, err
		}
		res.Partial = true
	}
	_ = ix.db.QueryRowContext(context.WithoutCancel(ctx), `SELECT COUNT(DISTINCT run_id) FROM search_files`).Scan(&res.Runs)
	res.TookMS = time.Since(start).Milliseconds()
	return res, nil
}

// Forget drops a run's indexed files (not used for archiving: archived
// sessions stay searchable with include_archived).
func (ix *Index) Forget(ctx context.Context, runID string) error {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	return sqlitex.Tx(ctx, ix.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM search_fts WHERE path IN (SELECT path FROM search_files WHERE run_id=?)`, runID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM search_files WHERE run_id=?`, runID)
		return err
	})
}

// KnownPath returns the indexed path of a run's most recently updated file
// ("" if none), so callers can skip re-resolving ended runs.
func (ix *Index) KnownPaths(ctx context.Context) (map[string][]string, error) {
	rows, err := ix.db.QueryContext(ctx, `SELECT run_id, path FROM search_files ORDER BY updated_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var run, path string
		if err := rows.Scan(&run, &path); err != nil {
			return nil, err
		}
		out[run] = append(out[run], path)
	}
	return out, rows.Err()
}
