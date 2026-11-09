package main

import (
	"context"
	"flag"
	"io"
	"log/slog"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/jhh3/pyre/internal/daemon"
	"github.com/jhh3/pyre/internal/memguard"
	"github.com/jhh3/pyre/internal/runtime/tmux"
)

func TestMemProtectionFlags(t *testing.T) {
	t.Cleanup(func() { tmux.SetRunScopeLimits(nil) })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	parse := func(args ...string) memOpts {
		var o memOpts
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		addMemFlags(fs, &o)
		if err := fs.Parse(args); err != nil {
			t.Fatal(err)
		}
		return o
	}
	// Defaults: guard on with 5%/80%, per-run scopes 65% / 25% swap.
	var cfg daemon.Config
	o := parse()
	if err := applyMemProtection(context.Background(), o, "/st", &cfg, log); err != nil {
		t.Fatal(err)
	}
	if cfg.MemoryGuard == nil || *cfg.MemoryGuard != (memguard.Config{AvailFrac: 0.05, SwapFrac: 0.8}) ||
		len(cfg.GuardSockets) != 1 || cfg.GuardSockets[0] != filepath.Join("/st", "tmux", "pyre.sock") {
		t.Fatalf("defaults: guard %+v sockets %v", cfg.MemoryGuard, cfg.GuardSockets)
	}
	if runtime.GOOS == "linux" && (cfg.RunScopes == nil || cfg.RunScopes.MemoryMax != "65%" || cfg.RunScopes.SwapMax != "25%" || cfg.RunScopes.MemoryHigh != "off") {
		t.Fatalf("run scopes %+v", cfg.RunScopes)
	}
	// Off switches.
	cfg = daemon.Config{}
	if err := applyMemProtection(context.Background(), parse("--memory-guard", "off", "--run-memory-max", "off"), "/st", &cfg, log); err != nil {
		t.Fatal(err)
	}
	if cfg.MemoryGuard != nil || cfg.RunScopes != nil {
		t.Fatalf("off: %+v %+v", cfg.MemoryGuard, cfg.RunScopes)
	}
	// Custom thresholds.
	cfg = daemon.Config{}
	if err := applyMemProtection(context.Background(), parse("--memory-guard-available", "10%", "--memory-guard-swap", "0.5"), "/st", &cfg, log); err != nil {
		t.Fatal(err)
	}
	if cfg.MemoryGuard.AvailFrac != 0.10 || cfg.MemoryGuard.SwapFrac != 0.5 {
		t.Fatalf("custom: %+v", cfg.MemoryGuard)
	}
	for _, bad := range [][]string{{"--memory-guard", "maybe"}, {"--memory-guard-available", "150%"}, {"--memory-guard-swap", "x"}, {"--run-memory-max", "lots"}} {
		cfg = daemon.Config{}
		if err := applyMemProtection(context.Background(), parse(bad...), "/st", &cfg, log); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}
