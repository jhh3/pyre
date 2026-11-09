package killguard

import (
	"strings"
	"testing"
)

// A fake host: no test here reads or signals a real process.
func fakeHost() Host {
	procs := []Proc{
		{PID: 1, PPID: 0, Comm: "systemd", Cmdline: "/sbin/init splash"},
		{PID: 1432, PPID: 1, Comm: "systemd", Cmdline: "/usr/lib/systemd/systemd --user"},
		{PID: 2000, PPID: 1432, Comm: "pyred", Cmdline: "/home/u/.local/share/pyre/current/bin/pyred run --state /home/u/.local/state/pyre"},
		{PID: 2001, PPID: 1432, Comm: "pyred", Cmdline: "/home/u/.local/bin/pyred sentinel run"},
		{PID: 2002, PPID: 1432, Comm: "pyre-hub", Cmdline: "/home/u/.local/share/pyre/current/bin/pyre-hub serve"},
		{PID: 3000, PPID: 2000, Comm: "tmux: server", Cmdline: "tmux -S /run/pyre.sock start-server"},
		{PID: 3100, PPID: 3000, Comm: "claude", Cmdline: "claude --settings /tmp/s.json"},
		{PID: 3200, PPID: 3100, Comm: "sh", Cmdline: "sh -c pyred hook claude PreToolUse"},
		{PID: 3201, PPID: 3200, Comm: "pyred", Cmdline: "pyred hook claude PreToolUse"},
		{PID: 4000, PPID: 1432, Comm: "bash", Cmdline: "bash -c while true; do pytest; done"},
		{PID: 4001, PPID: 4000, Comm: "pytest", Cmdline: "python -m pytest tests"},
	}
	return Host{Protected: Protect(procs, 3201)}
}

func TestBlocks(t *testing.T) {
	for _, line := range []string{
		"kill 1432",
		"kill -9 1432",
		"kill -s KILL 1432",
		"kill -TERM -- -1432",
		"kill -9 -1432",
		"kill 1",
		"kill -9 -1",
		"kill 2000",
		"kill 2001",
		"kill 3000",
		"kill 3100",
		"/bin/kill -9 1432",
		"sudo kill 1432",
		"timeout 5 kill 1432",
		"cd /tmp && kill -9 1432 2>/dev/null; echo done",
		"bash -c 'kill 1432'",
		"kill $PPID",
		"kill -9 ${PPID}",
		"kill $(ps -o ppid= -p 4001)",
		"kill -9 `ps -o ppid= -p 4001`",
		"P=$(ps -o ppid= -p 4001); kill $P",
		"ps -o ppid= -p 4001 | xargs kill",
		"kill $(awk '{print $4}' /proc/4001/stat)",
		"kill $(ps -ef | awk '{print $2}')",
		"kill $(pgrep systemd)",
		"kill $(pgrep -f pyred)",
		"kill $(pidof pyred)",
		"ps aux | grep pyre | awk '{print $2}' | xargs kill",
		"pkill systemd",
		"pkill -9 -f pyred",
		"pkill -f 'pyre'",
		"pkill -f 'systemd --user'",
		"pkill -P 1432",
		"pkill -P $PPID",
		"pkill -u jhh3",
		"killall pyred",
		"killall -9 systemd",
		"killall -u jhh3",
		"pkill tmux",
		"pkill claude",
		"systemctl --user stop pyred",
		"systemctl --user stop pyred.service",
		"systemctl --user kill pyre-sentinel",
		"systemctl --user kill --signal=KILL pyred.service",
		"systemctl --user kill -s KILL pyred.service",
		"systemctl --user stop 'pyre*'",
		"systemctl --user stop pyre-runs.slice",
		"systemctl --user disable --now pyred",
		"systemctl stop user@1000.service",
		"systemctl --user exit",
		"systemctl --user poweroff",
		"loginctl terminate-user jhh3",
		"loginctl kill-user jhh3",
	} {
		b := Check(line, fakeHost)
		if b == nil {
			t.Errorf("not blocked: %s", line)
			continue
		}
		if !strings.Contains(b.Reason, "systemctl --user stop <unit>") || !strings.Contains(b.Reason, "$!") {
			t.Errorf("%s: reason lacks the advice: %s", line, b.Reason)
		}
	}
}

func TestAllows(t *testing.T) {
	loads := 0
	host := func() Host { loads++; return fakeHost() }
	for _, line := range []string{
		"kill 4000",
		"kill -9 4001",
		"kill %1",
		"kill $!",
		"kill 0",
		"kill -l",
		"kill -0 4000",
		"kill $(cat /tmp/job.pid)",
		`kill "$(cat "$TMPDIR/loop.pid")"`,
		"kill $pid",
		"kill $(pgrep -f 'pytest tests/unit')",
		"pkill -f 'pytest tests/unit'",
		"pkill -P 4000",
		"killall pytest",
		"ps aux | grep pytest | awk '{print $2}' | xargs kill",
		"systemctl --user stop my-loop.service",
		"systemctl --user kill --signal=TERM my-loop",
		"systemctl --user restart pyred",
		"systemctl --user status pyred",
		"systemctl --user list-units 'pyre*'",
		"systemctl -H other stop pyred",
		"loginctl enable-linger jhh3",
		"echo 'kill 1432' > notes.txt",
		"git commit -m 'never kill 1432'",
		"go test ./internal/killguard",
		"ssh host 'kill 1432'",
	} {
		if b := Check(line, host); b != nil {
			t.Errorf("blocked %q: %s", line, b.Reason)
		}
	}
	loads = 0
	Check("go test ./... && git status", host)
	Check("make kill-tests", host) // has the word, needs no table
	if loads != 0 {
		t.Errorf("process table read %d times for lines without a signal target", loads)
	}
}

func TestProtectAncestors(t *testing.T) {
	h := fakeHost()
	for _, pid := range []int{1, 1432, 2000, 2001, 2002, 3000, 3100, 3200} {
		if _, ok := h.Protected[pid]; !ok {
			t.Errorf("PID %d not protected", pid)
		}
	}
	for _, pid := range []int{4000, 4001} {
		if _, ok := h.Protected[pid]; ok {
			t.Errorf("PID %d protected", pid)
		}
	}
	if !strings.Contains(h.Protected[1432].Label, "systemd --user") {
		t.Errorf("label %q", h.Protected[1432].Label)
	}
}

func TestParseStat(t *testing.T) {
	p, ok := parseStat(3000, "3000 (tmux: server) S 2000 3000 3000 0 -1")
	if !ok || p.Comm != "tmux: server" || p.PPID != 2000 {
		t.Fatalf("%+v %v", p, ok)
	}
	p, ok = parseStat(7, "7 (a) b) R 5 7")
	if !ok || p.Comm != "a) b" || p.PPID != 5 {
		t.Fatalf("%+v %v", p, ok)
	}
}

// LoadHost reads this machine's table without signalling anything; on
// Linux it finds PID 1 and this test's own parent.
func TestLoadHostReadsOnly(t *testing.T) {
	h := LoadHost()
	if _, ok := h.Protected[1]; !ok {
		t.Fatal("PID 1 not protected")
	}
}
