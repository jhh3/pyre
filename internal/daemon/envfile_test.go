package daemon

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/daemon/daemontest"
	"github.com/jhh3/pyre/internal/envfile"
)

// aideSecret is a synthetic stand-in for the Aide bridge token.
const aideSecret = "synthetic-bridge-token-0001"

func writeEnvFile(t *testing.T, dir, body string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, "aide.env")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

// dbContains reports whether any column of any table in db holds s.
func dbContains(t *testing.T, db *sql.DB, s string) string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		tables = append(tables, n)
	}
	rows.Close()
	for _, tb := range tables {
		r, err := db.Query(`SELECT * FROM "` + tb + `"`)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := r.Columns()
		for r.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			_ = r.Scan(ptrs...)
			for _, v := range vals {
				if strings.Contains(fmt.Sprintf("%s", v), s) {
					r.Close()
					return tb
				}
			}
		}
		r.Close()
	}
	return ""
}

// pyre start --env-file: the node reads the file, applies its variables to
// the harness process (an empty auth key unsets it), records only the path,
// and every relaunch (resume, account switch) reads the file again.
func TestEnvFileAppliedRecordedByPathAndReread(t *testing.T) {
	w := daemontest.NewWorld()
	fa := daemontest.New(w, contracts.HarnessClaude)
	dir := t.TempDir()
	n := startNode(t, dir, Config{Adapters: []contracts.RuntimeAdapter{fa}, Accounts: &fakeBinder{}})
	cwd := t.TempDir()
	envPath := writeEnvFile(t, t.TempDir(), "AGENT_BRIDGE_TOKEN="+aideSecret+"\nAGENT_INTAKE_URL=http://127.0.0.1:8091\nANTHROPIC_API_KEY=\nCLAUDE_CODE_OAUTH_TOKEN=\n", 0o600)

	sc := startCmd("ses_aide", "run_e1", contracts.HarnessClaude, cwd)
	sc.Args = append([]string{EnvFileArg + "=" + envPath}, aideArgs...)
	r1 := n.start(sc)
	if r1.AdapterData[adapterDataEnvFile] != envPath {
		t.Fatalf("env file not recorded: %v", r1.AdapterData)
	}
	p, _ := w.Proc("run_e1")
	if p.Launch.Env["AGENT_BRIDGE_TOKEN"] != aideSecret || p.Launch.Env["AGENT_INTAKE_URL"] != "http://127.0.0.1:8091" {
		t.Fatalf("launch env missing the file's variables: keys %v", keysOf(p.Launch.Env))
	}
	if p.Launch.SessionEnv["AGENT_BRIDGE_TOKEN"] != aideSecret || len(p.Launch.SessionEnv) != 2 {
		t.Fatalf("session env keys %v", keysOf(p.Launch.SessionEnv))
	}
	if _, set := p.Launch.Env["ANTHROPIC_API_KEY"]; set || !slices.Contains(p.Launch.Unset, "ANTHROPIC_API_KEY") || !slices.Contains(p.Launch.Unset, "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Fatalf("empty auth keys not unset: unset %v", p.Launch.Unset)
	}
	if p.Launch.Env["CLAUDE_CONFIG_DIR"] != "/profiles/acct_1" || p.Launch.Env["PYRE_SESSION_ID"] != "ses_aide" {
		t.Fatal("Pyre's own variables lost")
	}
	if !slices.Equal(p.Launch.Args, aideCanonical) {
		t.Fatalf("args %q", p.Launch.Args)
	}

	// pyre resume: no env file of its own; the recorded path is read again
	// (the file's current values).
	if err := os.WriteFile(envPath, []byte("AGENT_BRIDGE_TOKEN="+aideSecret+"-rotated\nANTHROPIC_API_KEY=\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	n.do(command(contracts.CmdStop, contracts.RunCommand{SessionID: "ses_aide", RunID: "run_e1"}))
	prev := n.run("run_e1").Run
	rc := contracts.ResumeCommand{StartRunCommand: startCmd("ses_aide", "run_e2", contracts.HarnessClaude, cwd)}
	rc.PreviousRun = &prev
	if res := n.do(command(contracts.CmdResume, rc)); res.Error != nil {
		t.Fatalf("resume: %+v", res.Error)
	}
	n.d.WaitLaunches()
	p2, _ := w.Proc("run_e2")
	if p2.Launch.Env["AGENT_BRIDGE_TOKEN"] != aideSecret+"-rotated" || n.run("run_e2").Run.AdapterData[adapterDataEnvFile] != envPath {
		t.Fatalf("resume did not reread the env file")
	}

	// Account switch (fallback/rebalance): a resume on another account.
	n.do(command(contracts.CmdStop, contracts.RunCommand{SessionID: "ses_aide", RunID: "run_e2"}))
	newRun, _, err := n.d.StartContinuation(context.Background(), ContinuationRequest{SessionID: "ses_aide", PreviousRunID: "run_e2",
		AccountID: "acct_1", Kind: contracts.ContinuationNativeResume, Prompt: "continue"})
	if err != nil {
		t.Fatal(err)
	}
	n.d.WaitLaunches()
	p3, _ := w.Proc(newRun)
	if p3.Launch.Env["AGENT_BRIDGE_TOKEN"] != aideSecret+"-rotated" {
		t.Fatal("account switch lost the env file")
	}

	// The values never reach the node database, the reports to the hub or
	// run adapter data.
	if tb := dbContains(t, n.db, aideSecret); tb != "" {
		t.Fatalf("env file value stored in table %s", tb)
	}
	for _, id := range []string{"run_e1", "run_e2", newRun} {
		for k, v := range n.run(id).Run.AdapterData {
			if strings.Contains(v, aideSecret) {
				t.Fatalf("value in adapter data %s", k)
			}
		}
	}

	// A resume whose file became writable by others is refused.
	n.do(command(contracts.CmdStop, contracts.RunCommand{SessionID: "ses_aide", RunID: newRun}))
	if err := os.Chmod(envPath, 0o666); err != nil {
		t.Fatal(err)
	}
	prev = n.run(newRun).Run
	rc = contracts.ResumeCommand{StartRunCommand: startCmd("ses_aide", "run_e4", contracts.HarnessClaude, cwd)}
	rc.PreviousRun = &prev
	if res := n.do(command(contracts.CmdResume, rc)); res.Error == nil || res.Error.Code != contracts.ErrInvalid || strings.Contains(res.Error.Message, aideSecret) {
		t.Fatalf("writable env file accepted on resume: %+v", res)
	}
	if _, ok := w.Proc("run_e4"); ok {
		t.Fatal("refused resume started a process")
	}
}

// A refused file never starts anything, and its error never echoes a value.
func TestEnvFileRefusedBeforeLaunch(t *testing.T) {
	w := daemontest.NewWorld()
	fa := daemontest.New(w, contracts.HarnessClaude)
	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{fa}, Accounts: &fakeBinder{}})
	cwd := t.TempDir()
	for i, body := range []string{
		"ANTHROPIC_API_KEY=" + aideSecret + "\n",
		"PYRE_SESSION_TOKEN=" + aideSecret + "\n",
		"PATH=/bin\n",
		"HOME=/tmp\n",
		"CODEX_HOME=/tmp/" + aideSecret + "\n",
		aideSecret + "\n",
	} {
		p := writeEnvFile(t, t.TempDir(), body, 0o600)
		run := fmt.Sprintf("run_bad%d", i)
		sc := startCmd(fmt.Sprintf("ses_bad%d", i), run, contracts.HarnessClaude, cwd)
		sc.Args = []string{EnvFileArg + "=" + p}
		res := n.do(command(contracts.CmdStartRun, sc))
		if res.Error == nil || res.Error.Code != contracts.ErrInvalid || strings.Contains(res.Error.Message, aideSecret) {
			t.Fatalf("%q: %+v", body, res)
		}
		if _, ok := w.Proc(run); ok {
			t.Fatalf("%q: refused launch started a process", body)
		}
	}
	sc := startCmd("ses_missing", "run_missing", contracts.HarnessClaude, cwd)
	sc.Args = []string{EnvFileArg, filepath.Join(cwd, "missing.env")}
	if res := n.do(command(contracts.CmdStartRun, sc)); res.Error == nil || res.Error.Code != contracts.ErrInvalid {
		t.Fatalf("missing file: %+v", res)
	}
	sc = startCmd("ses_rel", "run_rel", contracts.HarnessClaude, cwd)
	sc.Args = []string{EnvFileArg + "=aide.env"}
	if res := n.do(command(contracts.CmdStartRun, sc)); res.Error == nil || !strings.Contains(res.Error.Message, "absolute") {
		t.Fatalf("relative path: %+v", res)
	}
}

