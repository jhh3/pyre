package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/client"
	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/enrollment"
)

// Node-setup regressions found while adding office-box2 (Linux) and
// win-office (Windows 11).

// enroll-node --hub-url sets the plain ticket's hub URL (it was honoured by
// --bootstrap only, so the ticket JSON carried the hub's default address).
func TestEnrollNodePlainTicketHonorsHubURL(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	r := run(t, e, nil, "--json", "enroll-node", "--name", "synth-office2", "--hub-url", "https://hub.synthetic.ts.net:7443")
	expectCode(t, r, 0)
	if tk := mustJSON[c.EnrollmentTicket](t, r); tk.HubURL != "https://hub.synthetic.ts.net:7443" || tk.Secret == "" {
		t.Fatalf("ticket hub_url = %q", tk.HubURL)
	}
	r = run(t, e, nil, "enroll-node", "--name", "synth-office2", "--hub-url", "https://hub.synthetic.ts.net:7443")
	expectCode(t, r, 0)
	if !strings.Contains(r.stdout, "pyred enroll --hub https://hub.synthetic.ts.net:7443") {
		t.Fatalf("printed enroll command ignores --hub-url: %s", r)
	}
	expectCode(t, run(t, e, nil, "enroll-node", "--name", "x", "--hub-url", "http://insecure:7443"), ExitUsage)
	expectCode(t, run(t, e, nil, "enroll-node", "--name", "x", "--target-os", "windows"), ExitUsage)
}

// sshFake answers the bootstrap's target probe like a Windows OpenSSH host
// (cmd.exe login shell: no uname) and records every step.
type sshFake struct {
	mu     sync.Mutex
	calls  [][]string
	stdins []string
	unix   bool
}

func (f *sshFake) Run(_ context.Context, argv []string, stdin io.Reader, stdout, _ io.Writer) error {
	b, _ := io.ReadAll(stdin)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, argv)
	f.stdins = append(f.stdins, string(b))
	last := argv[len(argv)-1]
	switch {
	case last == "uname -s" && f.unix:
		io.WriteString(stdout, "Linux\n")
	case last == "uname -s":
		return errors.New("'uname' is not recognized as an internal or external command (exit status 1)")
	case strings.Contains(last, "OSVersion.Platform"):
		io.WriteString(stdout, "Win32NT\r\n")
	}
	return nil
}

// enroll-node --bootstrap --execute probes the target and enrolls a Windows
// host with PowerShell steps (it ran mkdir -p / chmod / "$HOME" there).
func TestEnrollNodeBootstrapWindowsTarget(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	fake := &sshFake{}
	r := run(t, e, &App{SSHRunner: fake}, "--json", "enroll-node", "--name", "synth-win", "--ssh", "synth-win", "--bootstrap", "--execute",
		"--hub-url", "https://hub.synthetic:7443", "--pyred", "/opt/synthetic/pyred-windows-amd64.exe")
	expectCode(t, r, 0)
	var out map[string]string
	if err := json.Unmarshal([]byte(r.stdout), &out); err != nil || out["target_os"] != enrollment.TargetWindows || !strings.HasPrefix(out["node_id"], "node_") {
		t.Fatalf("result %v: %s", err, r)
	}
	if len(fake.calls) != 6 { // uname, PowerShell probe, 4 steps
		t.Fatalf("calls: %q", fake.calls)
	}
	secretSeen := 0
	for i, argv := range fake.calls {
		joined := strings.Join(argv, " ")
		for _, bad := range []string{"mkdir -p", "chmod", `"$HOME"`, "pye_"} {
			if strings.Contains(joined, bad) {
				t.Fatalf("call %d carries %q: %s", i, bad, joined)
			}
		}
		if strings.HasPrefix(fake.stdins[i], "pye_") {
			secretSeen++
		}
	}
	if secretSeen != 1 || !strings.HasPrefix(fake.stdins[5], "pye_") {
		t.Fatalf("the secret must travel once, on the enroll step's stdin: %q", fake.stdins)
	}
	if got := strings.Join(fake.calls[3], " "); got != "scp -q /opt/synthetic/pyred-windows-amd64.exe synth-win:AppData/Local/Programs/Pyre/bin/.pyred.new" {
		t.Fatalf("copy step: %s", got)
	}

	// A Windows target needs an explicit pyred.exe (the local pyred is not one).
	expectCode(t, run(t, e, &App{SSHRunner: &sshFake{}}, "enroll-node", "--name", "synth-win2", "--ssh", "synth-win2", "--bootstrap", "--execute"), ExitUsage)

	// A unix target keeps the POSIX steps.
	ux := &sshFake{unix: true}
	r = run(t, e, &App{SSHRunner: ux}, "enroll-node", "--name", "synth-lin", "--ssh", "synth-lin", "--bootstrap", "--execute", "--hub-url", "https://hub.synthetic:7443", "--pyred", "/opt/synthetic/pyred")
	expectCode(t, r, 0)
	if len(ux.calls) != 5 || !strings.Contains(strings.Join(ux.calls[1], " "), "mkdir -p") {
		t.Fatalf("unix calls: %q", ux.calls)
	}

	// The dry run connects nowhere; --target-os windows previews the PowerShell steps.
	none := &sshFake{}
	r = run(t, e, &App{SSHRunner: none}, "enroll-node", "--name", "synth-win3", "--ssh", "synth-win3", "--bootstrap", "--target-os", "windows", "--hub-url", "https://hub.synthetic:7443", "--pyred", "/opt/synthetic/pyred.exe")
	expectCode(t, r, 0)
	if len(none.calls) != 0 || !strings.Contains(r.stdout, "powershell: ") || strings.Contains(r.stdout, "mkdir -p") {
		t.Fatalf("windows dry run (calls %d): %s", len(none.calls), r)
	}
}

