package search

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// Alias paths (W-dbspace). Pyre's account profiles share one transcript
// store: each profile's projects/ (Claude) and sessions/ (Codex) is a
// symlink to it. A session resumed on another account therefore names the
// same transcript file under a new path, and the index, keyed by path,
// indexed the whole conversation again for every run, then kept every copy
// up to date as the file grew. On server (2026-10-28) 1.4M of the 2.27M
// indexed entries were such copies (172 Claude sessions under 490 paths),
// most of the 1.6 GB the index took in pyred.db.
//
// Now the paths of one file index it once, under the path that has indexed
// the most (ties: the latest run), so a resume never re-indexes a
// transcript from the start. The other paths are recorded with alias_of
// set and skipped; PruneAliases deletes the entries they indexed before.
// Hits carry the indexed path's run, a run of the same session reading the
// same file.
//
// Two runs of a session resumed on the same account name the same path.
// Both used to be sources, so the path was grouped with itself and
// recorded as its own alias; PruneAliases then deleted its entries and the
// next pass indexed it from the start, every pass. On server (2026-10-28) that
// re-indexed about 90 of the largest transcripts (1.7 GB) a minute, and each
// prune read the whole FTS table: ~70 MB/s of reads and ~40 MB/s of writes
// for hours. update now passes each path once (uniquePaths).

// pathState is what the index holds for one source path.
type pathState struct {
	offset int64
	alias  string // the path this one is an alias of ("" if none)
	found  bool
}

// holds reports whether path state a (of source sa) should hold a file's
// entries rather than b (of sb): the one that indexed further (nothing is
// indexed twice), then one not already an alias, then the latest run (run
// IDs sort by creation time), then the greater path.
func holds(a, b pathState, sa, sb Source) bool {
	switch {
	case a.offset != b.offset:
		return a.offset > b.offset
	case (a.alias == "") != (b.alias == ""):
		return a.alias == ""
	case sa.RunID != sb.RunID:
		return sa.RunID > sb.RunID
	}
	return sa.Path > sb.Path
}

