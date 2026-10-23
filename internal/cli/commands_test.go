package cli

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	c "github.com/jhh3/pyre/internal/contracts"
)

func TestJSONOutputIsTheContractType(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)

	r := run(t, e, nil, "--json", "nodes")
	expectCode(t, r, 0)
	nodes := mustJSON[c.Page[c.Node]](t, r)
	if len(nodes.Items) == 0 {
		t.Fatalf("no nodes: %s", r)
	}
	if r.stderr != "" {
		t.Fatalf("--json must keep stderr free of noise for a plain list: %q", r.stderr)
	}

	r = run(t, e, nil, "--json", "sessions", "show", "ses_synth-b")
	expectCode(t, r, 0)
	sv := mustJSON[c.SessionView](t, r)
	if sv.Session.SessionID != "ses_synth-b" || sv.Run == nil {
		t.Fatalf("bad session view: %+v", sv)
	}

	// Names resolve to IDs (IDs are discoverable, names are aliases).
	r = run(t, e, nil, "--json", "nodes", "show", "synth-linux")
	expectCode(t, r, 0)
	if n := mustJSON[c.Node](t, r); n.NodeID != "node_synth-linux" {
		t.Fatalf("name lookup: %+v", n)
	}

	// pyre ls == pyre sessions; standalone filter returns only projectless sessions.
	r = run(t, e, nil, "--json", "ls", "--standalone")
	expectCode(t, r, 0)
	for _, s := range mustJSON[c.Page[c.SessionView]](t, r).Items {
		if s.Session.ProjectID != nil && *s.Session.ProjectID != "" {
			t.Fatalf("--standalone returned project session %s", s.Session.SessionID)
		}
	}

	// Human output is not JSON and mentions the mock.
	r = run(t, e, nil, "hello")
	expectCode(t, r, 0)
	if !strings.Contains(r.stdout, MockBanner) {
		t.Fatalf("hello must show the mock banner: %s", r)
	}
	r = run(t, e, nil, "--json", "hello")
	expectCode(t, r, 0)
	if hr := mustJSON[c.HelloResponse](t, r); !hr.Mock {
		t.Fatalf("hello json: %+v", hr)
	}
	if !strings.Contains(r.stderr, MockBanner) {
		t.Fatalf("--json hello must put the mock banner on stderr: %s", r)
	}
}

func TestExitCodeMapping(t *testing.T) {
	cases := map[c.ErrorCode]int{
		c.ErrInvalid: ExitUsage, c.ErrUnauthorized: ExitError, c.ErrForbidden: ExitError,
		c.ErrNotFound: ExitNotFound, c.ErrConflict: ExitConflict, c.ErrExpired: ExitConflict,
		c.ErrResetRequired: ExitConflict, c.ErrUnsupported: ExitUnsupported, c.ErrProtocolMismatch: ExitUnsupported,
		c.ErrCapacityExhausted: ExitUnavailable, c.ErrUnavailable: ExitUnavailable,
		c.ErrOutcomeIndeterminate: ExitIndeterminate, c.ErrPayloadTooLarge: ExitError, c.ErrInternal: ExitError,
		c.ErrApprovalRequested: ExitPending,
	}
	for code, want := range cases {
		if got := ExitCodeFor(c.Errorf(code, "x")); got != want {
			t.Errorf("%s: exit %d, want %d", code, got, want)
		}
		wrapped := errors.Join(errors.New("context"), c.Errorf(code, "x"))
		if got := ExitCodeFor(wrapped); got != want {
			t.Errorf("wrapped %s: exit %d, want %d", code, got, want)
		}
	}
	if ExitCodeFor(errors.New("plain")) != ExitError || ExitCodeFor(nil) != ExitOK || ExitCodeFor(usagef("x")) != ExitUsage {
		t.Fatal("basic mapping")
	}
	if ExitCodeFor(outcomeStatus(c.OutcomeIndeterminate, "")) != ExitIndeterminate || outcomeStatus(c.OutcomeDelivered, "") != nil {
		t.Fatal("outcome mapping")
	}
}

