package termhost

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/sessionhost"
)

func shortDir(t *testing.T) string {
	t.Helper()
	base := "/tmp" // short: socket paths are limited to ~100 bytes
	if runtime.GOOS == "windows" {
		base = ""
	}
	d, err := os.MkdirTemp(base, "th")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func TestNoServer(t *testing.T) {
	h, err := New(filepath.Join(shortDir(t), "none.sock"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if ok, err := h.HasSession(ctx, "x"); ok || err != nil {
		t.Fatalf("has: %v %v", ok, err)
	}
	if _, err := h.Pane(ctx, "x"); !errors.Is(err, sessionhost.ErrNoServer) {
		t.Fatalf("pane: %v", err)
	}
	if err := h.KillServer(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestAttachArgvRoundTrip(t *testing.T) {
	argv := AttachArgv(`C:\Program Files\Pyre\pyred.exe`, `C:\Users\u\AppData\Local\pyre\tmux\termhost.sock`, "run-1", true)
	ta, ok := ParseAttachArgv(argv)
	if !ok || ta.Name != "run-1" || !ta.ReadOnly || ta.Socket != argv[4] {
		t.Fatalf("%+v %v", ta, ok)
	}
	if _, ok := ParseAttachArgv([]string{"tmux", "attach"}); ok {
		t.Fatal("tmux argv parsed as termhost")
	}
	if _, ok := ParseAttachArgv(append(argv[:7:7], "-x")); ok {
		t.Fatal("unknown flag accepted")
	}
}

func TestAttachDescriptor(t *testing.T) {
	h := &Host{Socket: "/tmp/x/th.sock", Exe: "/opt/pyre/pyred"}
	now := time.Unix(100, 0)
	d, err := h.AttachDescriptor(sessionhost.AttachConfig{NodeID: "n1"}, "r1", "r1", contracts.AttachCommand{ReadOnly: true}, now)
	if err != nil || d.Mechanism != contracts.AttachLocalTmux || !d.ReadOnly {
		t.Fatalf("%+v %v", d, err)
	}
	if ta, ok := ParseAttachArgv(d.Command); !ok || !ta.ReadOnly {
		t.Fatalf("command %v", d.Command)
	}
	if _, err := h.AttachDescriptor(sessionhost.AttachConfig{NodeID: "n1"}, "r1", "r1", contracts.AttachCommand{ClientNodeID: "n2"}, now); err == nil {
		t.Fatal("remote attach without alias")
	}
	d, err = h.AttachDescriptor(sessionhost.AttachConfig{NodeID: "n1", SSHAlias: "winbox"}, "r1", "r1", contracts.AttachCommand{ClientNodeID: "n2"}, now)
	if err != nil || d.Mechanism != contracts.AttachSSHTmux || d.Command[2] != "winbox" {
		t.Fatalf("%+v %v", d, err)
	}
}

func TestQuoting(t *testing.T) {
	cases := map[string]string{
		`plain`:                  `plain`,
		`C:\Program Files\x.exe`: `"C:\Program Files\x.exe"`,
		`a"b`:                    `"a\"b"`,
		`tail\ `:                 `"tail\ "`,
		`dir with\`:              `"dir with\\"`,
		`q\"`:                    `"q\\\""`,
		``:                       `""`,
	}
	for in, want := range cases {
		if got := windowsQuote(in); got != want {
			t.Errorf("windowsQuote(%q) = %q, want %q", in, got, want)
		}
	}
	if got := posixQuote("it's"); got != `'it'"'"'s'` {
		t.Errorf("posixQuote: %s", got)
	}
}

// This test binary does not import hostproc, so it cannot serve the host
// process: a Host on it must say so instead of spawning a no-op.
func TestNotBuiltWithoutHostproc(t *testing.T) {
	if HostEntryBuilt() {
		t.Skip("host entry registered")
	}
	h, err := New(filepath.Join(shortDir(t), "th.sock"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Check(context.Background()); !errors.Is(err, ErrNotBuilt) {
		t.Fatalf("check: %v", err)
	}
	if err := h.NewSession(context.Background(), sessionhost.SessionSpec{Name: "x", Argv: []string{"sh"}}); !errors.Is(err, ErrNotBuilt) {
		t.Fatalf("new session: %v", err)
	}
}
