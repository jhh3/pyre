package cli

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/client"
	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/httpapi"
)

// Artifact commands accept a title (unique; several matches are a conflict).
func TestArtifactCommandsAcceptTitle(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	dir := t.TempDir()
	f1, f2 := filepath.Join(dir, "v1.txt"), filepath.Join(dir, "v2.txt")
	os.WriteFile(f1, []byte("synthetic v1\n"), 0o600)
	os.WriteFile(f2, []byte("synthetic v2\n"), 0o600)

	r := run(t, e, nil, "--json", "publish", f1, "--title", "Synthetic Title Report")
	expectCode(t, r, 0)
	v1 := mustJSON[c.PublishReceipt](t, r)
	// A new version by title.
	r = run(t, e, nil, "--json", "publish", f2, "--artifact", "Synthetic Title Report")
	expectCode(t, r, 0)
	v2 := mustJSON[c.PublishReceipt](t, r)
	if v2.Artifact.ArtifactID != v1.Artifact.ArtifactID || v2.Version.Seq != 2 {
		t.Fatalf("publish --artifact <title>: %+v", v2.Version)
	}
	r = run(t, e, nil, "--json", "artifact", "versions", "synthetic title report") // case-insensitive
	expectCode(t, r, 0)
	if p := mustJSON[c.Page[c.ArtifactVersion]](t, r); len(p.Items) != 2 {
		t.Fatalf("versions by title: %+v", p.Items)
	}
	// show/get/preview on a title use the latest version.
	r = run(t, e, nil, "--json", "artifact", "show", "Synthetic Title Report")
	expectCode(t, r, 0)
	if v := mustJSON[c.ArtifactVersion](t, r); v.VersionID != v2.Version.VersionID {
		t.Fatalf("show by title: %+v", v)
	}
	out := filepath.Join(dir, "got.txt")
	expectCode(t, run(t, e, nil, "artifact", "get", "Synthetic Title Report", "-o", out), 0)
	if b, _ := os.ReadFile(out); string(b) != "synthetic v2\n" {
		t.Fatalf("get by title: %q", b)
	}
	r = run(t, e, nil, "--json", "artifact", "preview", "Synthetic Title Report")
	expectCode(t, r, 0)
	if pu := mustJSON[c.PreviewURL](t, r); !strings.Contains(pu.URL, "/preview/"+v2.Version.VersionID+"/") {
		t.Fatalf("preview: %+v", pu)
	}
	r = run(t, e, nil, "artifact", "preview", v1.Version.VersionID)
	expectCode(t, r, 0)
	if !strings.Contains(r.stdout, "/preview/"+v1.Version.VersionID+"/") {
		t.Fatalf("preview text: %s", r)
	}
	// IDs still work; an unknown title is not found; a duplicate title is a conflict.
	expectCode(t, run(t, e, nil, "artifact", "versions", v1.Artifact.ArtifactID), 0)
	expectCode(t, run(t, e, nil, "artifact", "versions", "no such synthetic title"), ExitNotFound)
	expectCode(t, run(t, e, nil, "publish", f1, "--title", "Synthetic Title Report"), 0)
	r = run(t, e, nil, "artifact", "versions", "Synthetic Title Report")
	expectCode(t, r, ExitConflict)
	if !strings.Contains(r.stderr, v1.Artifact.ArtifactID) {
		t.Fatalf("conflict should list the candidates: %s", r)
	}
}

func TestAttentionShowAndConnectionsCheck(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	items, err := h.b.ListAttention(c.WithPrincipal(context.Background(), testOwner), c.ListAttentionRequest{})
	if err != nil || len(items.Items) == 0 {
		t.Fatalf("fixture attention: %v", err)
	}
	id := items.Items[0].AttentionID
	r := run(t, e, nil, "--json", "attention", "show", id)
	expectCode(t, r, 0)
	if it := mustJSON[c.AttentionItem](t, r); it.AttentionID != id {
		t.Fatalf("attention show: %+v", it)
	}
	expectCode(t, run(t, e, nil, "attention", "show", "att_synth-missing"), ExitNotFound)

	want, err := h.b.CheckConnectionGrant(c.WithPrincipal(context.Background(), testOwner), c.CheckGrantRequest{ConnectionID: "conn_synth-github", SessionID: "ses_synth-b"})
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"conn_synth-github", "github-synth"} {
		r = run(t, e, nil, "--json", "connections", "check", ref, "--session", "ses_synth-b")
		expectCode(t, r, 0)
		if d := mustJSON[c.GrantDecision](t, r); d != want {
			t.Fatalf("check %s: %+v want %+v", ref, d, want)
		}
	}
	r = run(t, e, nil, "connections", "check", "github-synth", "--session", "ses_synth-b")
	expectCode(t, r, 0)
	if !strings.Contains(r.stdout, want.Reason) {
		t.Fatalf("text output: %s", r)
	}
	expectCode(t, run(t, e, nil, "connections", "check", "github-synth"), ExitUsage)
}

