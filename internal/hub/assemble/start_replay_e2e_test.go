package assemble

import (
	"bytes"
	"context"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/cli"
	"github.com/jhh3/pyre/internal/client"
	c "github.com/jhh3/pyre/internal/contracts"
)

// loseAnswers stores starts on the real hub but loses the first `lose`
// answers (<0: every answer), as a hub restart or timeout after the commit
// would.
type loseAnswers struct {
	c.API
	mu   sync.Mutex
	lose int
}

func (l *loseAnswers) StartSession(ctx context.Context, req c.StartSessionRequest) (c.StartSessionResponse, error) {
	res, err := l.API.StartSession(ctx, req)
	l.mu.Lock()
	defer l.mu.Unlock()
	if err == nil && l.lose != 0 {
		if l.lose > 0 {
			l.lose--
		}
		return c.StartSessionResponse{}, c.Errorf(c.ErrUnavailable, "hub request timed out: context deadline exceeded").WithDetail(client.DetailUnreachable, "hub")
	}
	return res, err
}

// A child started by a session in a project, without --project, joins the
// parent's project. Its replay (same operation ID) must return that session,
// not "operation_id reused with a different request body": the hub fills in
// the project before recording the operation on the first execution only.
// Covers the CLI replay of a lost answer and the exit-7 --operation-id rerun
// against the real hub.
func TestStartReplayOfChildInParentProject(t *testing.T) {
	st, n := newStack(t)
	owner := c.WithPrincipal(context.Background(), ownerP)
	op := func() c.Mutation { return c.Mutation{OperationID: c.NewID(c.PrefixOperation)} }
	p, err := st.Hub.CreateProject(owner, c.CreateProjectRequest{Mutation: op(), Name: "synthetic-replay"})
	if err != nil {
		t.Fatal(err)
	}
	lead, err := st.Hub.StartSession(owner, c.StartSessionRequest{Mutation: op(), NodeID: n.nodeID, Harness: c.HarnessMock, Cwd: "/tmp/synth", Title: "synthetic lead", ProjectID: &p.ProjectID})
	if err != nil {
		t.Fatal(err)
	}
	n.idle()
	leadID := lead.Session.Session.SessionID
	asLead := c.WithPrincipal(context.Background(), c.Principal{Kind: c.PrincipalSession, ID: leadID})

	// The hub itself: the identical request twice gives one session.
	req := c.StartSessionRequest{Mutation: op(), NodeID: n.nodeID, Harness: c.HarnessMock, Cwd: "/tmp/synth", Title: "synthetic child 0"}
	a, err := st.Hub.StartSession(asLead, req)
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.Hub.StartSession(asLead, req)
	if err != nil {
		t.Fatalf("replay of the identical child start: %v", err)
	}
	if a.Session.Session.SessionID != b.Session.Session.SessionID || a.Session.Session.ProjectID == nil || *a.Session.Session.ProjectID != p.ProjectID {
		t.Fatalf("replay: %s vs %s, project %v", a.Session.Session.SessionID, b.Session.Session.SessionID, a.Session.Session.ProjectID)
	}
	n.idle()

	count := func(title string) (int, *string) {
		t.Helper()
		page, err := st.Hub.ListSessions(owner, c.ListSessionsRequest{PageRequest: c.PageRequest{Limit: 200}})
		if err != nil {
			t.Fatal(err)
		}
		k := 0
		var pid *string
		for _, v := range page.Items {
			if v.Session.Title == title {
				k++
				pid = v.Session.ProjectID
			}
		}
		return k, pid
	}
	fast := &cli.StartRecovery{Delays: []time.Duration{time.Millisecond, time.Millisecond}, Total: 5 * time.Second}

	// The CLI layer replays a lost answer and returns the stored session.
	ops := &cli.Ops{API: &loseAnswers{API: st.Hub, lose: 1}, Self: cli.Identity{SessionID: leadID}, StartRecover: fast}
	res, err := ops.StartSession(asLead, cli.StartParams{Node: n.nodeID, Harness: "mock", Cwd: "/tmp/synth", Title: "synthetic child 1"})
	if err != nil {
		t.Fatalf("lost answer not recovered: %v", err)
	}
	n.idle()
	if k, pid := count("synthetic child 1"); k != 1 || pid == nil || *pid != p.ProjectID || res.Session.Session.Parent() != leadID {
		t.Fatalf("child 1: %d sessions, project %v, parent %q", k, pid, res.Session.Session.Parent())
	}

	// The hub stays silent: exit 7 names the operation ID; re-running with
	// --operation-id returns the one session the hub stored.
	getenv := func(k string) string {
		if k == cli.EnvSessionID {
			return leadID
		}
		return ""
	}
	main := func(api c.API, args ...string) (int, string) {
		var out, errb bytes.Buffer
		app := &cli.App{API: api, Stdout: &out, Stderr: &errb, Stdin: strings.NewReader(""), Getenv: getenv, StartRecover: fast}
		code := app.Main(asLead, args)
		return code, out.String() + errb.String()
	}
	args := []string{"start", "--node", n.nodeID, "--harness", "mock", "--cwd", "/tmp/synth", "--title", "synthetic child 2"}
	silent := &loseAnswers{API: st.Hub, lose: -1}
	code, out := main(silent, args...)
	if code != cli.ExitIndeterminate {
		t.Fatalf("silent hub: exit %d\n%s", code, out)
	}
	m := regexp.MustCompile(`--operation-id (op_[A-Za-z0-9_-]+)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no operation ID in:\n%s", out)
	}
	n.idle()
	code, out = main(st.Hub, append([]string{"--json", "--operation-id", m[1]}, args...)...)
	if code != 0 {
		t.Fatalf("rerun with --operation-id: exit %d\n%s", code, out)
	}
	n.idle()
	if k, pid := count("synthetic child 2"); k != 1 || pid == nil || *pid != p.ProjectID {
		t.Fatalf("child 2: %d sessions, project %v", k, pid)
	}
}
