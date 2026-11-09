package codex

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/search"
)

// TranscriptFormat names the transcript format for the search indexer.
const TranscriptFormat = "codex"

// TranscriptFiles lists the run's rollout file for the node's search
// index: $CODEX_HOME/sessions/YYYY/MM/DD/rollout-*-<thread>.jsonl, or its
// archived_sessions copy. It reads only the run's state and globs (no
// app-server).
func (a *Adapter) TranscriptFiles(h contracts.RunHandle) (string, []string) {
	b, err := os.ReadFile(filepath.Join(h.StateDir, "run.json"))
	if err != nil {
		return TranscriptFormat, nil
	}
	var meta runMeta
	if json.Unmarshal(b, &meta) != nil {
		return TranscriptFormat, nil
	}
	if p := rolloutPath(h.StateDir, meta); p != "" {
		return TranscriptFormat, []string{p}
	}
	return TranscriptFormat, nil
}

// rolloutPathFile (in the run's state dir) remembers the run's rollout
// file once found, so the search indexer (every live run, every few
// minutes) does not glob a home with thousands of rollouts each time.
const rolloutPathFile = "rollout-path"

// rolloutFullGlobEvery bounds full globs of a home's sessions tree for a
// run whose rollout is not where its launch day says (a resumed older
// thread) or not written yet.
const rolloutFullGlobEvery = 10 * time.Minute

var rolloutMiss = struct {
	sync.Mutex
	at map[string]time.Time // state dir -> last full glob that found nothing
}{at: map[string]time.Time{}}

// rolloutPath finds a run's rollout file: the remembered path, else the
// launch day's folders (±1 day: folders are named by the node's local
// date), else archived_sessions, else (at most every
// rolloutFullGlobEvery) every day folder. A run on the owner's own login
// (no Pyre profile: meta.Home "") writes under the CODEX_HOME the
// app-server inherited, else ~/.codex (win-home, Oct 2026: an ambient run's
// rollout sat in C:\Users\<user>\.codex\sessions and was never found).
func rolloutPath(stateDir string, meta runMeta) string {
	home := codexHome(meta.Home)
	if home == "" || !safeThreadID(meta.ThreadID) {
		return ""
	}
	name := "rollout-*" + meta.ThreadID + ".jsonl"
	if b, err := os.ReadFile(filepath.Join(stateDir, rolloutPathFile)); err == nil {
		p := strings.TrimSpace(string(b))
		if rel, err := filepath.Rel(home, p); err == nil && !strings.HasPrefix(rel, "..") && strings.HasSuffix(p, meta.ThreadID+".jsonl") {
			if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
				return p
			}
		}
	}
	var pats []string
	if !meta.LaunchedAt.IsZero() {
		day := meta.LaunchedAt.Local()
		for _, d := range []int{0, -1, 1} {
			t := day.AddDate(0, 0, d)
			pats = append(pats, filepath.Join(home, "sessions", t.Format("2006"), t.Format("01"), t.Format("02"), name))
		}
	}
	pats = append(pats, filepath.Join(home, "archived_sessions", name))
	found := globFirst(pats)
	if found == "" {
		rolloutMiss.Lock()
		last, ok := rolloutMiss.at[stateDir]
		due := !ok || time.Since(last) >= rolloutFullGlobEvery
		if due {
			rolloutMiss.at[stateDir] = time.Now()
		}
		rolloutMiss.Unlock()
		if due {
			found = globFirst([]string{filepath.Join(home, "sessions", "*", "*", "*", name)})
		}
	}
	if found != "" {
		rolloutMiss.Lock()
		delete(rolloutMiss.at, stateDir)
		rolloutMiss.Unlock()
		_ = writeFileAtomic(filepath.Join(stateDir, rolloutPathFile), []byte(found))
	}
	return found
}

func globFirst(pats []string) string {
	for _, pat := range pats {
		if m, _ := filepath.Glob(pat); len(m) > 0 {
			return m[0]
		}
	}
	return ""
}

