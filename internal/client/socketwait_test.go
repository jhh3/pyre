package client

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/httpapi"
)

// socketClient talks to a pyred-style unix socket at path (as
// clientconn.UnixSocketHTTPClient does) and waits up to wait for it.
func socketClient(path string, wait time.Duration) *Client {
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		conn, err := d.DialContext(ctx, "unix", path)
		if err != nil {
			return nil, &SocketDialError{Path: path, Err: err}
		}
		return conn, nil
	}}
	o := fastOpts()
	o.HTTPClient, o.DaemonRestartWait = &http.Client{Transport: tr}, wait
	return New("http://pyred.local", "tok", o)
}

func serveSocket(t *testing.T, path string, api c.API) {
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Error(err)
		return
	}
	srv := &http.Server{Handler: httpapi.NewServer(api, httpapi.NewStaticTokens(map[string]c.Principal{"tok": owner}), httpapi.Options{StreamWait: time.Second}), ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
}

// shortSockDir is a directory short enough for unix socket paths.
func shortSockDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "pys")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// While pyred restarts its socket is missing (or left behind with nothing
// listening): calls wait for it to come back, saying so, and a mutation
// still reaches the hub exactly once under its operation id.
func TestSocketCallsWaitForPyredRestart(t *testing.T) {
	for _, stale := range []bool{false, true} {
		sock := filepath.Join(shortSockDir(t), "pyred.sock")
		if stale {
			ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			ln.SetUnlinkOnClose(false)
			ln.Close() // the file stays; nothing listens (ECONNREFUSED)
		}
		f := &fake{}
		time.AfterFunc(400*time.Millisecond, func() {
			os.Remove(sock)
			serveSocket(t, sock, f)
		})
		cl := socketClient(sock, 10*time.Second)
		var notes noticeLog
		mv, err := cl.SendMessage(notes.ctx(), c.SendMessageRequest{Mutation: c.Mutation{OperationID: "op_synth-restart"}, Body: c.MessageBody{Text: "synthetic"}})
		if err != nil {
			t.Fatalf("stale=%v: send through a pyred restart: %v", stale, err)
		}
		f.mu.Lock()
		ops := append([]string(nil), f.sendOps...)
		f.mu.Unlock()
		if mv.Envelope.OperationID != "op_synth-restart" || len(ops) != 1 {
			t.Fatalf("stale=%v: sent %v", stale, ops)
		}
		if got := notes.joined(); !strings.Contains(got, "pyred restarting, retrying") || strings.Contains(got, "\n") {
			t.Fatalf("stale=%v: notices %q (want one pyred restarting line)", stale, got)
		}
	}
}

// The wait is bounded and opt-in: past it the error is the usual "local
// daemon unreachable" (saying it waited); a single-attempt call, or a
// client without DaemonRestartWait, fails at once.
func TestSocketWaitBounded(t *testing.T) {
	sock := filepath.Join(shortSockDir(t), "none.sock")
	start := time.Now()
	_, err := socketClient(sock, 300*time.Millisecond).WhoAmI(context.Background())
	if !c.IsCode(err, c.ErrUnavailable) || !strings.Contains(err.Error(), "no socket at") || !strings.Contains(err.Error(), "waited 300ms for pyred") {
		t.Fatalf("after the wait: %v", err)
	}
	if d := time.Since(start); d < 300*time.Millisecond || d > 5*time.Second {
		t.Fatalf("waited %v", d)
	}
	for name, call := range map[string]func() error{
		"single attempt": func() error {
			_, err := socketClient(sock, time.Minute).WhoAmI(WithMaxAttempts(context.Background(), 1))
			return err
		},
		"no wait configured": func() error { _, err := socketClient(sock, 0).WhoAmI(context.Background()); return err },
	} {
		start := time.Now()
		if err := call(); !c.IsCode(err, c.ErrUnavailable) || time.Since(start) > 2*time.Second {
			t.Fatalf("%s: %v after %v", name, err, time.Since(start))
		}
	}
}
