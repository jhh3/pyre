package wintask

import (
	"reflect"
	"strings"
	"testing"
)

func TestOnDemandXML(t *testing.T) {
	x := OnDemand{Description: "d<1>", UserID: "S-1-5-21-1-2-3-1001", Hidden: true,
		Command: `C:\Users\Dev User\AppData\Local\Programs\Pyre\current\bin\pyred.exe`,
		Args:    []string{"__pyre_termhost", "serve", "-S", `C:\Users\Dev User\s\termhost.sock`},
		WorkDir: `C:\Users\Dev User\s`}.XML()
	for _, want := range []string{
		"<Triggers />",
		"<LogonType>InteractiveToken</LogonType>",
		"<RunLevel>LeastPrivilege</RunLevel>",
		"<UserId>S-1-5-21-1-2-3-1001</UserId>",
		"<MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>",
		"<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>",
		"<Hidden>true</Hidden>",
		"<Priority>5</Priority>",
		"<Description>d&lt;1&gt;</Description>",
		`<Command>&#34;C:\Users\Dev User\AppData\Local\Programs\Pyre\current\bin\pyred.exe&#34;</Command>`,
		`<Arguments>__pyre_termhost serve -S &#34;C:\Users\Dev User\s\termhost.sock&#34;</Arguments>`,
		`<WorkingDirectory>C:\Users\Dev User\s</WorkingDirectory>`,
	} {
		if !strings.Contains(x, want) {
			t.Errorf("XML lacks %q:\n%s", want, x)
		}
	}
	if strings.Contains(x, "LogonTrigger") || strings.Contains(x, "RestartOnFailure") {
		t.Error("on-demand task must have no trigger and no restart policy")
	}
}

func TestParseTaskNames(t *testing.T) {
	out := "\"\\Pyre pyred dev\",\"N/A\",\"Running\"\r\n" +
		"\"\\Pyre Session Host dev 0a1b2c3d\",\"N/A\",\"Bereit\"\r\n" +
		"\r\n" +
		"\"TaskName\",\"Next Run Time\",\"Status\"\r\n" +
		"\"\\Microsoft\\Windows\\X\",\"N/A\",\"Ready\"\r\n" +
		"\"\\Pyre pyred dev\",\"N/A\",\"Running\"\r\n"
	got := ParseTaskNames(out)
	want := []string{"Pyre pyred dev", "Pyre Session Host dev 0a1b2c3d", `Microsoft\Windows\X`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestSafeName(t *testing.T) {
	if SafeName("Dev User") != "Dev_User" || SafeName("***") != "user" {
		t.Error(SafeName("Dev User"), SafeName("***"))
	}
}