// codexHome is the CODEX_HOME an app-server for home uses: home itself, or
// for the ambient login ("") the daemon's CODEX_HOME, else ~/.codex.
func codexHome(home string) string {
	if home != "" {
		return home
	}
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h
	}
	u, err := os.UserHomeDir()
	if err != nil || u == "" {
		return ""
	}
	return filepath.Join(u, ".codex")
}

// safeThreadID keeps glob metacharacters and separators out of patterns.
func safeThreadID(id string) bool {
	return id != "" && len(id) <= 128 && !strings.ContainsAny(id, `*?[]\/`)
}

// rolloutParsed caches parsed rollouts by path, size and mtime, so paging a
// stopped run's transcript does not re-parse the file per page, and a
// rollout that grew (a running pyre run) is parsed only from where the
// last parse ended.
var rolloutParsed = struct {
	sync.Mutex
	m map[string]rolloutParse
}{m: map[string]rolloutParse{}}

type rolloutParse struct {
	size    int64
	mod     time.Time
	entries []contracts.TranscriptEntry // default cut, ascending Seq
	offs    []int64                     // offset of each entry's line
	end     int64                       // after the last complete line parsed
	seq     int64                       // entries numbered before end
	head    []byte                      // the file's first bytes (session_meta)
	tail    []byte                      // the bytes just before end
}

const (
	rolloutParsedMax = 8
	// Bytes that must be unchanged to parse only what follows a cached
	// parse: the file's first (its session_meta and thread id) and those
	// before the parse's end.
	rolloutHeadCheck = 256
	rolloutTailCheck = 64
)

// parsedRollout returns path's entries, parsing only what was appended
// since a cached parse (Codex only appends to a rollout) when the file did
// not shrink and its first bytes and those before the parse's end are
// unchanged; otherwise the whole file.
func parsedRollout(path string) (rolloutParse, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return rolloutParse{}, false
	}
	rolloutParsed.Lock()
	c, ok := rolloutParsed.m[path]
	rolloutParsed.Unlock()
	if ok && c.size == fi.Size() && c.mod.Equal(fi.ModTime()) {
		return c, true
	}
	if !ok || fi.Size() < c.size || !sameEnds(path, c) {
		c = rolloutParse{}
	}
	// Appending never writes into the cached slices' arrays, so a reader
	// holding the previous parse keeps a consistent view.
	entries, offs := c.entries[:len(c.entries):len(c.entries)], c.offs[:len(c.offs):len(c.offs)]
	end, seq, err := search.ScanFile(path, search.FormatCodex, c.end, c.seq, func(p search.Entry, off int64) {
		entries = append(entries, rolloutEntry(p))
		offs = append(offs, off)
	})
	if err != nil {
		return rolloutParse{}, false
	}
	c = rolloutParse{size: fi.Size(), mod: fi.ModTime(), entries: entries, offs: offs, end: end, seq: seq,
		head: readAt(path, 0, min(end, rolloutHeadCheck)), tail: readAt(path, end-rolloutTailCheck, end)}
	rolloutParsed.Lock()
	if _, had := rolloutParsed.m[path]; !had && len(rolloutParsed.m) >= rolloutParsedMax {
		for k := range rolloutParsed.m {
			delete(rolloutParsed.m, k)
			break
		}
	}
	rolloutParsed.m[path] = c
	rolloutParsed.Unlock()
	return c, true
}

// sameEnds: the first bytes and those just before a cached parse's end
// are still there.
func sameEnds(path string, c rolloutParse) bool {
	if c.end == 0 {
		return true
	}
	return len(c.head) > 0 && len(c.tail) > 0 && string(readAt(path, 0, int64(len(c.head)))) == string(c.head) &&
		string(readAt(path, c.end-int64(len(c.tail)), c.end)) == string(c.tail)
}

