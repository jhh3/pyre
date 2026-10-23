package cli

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"runtime"
	"time"

	"github.com/jhh3/pyre/internal/winenv"
)

// localStatusPath mirrors daemon.PathLocalStatus (kept as a literal so the
// CLI does not link the daemon).
const localStatusPath = "/v1/local/status"

// discoverLocalNode returns the node ID of a pyred running on this machine,
// or "" if none answers quickly. It lets attach requests say "the client is
// on node X", so a session on this machine gets a local tmux attach while
// one elsewhere gets ssh. Lookup order: $PYRE_SOCKET, then
// $PYRE_STATE_DIR/pyred.sock, then the platform default state dir.
func discoverLocalNode(getenv func(string) string) string {
	for _, sock := range localSockets(getenv) {
		if id := localNodeID(sock); id != "" {
			return id
		}
	}
	return ""
}

// localSockets are the places a pyred on this machine serves its socket.
func localSockets(getenv func(string) string) []string {
	var socks []string
	if s := getenv(EnvSocket); s != "" {
		socks = append(socks, s)
	}
	if d := getenv("PYRE_STATE_DIR"); d != "" {
		socks = append(socks, filepath.Join(d, "pyred.sock"))
	}
	if d := winenv.StateDir(getenv); d != "" {
		socks = append(socks, filepath.Join(d, "pyred.sock")) // %LOCALAPPDATA%\Pyre
	} else if home := getenv("HOME"); home != "" {
		if runtime.GOOS == "darwin" {
			socks = append(socks, filepath.Join(home, "Library", "Application Support", "pyre", "pyred.sock"))
		} else if x := getenv("XDG_STATE_HOME"); x != "" {
			socks = append(socks, filepath.Join(x, "pyre", "pyred.sock"))
		} else {
			socks = append(socks, filepath.Join(home, ".local", "state", "pyre", "pyred.sock"))
		}
	}
	return socks
}

func localNodeID(sock string) string {
	hc := &http.Client{Timeout: 500 * time.Millisecond, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}
	resp, err := hc.Get("http://pyred" + localStatusPath)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var st struct {
		NodeID string `json:"node_id"`
	}
	if json.NewDecoder(resp.Body).Decode(&st) != nil {
		return ""
	}
	return st.NodeID
}
