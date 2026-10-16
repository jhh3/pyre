//go:build !windows

// Package stack brings up a real local Pyre stack for resilience, security
// and budget tests: the pyre-hub and pyred binaries built from this module,
// run as separate processes in an isolated directory on a loopback port, so
// tests can SIGKILL and restart them, sample their resources by PID, and
// scan their on-disk state. It mirrors tests/e2e/local_stack.sh but is
// driven from Go with per-test directories and ports.
//
// Nothing here touches ~/.config/pyre, the default tmux server or real
// credentials: every state directory lives under a short /tmp directory
// (Unix socket paths are length-limited) and every credential is minted for
// the test.
package stack

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/client"
	"github.com/jhh3/pyre/internal/contracts"
)

// Bins are built binaries.
type Bins struct {
	Hub, Pyred string
	dir        string
}

var (
	buildOnce sync.Once
	built     Bins
	buildErr  error
)

// ModuleRoot returns the repository root.
func ModuleRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}

// PrebuiltBinsEnv names a directory holding prebuilt pyre-hub and pyred
// binaries. It lets a cross-compiled test binary run the real-stack
// scenarios on a host without a Go toolchain (e.g. linux test binaries
// built on a Mac and copied to a Linux host).
const PrebuiltBinsEnv = "PYRE_TEST_STACK_BINS"

var errNoToolchain = fmt.Errorf("no go toolchain on PATH and %s unset", PrebuiltBinsEnv)

// Build compiles pyre-hub and pyred once per test process, or uses the
// prebuilt binaries in $PYRE_TEST_STACK_BINS. Without either a toolchain or
// prebuilt binaries the calling test is skipped.
func Build(t testing.TB) Bins {
	t.Helper()
	buildOnce.Do(func() {
		if pre := os.Getenv(PrebuiltBinsEnv); pre != "" {
			b := Bins{Hub: filepath.Join(pre, "pyre-hub"), Pyred: filepath.Join(pre, "pyred")}
			for _, p := range []string{b.Hub, b.Pyred} {
				if st, err := os.Stat(p); err != nil || st.Mode()&0o111 == 0 {
					buildErr = fmt.Errorf("%s: %s is not an executable (%v)", PrebuiltBinsEnv, p, err)
					return
				}
			}
			built = b
			return
		}
		// `go test` puts its own $GOROOT/bin first on the test binary's PATH,
		// so this is the toolchain running the test (runtime.GOROOT is
		// deprecated: it reports the build-time root).
		gobin, err := exec.LookPath("go")
		if err != nil {
			buildErr = errNoToolchain
			return
		}
		dir, err := os.MkdirTemp("", "pyre-wp18-bin-")
		if err != nil {
			buildErr = err
			return
		}
		for _, name := range []string{"pyre-hub", "pyred"} {
			cmd := exec.Command(gobin, "build", "-o", filepath.Join(dir, name), "./cmd/"+name)
			cmd.Dir = ModuleRoot()
			cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
			if out, err := cmd.CombinedOutput(); err != nil {
				buildErr = fmt.Errorf("build %s: %v\n%s", name, err, out)
				return
			}
		}
		built = Bins{Hub: filepath.Join(dir, "pyre-hub"), Pyred: filepath.Join(dir, "pyred"), dir: dir}
	})
	if buildErr == errNoToolchain {
		t.Skip(buildErr)
	}
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return built
}

// Cleanup removes built binaries (call from TestMain after m.Run).
func Cleanup() {
	if built.dir != "" {
		_ = os.RemoveAll(built.dir)
	}
}

// ShortDir returns a short temp directory removed at test end.
func ShortDir(t testing.TB) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "pw18-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// FreePort returns a loopback address that was free a moment ago.
func FreePort(t testing.TB) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

// Proc is a supervised child process with its combined output in a file.
type Proc struct {
	Name    string
	Args    []string
	Env     []string
	LogPath string
	Stdin   string

	mu   sync.Mutex
	cmd  *exec.Cmd
	done chan struct{}
	log  *os.File
}

// Start launches the process (appending to LogPath).
func (p *Proc) Start(t testing.TB) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	f, err := os.OpenFile(p.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(p.Args[0], p.Args[1:]...)
	cmd.Env = append(os.Environ(), p.Env...)
	cmd.Stdout, cmd.Stderr = f, f
	if p.Stdin != "" {
		cmd.Stdin = strings.NewReader(p.Stdin)
	}
	// Own process group so a kill never reaches the test runner.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		f.Close()
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); f.Close(); close(done) }()
	p.cmd, p.done, p.log = cmd, done, f
}