// skipAliases returns sources without those whose file is indexed under
// another source's path, recording each skipped path as an alias.
func (ix *Index) skipAliases(ctx context.Context, sources []Source) ([]Source, error) {
	groups := sameFileGroups(sources)
	if len(groups) == 0 {
		return sources, nil
	}
	type aliasRow struct {
		s     Source
		canon string
		found bool
	}
	skip := map[int]bool{}
	var record []aliasRow
	for _, g := range groups {
		states := make([]pathState, len(g))
		best := 0
		for k, i := range g {
			st, err := ix.pathState(ctx, sources[i].Path)
			if err != nil {
				return nil, err
			}
			states[k] = st
			if k > 0 && holds(st, states[best], sources[i], sources[g[best]]) {
				best = k
			}
		}
		canon := sources[g[best]].Path
		for k, i := range g {
			if k == best {
				continue
			}
			skip[i] = true
			// A path is never recorded as an alias of itself (update passes
			// unique paths; this guards the invariant PruneAliases relies on).
			if sources[i].Path != canon && states[k].alias != canon {
				record = append(record, aliasRow{sources[i], canon, states[k].found})
			}
		}
	}
	if len(record) > 0 {
		now := sqlitex.FormatTime(ix.now())
		err := sqlitex.Tx(ctx, ix.db, func(tx *sql.Tx) error {
			for _, r := range record {
				var err error
				if r.found {
					_, err = tx.ExecContext(ctx, `UPDATE search_files SET alias_of=? WHERE path=?`, r.canon, r.s.Path)
				} else {
					_, err = tx.ExecContext(ctx, `INSERT INTO search_files(path, run_id, session_id, harness, title, format, updated_at, alias_of) VALUES(?,?,?,?,?,?,?,?)`,
						r.s.Path, r.s.RunID, r.s.SessionID, string(r.s.Harness), r.s.Title, r.s.Format, now, r.canon)
				}
				if err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	out := make([]Source, 0, len(sources)-len(skip))
	for i, s := range sources {
		if !skip[i] {
			out = append(out, s)
		}
	}
	return out, nil
}

// sameFileGroups returns the indexes of sources naming one existing file
// under two or more paths. Such paths share the file's name (<uuid>.jsonl,
// rollout-<time>-<uuid>.jsonl), and os.SameFile tells which of those are
// one file (on Unix by device and inode, so a file being appended to still
// matches itself).
func sameFileGroups(sources []Source) [][]int {
	byName := map[string][]int{}
	for i, s := range sources {
		if s.Path != "" {
			b := filepath.Base(s.Path)
			byName[b] = append(byName[b], i)
		}
	}
	var groups [][]int
	for _, idx := range byName {
		if len(idx) < 2 {
			continue
		}
		var gs [][]int
		var fis []os.FileInfo
	next:
		for _, i := range idx {
			fi, err := os.Stat(sources[i].Path)
			if err != nil {
				continue
			}
			for g := range gs {
				if os.SameFile(fis[g], fi) {
					gs[g] = append(gs[g], i)
					continue next
				}
			}
			gs = append(gs, []int{i})
			fis = append(fis, fi)
		}
		for _, g := range gs {
			if len(g) > 1 {
				groups = append(groups, g)
			}
		}
	}
	return groups
}

func (ix *Index) pathState(ctx context.Context, path string) (pathState, error) {
	var st pathState
	err := ix.db.QueryRowContext(ctx, `SELECT offset, alias_of FROM search_files WHERE path=?`, path).Scan(&st.offset, &st.alias)
	if errors.Is(err, sql.ErrNoRows) {
		return st, nil
	}
	st.found = err == nil
	return st, err
}

// aliasPause separates PruneAliases' delete transactions so other writers
// get the lock. A variable for tests.
var aliasPause = 5 * time.Millisecond

// PruneAliases deletes the entries indexed under alias paths before they
// became aliases, in transactions of sqlitex.PruneBatch rows, until done
// or ctx ends (the rest goes next time). FTS5 cannot find a path's entries
// without reading every entry (path is not indexed), so one call reads the
// whole table once; it is called only while such entries exist, which
// after the first cleanup is only when an indexed path turns out to share
// its file with one that indexed further. It returns the entries deleted.
func (ix *Index) PruneAliases(ctx context.Context) (int64, error) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	rows, err := ix.db.QueryContext(ctx, `SELECT path FROM search_files WHERE alias_of != '' AND alias_of != path AND (entries > 0 OR offset > 0)`)
	if err != nil {
		return 0, err
	}
	var paths []any
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return 0, err
		}
		paths = append(paths, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(paths) == 0 {
		return 0, err
	}
	in := "?" + strings.Repeat(",?", len(paths)-1)
	rows, err = ix.db.QueryContext(ctx, `SELECT rowid FROM search_fts WHERE path IN (`+in+`)`, paths...)
	if err != nil {
		return 0, err
	}
	var ids []any
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	var deleted int64
	for first := true; len(ids) > 0; first = false {
		if !first {
			t := time.NewTimer(aliasPause)
			select {
			case <-ctx.Done():
				t.Stop()
				return deleted, ctx.Err()
			case <-t.C:
			}
		}
		batch := ids[:min(len(ids), sqlitex.PruneBatch)]
		ids = ids[len(batch):]
		var n int64
		err := sqlitex.Tx(ctx, ix.db, func(tx *sql.Tx) error {
			res, err := tx.ExecContext(ctx, `DELETE FROM search_fts WHERE rowid IN (?`+strings.Repeat(",?", len(batch)-1)+`)`, batch...)
			if err != nil {
				return err
			}
			n, err = res.RowsAffected()
			return err
		})
		if err != nil {
			return deleted, err
		}
		deleted += n
	}
	// Every entry of these paths is gone: reset them, so a path that holds
	// the file again later (its holder deleted) indexes it from the start.
	if _, err = ix.db.ExecContext(ctx, `UPDATE search_files SET offset=0, seq=0, size=0, entries=0 WHERE alias_of != '' AND path IN (`+in+`)`, paths...); err != nil {
		return deleted, err
	}
	return deleted, ix.merge(ctx)
}

// mergePages bounds one FTS5 merge step (pages written; one write
// transaction of a few MB).
const mergePages = 1000

// merge merges the full-text index's segments in bounded steps until none
// are left to merge or ctx ends. Deleted entries leave delete markers in
// the index until their segments merge; after the first alias cleanup on
// server that was about 340 MB (search_fts_data 329 MB -> 453 MB, 110 MB
// merged). FTS5's own automerge would get there only as new entries come.
func (ix *Index) merge(ctx context.Context) error {
	conn, err := ix.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	changes := func() (int64, error) {
		var n int64
		err := conn.QueryRowContext(ctx, `SELECT total_changes()`).Scan(&n)
		return n, err
	}
	for first := true; ; first = false {
		if !first {
			t := time.NewTimer(aliasPause)
			select {
			case <-ctx.Done():
				t.Stop()
				return ctx.Err()
			case <-t.C:
			}
		}
		before, err := changes()
		if err != nil {
			return err
		}
		// A negative page count merges every segment, not only those at
		// one level (FTS5 'merge' command).
		if _, err := conn.ExecContext(ctx, `INSERT INTO search_fts(search_fts, rank) VALUES('merge', ?)`, -mergePages); err != nil {
			return err
		}
		after, err := changes()
		if err != nil {
			return err
		}
		if after-before < 2 { // nothing left to merge (documented FTS5 test)
			return nil
		}
	}
}
