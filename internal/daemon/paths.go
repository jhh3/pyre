package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"

	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// DefaultStateDir returns the platform state directory for pyred:
// macOS ~/Library/Application Support/pyre; Linux $XDG_STATE_HOME/pyre or
// ~/.local/state/pyre. PYRE_STATE_DIR overrides both.
func DefaultStateDir() string {
	if v := os.Getenv("PYRE_STATE_DIR"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "pyre")
	}
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		return filepath.Join(x, "pyre")
	}
	return filepath.Join(home, ".local", "state", "pyre")
}

// EnsureNodeID migrates the daemon namespace in db and returns the node ID:
// configured if non-empty (persisted), else the stored or a new local ID.
func EnsureNodeID(ctx context.Context, db *sql.DB, configured string) (string, error) {
	if err := sqlitex.Migrate(ctx, db, Migrations...); err != nil {
		return "", err
	}
	d := &Daemon{st: &store{db: db}, cfg: Config{NodeID: configured}}
	if err := d.initNodeID(ctx); err != nil {
		return "", err
	}
	return d.nodeID, nil
}

// PathLocalStatus is the local-socket-only status endpoint used by
// `pyred status` (not part of the hub API).
const PathLocalStatus = "/v1/local/status"

// LocalStatus is returned by PathLocalStatus.
type LocalStatus struct {
	NodeID      string            `json:"node_id"`
	NodeName    string            `json:"node_name,omitempty"`
	StateDir    string            `json:"state_dir"`
	Socket      string            `json:"socket"`
	Versions    map[string]string `json:"versions"`
	LiveRuns    int               `json:"live_runs"`
	Actions     []string          `json:"actions"`
	Diagnostics DiagnosticsResult `json:"diagnostics"`
}

func (d *Daemon) localStatusHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st := LocalStatus{NodeID: d.nodeID, NodeName: d.cfg.NodeName, StateDir: d.cfg.StateDir, Socket: d.SocketPath(),
			Versions: d.versions(), LiveRuns: len(d.liveRuns()), Actions: d.Actions(), Diagnostics: d.Diagnostics(r.Context())}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(st)
	})
}
