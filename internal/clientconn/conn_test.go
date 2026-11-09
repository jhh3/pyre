//go:build !windows

package clientconn

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// fakePyred serves pyred's local status on <dir>/pyred.sock (mode 0600, as
// pyred makes it) in a short temp dir (sun_path is ~104 bytes on macOS).
// *owner is its owner_credential; nil: an older pyred without the field.
func fakePyred(t *testing.T, owner *bool) (dir, sock string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "pyre-lp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock = filepath.Join(dir, "pyred.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != localStatusPath {
			http.NotFound(w, r)
			return
		}
		st := map[string]any{"node_id": "node_synthetic"}
		if owner != nil {
			st["owner_credential"] = *owner
		}
		json.NewEncoder(w).Encode(st)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	if err := os.Chmod(sock, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, sock
}

func ptr[T any](v T) *T { return &v }

type envMap map[string]string

func (e envMap) get(k string) string { return e[k] }

func TestDefaultDaemonSocket(t *testing.T) {
	if got := DefaultDaemonSocket(envMap{}.get); got != "" {
		t.Fatalf("no HOME must find nothing (never the real home): %q", got)
	}
	if got := DefaultDaemonSocket(envMap{"HOME": "/h", EnvStateDir: "/s"}.get); got != "/s/pyred.sock" {
		t.Fatalf("PYRE_STATE_DIR: %q", got)
	}
	got := DefaultDaemonSocket(envMap{"HOME": "/h"}.get)
	want := "/h/.local/state/pyre/pyred.sock"
	if runtime.GOOS == "darwin" {
		want = "/h/Library/Application Support/pyre/pyred.sock"
	} else if got := DefaultDaemonSocket(envMap{"HOME": "/h", "XDG_STATE_HOME": "/x"}.get); got != "/x/pyre/pyred.sock" {
		t.Fatalf("XDG_STATE_HOME: %q", got)
	}
	if got != want {
		t.Fatalf("default: %q, want %q", got, want)
	}
}

func TestCheckOwnSocket(t *testing.T) {
	dir, sock := fakePyred(t, ptr(true))
	if err := CheckOwnSocket(sock); err != nil {
		t.Fatalf("own 0600 socket: %v", err)
	}
	if err := CheckOwnSocket(filepath.Join(dir, "missing.sock")); !os.IsNotExist(err) {
		t.Fatalf("missing: %v", err)
	}
	link := filepath.Join(dir, "link.sock")
	if err := os.Symlink(sock, link); err != nil {
		t.Fatal(err)
	}
	if err := CheckOwnSocket(link); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink must be refused: %v", err)
	}
	plain := filepath.Join(dir, "plain")
	if err := os.WriteFile(plain, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckOwnSocket(plain); err == nil || !strings.Contains(err.Error(), "not a unix socket") {
		t.Fatalf("regular file must be refused: %v", err)
	}
	for _, mode := range []os.FileMode{0o666, 0o660, 0o606, 0o777} {
		if err := os.Chmod(sock, mode); err != nil {
			t.Fatal(err)
		}
		if err := CheckOwnSocket(sock); err == nil || !strings.Contains(err.Error(), "user-only") {
			t.Fatalf("mode %04o must be refused: %v", mode, err)
		}
	}
	if err := os.Chmod(sock, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := CheckOwnSocket(sock); err != nil {
		t.Fatalf("0700 socket: %v", err)
	}
	// Owned by another user.
	old := socketForeignOwner
	socketForeignOwner = func(string, os.FileInfo) (string, bool) { return "uid 4242", true }
	err := CheckOwnSocket(sock)
	socketForeignOwner = old
	if err == nil || !strings.Contains(err.Error(), "owned by uid 4242") {
		t.Fatalf("foreign owner must be refused: %v", err)
	}
	// A socket file nobody listens on (pyred crashed).
	stale := filepath.Join(dir, "stale.sock")
	ln, err := net.Listen("unix", stale)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	if err := os.Chmod(stale, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckOwnSocket(stale); err == nil || !strings.Contains(err.Error(), "nothing is listening") {
		t.Fatalf("stale socket must be refused: %v", err)
	}
}

func TestResolveLocalPyredFallback(t *testing.T) {
	dir, sock := fakePyred(t, ptr(true))
	cfgPath := filepath.Join(dir, "client.json")
	base := envMap{"HOME": dir, EnvStateDir: dir, EnvConfig: cfgPath}

	// No hub, socket or config: this machine's pyred, no credential (pyred
	// treats a bearer-less local call as its owner).
	conn, err := Resolve(ConnOptions{Getenv: base.get})
	if err != nil {
		t.Fatalf("fallback: %v", err)
	}
	if conn.Mode != "socket" || conn.Socket != sock || conn.Source != SourceLocalPyred || conn.TokenSource != "none" {
		t.Fatalf("fallback: %+v", conn)
	}

	// Precedence: PYRE_SOCKET, PYRE_HUB, --hub and a client config all beat it.
	with := func(kv ...string) envMap {
		e := envMap{}
		for k, v := range base {
			e[k] = v
		}
		for i := 0; i+1 < len(kv); i += 2 {
			e[kv[i]] = kv[i+1]
		}
		return e
	}
	if conn, err := Resolve(ConnOptions{Getenv: with(EnvSocket, "/tmp/other-synthetic.sock").get}); err != nil || conn.Socket != "/tmp/other-synthetic.sock" || conn.Source != EnvSocket {
		t.Fatalf("PYRE_SOCKET: %+v %v", conn, err)
	}
	if conn, err := Resolve(ConnOptions{Getenv: with(EnvHub, "http://hub.synthetic:1").get}); err != nil || conn.Mode != "hub" || conn.Source != EnvHub {
		t.Fatalf("PYRE_HUB: %+v %v", conn, err)
	}
	if conn, err := Resolve(ConnOptions{HubFlag: "http://flag.synthetic:2", Getenv: base.get}); err != nil || conn.Mode != "hub" || conn.Source != "--hub" {
		t.Fatalf("--hub: %+v %v", conn, err)
	}
	if err := SaveConfig(cfgPath, ClientConfig{HubURL: "https://hub.synthetic:7788", Token: "pyc_config-token-synthetic"}); err != nil {
		t.Fatal(err)
	}
	if conn, err := Resolve(ConnOptions{Getenv: base.get}); err != nil || conn.Mode != "hub" || conn.Source != "config "+cfgPath {
		t.Fatalf("config: %+v %v", conn, err)
	}
	// A config that is there but unusable is an error, not a fallback.
	if err := os.Chmod(cfgPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if conn, err := Resolve(ConnOptions{Getenv: base.get}); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("loose config must not fall back: %+v %v", conn, err)
	}
	os.Remove(cfgPath)

	// A loose socket is refused with today's error, naming why.
	if err := os.Chmod(sock, 0o666); err != nil {
		t.Fatal(err)
	}
	_, err = Resolve(ConnOptions{Getenv: base.get})
	if !contracts.IsCode(err, contracts.ErrUnavailable) || !strings.Contains(err.Error(), "no hub configured") || !strings.Contains(err.Error(), "was not used") || !strings.Contains(err.Error(), "mode 0666") {
		t.Fatalf("loose socket: %v", err)
	}
	// A pyred without an owner credential (or too old to say) cannot act
	// for the owner on the hub: today's error, naming why.
	for owner, why := range map[*bool]string{ptr(false): "it holds no owner credential", nil: "too old to say whether it holds an owner credential"} {
		d, s := fakePyred(t, owner)
		_, err := Resolve(ConnOptions{Getenv: envMap{"HOME": d, EnvStateDir: d, EnvConfig: filepath.Join(d, "client.json")}.get})
		if !contracts.IsCode(err, contracts.ErrUnavailable) || !strings.Contains(err.Error(), "no hub configured") || !strings.Contains(err.Error(), s+" was not used: ") || !strings.Contains(err.Error(), why) {
			t.Fatalf("owner_credential %v: %v", owner, err)
		}
	}
	// No socket at all: exactly today's error.
	_, err = Resolve(ConnOptions{Getenv: envMap{"HOME": dir, EnvStateDir: filepath.Join(dir, "none"), EnvConfig: cfgPath}.get})
	if !contracts.IsCode(err, contracts.ErrUnavailable) || !strings.Contains(err.Error(), "no hub configured (no client config at "+cfgPath+")") || strings.Contains(err.Error(), "was not used") {
		t.Fatalf("no socket: %v", err)
	}
}