// pyre inventory takes the node as a positional argument, like
// `pyre nodes show` (it failed with `unknown command "win-home"`).
func TestInventoryPositionalNode(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	r := run(t, e, nil, "--json", "inventory", "synth-mac")
	expectCode(t, r, 0)
	if inv := mustJSON[c.NodeInventory](t, r); inv.NodeID != "node_synth-mac" {
		t.Fatalf("inventory for %q", inv.NodeID)
	}
	expectCode(t, run(t, e, nil, "inventory", "synth-mac", "--node", "synth-linux"), ExitUsage)
	expectCode(t, run(t, e, nil, "inventory", "synth-mac", "synth-linux"), ExitUsage)
}

// An owner command run from inside an agent session's shell (the session's
// token removed, PYRE_SESSION_ID/RUN_ID still set) starts a top-level
// session: it neither becomes that session's child nor joins its project.
func TestOwnerStartIgnoresEnclosingSessionEnv(t *testing.T) {
	h := newHub(t)
	owner := newEnv(t, h).with(EnvSessionID, "ses_synth-a", EnvRunID, "run_synth-a1", EnvNodeID, "node_synth-mac")
	r := run(t, owner, nil, "--json", "start", "--node", "synth-mac", "--harness", "claude", "--cwd", "/home/synth/release", "--title", "release")
	expectCode(t, r, 0)
	if s := mustJSON[c.StartSessionResponse](t, r).Session.Session; s.Lineage.ParentSessionID != "" {
		t.Fatalf("owner start inherited the enclosing session as parent: %+v", s.Lineage)
	}
	// --parent makes the lineage explicit.
	r = run(t, owner, nil, "--json", "start", "--node", "synth-mac", "--harness", "claude", "--cwd", "/home/synth/release", "--parent", "ses_synth-a")
	expectCode(t, r, 0)
	if s := mustJSON[c.StartSessionResponse](t, r).Session.Session; s.Lineage.ParentSessionID != "ses_synth-a" {
		t.Fatalf("--parent ignored: %+v", s.Lineage)
	}
	// A session credential still records its own session as the parent;
	// --parent cannot change that (the hub sets it), so it is refused.
	agent := owner.with(EnvToken, "", EnvSessionToken, sessAToken)
	expectCode(t, run(t, agent, nil, "start", "--node", "synth-mac", "--harness", "claude", "--cwd", "/home/synth/peer", "--parent", "ses_synth-b"), ExitUsage)
	r = run(t, agent, nil, "--json", "start", "--node", "synth-mac", "--harness", "claude", "--cwd", "/home/synth/peer")
	expectCode(t, r, 0)
	if s := mustJSON[c.StartSessionResponse](t, r).Session.Session; s.Lineage.ParentSessionID != "ses_synth-a" {
		t.Fatalf("agent start lost its parent: %+v", s.Lineage)
	}
}

// lossyAPI stores starts but loses the first lost answers (a hub restart
// or a timeout after the commit).
type lossyAPI struct {
	c.API
	mu    sync.Mutex
	lose  int // answers to lose; <0 loses every answer
	calls int
	ops   map[string]bool
}

func (l *lossyAPI) StartSession(ctx context.Context, req c.StartSessionRequest) (c.StartSessionResponse, error) {
	res, err := l.API.StartSession(ctx, req)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	if l.ops == nil {
		l.ops = map[string]bool{}
	}
	l.ops[req.OperationID] = true
	if err == nil && (l.lose < 0 || l.lose > 0) {
		if l.lose > 0 {
			l.lose--
		}
		// What the client reports when the round trip fails (client.transportError).
		return c.StartSessionResponse{}, c.Errorf(c.ErrUnavailable, "hub request timed out: context deadline exceeded").WithDetail(client.DetailUnreachable, "hub")
	}
	return res, err
}

func countTitled(t *testing.T, api c.API, title string) int {
	t.Helper()
	page, err := api.ListSessions(c.WithPrincipal(context.Background(), testOwner), c.ListSessionsRequest{Query: title})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, s := range page.Items {
		if s.Session.Title == title {
			n++
		}
	}
	return n
}

