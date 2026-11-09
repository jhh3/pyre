package memguard

import "testing"

func TestParsePSDarwin(t *testing.T) {
	out := []byte("  100     1   10240 Sat Oct 24 10:01:02 2026     /opt/homebrew/bin/tmux\n" +
		"  101   100 2097152 Sat Oct 24 10:01:03 2026     /Users/synth/.local/bin/claude\n" +
		"  103   101 24117248 Sat Oct  4 09:00:00 2026     /usr/bin/python3\n" +
		"garbage line\n")
	m := parsePS(out)
	if len(m) != 3 {
		t.Fatalf("rows %v", m)
	}
	p := m[103]
	if p.PPID != 101 || p.Name != "python3" || p.RSS != 24117248<<10 || p.Start == 0 {
		t.Fatalf("row %+v", p)
	}
	if !Protected(m[101].Name) || !Protected(m[100].Name) {
		t.Fatalf("harness names %q %q", m[101].Name, m[100].Name)
	}
	if c, ok := PickVictim(m, Roots{Hosts: []int{100}}, 1, 1<<30); !ok || c.PID != 103 {
		t.Fatalf("victim %+v", c)
	}
}
