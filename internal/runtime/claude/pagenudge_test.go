package claude

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/agentguide"
	"github.com/jhh3/pyre/internal/client"
	"github.com/jhh3/pyre/internal/clientconn"
	"github.com/jhh3/pyre/internal/contracts"
)

// fakeHubRole makes hubProjectRole answer role (or fail with err) for the
// test and counts the calls.
func fakeHubRole(t *testing.T, role *contracts.ProjectRole, err error) *int {
	t.Helper()
	calls := new(int)
	prev := hubProjectRole
	hubProjectRole = func(context.Context) (*contracts.ProjectRole, error) {
		*calls++
		return role, err
	}
	t.Cleanup(func() { hubProjectRole = prev })
	return calls
}

// pageRun is a run in a project whose hub says it leads the project.
func pageRun(t *testing.T) string {
	t.Helper()
	dir := nudgeRun(t, true)
	t.Setenv(envProjectID, "prj_synthpage")
	t.Setenv(agentguide.EnvProjectLead, "")
	fakeHubRole(t, &contracts.ProjectRole{ProjectID: "prj_synthpage", LeadSessionID: "ses_synthself", Lead: true}, nil)
	return dir
}

func wantPageNudge(t *testing.T, out []byte) {
	t.Helper()
	var d map[string]string
	if json.Unmarshal(out, &d) != nil || d["decision"] != "block" || d["reason"] != PageNudgeText {
		t.Fatalf("want page nudge, got %q", out)
	}
}

func workTurn(t *testing.T, dir, prompt string) {
	t.Helper()
	hookJSON(t, dir, "UserPromptSubmit", map[string]any{"prompt": prompt})
	hookJSON(t, dir, "PreToolUse", map[string]any{"tool_name": "Edit", "tool_input": map[string]any{"file_path": "/tmp/synth.go"}})
}

func TestPageNudgeLeadOncePerTurnAndThrottled(t *testing.T) {
	dir := pageRun(t)
	workTurn(t, dir, "launch: build the synthetic prototype")
	wantPageNudge(t, stopHook(t, dir, "Built the synthetic prototype.", false))
	// The continued turn stops normally.
	if out := stopHook(t, dir, "Page checked.", true); out != nil {
		t.Fatalf("second stop in the turn: %q", out)
	}
	// Next turn (a Pyre message): reminded at most every 3 hours.
	workTurn(t, dir, "[pyre-msg:msg_synthnext] synthetic follow-up")
	if out := stopHook(t, dir, "Done.", false); out != nil {
		t.Fatalf("throttled: %q", out)
	}
	recs, _, _ := readEvents(dir, 0)
	if out := pageNudge(dir, hookRecord{Event: "Stop"}, map[string]any{}, time.Now().Add(pageStaleAfter+time.Minute)); out == nil {
		t.Fatalf("after 3h: no nudge (%d records)", len(recs))
	}
}

func TestPageNudgeSkips(t *testing.T) {
	// Updating the page this turn (MCP or CLI) satisfies it.
	for _, post := range []map[string]any{
		{"tool_name": "mcp__pyre__update_project_page", "tool_input": map[string]any{"content": "# synthetic"}},
		{"tool_name": "Bash", "tool_input": map[string]any{"command": "pyre project page set --file /tmp/synth-page.md"}},
	} {
		dir := pageRun(t)
		workTurn(t, dir, "launch: synthetic")
		hookJSON(t, dir, "PostToolUse", post)
		if out := stopHook(t, dir, "Done.", false); out != nil {
			t.Fatalf("after a page update (%v): %q", post["tool_name"], out)
		}
	}
	// No work this turn (only pyre tools).
	dir := pageRun(t)
	hookJSON(t, dir, "UserPromptSubmit", map[string]any{"prompt": "launch: synthetic"})
	hookJSON(t, dir, "PreToolUse", map[string]any{"tool_name": "mcp__pyre__inbox"})
	if out := stopHook(t, dir, "Nothing to do.", false); out != nil {
		t.Fatalf("no work: %q", out)
	}
	// Not in a project: the hub is not even asked.
	dir = pageRun(t)
	calls := fakeHubRole(t, &contracts.ProjectRole{Lead: true}, nil)
	t.Setenv(envProjectID, "")
	workTurn(t, dir, "launch: synthetic")
	if out := stopHook(t, dir, "Done.", false); out != nil || *calls != 0 {
		t.Fatalf("no project: %q (%d hub calls)", out, *calls)
	}
}