// PID of the running process (0 if not running).
func (p *Proc) PID() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd == nil || p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

// Running reports whether the process is alive.
func (p *Proc) Running() bool {
	p.mu.Lock()
	done := p.done
	p.mu.Unlock()
	if done == nil {
		return false
	}
	select {
	case <-done:
		return false
	default:
		return true
	}
}

// Signal sends sig to the process (only; not its group) and waits for exit.
func (p *Proc) Signal(sig syscall.Signal, wait time.Duration) bool {
	p.mu.Lock()
	cmd, done := p.cmd, p.done
	p.mu.Unlock()
	if cmd == nil {
		return true
	}
	_ = cmd.Process.Signal(sig)
	select {
	case <-done:
		return true
	case <-time.After(wait):
		return false
	}
}

// Kill is SIGKILL (a crash). Stop is SIGTERM with a SIGKILL fallback.
func (p *Proc) Kill() { p.Signal(syscall.SIGKILL, 10*time.Second) }
func (p *Proc) Stop() {
	if !p.Signal(syscall.SIGTERM, 15*time.Second) {
		p.Kill()
	}
}

// Log returns the process output so far.
func (p *Proc) Log() string {
	b, _ := os.ReadFile(p.LogPath)
	return string(b)
}

// Hub is a pyre-hub process.
type Hub struct {
	Bins     Bins
	Dir      string // working dir (hub data under Dir/hub)
	Data     string
	Addr     string
	URL      string
	Pin      string
	Owner    contracts.ClientCredential
	Proc     *Proc
	ExtraEnv []string
}

type ownerCredFile struct {
	HubURL        string `json:"hub_url"`
	HubCertSHA256 string `json:"hub_cert_sha256"`
	ClientID      string `json:"client_id"`
	Token         string `json:"token"`
}

// OwnerCredentialPath is a copy of the owner credential written by
// pyre-hub init, kept outside the data directory (backups do not carry it).
func (h *Hub) OwnerCredentialPath() string { return filepath.Join(h.Dir, "owner-credential.json") }

