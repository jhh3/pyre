package claude

import (
	"github.com/jhh3/pyre/internal/contracts"
)

// TranscriptFormat names the transcript format for the search indexer.
const TranscriptFormat = "claude"

// TranscriptFiles lists the native transcript files of a run for the
// node's search index, without loading the run's state: the path the
// adapter already follows, else every transcript_path the hooks reported
// (a /clear starts a new file), else the computed path. Cheap enough to
// call for every run; the indexer caches what it found.
func (a *Adapter) TranscriptFiles(h contracts.RunHandle) (string, []string) {
	a.mu.Lock()
	e := a.runs[h.RunID]
	a.mu.Unlock()
	if e != nil {
		if p, _ := e.tpath.Load().(string); p != "" {
			return TranscriptFormat, []string{p}
		}
	}
	var out []string
	seen := map[string]bool{}
	if recs, _, err := readEvents(h.StateDir, 0); err == nil {
		for _, r := range recs {
			if r.TranscriptPath != "" && !seen[r.TranscriptPath] {
				seen[r.TranscriptPath] = true
				out = append(out, r.TranscriptPath)
			}
		}
	}
	if len(out) == 0 {
		tmp := &runEntry{rs: newRunState()}
		if m := a.loadMeta(h, tmp); m != nil {
			if p := guessTranscriptPath(m.ConfigDir, m.Cwd, m.SessionUUID); p != "" {
				out = append(out, p)
			}
		}
	}
	return TranscriptFormat, out
}