func TestProjectUpdateCheckout(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	r := run(t, e, nil, "--json", "project", "new", "synthetic-checkouts")
	expectCode(t, r, 0)
	p := mustJSON[c.Project](t, r)
	r = run(t, e, nil, "--json", "project", "update", p.ProjectID,
		"--checkout", "node_synth-mac:/srv/synth/repo:app:worktree:main",
		"--checkout", "node_synth-linux:/home/synth/app:app:shared")
	expectCode(t, r, 0)
	got := mustJSON[c.Project](t, r)
	want := []c.CheckoutBinding{
		{NodeID: "node_synth-mac", Path: "/srv/synth/repo", RepoID: "app", Mode: c.CheckoutWorktree, Branch: "main"},
		{NodeID: "node_synth-linux", Path: "/home/synth/app", RepoID: "app", Mode: c.CheckoutShared},
	}
	if len(got.Checkouts) != 2 || got.Checkouts[0] != want[0] || got.Checkouts[1] != want[1] {
		t.Fatalf("checkouts: %+v", got.Checkouts)
	}
	for _, bad := range []string{"node_synth-mac:/x", "node_synth-mac:/x:app:sideways", "node_synth-mac:relative:app:shared"} {
		expectCode(t, run(t, e, nil, "project", "update", p.ProjectID, "--checkout", bad), ExitUsage)
	}
}

// The bootstrap dry run carries the TLS pin from the client config, so the
// printed pyred enroll step works against a self-signed hub.
func TestEnrollBootstrapDryRunIncludesConfigPin(t *testing.T) {
	h := newHub(t)
	auth := httpapi.NewStaticTokens(map[string]c.Principal{ownerToken: testOwner})
	srv := httptest.NewTLSServer(httpapi.NewServer(h.b, auth, httpapi.Options{}))
	t.Cleanup(srv.Close)
	pin := client.CertFingerprint(srv.Certificate().Raw)
	e := newEnv(t, nil)
	if err := SaveConfig(e[EnvConfig], ClientConfig{Version: 1, HubURL: srv.URL, HubCertSHA256: pin, ClientID: testOwner.ID, Token: ownerToken}); err != nil {
		t.Fatal(err)
	}
	r := run(t, e, nil, "enroll-node", "--name", "synth-new", "--ssh", "synth-new", "--bootstrap", "--pyred", "/opt/synthetic/pyred")
	expectCode(t, r, 0)
	if !strings.Contains(r.stdout, "--pin "+pin) || !strings.Contains(r.stdout, srv.URL) {
		t.Fatalf("dry run lacks the config pin/hub: %s", r)
	}
}

func TestNodesCommandShowsResult(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	res, err := h.b.InvokeNodeAction(c.WithPrincipal(context.Background(), testOwner), c.NodeActionRequest{Mutation: c.Mutation{OperationID: "op_synth-inv"}, NodeID: "node_synth-mac", Action: c.ActionInventory})
	if err != nil {
		t.Fatal(err)
	}
	r := run(t, e, nil, "--json", "nodes", "command", res.CommandID)
	expectCode(t, r, 0)
	if got := mustJSON[c.NodeActionResult](t, r); got.CommandID != res.CommandID || got.Outcome != res.Outcome {
		t.Fatalf("nodes command: %+v", got)
	}
	expectCode(t, run(t, e, nil, "nodes", "command", "cmd_synth-missing"), ExitNotFound)
}

