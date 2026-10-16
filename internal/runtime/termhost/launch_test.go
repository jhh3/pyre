package termhost

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestHostLauncher(t *testing.T) {
	type rec struct {
		calls []string
		log   strings.Builder
	}
	run := func(mode string, breakaway, taskErr error) (*rec, error) {
		r := &rec{}
		l := hostLauncher{
			mode: mode,
			direct: func(b bool) error {
				if b {
					r.calls = append(r.calls, "direct+breakaway")
					return breakaway
				}
				r.calls = append(r.calls, "direct")
				return nil
			},
			viaTask: func() (string, error) {
				r.calls = append(r.calls, "task")
				return "Pyre Session Host dev 01234567", taskErr
			},
			log: &r.log,
		}
		return r, l.launch()
	}
	denied := fmt.Errorf("%w: Access is denied.", errBreakawayDenied)
	for _, c := range []struct {
		name, mode     string
		breakaway, tsk error
		calls          string
		log            string
		fail           bool
	}{
		{name: "breakaway works", calls: "direct+breakaway", log: "started the host directly"},
		{name: "breakaway refused: task", breakaway: denied, calls: "direct+breakaway,task", log: `Task Scheduler task "Pyre Session Host dev 01234567"`},
		{name: "task fails: shared job", breakaway: denied, tsk: errors.New("schtasks: no"), calls: "direct+breakaway,task,direct", log: "shares pyred's job"},
		{name: "other spawn error", breakaway: errors.New("file not found"), calls: "direct+breakaway", fail: true},
		{name: "forced task", mode: "task", calls: "task", log: "PYRE_TERMHOST_SPAWN=task"},
		{name: "forced direct", mode: "direct", breakaway: denied, calls: "direct+breakaway,direct", log: "shares pyred's job"},
	} {
		r, err := run(c.mode, c.breakaway, c.tsk)
		if (err != nil) != c.fail {
			t.Errorf("%s: err %v", c.name, err)
		}
		if got := strings.Join(r.calls, ","); got != c.calls {
			t.Errorf("%s: calls %s, want %s", c.name, got, c.calls)
		}
		if !strings.Contains(r.log.String(), c.log) {
			t.Errorf("%s: log %q lacks %q", c.name, r.log.String(), c.log)
		}
	}
}

func TestSessionTask(t *testing.T) {
	a := SessionTaskName("Dev User", `C:\Users\Dev User\AppData\Local\Pyre\tmux\termhost.sock`)
	b := SessionTaskName("Dev User", `c:\users\dev user\appdata\local\pyre\tmux\termhost.sock`)
	if a != b || !strings.HasPrefix(a, SessionTaskPrefix("Dev User")) || !strings.HasPrefix(a, "Pyre Session Host Dev_User ") || len(a) != len("Pyre Session Host Dev_User ")+8 {
		t.Fatalf("names %q %q", a, b)
	}
	if SessionTaskName("Dev User", "/other/termhost.sock") == a {
		t.Fatal("different sockets share a task")
	}
	args := sessionTaskArgs([]string{ServeArg, "serve", "-S", `C:\s\termhost.sock`}, `C:\s\termhost.log`, `C:\s\termhost-env-1.json`)
	want := []string{ServeArg, "serve", "-S", `C:\s\termhost.sock`, "--log", `C:\s\termhost.log`, "--env-file", `C:\s\termhost-env-1.json`, "--hide-console"}
	if strings.Join(args, "|") != strings.Join(want, "|") {
		t.Fatalf("args %q", args)
	}
	x := sessionTask("S-1-5-21-1-2-3-1001", `C:\P\current\bin\pyred.exe`, args, `C:\s\termhost.sock`).XML()
	for _, w := range []string{"<Hidden>true</Hidden>", `<Command>C:\P\current\bin\pyred.exe</Command>`,
		"<UserId>S-1-5-21-1-2-3-1001</UserId>", "__pyre_termhost serve -S C:\\s\\termhost.sock --log", "--hide-console</Arguments>"} {
		if !strings.Contains(x, w) {
			t.Errorf("task XML lacks %q:\n%s", w, x)
		}
	}
}