// A delegated lead (started by a session outside its project) owns the
// page and is nudged like a top-level lead when the hub says it leads.
func TestPageNudgeDelegatedLead(t *testing.T) {
	dir := pageRun(t)
	t.Setenv(agentguide.EnvParentSessionID, "ses_synthcoordinator")
	t.Setenv(agentguide.EnvProjectLead, "1")
	workTurn(t, dir, "[pyre-msg:msg_synthbrief] synthetic ticket")
	wantPageNudge(t, stopHook(t, dir, "Researched the synthetic ticket.", false))
}

// iss_…: launched as a delegated lead (its parent
// was outside the project), but the parent joined the project afterwards,
// so the hub says the parent leads and refuses this session's page writes.
// The live answer wins over the launch flag: no nudge.
func TestPageNudgeSuppressedWhenHubSaysAnotherLeads(t *testing.T) {
	dir := pageRun(t)
	t.Setenv(agentguide.EnvParentSessionID, "ses_synthparent")
	t.Setenv(agentguide.EnvProjectLead, "1")
	calls := fakeHubRole(t, &contracts.ProjectRole{ProjectID: "prj_synthpage", LeadSessionID: "ses_synthparent"}, nil)
	workTurn(t, dir, "[pyre-msg:msg_synthbrief] synthetic cleanup")
	if out := stopHook(t, dir, "Cleaned up.", false); out != nil {
		t.Fatalf("another session leads: %q", out)
	}
	if *calls != 1 {
		t.Fatalf("hub calls %d, want 1", *calls)
	}
	// The answer is cached: the next worked turn does not ask again.
	workTurn(t, dir, "[pyre-msg:msg_synthmore] more")
	if out := stopHook(t, dir, "Done.", false); out != nil || *calls != 1 {
		t.Fatalf("cached answer: %q (%d hub calls)", out, *calls)
	}
	// Once the cache is stale the hub is asked again; a session that now
	// leads (the parent left the project) is nudged.
	fakeHubRole(t, &contracts.ProjectRole{ProjectID: "prj_synthpage", LeadSessionID: "ses_synthself", Lead: true}, nil)
	if out := pageNudge(dir, hookRecord{Event: "Stop"}, map[string]any{}, time.Now().Add(pageRoleTTL+time.Minute)); out == nil {
		t.Fatal("leads after the cache expired: no nudge")
	}
}

// A top-level session with no launch lead flag is nudged when the hub says
// it leads (no parent: the hub's rule).
func TestPageNudgeShownWhenHubSaysThisSessionLeads(t *testing.T) {
	dir := pageRun(t)
	calls := fakeHubRole(t, &contracts.ProjectRole{ProjectID: "prj_synthpage", LeadSessionID: "ses_synthself", Lead: true}, nil)
	workTurn(t, dir, "launch: synthetic")
	wantPageNudge(t, stopHook(t, dir, "Done.", false))
	if *calls != 1 {
		t.Fatalf("hub calls %d, want 1", *calls)
	}
}