// Onboarding a rota account: hub record + node registration in one verb;
// registering the same rota account on a second node reuses the record.
func TestAccountsRegisterCheckoutsCheckin(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	r := run(t, e, nil, "--json", "accounts", "register", "--node", "node_synth-mac", "--provider", "anthropic", "--rota", "synth-max", "--label", "Synthetic Max", "--independent")
	expectCode(t, r, 0)
	res := mustJSON[AccountRegistration](t, r)
	if res.Account.AccountID == "" || res.Account.RotaRef != "synth-max" || res.Account.Label != "Synthetic Max" || !res.Account.PerMachineIndependent ||
		res.Account.ProviderRef != "synthetic-identity-synth-max" || res.NodeID != "node_synth-mac" || !res.Verified {
		t.Fatalf("register: %+v", res)
	}
	r = run(t, e, nil, "--json", "accounts", "register", "--node", "node_synth-linux", "--provider", "anthropic", "--rota", "synth-max")
	expectCode(t, r, 0)
	if again := mustJSON[AccountRegistration](t, r); again.Account.AccountID != res.Account.AccountID || again.Account.Label != "Synthetic Max" || !again.Account.PerMachineIndependent {
		t.Fatalf("second node reused account: %+v", again)
	}
	expectCode(t, run(t, e, nil, "accounts", "register", "--node", "node_synth-mac", "--provider", "gemini", "--rota", "x"), ExitUsage)
	expectCode(t, run(t, e, nil, "accounts", "register", "--provider", "anthropic", "--rota", "x"), ExitUsage)

	r = run(t, e, nil, "--json", "accounts", "checkouts", "--node", "node_synth-mac")
	expectCode(t, r, 0)
	r = run(t, e, nil, "--json", "accounts", "checkin", res.Account.AccountID, "--node", "node_synth-mac")
	expectCode(t, r, 0)
	if !strings.Contains(r.stdout, "released") {
		t.Fatalf("checkin: %s", r)
	}
	// A label resolves to the account too.
	expectCode(t, run(t, e, nil, "accounts", "checkin", "Synthetic Max", "--node", "node_synth-mac"), 0)
}

// An unknown subcommand of a command group is a usage error, not help + 0.
func TestUnknownSubcommandIsUsageError(t *testing.T) {
	e := newEnv(t, newHub(t))
	for _, args := range [][]string{{"msg", "frobnicate"}, {"artifact", "nope"}, {"project", "bogus"}} {
		r := run(t, e, nil, args...)
		expectCode(t, r, ExitUsage)
		if !strings.Contains(r.stderr, "unknown command") {
			t.Fatalf("%v: %s", args, r)
		}
	}
	// The bare group still prints help successfully.
	expectCode(t, run(t, e, nil, "msg"), 0)
}

// `pyre start --account` accepts the account ID, a unique label or rota
// name, or a unique prefix of either (seen live: a label answered
// not_found); an ambiguous prefix is refused with the candidates.
func TestStartAccountByLabel(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	reg := func(rota, label string) AccountRegistration {
		r := run(t, e, nil, "--json", "accounts", "register", "--node", "node_synth-mac", "--provider", "anthropic", "--rota", rota, "--label", label)
		expectCode(t, r, 0)
		return mustJSON[AccountRegistration](t, r)
	}
	a := reg("synth-alpha@synthetic.invalid", "acct-alpha")
	b := reg("synth-beta@synthetic.invalid", "acct-beta")
	ops := &Ops{API: h.b}
	for ref, want := range map[string]string{
		a.Account.AccountID:            a.Account.AccountID,
		"acct-alpha":                   a.Account.AccountID,
		"ACCT-BETA":                    b.Account.AccountID,
		"synth-beta@synthetic.invalid": b.Account.AccountID,
		"synth-alpha":                  a.Account.AccountID, // email local part (prefix)
		"acct-b":                       b.Account.AccountID, // unique label prefix
	} {
		got, err := ops.accountRef(context.Background(), ref, c.ProviderAnthropic)
		if err != nil || got != want {
			t.Fatalf("%q -> %q (%v), want %q", ref, got, err, want)
		}
	}
	if _, err := ops.accountRef(context.Background(), "acct-", c.ProviderAnthropic); !c.IsCode(err, c.ErrConflict) {
		t.Fatalf("ambiguous prefix: %v", err)
	}
	if _, err := ops.accountRef(context.Background(), "nobody", c.ProviderAnthropic); !c.IsCode(err, c.ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	if _, err := ops.accountRef(context.Background(), "acct-alpha", c.ProviderOpenAI); !c.IsCode(err, c.ErrNotFound) {
		t.Fatalf("other provider: %v", err)
	}
	// Through the command.
	r := run(t, e, nil, "--json", "start", "--node", "node_synth-mac", "--harness", "claude", "--cwd", "/synthetic", "--account", "acct-beta")
	expectCode(t, r, 0)
}
