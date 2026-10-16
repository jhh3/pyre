// Command digestdemo runs the Pyre TUI against the synthetic digest
// fixture (internal/digest.SyntheticInput over the in-process mock) with a
// throwaway state directory, so the "While you were away" panel can be
// reviewed and screenshotted:
//
//	go build -o /tmp/pyre-digest-demo ./internal/tui/digestdemo
//	python tests/tui/screenshot.py --cmd /tmp/pyre-digest-demo --out /tmp/shots "digest@2="
//
// Nothing outside a temporary directory is read or written.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/digest"
	"github.com/jhh3/pyre/internal/runtime/mock"
	"github.com/jhh3/pyre/internal/tui"
	"github.com/jhh3/pyre/internal/tui/tuitest"
)

func main() {
	noColor := flag.Bool("no-color", false, "monochrome output")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, *noColor); err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "digestdemo:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, noColor bool) error {
	dir, err := os.MkdirTemp("", "pyre-digest-demo-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	in := digest.SyntheticInput()
	state := filepath.Join(dir, "tui-state.json")
	b, _ := json.Marshal(map[string]bool{"tips_dismissed": true})
	if err := os.WriteFile(state, b, 0o600); err != nil {
		return err
	}
	if err := digest.SaveLastSeen(filepath.Join(dir, "digest-state.json"), digest.LastSeen{Cursor: in.SinceCursor, At: in.Since}); err != nil {
		return err
	}
	owner := contracts.Principal{Kind: contracts.PrincipalOwner, ID: "cli_synth-laptop", Name: "owner (synthetic)"}
	clk := mock.NewFakeClock(digest.SyntheticNow)
	be, err := mock.NewE(mock.Options{Clock: clk, DefaultPrincipal: &owner, FixtureDirs: []string{tuitest.ContractFixtureDir()}})
	if err != nil {
		return err
	}
	api := &digest.FixtureAPI{API: be, In: in}
	return tui.Run(ctx, api, tui.Options{
		NoColor:   noColor,
		Clock:     clk,
		StateFile: state,
		KeysFile:  "none",
		StartTerm: tuitest.FakeTermStarter,
		// No live watcher: the fixture is a fixed snapshot.
		RefreshInterval: -1,
	})
}
