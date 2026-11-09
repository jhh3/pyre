package daemon

import (
	"context"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/daemon/daemontest"
)

// An account switch resumes the session with its existing credential (the
// hub mints session credentials only for its own start and resume
// commands): the successor's process gets it as PYRE_SESSION_TOKEN, which
// its MCP server, hooks and CLI inherit, and the local socket still
// resolves it to the session after the stopped run's end. The hub keeps
// the credential valid across the switch (hub archivewhen.go,
// iss_…).
func TestAccountSwitchSuccessorKeepsSessionCredential(t *testing.T) {
	w := daemontest.NewWorld()
	fa := daemontest.New(w, contracts.HarnessClaude)
	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{fa}, Accounts: &fakeBinder{}})
	const cred = "synthetic-session-credential-switch"
	sc := startCmd("ses_sw", "run_sw1", contracts.HarnessClaude, t.TempDir())
	sc.SessionToken = cred
	n.start(sc)
	if p, _ := w.Proc("run_sw1"); p.Launch.Env["PYRE_SESSION_TOKEN"] != cred {
		t.Fatal("first run lacks the session credential")
	}

	n.do(command(contracts.CmdStop, contracts.RunCommand{SessionID: "ses_sw", RunID: "run_sw1"}))
	next, _, err := n.d.StartContinuation(context.Background(), ContinuationRequest{SessionID: "ses_sw", PreviousRunID: "run_sw1",
		AccountID: "acct_1", Kind: contracts.ContinuationNativeResume, Prompt: "continue"})
	if err != nil {
		t.Fatal(err)
	}
	n.d.WaitLaunches()
	p, _ := w.Proc(next)
	if p.Launch.Env["PYRE_SESSION_TOKEN"] != cred || p.Launch.Env["PYRE_RUN_ID"] != next {
		t.Fatalf("successor env: credential carried %v, run %q", p.Launch.Env["PYRE_SESSION_TOKEN"] == cred, p.Launch.Env["PYRE_RUN_ID"])
	}
	_ = n.d.st.pruneSecrets(context.Background()) // the stopped run's row may go; the successor's stays
	la := &localAuth{d: n.d, cache: map[string]cachedPrincipal{}}
	pr, err := la.Authenticate(context.Background(), cred)
	if err != nil || pr.Kind != contracts.PrincipalSession || pr.ID != "ses_sw" {
		t.Fatalf("local socket refuses the successor's credential: %v %+v", err, pr)
	}
}