// Fail quiet: when the hub cannot answer (unreachable, or an older hub
// without roles) there is no nudge, even for a top-level session; the
// failure is retried after pageRoleRetry, not on every stop.
func TestPageNudgeQuietWhenHubUnreachable(t *testing.T) {
	for name, fake := range map[string]struct {
		role *contracts.ProjectRole
		err  error
	}{
		"unreachable": {nil, contracts.Errorf(contracts.ErrUnavailable, "synthetic: hub unreachable")},
		"older hub":   {nil, nil},
	} {
		t.Run(name, func(t *testing.T) {
			dir := pageRun(t)
			calls := fakeHubRole(t, fake.role, fake.err)
			workTurn(t, dir, "launch: synthetic")
			if out := stopHook(t, dir, "Done.", false); out != nil {
				t.Fatalf("hub could not answer: %q", out)
			}
			workTurn(t, dir, "[pyre-msg:msg_synthnext] next")
			if out := stopHook(t, dir, "Done.", false); out != nil || *calls != 1 {
				t.Fatalf("retry too soon: %q (%d hub calls)", out, *calls)
			}
			fakeHubRole(t, &contracts.ProjectRole{ProjectID: "prj_synthpage", Lead: true}, nil)
			if out := pageNudge(dir, hookRecord{Event: "Stop"}, map[string]any{}, time.Now().Add(pageRoleRetry+time.Second)); out == nil {
				t.Fatal("hub back: no nudge")
			}
		})
	}
}

// iss_… (I63): the page has to be stale, not this
// session's own updates. A docs helper that leads alongside the build lead
// was told to rewrite the page 13 minutes after the build lead wrote it.
func TestPageNudgeQuietWhenAnotherSessionUpdatedThePage(t *testing.T) {
	recent := time.Now().Add(-13 * time.Minute)
	dir := pageRun(t)
	calls := fakeHubRole(t, &contracts.ProjectRole{ProjectID: "prj_synthpage", LeadSessionID: "ses_synthself", Lead: true,
		PageOwnerSessionID: "ses_synthself", PageOwner: true, PageUpdatedAt: &recent}, nil)
	workTurn(t, dir, "[pyre-msg:msg_synthhourly] synthetic docs pass")
	if out := stopHook(t, dir, "Docs checked.", false); out != nil {
		t.Fatalf("page updated 13 minutes ago by another session: %q", out)
	}
	if *calls != 1 {
		t.Fatalf("hub calls %d, want 1", *calls)
	}
	// Once nobody has written it for pageStaleAfter, its owner is nudged.
	stale := time.Now().Add(-pageStaleAfter - time.Minute)
	dir = pageRun(t)
	fakeHubRole(t, &contracts.ProjectRole{ProjectID: "prj_synthpage", LeadSessionID: "ses_synthself", Lead: true,
		PageOwnerSessionID: "ses_synthself", PageOwner: true, PageUpdatedAt: &stale}, nil)
	workTurn(t, dir, "[pyre-msg:msg_synthhourly2] synthetic docs pass")
	wantPageNudge(t, stopHook(t, dir, "Docs checked.", false))
}

// With two live leads and a stale page, only the page owner the hub names
// is nudged; the other lead is not.
func TestPageNudgeOnlyThePageOwnerAmongLeads(t *testing.T) {
	stale := time.Now().Add(-5 * time.Hour)
	for _, tc := range []struct {
		name  string
		owner bool
	}{{"page owner", true}, {"co-lead", false}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := pageRun(t)
			t.Setenv(agentguide.EnvParentSessionID, "ses_synthcoordinator")
			t.Setenv(agentguide.EnvProjectLead, "1")
			ownerID := "ses_synthbuildlead"
			if tc.owner {
				ownerID = "ses_synthself"
			}
			fakeHubRole(t, &contracts.ProjectRole{ProjectID: "prj_synthpage", LeadSessionID: "ses_synthself", Lead: true,
				PageOwnerSessionID: ownerID, PageOwner: tc.owner, CoLeads: []string{"ses_synthother"}, PageUpdatedAt: &stale}, nil)
			workTurn(t, dir, "[pyre-msg:msg_synthwork] synthetic work")
			out := stopHook(t, dir, "Done.", false)
			if tc.owner {
				wantPageNudge(t, out)
			} else if out != nil {
				t.Fatalf("a co-lead that does not keep the page was nudged: %q", out)
			}
		})
	}
}

