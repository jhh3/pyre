package tui_test

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/tui"
	"github.com/jhh3/pyre/internal/tui/tuitest"
)

var update = flag.Bool("update", false, "rewrite golden files")

// start builds a harness over the fixture-seeded mock at w x h and waits
// for the initial snapshot.
func start(t *testing.T, w, ht int, wrap func(contracts.API) contracts.API, mod func(*tui.Options)) (*tuitest.Harness, contracts.API) {
	t.Helper()
	b, clk := tuitest.NewMock(t)
	var api contracts.API = b
	if wrap != nil {
		api = wrap(api)
	}
	opts := tuitest.Options()
	opts.Clock = clk
	if mod != nil {
		mod(&opts)
	}
	h := tuitest.New(t, api, opts, w, ht)
	h.WaitFor("initial sessions", 5e9, func() bool { return h.M.Loaded("sessions") && h.M.Loaded("nodes") && h.M.Loaded("attention") })
	h.Settle()
	return h, api
}

// golden compares the rendered view against tests/tui/golden/<name>.txt.
func golden(t *testing.T, name, got string) {
	t.Helper()
	// Trailing spaces are layout padding; trim them so goldens diff cleanly.
	lines := strings.Split(got, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	got = strings.Join(lines, "\n") + "\n"
	path := filepath.Join("golden", name+".txt")
	if *update {
		if err := os.MkdirAll("golden", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("missing golden %s (run go test ./tests/tui -update): %v", path, err)
	}
	if string(want) != got {
		t.Errorf("view %s differs from golden.\n--- want\n%s\n--- got\n%s", name, want, got)
	}
}
