package daemon

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/daemon/daemontest"
)

// aideArgs are the extra arguments of the Mac mini "Aide" session
// (synthetic path).
var aideArgs = []string{"--dangerously-load-development-channels", "plugin:sample-bridge@sample",
	"--append-system-prompt-file", "/Users/synthetic/Documents/sample/autonomy/aide.md"}

var aideCanonical = []string{"--dangerously-load-development-channels=plugin:sample-bridge@sample",
	"--append-system-prompt-file=/Users/synthetic/Documents/sample/autonomy/aide.md"}

// A session's extra harness arguments are validated, recorded on the run
// and given to the adapter; a resume (no args of its own) and an account
// switch (a resume on another account) relaunch with the same arguments.
func TestHarnessArgsKeptOnResumeAndAccountSwitch(t *testing.T) {
	w := daemontest.NewWorld()
	fa := daemontest.New(w, contracts.HarnessClaude)
	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{fa}, Accounts: &fakeBinder{}})
	cwd := t.TempDir()

	sc := startCmd("ses_aide", "run_m1", contracts.HarnessClaude, cwd)
	sc.Args = append([]string{"--pyre-permissions=bypass"}, aideArgs...)
	r1 := n.start(sc)
	if r1.AdapterData[adapterDataPermissionsCustom] != "" {
		t.Fatalf("aide args are not custom permissions: %v", r1.AdapterData)
	}
	if r1.AdapterData[adapterDataHarnessArgs] == "" {
		t.Fatalf("args not recorded: %v", r1.AdapterData)
	}
	p, _ := w.Proc("run_m1")
	if !slices.Equal(p.Launch.Args, aideCanonical) {
		t.Fatalf("first launch args %q", p.Launch.Args)
	}

	// pyre resume: the hub sends no args; the previous run's are reused.
	n.do(command(contracts.CmdStop, contracts.RunCommand{SessionID: "ses_aide", RunID: "run_m1"}))
	prev := n.run("run_m1").Run
	rc := contracts.ResumeCommand{StartRunCommand: startCmd("ses_aide", "run_m2", contracts.HarnessClaude, cwd)}
	rc.PreviousRun = &prev
	if res := n.do(command(contracts.CmdResume, rc)); res.Error != nil {
		t.Fatalf("resume: %+v", res.Error)
	}
	n.d.WaitLaunches()
	p2, _ := w.Proc("run_m2")
	r2 := n.run("run_m2").Run
	if !slices.Equal(p2.Launch.Args, aideCanonical) || r2.AdapterData[adapterDataHarnessArgs] != r1.AdapterData[adapterDataHarnessArgs] {
		t.Fatalf("resume args %q data %q", p2.Launch.Args, r2.AdapterData[adapterDataHarnessArgs])
	}
	if r2.ContinuationKind != contracts.ContinuationNativeResume || r2.AdapterData[adapterDataPermissions] != PermissionsBypass {
		t.Fatalf("resume = %+v", r2)
	}

	// Account switch (fallback/rebalance): a resume on another account.
	n.do(command(contracts.CmdStop, contracts.RunCommand{SessionID: "ses_aide", RunID: "run_m2"}))
	newRun, _, err := n.d.StartContinuation(context.Background(), ContinuationRequest{SessionID: "ses_aide", PreviousRunID: "run_m2",
		AccountID: "acct_1", Kind: contracts.ContinuationNativeResume, Prompt: "continue"})
	if err != nil {
		t.Fatal(err)
	}
	n.d.WaitLaunches()
	p3, _ := w.Proc(newRun)
	if !slices.Equal(p3.Launch.Args, aideCanonical) {
		t.Fatalf("account switch args %q", p3.Launch.Args)
	}

	// Disallowed arguments are refused before anything starts.
	bad := startCmd("ses_bad", "run_bad", contracts.HarnessClaude, cwd)
	bad.Args = []string{"--resume", "00000000-0000-4000-8000-000000000000"}
	if res := n.do(command(contracts.CmdStartRun, bad)); res.Error == nil || res.Error.Code != contracts.ErrInvalid {
		t.Fatalf("--resume accepted: %+v", res)
	}
	if _, ok := w.Proc("run_bad"); ok {
		t.Fatal("refused launch started a process")
	}
}

// locatingAdapter finds every native thread on this node (the Claude
// adapter searches its config homes).
type locatingAdapter struct {
	contracts.RuntimeAdapter
	found, busy bool
}

