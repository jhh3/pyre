package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
)

// TranscriptFormat names the transcript format for the search indexer.
const TranscriptFormat = "codex"

// TranscriptFiles lists the run's rollout files for the node's search
// index: $CODEX_HOME/sessions/YYYY/MM/DD/rollout-*-<thread>.jsonl and its
// archived_sessions copy. It reads only run.json and globs (no app-server).
func (a *Adapter) TranscriptFiles(h contracts.RunHandle) (string, []string) {
	b, err := os.ReadFile(filepath.Join(h.StateDir, "run.json"))
	if err != nil {
		return TranscriptFormat, nil
	}
	var meta runMeta
	if json.Unmarshal(b, &meta) != nil || meta.Home == "" || !safeThreadID(meta.ThreadID) {
		return TranscriptFormat, nil
	}
	var out []string
	for _, pat := range []string{
		filepath.Join(meta.Home, "sessions", "*", "*", "*", "rollout-*"+meta.ThreadID+".jsonl"),
		filepath.Join(meta.Home, "archived_sessions", "rollout-*"+meta.ThreadID+".jsonl"),
	} {
		m, _ := filepath.Glob(pat)
		out = append(out, m...)
	}
	return TranscriptFormat, out
}

// safeThreadID keeps glob metacharacters and separators out of patterns.
func safeThreadID(id string) bool {
	return id != "" && len(id) <= 128 && !strings.ContainsAny(id, `*?[]\/`)
}