func TestExitCodesEndToEnd(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)

	expectCode(t, run(t, e, nil, "sessions", "show", "ses_does-not-exist"), ExitNotFound)
	expectCode(t, run(t, e, nil, "nodes", "--no-such-flag"), ExitUsage)
	expectCode(t, run(t, e, nil, "no-such-command"), ExitUsage)
	expectCode(t, run(t, e, nil, "send"), ExitUsage)
	expectCode(t, run(t, e, nil, "start", "--node", "synth-mac", "--harness", "emacs"), ExitUsage)
	expectCode(t, run(t, e, nil, "reissue", "msg_synth-indeterminate"), ExitUsage)

	// JSON errors go to stderr as an ErrorBody; stdout stays empty.
	r := run(t, e, nil, "--json", "sessions", "show", "ses_does-not-exist")
	expectCode(t, r, ExitNotFound)
	if r.stdout != "" || !strings.Contains(r.stderr, `"code":"not_found"`) {
		t.Fatalf("json error shape: %s", r)
	}

	// Unreachable hub: unavailable (5).
	down := e.with(EnvHub, "http://127.0.0.1:1")
	expectCode(t, run(t, down, nil, "nodes"), ExitUnavailable)

	// No configuration at all: unavailable with guidance.
	none := newEnv(t, nil)
	r = run(t, none, nil, "nodes")
	expectCode(t, r, ExitUnavailable)
	if !strings.Contains(r.stderr, "pyre pair") {
		t.Fatalf("missing setup guidance: %s", r)
	}

	// Wrong credential: 1.
	expectCode(t, run(t, e.with(EnvToken, "wrong"), nil, "nodes"), ExitError)
}

// indeterminateAPI returns command receipts whose outcome is unknown.
type indeterminateAPI struct{ c.API }

func (indeterminateAPI) GetSession(ctx context.Context, req c.GetRequest) (c.SessionView, error) {
	return c.SessionView{Session: c.Session{SessionID: req.ID}}, nil
}

func (indeterminateAPI) StopSession(ctx context.Context, req c.SessionCommandRequest) (c.CommandReceipt, error) {
	return c.CommandReceipt{CommandID: "cmd_x", Accepted: true, Outcome: c.OutcomeIndeterminate, Detail: "node lost before ack"}, nil
}

func TestIndeterminateOutcomeExitCode(t *testing.T) {
	e := newEnv(t, nil)
	r := run(t, e, &App{API: indeterminateAPI{}}, "stop", "ses_x")
	expectCode(t, r, ExitIndeterminate)
	if !strings.Contains(r.stdout, "UNKNOWN") || !strings.Contains(r.stderr, "never blindly resend") {
		t.Fatalf("indeterminate must be explicit: %s", r)
	}
	r = run(t, e, &App{API: indeterminateAPI{}}, "--json", "stop", "ses_x")
	expectCode(t, r, ExitIndeterminate)
	if rc := mustJSON[c.CommandReceipt](t, r); rc.Outcome != c.OutcomeIndeterminate {
		t.Fatalf("receipt: %+v", rc)
	}
}