// A start whose answer is lost after the hub stored it replays the same
// operation ID and exits 0 with that session; it never launches twice. If
// the hub stays silent, the exit is 7 (indeterminate) with the operation ID
// to retry with, and that retry returns the one stored session.
func TestStartRecoversLostAnswerWithoutDuplicate(t *testing.T) {
	old := startRecoverDefault
	startRecoverDefault = StartRecovery{Delays: []time.Duration{time.Millisecond, time.Millisecond}, Total: 5 * time.Second}
	t.Cleanup(func() { startRecoverDefault = old })
	h := newHub(t)
	e := newEnv(t, h)

	api := &lossyAPI{API: h.b, lose: 1}
	r := run(t, e, &App{API: api}, "--json", "start", "--node", "synth-mac", "--harness", "claude", "--cwd", "/home/synth/rel", "--title", "release-a")
	expectCode(t, r, 0)
	if s := mustJSON[c.StartSessionResponse](t, r); s.Session.Session.SessionID == "" {
		t.Fatalf("no session: %s", r)
	}
	if n := countTitled(t, h.b, "release-a"); n != 1 || api.calls != 2 || len(api.ops) != 1 {
		t.Fatalf("sessions=%d calls=%d ops=%d (want one session, one operation)", n, api.calls, len(api.ops))
	}
	var opA string
	for id := range api.ops {
		opA = id
	}
	if strings.Count(r.stderr, "retrying with operation "+opA) != 1 {
		t.Fatalf("one stderr line must say the start is being retried: %q", r.stderr)
	}

	silent := &lossyAPI{API: h.b, lose: -1}
	r = run(t, e, &App{API: silent}, "--json", "start", "--node", "synth-mac", "--harness", "claude", "--cwd", "/home/synth/rel", "--title", "release-b")
	expectCode(t, r, ExitIndeterminate)
	var opID string
	for id := range silent.ops {
		opID = id
	}
	if len(silent.ops) != 1 || !strings.Contains(r.stderr, "--operation-id "+opID) {
		t.Fatalf("indeterminate start must name its operation ID %q: %s", opID, r)
	}
	r = run(t, e, nil, "--json", "--operation-id", opID, "start", "--node", "synth-mac", "--harness", "claude", "--cwd", "/home/synth/rel", "--title", "release-b")
	expectCode(t, r, 0)
	if n := countTitled(t, h.b, "release-b"); n != 1 {
		t.Fatalf("retry with the operation ID made %d sessions", n)
	}

	// The whole recovery is bounded: long delays are cut off at Total.
	startRecoverDefault = StartRecovery{Delays: []time.Duration{time.Hour}, Total: 50 * time.Millisecond}
	began := time.Now()
	r = run(t, e, &App{API: &lossyAPI{API: h.b, lose: -1}}, "start", "--node", "synth-mac", "--harness", "claude", "--cwd", "/home/synth/rel", "--title", "release-d")
	expectCode(t, r, ExitIndeterminate)
	if time.Since(began) > 10*time.Second {
		t.Fatalf("recovery not bounded: %s", time.Since(began))
	}

	// A definite answer from the hub is not replayed: a hub-reported
	// "unavailable" (node disconnected) fails at once with its own code.
	refused := &refusingAPI{API: h.b}
	r = run(t, e, &App{API: refused}, "start", "--node", "synth-mac", "--harness", "claude", "--cwd", "/home/synth/rel", "--title", "release-c")
	expectCode(t, r, ExitUnavailable)
	if refused.calls != 1 || countTitled(t, h.b, "release-c") != 0 {
		t.Fatalf("hub refusal replayed: calls=%d", refused.calls)
	}
}

// refusingAPI answers every start the way the hub does for a disconnected node.
type refusingAPI struct {
	c.API
	calls int
}

func (r *refusingAPI) StartSession(context.Context, c.StartSessionRequest) (c.StartSessionResponse, error) {
	r.calls++
	return c.StartSessionResponse{}, c.Errorf(c.ErrUnavailable, "node is disconnected; launch requires a connected node")
}

// connections verify reports each node's real result and exits non-zero
// unless every node verified it (scripts/outfit-sync.sh printed "verified"
// for a connection whose restore had failed: the command exited 0 for any
// state).
func TestConnectionsVerifyExitsNonZeroUnlessVerified(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	expectCode(t, run(t, e, nil, "connections", "verify", "github-synth", "--node", "synth-mac"), 0)
	r := run(t, e, nil, "--json", "connections", "verify", "github-synth", "--node", "synth-linux")
	expectCode(t, r, ExitError)
	if avs := mustJSON[[]c.ConnectionAvailability](t, r); len(avs) != 1 || avs[0].State == "verified" || avs[0].NodeID != "node_synth-linux" {
		t.Fatalf("verify on a node without it: %+v", avs)
	}
	if !strings.Contains(r.stderr, "not verified") {
		t.Fatalf("stderr: %s", r.stderr)
	}
	r = run(t, e, nil, "connections", "verify", "github-synth", "--node", "synth-linux")
	expectCode(t, r, ExitError)
	if !strings.Contains(r.stdout, "checked by pyred") {
		t.Fatalf("text output: %s", r)
	}
}
