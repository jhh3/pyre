package supervisor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// shortDir returns a short temp dir: unix socket paths are limited to ~104
// bytes on macOS, and t.TempDir() paths are long.
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "pyre-sv-")
	if err != nil {
		d = t.TempDir()
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func needTmux(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
}

func TestTmuxLifecycleIsolated(t *testing.T) {
	needTmux(t)
	root := shortDir(t)
	tm := Tmux{Socket: filepath.Join(root, "r1", "tmux.sock"), OwnedRoot: root}
	ctx := context.Background()
	t.Cleanup(func() { tm.Kill(context.Background()) })

	ok, err := tm.HasSession(ctx, "pyre")
	if err != nil || ok {
		t.Fatalf("HasSession before start = %v, %v", ok, err)
	}
	spec := SessionSpec{
		Name:         "pyre",
		Cwd:          root,
		Env:          map[string]string{"PYRE_TEST_SECRET": "s3cret-value"},
		Argv:         []string{"/bin/sh", "-c", `echo "secret=$PYRE_TEST_SECRET"; sleep 30`},
		RemainOnExit: true,
	}
	rec, err := tm.Ensure(ctx, spec)
	if err != nil || rec {
		t.Fatalf("Ensure first = %v, %v", rec, err)
	}
	rec, err = tm.Ensure(ctx, spec)
	if err != nil || !rec {
		t.Fatalf("Ensure second should recover, got %v, %v", rec, err)
	}
	st, err := os.Stat(tm.Socket)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm()&0o077 != 0 {
		t.Fatalf("socket mode %v is group/other accessible", st.Mode().Perm())
	}
	pane, err := tm.Pane(ctx, "pyre")
	if err != nil || pane.PID <= 0 || pane.Dead {
		t.Fatalf("pane = %+v, %v", pane, err)
	}
	if !Alive(pane.PID) {
		t.Fatalf("pane pid %d not alive", pane.PID)
	}
	// The env reached the pane via the server environment.
	deadline := time.Now().Add(5 * time.Second)
	for {
		out, _ := tm.Capture(ctx, "pyre", 10)
		if strings.Contains(out, "secret=s3cret-value") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pane output missing env: %q", out)
		}
		time.Sleep(50 * time.Millisecond)
	}
	n, err := tm.Clients(ctx, "pyre")
	if err != nil || n != 0 {
		t.Fatalf("clients = %d, %v", n, err)
	}
	argv := tm.AttachArgv("pyre", true)
	if argv[len(argv)-1] != "-r" {
		t.Fatalf("read-only attach argv = %v", argv)
	}
	if err := tm.Kill(ctx); err != nil {
		t.Fatal(err)
	}
	if Alive(pane.PID) {
		time.Sleep(200 * time.Millisecond)
		if Alive(pane.PID) {
			t.Fatalf("pane pid %d survived kill-server", pane.PID)
		}
	}
	ok, _ = tm.HasSession(ctx, "pyre")
	if ok {
		t.Fatal("session survived Kill")
	}
}

func TestTmuxRemainOnExitReportsStatus(t *testing.T) {
	needTmux(t)
	root := shortDir(t)
	tm := Tmux{Socket: filepath.Join(root, "x.sock"), OwnedRoot: root}
	ctx := context.Background()
	t.Cleanup(func() { tm.Kill(context.Background()) })
	if err := tm.NewSession(ctx, SessionSpec{Name: "pyre", Cwd: root, Argv: []string{"/bin/sh", "-c", "exit 7"}, RemainOnExit: true}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		p, err := tm.Pane(ctx, "pyre")
		if err == nil && p.Dead {
			if p.ExitStatus != 7 {
				t.Fatalf("exit status = %d", p.ExitStatus)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pane never dead: %+v %v", p, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestKillRefusesForeignSocket(t *testing.T) {
	root := shortDir(t)
	other := shortDir(t)
	tm := Tmux{Socket: filepath.Join(other, "tmux.sock"), OwnedRoot: root}
	if err := tm.Kill(context.Background()); err != ErrNotOurs {
		t.Fatalf("Kill foreign = %v, want ErrNotOurs", err)
	}
	tm = Tmux{Socket: filepath.Join(root, "..", filepath.Base(other), "tmux.sock"), OwnedRoot: root}
	if tm.Owned() {
		t.Fatal("path traversal considered owned")
	}
	tm = Tmux{Socket: "/tmp/tmux-501/default"}
	if tm.Owned() {
		t.Fatal("no owned root must never be owned")
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{"": "''", "abc": "abc", "a b": "'a b'", "it's": `'it'"'"'s'`, "/x/y-z.sh": "/x/y-z.sh"}
	for in, want := range cases {
		if got := ShellQuote(in); got != want {
			t.Errorf("ShellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTreeUsage(t *testing.T) {
	table := parsePS([]byte(`
    1     0   100  0.0
   10     1  2000  1.5
   11    10  3000  2.5
   12    11   500  0.5
   20     1  9999  9.0
`))
	rss, cpu, n := TreeUsage(table, []int{10, 11})
	if rss != 5500 || n != 3 || cpu < 4.49 || cpu > 4.51 {
		t.Fatalf("TreeUsage = %d %f %d", rss, cpu, n)
	}
}

func TestProcessTableAndSelf(t *testing.T) {
	tab, err := ProcessTable(context.Background())
	if err != nil {
		t.Skipf("ps unavailable: %v", err)
	}
	if _, ok := tab[os.Getpid()]; !ok {
		t.Fatal("own pid missing from process table")
	}
	var s SelfSampler
	s.Sample(context.Background())
	x := 0
	for i := 0; i < 2_000_000; i++ {
		x += i
	}
	_ = x
	rss, cpu := s.Sample(context.Background())
	if rss <= 0 || cpu < 0 {
		t.Fatalf("self sample rss=%f cpu=%f", rss, cpu)
	}
	h := Host(context.Background())
	if h.CPUCount <= 0 || h.MemTotalMB <= 0 {
		t.Fatalf("host stats = %+v", h)
	}
}

func TestParseVMStat(t *testing.T) {
	s := `Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                               1000.
Pages active:                             5000.
Pages inactive:                           2000.
Pages speculative:                         100.
Pages purgeable:                            50.
`
	if got := parseVMStat(s); got != 3150*16384 {
		t.Fatalf("parseVMStat = %d", got)
	}
}

func TestRotatingFileBounded(t *testing.T) {
	dir := t.TempDir()
	r := &RotatingFile{Path: filepath.Join(dir, "logs", "pyred.log"), MaxBytes: 100, Backups: 2}
	line := []byte(strings.Repeat("x", 39) + "\n")
	for i := 0; i < 50; i++ {
		if _, err := r.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	r.Close()
	ents, _ := os.ReadDir(filepath.Join(dir, "logs"))
	if len(ents) != 3 {
		t.Fatalf("files = %d, want 3 (current + 2 backups)", len(ents))
	}
	var total int64
	for _, e := range ents {
		st, _ := os.Stat(filepath.Join(dir, "logs", e.Name()))
		total += st.Size()
		if st.Size() > 100 {
			t.Fatalf("%s size %d > max", e.Name(), st.Size())
		}
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %v", e.Name(), st.Mode().Perm())
		}
	}
	if total > 300 {
		t.Fatalf("total %d exceeds bound", total)
	}
}