func TestMessagingRoundTripWithReplyAndWait(t *testing.T) {
	h := newHub(t)
	owner := newEnv(t, h)
	// Agent session B: session token, its own IDs in the environment.
	agentB := owner.with(EnvToken, "", EnvSessionToken, sessBToken, EnvSessionID, "ses_synth-b", EnvNodeID, "node_synth-linux")

	r := run(t, owner, nil, "--json", "send", "ses_synth-b", "please review ver_synth-report-2", "--request-ack", "--attach", "ver_synth-report-2")
	expectCode(t, r, 0)
	sent := mustJSON[c.MessageView](t, r)
	if sent.Status.Storage != c.StorageStored || sent.Envelope.Recipient.ID != "ses_synth-b" || len(sent.Envelope.Attachments) != 1 {
		t.Fatalf("send: %+v", sent)
	}
	if run(t, owner, nil, "send", "ses_synth-b", "x", "--attach", "./report.md").code != ExitUsage {
		t.Fatal("a file path must not be accepted as an attachment")
	}

	// The agent's inbox defaults to its own session.
	r = run(t, agentB, nil, "--json", "inbox", "--unacked")
	expectCode(t, r, 0)
	found := false
	for _, m := range mustJSON[c.Page[c.MessageView]](t, r).Items {
		if m.Envelope.Recipient.Kind != c.AddrSession || m.Envelope.Recipient.ID != "ses_synth-b" {
			t.Fatalf("inbox returned someone else's mail: %+v", m.Envelope.Recipient)
		}
		found = found || m.Envelope.MessageID == sent.Envelope.MessageID
	}
	if !found {
		t.Fatalf("message not in the agent's inbox: %s", r)
	}

	// Reply with only --reply-to: goes back to the sender (the owner).
	r = run(t, agentB, nil, "--json", "send", "--reply-to", sent.Envelope.MessageID, "looks good")
	expectCode(t, r, 0)
	reply := mustJSON[c.MessageView](t, r)
	if reply.Envelope.Recipient.Kind != c.AddrOwner || reply.Envelope.Sender.ID != "ses_synth-b" || reply.Envelope.InReplyTo != sent.Envelope.MessageID {
		t.Fatalf("reply routing: %+v", reply.Envelope)
	}

	r = run(t, owner, nil, "--json", "wait-reply", sent.Envelope.MessageID, "--timeout", "5")
	expectCode(t, r, 0)
	wr := mustJSON[WaitReplyResult](t, r)
	if wr.Reply == nil || wr.Reply.Envelope.MessageID != reply.Envelope.MessageID || wr.Message.Status.Ack != c.AckReplied {
		t.Fatalf("wait-reply: %+v", wr)
	}

	// Human msg show keeps the facts separate.
	r = run(t, owner, nil, "msg", "show", sent.Envelope.MessageID)
	expectCode(t, r, 0)
	for _, want := range []string{"stored:", "runtime:", "acknowledged:  replied"} {
		if !strings.Contains(r.stdout, want) {
			t.Fatalf("msg show lacks %q: %s", want, r)
		}
	}

	// Waiting on a message nobody answers times out with exit 5.
	r = run(t, owner, nil, "wait-reply", "msg_synth-indeterminate", "--timeout", "1")
	expectCode(t, r, ExitUnavailable)
	if !strings.Contains(r.stderr, "INDETERMINATE") {
		t.Fatalf("timeout should show the latest delivery state: %s", r)
	}

	// Indeterminate deliveries are shown as unknown, with the safe actions.
	r = run(t, owner, nil, "msg", "show", "msg_synth-indeterminate")
	expectCode(t, r, 0)
	if !strings.Contains(r.stdout, "Do not resend") || !strings.Contains(r.stdout, "--accept-duplicate-risk") {
		t.Fatalf("indeterminate guidance missing: %s", r)
	}
	r = run(t, owner, nil, "--json", "reissue", "msg_synth-indeterminate", "--accept-duplicate-risk")
	expectCode(t, r, 0)
	if m := mustJSON[c.MessageView](t, r); m.Envelope.Reissues != "msg_synth-indeterminate" {
		t.Fatalf("reissue must reference the original: %+v", m.Envelope)
	}
}

func TestStartIsStandaloneAndRecordsParent(t *testing.T) {
	h := newHub(t)
	agentA := newEnv(t, h).with(EnvToken, "", EnvSessionToken, sessAToken, EnvSessionID, "ses_synth-a", EnvNodeID, "node_synth-mac")

	// No --node: defaults to the calling session's node. No project required.
	r := run(t, agentA, nil, "--json", "start", "--harness", "claude", "--cwd", "/home/synth/peer", "--prompt", "run the tests", "--title", "peer")
	expectCode(t, r, 0)
	res := mustJSON[c.StartSessionResponse](t, r)
	s := res.Session
	if s.Session.ProjectID != nil && *s.Session.ProjectID != "" {
		t.Fatalf("standalone launch got a project: %+v", s.Session)
	}
	if s.Session.Lineage.ParentSessionID != "ses_synth-a" {
		t.Fatalf("peer launch must record the parent session: %+v", s.Session.Lineage)
	}
	if s.Run == nil || s.Run.NodeID != "node_synth-mac" {
		t.Fatalf("default node: %+v", s.Run)
	}
	// --no-report opts out of automatic reports to the starting session.
	r = run(t, agentA, nil, "--json", "start", "--harness", "claude", "--cwd", "/home/synth/peer2", "--no-report")
	expectCode(t, r, 0)
	if tags := mustJSON[c.StartSessionResponse](t, r).Session.Session.Tags; !slices.Contains(tags, c.TagReportNone) {
		t.Fatalf("--no-report: tags %v", tags)
	}

	// The same operation ID returns the same session instead of launching twice.
	owner := newEnv(t, h)
	args := []string{"--json", "--operation-id", "op_test-start-1", "start", "--node", "synth-mac", "--harness", "claude", "--cwd", "/home/synth/x"}
	a1 := mustJSON[c.StartSessionResponse](t, run(t, owner, nil, args...))
	a2 := mustJSON[c.StartSessionResponse](t, run(t, owner, nil, args...))
	if a1.Session.Session.SessionID != a2.Session.Session.SessionID {
		t.Fatalf("retried start launched twice: %s vs %s", a1.Session.Session.SessionID, a2.Session.Session.SessionID)
	}

	// --account and --ambient conflict.
	expectCode(t, run(t, owner, nil, "start", "--node", "synth-mac", "--harness", "claude", "--cwd", "/x", "--account", "acct_a", "--ambient"), ExitUsage)
}

