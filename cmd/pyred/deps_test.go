package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// pyred is exec'd for every Claude hook (several per turn), so its process
// start is hook overhead. Linking the CLI/TUI pulled in go-runewidth, whose
// package init alone took ~19 ms on an M2 Mac (pyred start 33 ms -> 10 ms
// without it; A20 evidence 2026-10-05). Keep the TUI stack out of pyred.
// The termhost's host process (termhost/hostproc, with the VT emulator) is
// linked only on Windows or with -tags pyre_termhost, so the macOS and
// Linux builds are checked whatever the test's own platform.
func TestPyredDoesNotLinkTheTUI(t *testing.T) {
	// The go command exports GOROOT to the test binaries it runs; fall back
	// to PATH when the test binary is run by hand.
	gobin := filepath.Join(os.Getenv("GOROOT"), "bin", "go")
	if os.Getenv("GOROOT") == "" {
		p, err := exec.LookPath("go")
		if err != nil {
			t.Skipf("go not found: %v", err)
		}
		gobin = p
	}
	for _, goos := range []string{"darwin", "linux"} {
		cmd := exec.Command(gobin, "list", "-deps", ".")
		cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH=arm64")
		out, err := cmd.Output()
		if err != nil {
			t.Skipf("go list unavailable: %v", err)
		}
		for _, pkg := range strings.Fields(string(out)) {
			switch {
			case pkg == "github.com/jhh3/pyre/internal/cli", pkg == "github.com/jhh3/pyre/internal/tui",
				pkg == "github.com/jhh3/pyre/internal/runtime/termhost/hostproc",
				strings.HasPrefix(pkg, "charm.land/"), pkg == "github.com/mattn/go-runewidth",
				pkg == "github.com/charmbracelet/x/vt", pkg == "github.com/charmbracelet/ultraviolet":
				t.Errorf("pyred for %s links %s (heavy package init on every hook invocation)", goos, pkg)
			}
		}
	}
}