func (l *locatingAdapter) NativeThreadOnNode(prev contracts.Run) bool { return l.found }
func (l *locatingAdapter) ConversationInUse(cwd, sid string) (bool, string) {
	return l.busy, "synthetic: written just now"
}

// pyre start --native-resume: the new session's first run is a native
// resume of a conversation started outside Pyre, with the session's extra
// arguments; a conversation not on the node is refused before launch.
func TestNativeResumeAdoptsConversation(t *testing.T) {
	w := daemontest.NewWorld()
	la := &locatingAdapter{RuntimeAdapter: daemontest.New(w, contracts.HarnessClaude), found: true}
	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{la}, Accounts: &fakeBinder{}})
	cwd := t.TempDir()
	sid := "0b7c1f3e-1111-4222-8333-444455556666"
	sc := startCmd("ses_adopt", "run_a1", contracts.HarnessClaude, cwd)
	sc.Args = append([]string{NativeResumeArg + "=" + sid}, aideArgs...)
	r := n.start(sc)
	p, _ := w.Proc("run_a1")
	if p.Launch.Resume == nil || p.Launch.Resume.ID != sid || p.Launch.Resume.Kind != "claude_session" {
		t.Fatalf("launch resume = %+v", p.Launch.Resume)
	}
	if r.ContinuationKind != contracts.ContinuationNativeResume || r.PreviousRunID != "" || !slices.Equal(p.Launch.Args, aideCanonical) {
		t.Fatalf("run = %+v args %q", r, p.Launch.Args)
	}

	la.busy = true
	sc = startCmd("ses_adoptb", "run_ab", contracts.HarnessClaude, cwd)
	sc.Args = []string{NativeResumeArg + "=" + sid}
	if res := n.do(command(contracts.CmdStartRun, sc)); res.Error == nil || res.Error.Code != contracts.ErrConflict {
		t.Fatalf("live conversation adopted: %+v", res)
	}
	la.busy = false

	la.found = false
	sc = startCmd("ses_adopt2", "run_a2", contracts.HarnessClaude, cwd)
	sc.Args = []string{NativeResumeArg + "=" + sid}
	if res := n.do(command(contracts.CmdStartRun, sc)); res.Error == nil || res.Error.Code != contracts.ErrNotFound {
		t.Fatalf("missing conversation: %+v", res)
	}
	sc = startCmd("ses_adopt3", "run_a3", contracts.HarnessClaude, cwd)
	sc.Args = []string{NativeResumeArg + "=not-a-uuid"}
	if res := n.do(command(contracts.CmdStartRun, sc)); res.Error == nil || res.Error.Code != contracts.ErrInvalid {
		t.Fatalf("bad id: %+v", res)
	}
}

// A session's --settings file is read on the node: one that would override
// the account binding, permissions or hooks is refused before launch.
func TestSessionSettingsChecked(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ok := write("ok.json", `{"theme":"dark","permissions":{"allow":["Read"]}}`)
	if _, err := runHarnessArgs(contracts.HarnessClaude, []string{"--settings", ok}, nil); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"env":{"ANTHROPIC_API_KEY":"x"}}`, `{"apiKeyHelper":"/bin/echo"}`, `{"permissions":{"defaultMode":"bypassPermissions"}}`, `{"disableAllHooks":true}`, `[1]`} {
		p := write("bad.json", body)
		if _, err := runHarnessArgs(contracts.HarnessClaude, []string{"--settings=" + p}, nil); err == nil || !contracts.IsCode(err, contracts.ErrInvalid) {
			t.Errorf("%s accepted: %v", body, err)
		}
	}
	if _, err := runHarnessArgs(contracts.HarnessClaude, []string{"--settings", filepath.Join(dir, "missing.json")}, nil); err == nil {
		t.Error("missing settings file accepted")
	}
}

func TestPermissionsCustomRecorded(t *testing.T) {
	r := contracts.Run{Harness: contracts.HarnessClaude, AdapterData: map[string]string{}}
	recordHarnessArgs(&r, []string{"--plugin-dir=/p", "--append-system-prompt-file=/f"})
	if r.AdapterData[adapterDataPermissionsCustom] != "--plugin-dir" {
		t.Fatalf("custom = %q", r.AdapterData[adapterDataPermissionsCustom])
	}
}