func TestApplyEnvFilePyreWins(t *testing.T) {
	env := map[string]string{"CONN_TOKEN": "pyre", "ANTHROPIC_API_KEY": "bound"}
	applied, unset, kept := applyEnvFile(env, []string{"OPENAI_API_KEY"}, envVars(map[string]string{"CONN_TOKEN": "file", "X": "1"}, "ANTHROPIC_API_KEY", "OPENAI_API_KEY", "CODEX_HOME"))
	if env["CONN_TOKEN"] != "pyre" || env["X"] != "1" || len(applied) != 1 {
		t.Fatalf("env %v applied %v", env, applied)
	}
	if !slices.Equal(unset, []string{"OPENAI_API_KEY", "CODEX_HOME"}) {
		t.Fatalf("unset %v", unset)
	}
	slices.Sort(kept)
	if !slices.Equal(kept, []string{"ANTHROPIC_API_KEY", "CONN_TOKEN"}) {
		t.Fatalf("kept %v", kept)
	}
}

func keysOf(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func envVars(set map[string]string, unset ...string) envfile.Vars {
	return envfile.Vars{Set: set, Unset: unset}
}

// pyre resume --env-file none drops the session's file, --env-file PATH
// replaces it; a broken recorded file names the run and the way out, also
// for an account switch; a previous run without adapter data falls back to
// this node's record of it.
func TestEnvFileResumeReplaceDropAndRecover(t *testing.T) {
	w := daemontest.NewWorld()
	fa := daemontest.New(w, contracts.HarnessClaude)
	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{fa}, Accounts: &fakeBinder{}})
	cwd := t.TempDir()
	envPath := writeEnvFile(t, t.TempDir(), "AGENT_BRIDGE_TOKEN="+aideSecret+"\n", 0o600)
	other := writeEnvFile(t, t.TempDir(), "AGENT_INTAKE_URL=http://127.0.0.1:8091\n", 0o600)
	sc := startCmd("ses_r", "run_r1", contracts.HarnessClaude, cwd)
	sc.Args = []string{EnvFileArg + "=" + envPath}
	n.start(sc)
	resume := func(prevID, runID string, args []string, strip bool) contracts.CommandResult {
		t.Helper()
		n.do(command(contracts.CmdStop, contracts.RunCommand{SessionID: "ses_r", RunID: prevID}))
		prev := n.run(prevID).Run
		if strip {
			prev.AdapterData = nil // a hub copy without adapter data
		}
		rc := contracts.ResumeCommand{StartRunCommand: startCmd("ses_r", runID, contracts.HarnessClaude, cwd)}
		rc.PreviousRun, rc.Args = &prev, args
		res := n.do(command(contracts.CmdResume, rc))
		n.d.WaitLaunches()
		return res
	}
	// No adapter data on the previous run: this node's record supplies it.
	if res := resume("run_r1", "run_r2", nil, true); res.Error != nil {
		t.Fatal(res.Error)
	}
	if p, _ := w.Proc("run_r2"); p.Launch.Env["AGENT_BRIDGE_TOKEN"] != aideSecret {
		t.Fatal("env file dropped for a previous run without adapter data")
	}
	// Replace.
	if res := resume("run_r2", "run_r3", []string{EnvFileArg + "=" + other}, false); res.Error != nil {
		t.Fatal(res.Error)
	}
	p, _ := w.Proc("run_r3")
	if _, old := p.Launch.Env["AGENT_BRIDGE_TOKEN"]; old || p.Launch.Env["AGENT_INTAKE_URL"] == "" || n.run("run_r3").Run.AdapterData[adapterDataEnvFile] != other {
		t.Fatal("replacement env file not applied")
	}
	// Drop.
	if res := resume("run_r3", "run_r4", []string{EnvFileArg + "=" + EnvFileNone}, false); res.Error != nil {
		t.Fatal(res.Error)
	}
	p, _ = w.Proc("run_r4")
	if _, set := p.Launch.Env["AGENT_INTAKE_URL"]; set || n.run("run_r4").Run.AdapterData[adapterDataEnvFile] != "" {
		t.Fatal("env file not dropped")
	}
	// A later resume keeps none.
	if res := resume("run_r4", "run_r5", nil, false); res.Error != nil || n.run("run_r5").Run.AdapterData[adapterDataEnvFile] != "" {
		t.Fatalf("after none: %+v", res.Error)
	}
	// Back to a file that then goes missing: resume and account switch say
	// which run recorded it and how to recover.
	if res := resume("run_r5", "run_r6", []string{EnvFileArg + "=" + envPath}, false); res.Error != nil {
		t.Fatal(res.Error)
	}
	n.do(command(contracts.CmdStop, contracts.RunCommand{SessionID: "ses_r", RunID: "run_r6"}))
	if err := os.Remove(envPath); err != nil {
		t.Fatal(err)
	}
	_, _, err := n.d.StartContinuation(context.Background(), ContinuationRequest{SessionID: "ses_r", PreviousRunID: "run_r6",
		AccountID: "acct_1", Kind: contracts.ContinuationNativeResume, Prompt: "continue"})
	if err == nil || !contracts.IsCode(err, contracts.ErrInvalid) || !strings.Contains(err.Error(), "recorded on run run_r6") || !strings.Contains(err.Error(), "--env-file none") {
		t.Fatalf("account switch with a missing env file: %v", err)
	}
	// none on a fresh start is a mistake.
	bad := startCmd("ses_none", "run_none", contracts.HarnessClaude, cwd)
	bad.Args = []string{EnvFileArg + "=" + EnvFileNone}
	if res := n.do(command(contracts.CmdStartRun, bad)); res.Error == nil || res.Error.Code != contracts.ErrInvalid {
		t.Fatalf("none on start: %+v", res)
	}
}

// On Windows names are case-insensitive: the file's Path never sits beside
// Pyre's PATH, and an unset matches whatever case Pyre used.
func TestApplyEnvFileFoldsCaseOnWindows(t *testing.T) {
	env := map[string]string{"PATH": `C:\pyre\bin`, "CONN_TOKEN": "pyre"}
	applied, unset, kept := applyEnvFileFold(env, []string{"OPENAI_API_KEY"},
		envVars(map[string]string{"Path": `C:\evil`, "conn_token": "file", "X": "1"}, "openai_api_key", "Anthropic_Api_Key"), strings.ToUpper)
	if env["PATH"] != `C:\pyre\bin` || env["Path"] != "" || env["conn_token"] != "" || len(applied) != 1 {
		t.Fatalf("env %v", env)
	}
	if !slices.Equal(unset, []string{"OPENAI_API_KEY", "Anthropic_Api_Key"}) {
		t.Fatalf("unset %v", unset)
	}
	slices.Sort(kept)
	if !slices.Equal(kept, []string{"Path", "conn_token"}) {
		t.Fatalf("kept %v", kept)
	}
}