// An answer that would nudge is reused only briefly: another lead may have
// written the page since. One that would not is reused for pageRoleTTL.
func TestPageRoleCacheRefreshesAnswerThatWouldNudge(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	calls := fakeHubRole(t, &contracts.ProjectRole{ProjectID: "prj_synthpage", LeadSessionID: "ses_synthself", Lead: true,
		PageOwnerSessionID: "ses_synthself", PageOwner: true}, nil)
	if !ownsStalePage(dir, now) || !ownsStalePage(dir, now.Add(30*time.Second)) || *calls != 1 {
		t.Fatalf("first answer: %d hub calls", *calls)
	}
	fresh := now.Add(2 * time.Minute)
	calls = fakeHubRole(t, &contracts.ProjectRole{ProjectID: "prj_synthpage", LeadSessionID: "ses_synthself", Lead: true,
		PageOwnerSessionID: "ses_synthself", PageOwner: true, PageUpdatedAt: &fresh}, nil)
	if ownsStalePage(dir, now.Add(pageRoleRetry+3*time.Minute)) || *calls != 1 {
		t.Fatalf("after pageRoleRetry the hub is asked again and the page is current: %d hub calls", *calls)
	}
	if ownsStalePage(dir, now.Add(pageRoleRetry+8*time.Minute)) || *calls != 1 {
		t.Fatalf("a no-nudge answer is reused for pageRoleTTL: %d hub calls", *calls)
	}
}

func TestPageNudgeYieldsToAskNudge(t *testing.T) {
	dir := pageRun(t)
	workTurn(t, dir, "launch: synthetic")
	wantNudge(t, stopHook(t, dir, nudgeQuestion, false))
	if out := stopHook(t, dir, "Filed the ask.", true); out != nil {
		t.Fatalf("second nudge in the turn: %q", out)
	}
}

// hubProjectRole talks only to the run's node socket with its session
// token, returns the hub's role, and treats a node-cache copy (hub down) or
// a missing socket as no answer.
func TestHubProjectRoleOverNodeSocket(t *testing.T) {
	sock := filepath.Join(shortTempDir(t), "pyred.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var gotAuth, gotPath string
	fromCache := false
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		if fromCache {
			w.Header().Set(client.FreshnessHeader, `{"observed_at":"2026-01-01T00:00:00Z","source":"node-cache:node_synth","stale":true}`)
		}
		_ = json.NewEncoder(w).Encode(contracts.LaunchContext{References: []contracts.ObjectRef{},
			Role: &contracts.ProjectRole{ProjectID: "prj_synthpage", LeadSessionID: "ses_synthparent"}})
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	t.Setenv(clientconn.EnvSocket, sock)
	t.Setenv(clientconn.EnvSessionToken, "synthetic-session-token")
	ctx := context.Background()
	role, err := hubProjectRole(ctx)
	if err != nil || role == nil || role.Lead || role.LeadSessionID != "ses_synthparent" {
		t.Fatalf("role %+v err %v", role, err)
	}
	if gotAuth != "Bearer synthetic-session-token" || gotPath != contracts.RPCPath(contracts.OpGetLaunchContext) {
		t.Fatalf("request: auth %q path %q", gotAuth, gotPath)
	}
	fromCache = true
	if role, err := hubProjectRole(ctx); err == nil {
		t.Fatalf("node-cache copy accepted: %+v", role)
	}
	t.Setenv(clientconn.EnvSocket, filepath.Join(filepath.Dir(sock), "missing.sock"))
	fromCache = false
	if _, err := hubProjectRole(ctx); err == nil {
		t.Fatal("no daemon: no error")
	}
	t.Setenv(clientconn.EnvSocket, "")
	if _, err := hubProjectRole(ctx); err == nil {
		t.Fatal("no socket: no error")
	}
}

// shortTempDir is a temp dir short enough for a unix socket path.
func shortTempDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "pyre-pn-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}
