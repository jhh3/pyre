package cli

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverLocalNode(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "pyrecli")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "pyred.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != localStatusPath {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"node_id":"node_local-synth","state_dir":"x","socket":"y","versions":{},"live_runs":0,"actions":[],"diagnostics":{}}`))
	})}
	go srv.Serve(ln)
	defer srv.Close()
	env := map[string]string{"PYRE_STATE_DIR": dir}
	if got := discoverLocalNode(func(k string) string { return env[k] }); got != "node_local-synth" {
		t.Fatalf("got %q", got)
	}
	// Nothing listening: empty, quickly.
	empty := map[string]string{"PYRE_STATE_DIR": filepath.Join(dir, "none")}
	if got := discoverLocalNode(func(k string) string { return empty[k] }); got != "" {
		t.Fatalf("got %q", got)
	}
}
