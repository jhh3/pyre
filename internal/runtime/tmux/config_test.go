package tmux

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/supervisor"
)

func TestConfigForGatesExtendedKeys(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	for _, c := range []struct {
		v    Version
		env  map[string]string
		want bool
	}{
		{Version{Major: 3, Minor: 4}, nil, false},
		{Version{Major: 3, Minor: 3}, nil, false},
		{Version{}, nil, false}, // unreadable version
		{Version{Major: 3, Minor: 5}, nil, true},
		{Version{Major: 3, Minor: 6}, nil, true},
		{Version{Major: 4, Minor: 0}, nil, true},
		{Version{Major: 3, Minor: 6}, map[string]string{"PYRE_TMUX_EXTENDED_KEYS": "0"}, false},
	} {
		conf := ConfigFor(c.v, env(c.env))
		if !strings.HasPrefix(conf, DefaultConfig) {
			t.Fatalf("%v: base config missing", c.v)
		}
		if got := strings.Contains(conf, "extended-keys on"); got != c.want {
			t.Fatalf("tmux %d.%d env %v: extended keys %v, want %v", c.v.Major, c.v.Minor, c.env, got, c.want)
		}
	}
}

// TestConcurrentEnsureServer: adapters share one socket and may start runs
// at the same moment; exactly one server results and the placement record
// names that server.
func TestConcurrentEnsureServer(t *testing.T) {
	a := testServer(t)
	b := &Server{Bin: a.Bin, Socket: a.Socket, Config: a.Config}
	ctx := context.Background()
	errs := make(chan error, 2)
	for _, s := range []*Server{a, b} {
		s := s
		go func() { errs <- s.EnsureServer(ctx) }()
	}
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	out, err := a.Run(ctx, nil, "display-message", "-p", "#{pid}")
	if err != nil {
		t.Fatal(err)
	}
	pl := supervisor.ReadPlacement(a.Socket)
	if !strings.Contains(pl, "server pid "+strings.TrimSpace(out)) {
		t.Fatalf("placement %q does not name server pid %s", pl, strings.TrimSpace(out))
	}
	t.Logf("placement: %s", pl)
}

// TestWrittenConfigLoads starts a real server with the config written for
// the installed tmux and checks the options took effect (or are absent on
// tmux < 3.5).
func TestWrittenConfigLoads(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	v, err := s.Version(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(s.Config)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.NewSession(ctx, SessionSpec{Name: "cfg", Argv: []string{"sleep", "30"}}); err != nil {
		t.Fatalf("server with generated config (%s) did not start: %v\n%s", v, err, b)
	}
	if m, err := s.Run(ctx, nil, "show-options", "-gv", "mouse"); err != nil || strings.TrimSpace(m) != "on" {
		t.Fatalf("mouse: %q %v", m, err)
	}
	out, err := s.Run(ctx, nil, "show-options", "-s", "extended-keys")
	if err != nil {
		t.Fatal(err)
	}
	want := "extended-keys off"
	if v.AtLeast(ExtendedKeysMin.Major, ExtendedKeysMin.Minor) && os.Getenv("PYRE_TMUX_EXTENDED_KEYS") != "0" {
		want = "extended-keys on"
		f, err := s.Run(ctx, nil, "show-options", "-s", "extended-keys-format")
		if err != nil || strings.TrimSpace(f) != "extended-keys-format csi-u" {
			t.Fatalf("extended-keys-format: %q %v", f, err)
		}
	}
	if strings.TrimSpace(out) != want {
		t.Fatalf("tmux %s: %q, want %q", v, strings.TrimSpace(out), want)
	}
	t.Logf("tmux %s: %s; placement: %s", v, strings.TrimSpace(out), supervisor.ReadPlacement(s.Socket))
}
