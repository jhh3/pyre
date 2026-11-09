package cli

import (
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

// iss_…: pyred restarts (its socket is gone for a
// few seconds) while `pyre publish` runs through it. The CLI says "pyred
// restarting, retrying" on stderr, waits for the socket, and publishes
// exactly one version; --json output stays parseable.
func TestPublishWaitsForPyredRestart(t *testing.T) {
	h := newHub(t)
	sock := filepath.Join(shortDir(t), "pyred.sock")
	auth := httpapi.NewStaticTokens(map[string]c.Principal{sessBToken: {Kind: c.PrincipalSession, ID: "ses_synth-b"}})
	srv := &http.Server{Handler: httpapi.NewServer(h.b, auth, httpapi.Options{StreamWait: time.Second}), ReadHeaderTimeout: 5 * time.Second}
	t.Cleanup(func() { srv.Close() })
	time.AfterFunc(600*time.Millisecond, func() {
		ln, err := net.Listen("unix", sock)
		if err != nil {
			t.Error(err)
			return
		}
		go srv.Serve(ln)
	})
	file := filepath.Join(t.TempDir(), "proof.txt")
	if err := os.WriteFile(file, []byte("synthetic proof\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, nil).with(EnvSocket, sock, EnvSessionToken, sessBToken, EnvSessionID, "ses_synth-b", EnvSocketWait, "20s")
	r := run(t, e, nil, "--json", "publish", file, "--title", "restart proof")
	expectCode(t, r, 0)
	rec := mustJSON[c.PublishReceipt](t, r)
	if !strings.Contains(r.stderr, "pyre: pyred restarting, retrying") {
		t.Fatalf("stderr lacks the restart notice:\n%s", r.stderr)
	}
	vers, err := h.b.ListVersions(ownerCtxFor(), c.ListVersionsRequest{ArtifactID: rec.Artifact.ArtifactID})
	if err != nil || len(vers.Items) != 1 {
		t.Fatalf("versions %d (%v), want exactly 1", len(vers.Items), err)
	}
}

// PYRE_SOCKET_WAIT must be a duration.
func TestSocketWaitEnvValidated(t *testing.T) {
	e := newEnv(t, nil).with(EnvSocket, "/tmp/pyre-synthetic-none.sock", EnvSocketWait, "soon")
	r := run(t, e, nil, "sessions")
	expectCode(t, r, ExitUsage)
	if !strings.Contains(r.stderr, "PYRE_SOCKET_WAIT") {
		t.Fatalf("stderr: %s", r.stderr)
	}
}
