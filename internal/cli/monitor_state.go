package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A monitor's loop state (monitorState) lives in a small file on its
// machine, <state dir>/monitors/<monitor session>.json, so a relaunch
// after a reboot or crash (daemon/monitorrelaunch.go) continues instead of
// starting over. Files of monitors that ended long ago are pruned.

const monitorStateKeep = 30 * 24 * time.Hour

func monitorStatePath(stateDir, sessionID string) string {
	return filepath.Join(stateDir, "monitors", filepath.Base(sessionID)+".json")
}

func loadMonitorState(path string) (monitorState, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return monitorState{}, false
	}
	var st monitorState
	if json.Unmarshal(b, &st) != nil || st.Start.IsZero() {
		return monitorState{}, false
	}
	return st, true
}

// saveMonitorState writes st atomically (temp file, then rename), private
// to the user: outputs may name private paths.
func saveMonitorState(path string, st monitorState) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path))
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(b)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp, path)
	}
	if werr != nil {
		os.Remove(tmp)
		return werr
	}
	pruneMonitorStates(dir, path)
	return nil
}

// pruneMonitorStates removes other monitors' state files untouched for
// monitorStateKeep (a live monitor rewrites its own every check).
func pruneMonitorStates(dir, keep string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		p := filepath.Join(dir, e.Name())
		if p == keep || !strings.HasSuffix(e.Name(), ".json") && !strings.HasPrefix(e.Name(), ".tmp-") {
			continue
		}
		if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > monitorStateKeep {
			os.Remove(p)
		}
	}
}