// readAt returns path's bytes [from, to) (from clamped to 0); nil on error.
func readAt(path string, from, to int64) []byte {
	from = max(from, 0)
	if to <= from {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	b := make([]byte, to-from)
	if _, err := f.ReadAt(b, from); err != nil {
		return nil
	}
	return b
}

// rolloutTranscript reads a run's transcript from its rollout file, for a
// stopped run or one whose app-server is confirmed absent (the child report
// read right after archive-when-done stopped a run found nothing). Kinds
// follow the live reader: a Pyre message is pyre_message with its id, a
// background-command notice is system. Entries are numbered over the
// rollout's messages and tool calls, not the app-server's items, so a
// cursor from a live read does not carry over; the response says it is
// stale. ok is false when there is no rollout.
func rolloutTranscript(stateDir string, meta runMeta, runID string, req contracts.ReadSessionRequest, limit int) ([]contracts.TranscriptEntry, bool) {
	path := rolloutPath(stateDir, meta)
	if path == "" {
		return nil, false
	}
	c, ok := parsedRollout(path)
	if !ok {
		return nil, false
	}
	var start int64
	fmt.Sscanf(req.Cursor, "%d", &start)
	var sel []contracts.TranscriptEntry
	for _, e := range c.entries {
		if e.Seq > start {
			e.RunID = runID
			sel = append(sel, e)
		}
	}
	if req.Tail && len(sel) > limit {
		sel = sel[len(sel)-limit:]
	} else if len(sel) > limit {
		sel = sel[:limit]
	}
	return sel, true
}

// rolloutFullText returns the whole text (newlines kept, no cut) of the
// rollout entries numbered seqs as rolloutTranscript numbers them. Each is
// parsed again from its line's offset: the file is not parsed again. A seq
// with no entry is left out.
func rolloutFullText(stateDir string, meta runMeta, seqs []int64) (map[int64]string, error) {
	path := rolloutPath(stateDir, meta)
	if path == "" {
		return nil, nil
	}
	c, ok := parsedRollout(path)
	if !ok {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := make(map[int64]string, len(seqs))
	br := bufio.NewReaderSize(f, 64<<10)
	for _, q := range seqs {
		i := sort.Search(len(c.entries), func(i int) bool { return c.entries[i].Seq >= q })
		if i == len(c.entries) || c.entries[i].Seq != q {
			continue
		}
		if _, err := f.Seek(c.offs[i], io.SeekStart); err != nil {
			return out, err
		}
		br.Reset(f)
		line, err := br.ReadBytes('\n')
		if err != nil {
			return out, err
		}
		if e, ok := search.CodexLineFull(line[:len(line)-1]); ok {
			out[q] = e.Text
		}
	}
	return out, nil
}

// rolloutEntry converts a parsed rollout entry with the live reader's kinds.
func rolloutEntry(p search.Entry) contracts.TranscriptEntry {
	e := contracts.TranscriptEntry{Seq: p.Seq, At: p.At, Kind: p.Kind, Text: p.Text}
	if e.Kind == "user" || e.Kind == "message" {
		e.Kind = "user"
		switch t := strings.TrimSpace(e.Text); {
		case strings.HasPrefix(t, "[pyre-msg:"):
			e.Kind = "pyre_message"
			id := strings.TrimPrefix(t, "[pyre-msg:")
			if i := strings.IndexAny(id, "] "); i > 0 && safeIDRe.MatchString(id[:i]) {
				e.MessageID = id[:i]
			}
		case isWakeNotice(t), isMailNotice(t):
			e.Kind = "system" // Pyre's background-command or mail notice, not typed input
		}
	}
	if len(e.Text) > 2000 {
		e.Text, e.Truncated = oneLine(e.Text, 2000), true
	}
	return e
}

// isWakeNotice: the text of a background-command wake turn (wakeNotice).
func isWakeNotice(t string) bool {
	first, _, _ := strings.Cut(t, "\n")
	return strings.HasPrefix(first, "[pyre] ") && (strings.Contains(first, " background command finished") || strings.Contains(first, " background commands finished"))
}
