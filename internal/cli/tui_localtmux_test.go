package cli

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLocalTmuxSocketFindsPyredsSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no tmux runs on Windows")
	}
	dir, err := os.MkdirTemp("", "pyre-lts-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	env := map[string]string{"PYRE_STATE_DIR": dir, "HOME": filepath.Join(dir, "nohome")}
	getenv := func(k string) string { return env[k] }
	if got := localTmuxSocket(getenv, "node_synth"); got != "" {
		t.Fatalf("no socket yet, got %q", got)
	}
	sock := filepath.Join(dir, "tmux", "pyre.sock")
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if got := localTmuxSocket(getenv, "node_synth"); got != sock {
		t.Fatalf("got %q, want %q", got, sock)
	}
	if got := localTmuxSocket(getenv, ""); got != "" {
		t.Fatalf("a client with no node here got %q", got)
	}
}
