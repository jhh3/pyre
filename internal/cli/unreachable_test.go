package cli

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// shortDir returns a temp dir short enough for unix socket paths (macOS
// t.TempDir paths can exceed the 104-byte sun_path limit).
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "pyc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// A missing or dead local daemon socket is reported as the local daemon
// (with the socket path and what to do), never as "hub unreachable".
func TestUnreachableNamesTheEndpoint(t *testing.T) {
	dir := shortDir(t)
	stale := filepath.Join(dir, "stale.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: stale, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)
	ln.Close() // the file stays; nothing listens

	long := filepath.Join(dir, strings.Repeat("d", 120), "pyred.sock")
	for _, tc := range []struct {
		name, sock string
		want       []string
	}{
		{"missing", filepath.Join(dir, "nope", "pyred.sock"), []string{"local daemon unreachable: no socket at " + filepath.Join(dir, "nope", "pyred.sock"), "start pyred", "unset PYRE_SOCKET", "tried local daemon socket"}},
		{"stale", stale, []string{"nothing is listening on " + stale, "start pyred"}},
		{"too long", long, []string{"longer than unix sockets allow"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, nil).with(EnvSocket, tc.sock)
			r := run(t, e, nil, "sessions")
			expectCode(t, r, ExitUnavailable)
			if strings.Contains(r.stderr, "hub unreachable") {
				t.Fatalf("socket failure blamed on the hub:\n%s", r.stderr)
			}
			for _, w := range tc.want {
				if !strings.Contains(r.stderr, w) {
					t.Fatalf("stderr lacks %q:\n%s", w, r.stderr)
				}
			}
		})
	}

	// Hub mode says which hub was tried, from where, and what to do.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	e := newEnv(t, nil).with(EnvHub, "http://"+addr)
	r := run(t, e, nil, "sessions")
	expectCode(t, r, ExitUnavailable)
	for _, w := range []string{"hub unreachable", "tried hub http://" + addr + " (from PYRE_HUB", "pyre hello"} {
		if !strings.Contains(r.stderr, w) {
			t.Fatalf("stderr lacks %q:\n%s", w, r.stderr)
		}
	}
	// --json keeps a parseable error with the endpoint in details.
	r = run(t, newEnv(t, nil).with(EnvSocket, filepath.Join(dir, "nope.sock")), nil, "--json", "sessions")
	if !strings.Contains(r.stderr, `"unreachable":"local_daemon"`) || !strings.Contains(r.stderr, `"socket":`) {
		t.Fatalf("json error lacks endpoint details: %s", r.stderr)
	}
}