func TestApproveBindsRunAndValidatesOption(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	expectCode(t, run(t, e, nil, "approve", "apr_synth-pending"), ExitUsage)
	expectCode(t, run(t, e, nil, "approve", "apr_synth-pending", "--option", "maybe"), ExitUsage)
	r := run(t, e, nil, "--json", "approve", "apr_synth-pending", "--option", "allow")
	expectCode(t, r, 0)
	ap := mustJSON[c.Approval](t, r)
	if ap.State != c.ApprovalResolved || ap.Resolution == nil || ap.Resolution.OptionID != "allow" {
		t.Fatalf("approve: %+v", ap)
	}
	// A second, different answer is refused (only one resolution commits).
	r = run(t, e, nil, "approve", "apr_synth-pending", "--option", "deny")
	if r.code == 0 {
		t.Fatalf("second answer must not succeed: %s", r)
	}
}

func TestMemoryAndNodeActions(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	r := run(t, e, nil, "--json", "memory", "search", "brief", "--node", "synth-mac")
	expectCode(t, r, 0)
	mustJSON[c.MemorySearchResult](t, r)
	r = run(t, e, nil, "--json", "memory", "status", "--node", "synth-mac")
	expectCode(t, r, 0)
	mustJSON[c.MemoryStatus](t, r)
	r = run(t, e, nil, "--json", "update", "ls", "--node", "synth-mac")
	expectCode(t, r, 0)
	mustJSON[[]c.ToolVersion](t, r)
	r = run(t, e, nil, "--json", "inventory", "--node", "synth-mac")
	expectCode(t, r, 0)
	mustJSON[c.NodeInventory](t, r)
	// Several connected nodes and no session node: --node is required.
	expectCode(t, run(t, e, nil, "inventory"), ExitUsage)
}

func TestNoArgsLaunchesTUIOnlyOnATerminal(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	called := false
	app := &App{IsTerminal: func() bool { return true }, RunTUI: func(ctx context.Context, api c.API, opts TUIOptions) error {
		called = api != nil
		return nil
	}}
	expectCode(t, run(t, e, app, []string{}...), 0)
	if !called {
		t.Fatal("pyre with no args on a TTY must launch the TUI with the resolved API")
	}
	r := run(t, e, &App{IsTerminal: func() bool { return false }})
	expectCode(t, r, 0)
	if !strings.Contains(r.stdout, "Usage:") {
		t.Fatalf("non-TTY should print help: %s", r)
	}
	// The default stub reports unsupported until WP15 lands.
	expectCode(t, run(t, e, &App{IsTerminal: func() bool { return true }, RunTUI: RunTUI}), ExitUnsupported)
}

// Main(nil) means "no arguments", not "the process arguments": cobra falls
// back to os.Args[1:] for nil args, which made the test above depend on how
// the test binary was invoked (`-test.parallel 2` -> unknown command "2").
func TestMainNilArgsIgnoresProcessArgs(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	old := os.Args
	os.Args = []string{"pyre", "synthetic-bogus-command"}
	t.Cleanup(func() { os.Args = old })
	r := run(t, e, &App{IsTerminal: func() bool { return false }})
	expectCode(t, r, 0)
	if !strings.Contains(r.stdout, "Usage:") {
		t.Fatalf("Main(nil) should print help: %s", r)
	}
}

