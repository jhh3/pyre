package codex

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
// stopped run's transcript does not re-parse the file per page.
var rolloutParsed = struct {
	sync.Mutex
	m map[string]rolloutParse
}{m: map[string]rolloutParse{}}

type rolloutParse struct {
	size    int64
	mod     time.Time
	entries []contracts.TranscriptEntry
}

const rolloutParsedMax = 8

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
	fi, err := os.Stat(path)
	if err != nil {
		return nil, false
	}
	rolloutParsed.Lock()
	c, ok := rolloutParsed.m[path]
	rolloutParsed.Unlock()
	if !ok || c.size != fi.Size() || !c.mod.Equal(fi.ModTime()) {
		parsed, err := search.ParseFile(path, search.FormatCodex)
		if err != nil {
			return nil, false
		}
		c = rolloutParse{size: fi.Size(), mod: fi.ModTime(), entries: make([]contracts.TranscriptEntry, 0, len(parsed))}
		for _, p := range parsed {
			c.entries = append(c.entries, rolloutEntry(p))
		}
		rolloutParsed.Lock()
		if len(rolloutParsed.m) >= rolloutParsedMax {
			for k := range rolloutParsed.m {
				delete(rolloutParsed.m, k)
				break
			}
		}
		rolloutParsed.m[path] = c
		rolloutParsed.Unlock()
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
		case isWakeNotice(t):
			e.Kind = "system" // Pyre's background-command notice, not typed input
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