// StartHub initializes (once) and serves a hub in dir/hub on a free port.
func StartHub(t testing.TB, b Bins, dir string) *Hub {
	t.Helper()
	addr := FreePort(t)
	h := &Hub{Bins: b, Dir: dir, Data: filepath.Join(dir, "hub"), Addr: addr, URL: "https://" + addr}
	out, err := exec.Command(b.Hub, "init", "--data", h.Data, "--hub-url", h.URL, "--tls-host", "127.0.0.1").CombinedOutput()
	if err != nil {
		t.Fatalf("pyre-hub init: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(filepath.Join(h.Data, "owner-credential.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.OwnerCredentialPath(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	h.loadOwner(t)
	h.Proc = &Proc{Name: "pyre-hub", LogPath: filepath.Join(dir, "hub.log")}
	h.Start(t)
	t.Cleanup(h.Proc.Stop)
	return h
}

func (h *Hub) loadOwner(t testing.TB) {
	t.Helper()
	raw, err := os.ReadFile(h.OwnerCredentialPath())
	if err != nil {
		t.Fatal(err)
	}
	var oc ownerCredFile
	if err := json.Unmarshal(raw, &oc); err != nil {
		t.Fatal(err)
	}
	h.Pin, h.Owner = oc.HubCertSHA256, contracts.ClientCredential{ClientID: oc.ClientID, Token: oc.Token}
}

// Start (re)starts serving the current data directory on the same port.
func (h *Hub) Start(t testing.TB) {
	t.Helper()
	h.Proc.Args = []string{h.Bins.Hub, "serve", "--data", h.Data, "--listen", h.Addr, "--hub-url", h.URL}
	h.Proc.Env = h.ExtraEnv
	deadline := time.Now().Add(15 * time.Second)
	for {
		h.Proc.Start(t)
		if WaitHealthy(h.URL, 10*time.Second) {
			return
		}
		h.Proc.Kill()
		if time.Now().After(deadline) {
			t.Fatalf("pyre-hub did not become healthy:\n%s", tail(h.Proc.Log(), 4000))
		}
		time.Sleep(200 * time.Millisecond) // port may still be in TIME_WAIT
	}
}

// UseData points the hub at another data directory (restore). The owner
// credential and pin are kept: tokens live hashed in the restored database
// and the TLS identity is part of the backup.
func (h *Hub) UseData(data string) { h.Data = data }

// WaitHealthy polls /healthz.
func WaitHealthy(url string, d time.Duration) bool {
	hc := &http.Client{Timeout: time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // health probe only
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		resp, err := hc.Get(url + contracts.PathHealth)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return true
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// Client returns an owner client pinned to the hub certificate.
func (h *Hub) Client() *client.Client {
	return client.New(h.URL, h.Owner.Token, client.Options{CertSHA256: h.Pin, RetryBase: 20 * time.Millisecond, RetryMax: 500 * time.Millisecond, MaxAttempts: 8})
}

// OwnerCtx is a context for owner calls through Client (the client carries
// the credential; the context carries deadlines only).
func OwnerCtx(t testing.TB) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// Node is a node daemon process (pyred, or the test-binary daemon role).
type Node struct {
	Hub      *Hub
	Name     string
	NodeID   string
	Token    string
	State    string
	CredFile string
	// HubURL overrides the credential's hub URL (e.g. through a fault proxy).
	HubURL string
	// Exec is the daemon command prefix (default: pyred run). The test
	// binary daemon role uses [os.Args[0]] with Env PYRE_RESILIENCE_ROLE.
	Exec  []string
	Env   []string
	Extra []string // additional flags
	Proc  *Proc
}

// EnrollNode creates a ticket through the API and redeems it with
// `pyred enroll --secret-stdin`.
func EnrollNode(t testing.TB, h *Hub, dir, name string) *Node {
	t.Helper()
	ctx := OwnerCtx(t)
	tk, err := h.Client().CreateEnrollment(ctx, contracts.EnrollmentRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	n := &Node{Hub: h, Name: name, NodeID: tk.NodeID, State: filepath.Join(dir, "node-"+name)}
	if err := os.MkdirAll(n.State, 0o700); err != nil {
		t.Fatal(err)
	}
	n.CredFile = filepath.Join(n.State, "node-credential.json")
	cmd := exec.Command(h.Bins.Pyred, "enroll", "--hub", h.URL, "--pin", h.Pin, "--node-id", tk.NodeID, "--credential-file", n.CredFile, "--secret-stdin")
	cmd.Stdin = strings.NewReader(tk.Secret + "\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("pyred enroll: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(n.CredFile)
	if err != nil {
		t.Fatal(err)
	}
	var cf struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(raw, &cf)
	n.Token = cf.Token
	n.Proc = &Proc{Name: "node-" + name, LogPath: filepath.Join(dir, "node-"+name+".log")}
	t.Cleanup(n.Proc.Stop)
	return n
}

// Start launches the daemon.
func (n *Node) Start(t testing.TB) {
	t.Helper()
	ex := n.Exec
	if len(ex) == 0 {
		ex = []string{n.Hub.Bins.Pyred}
	}
	args := append([]string{}, ex...)
	args = append(args, "run", "--state", n.State, "--credential", n.CredFile, "--owner-credential", n.Hub.OwnerCredentialPath(),
		"--name", n.Name, "--no-integrations")
	if n.HubURL != "" {
		args = append(args, "--hub", n.HubURL)
	}
	args = append(args, n.Extra...)
	n.Proc.Args, n.Proc.Env = args, n.Env
	n.Proc.Start(t)
}

// WaitConnected waits until the hub reports the node connected.
func (n *Node) WaitConnected(t testing.TB, d time.Duration) {
	t.Helper()
	WaitFor(t, d, "node "+n.Name+" connected", func() bool {
		nd, err := n.Hub.Client().GetNode(context.Background(), contracts.GetRequest{ID: n.NodeID})
		return err == nil && nd.Connection.State == contracts.ConnConnected && len(nd.Harnesses) > 0
	})
}

// WaitFor polls cond.
func WaitFor(t testing.TB, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %s", d, what)
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// Tail returns the last n bytes of a process log (for failure messages).
func (p *Proc) Tail(n int) string { return tail(p.Log(), n) }

// RunCLI runs a binary to completion and returns combined output.
func RunCLI(args ...string) (string, error) {
	var buf bytes.Buffer
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	return buf.String(), err
}
