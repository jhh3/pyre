package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestWinQuoteArg(t *testing.T) {
	for in, want := range map[string]string{
		``:                   `""`,
		`plain`:              `plain`,
		`C:\Program Files\x`: `"C:\Program Files\x"`,
		`a"b`:                `"a\"b"`,
		`C:\dir with space\`: `"C:\dir with space\\"`,
		`back\\"quote`:       `"back\\\\\"quote"`,
	} {
		if got := winQuoteArg(in); got != want {
			t.Errorf("winQuoteArg(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestTaskXML(t *testing.T) {
	o := serviceOpts{action: "install", state: `C:\Users\Dev User\AppData\Local\Pyre`, logDir: `C:\Users\Dev User\AppData\Local\Pyre\logs`,
		exe: `C:\Users\Dev User\AppData\Local\Programs\Pyre\current\bin\pyred.exe`, runArgs: []string{"--name", "win<1>"}}
	x := taskXML(o, "S-1-5-21-1-2-3-1001")
	for _, want := range []string{
		"<LogonType>InteractiveToken</LogonType>",
		"<UserId>S-1-5-21-1-2-3-1001</UserId>",
		"<MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>",
		"<Interval>PT5M</Interval>",
		"<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>",
		`<Command>&#34;C:\Users\Dev User\AppData\Local\Programs\Pyre\current\bin\pyred.exe&#34;</Command>`,
		"supervise --hide-console --log",
		`-- run --state &#34;C:\Users\Dev User\AppData\Local\Pyre&#34; --name win&lt;1&gt;`,
	} {
		if !strings.Contains(x, want) {
			t.Errorf("task XML lacks %q:\n%s", want, x)
		}
	}
	if defaultTaskName(`Dev User`) != "Pyre pyred Dev_User" {
		t.Error(defaultTaskName(`Dev User`))
	}
}

func TestServiceArgs(t *testing.T) {
	if _, err := parseServiceArgs([]string{"install", "--state", "/s", "--", "--state", "/other"}); err == nil {
		t.Error("--state after -- must be refused")
	}
	o, err := parseServiceArgs([]string{"install", "--state", "/s", "--exe", "/x/pyred", "--", "--name", "n"})
	if err != nil || o.logDir != filepath.Join("/s", "logs") || len(o.runArgs) != 2 {
		t.Fatalf("%+v %v", o, err)
	}
}

func TestTrampolineRequest(t *testing.T) {
	r := &execRequest{path: "/opt/pyre/v2/bin/pyred", why: "test", env: []string{
		"PATH=/bin", "PYRE_TOKEN=secret", envLauncher + "=/opt/pyre/current/bin/pyred", envHandoffWatch + "=1"}}
	tr := requestFromExec(r)
	if tr.Set[envLauncher] != "/opt/pyre/current/bin/pyred" || tr.Set[envHandoffWatch] != "1" || len(tr.Set) != 2 {
		t.Fatalf("handoff vars: %+v", tr.Set)
	}
	f := filepath.Join(t.TempDir(), "req.json")
	if err := writeTrampolineRequest(f, tr); err != nil {
		t.Fatal(err)
	}
	got, ok := readTrampolineRequest(f)
	if !ok || got.Path != r.path || got.Set[envLauncher] != tr.Set[envLauncher] {
		t.Fatalf("roundtrip: %+v %v", got, ok)
	}
	if _, ok := readTrampolineRequest(f); ok {
		t.Fatal("request file must be consumed")
	}
	env := envWithout([]string{"A=1", "pyre_exec_trampoline=x", "B=2"}, envTrampoline)
	if strings.Join(env, ",") != "A=1,B=2" {
		t.Fatal(env)
	}
}