func TestEnrollNodeBootstrapDryRunCreatesNothing(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	before, _ := h.b.ListNodes(context.Background(), c.ListNodesRequest{IncludeRevoked: true})
	r := run(t, e, nil, "enroll-node", "--name", "synth-new", "--ssh", "synth-new", "--bootstrap", "--hub-url", "https://hub.synthetic:7788", "--pyred", "/opt/synthetic/pyred")
	expectCode(t, r, 0)
	for _, want := range []string{"dry run", "ssh synth-new", "scp", "/opt/synthetic/pyred", "--execute"} {
		if !strings.Contains(r.stdout, want) {
			t.Fatalf("plan lacks %q: %s", want, r)
		}
	}
	if strings.Contains(r.stdout, "placeholder") {
		t.Fatalf("plan must redact the secret: %s", r)
	}
	after, _ := h.b.ListNodes(context.Background(), c.ListNodesRequest{IncludeRevoked: true})
	if len(after.Items) != len(before.Items) {
		t.Fatal("a dry run must not create an enrollment")
	}
	expectCode(t, run(t, e, nil, "enroll-node", "--name", "x", "--bootstrap"), ExitUsage)

	// Plain ticket: the secret is shown once, with a warning on stderr.
	r = run(t, e, nil, "--json", "enroll-node", "--name", "synth-new")
	expectCode(t, r, 0)
	if tk := mustJSON[c.EnrollmentTicket](t, r); tk.NodeID == "" || tk.Secret == "" {
		t.Fatalf("ticket: %+v", tk)
	}
	if !strings.Contains(r.stderr, "shown once") {
		t.Fatalf("missing one-time warning: %s", r)
	}
	r = run(t, e, nil, "--json", "pair-client", "--name", "phone", "--kind", "android")
	expectCode(t, r, 0)
	if pt := mustJSON[c.PairingTicket](t, r); !strings.HasPrefix(pt.URI, "pyre://pair?") {
		t.Fatalf("pairing ticket: %+v", pt)
	}
}

// The owner's `pyre inbox` lists mail still waiting on him; --all adds
// updates and handled mail; `pyre msg show` reads (acknowledges) one.
func TestOwnerInboxListsWhatWaits(t *testing.T) {
	h := newHub(t)
	owner := newEnv(t, h)
	agentB := owner.with(EnvToken, "", EnvSessionToken, sessBToken, EnvSessionID, "ses_synth-b", EnvNodeID, "node_synth-linux")
	r := run(t, agentB, nil, "--json", "send", "owner", "Synthetic digest: merged two PRs. Nothing urgent.")
	expectCode(t, r, 0)
	upd := mustJSON[c.MessageView](t, r)
	if upd.Status.AckRef != c.AckRefUpdate {
		t.Fatalf("update: %+v", upd.Status)
	}
	r = run(t, agentB, nil, "--json", "send", "owner", "Should I merge the synthetic PR?")
	expectCode(t, r, 0)
	q := mustJSON[c.MessageView](t, r)

	ids := func(args ...string) map[string]bool {
		t.Helper()
		r := run(t, owner, nil, append([]string{"--json", "inbox"}, args...)...)
		expectCode(t, r, 0)
		out := map[string]bool{}
		for _, m := range mustJSON[c.Page[c.MessageView]](t, r).Items {
			out[m.Envelope.MessageID] = true
		}
		return out
	}
	if got := ids(); !got[q.Envelope.MessageID] || got[upd.Envelope.MessageID] {
		t.Fatalf("owner inbox: %v", got)
	}
	if got := ids("--all"); !got[q.Envelope.MessageID] || !got[upd.Envelope.MessageID] {
		t.Fatalf("owner inbox --all: %v", got)
	}
	r = run(t, owner, nil, "inbox", "--all")
	expectCode(t, r, 0)
	if !strings.Contains(r.stdout, "update") {
		t.Fatalf("update not labelled:\n%s", r)
	}
	expectCode(t, run(t, owner, nil, "msg", "show", q.Envelope.MessageID), 0)
	if got := ids(); got[q.Envelope.MessageID] {
		t.Fatalf("a message he read still waits: %v", got)
	}
}
