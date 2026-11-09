package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/jhh3/pyre/internal/daemon"
	"github.com/jhh3/pyre/internal/memguard"
	"github.com/jhh3/pyre/internal/runtime/tmux"
	"github.com/jhh3/pyre/internal/supervisor"
)

// Node memory protection (decision I66): per-run systemd scopes with
// memory caps (Linux) and the low-memory watchdog (Linux, macOS).
type memOpts struct {
	runMemoryMax, runMemoryHigh, runSwapMax string
	guard, guardAvail, guardSwap            string
}

func addMemFlags(fs *flag.FlagSet, o *memOpts) {
	fs.StringVar(&o.runMemoryMax, "run-memory-max", supervisor.DefaultRunMemoryMax, "Linux: MemoryMax of each run's own systemd scope (the harness and everything it starts; a Codex app-server and its threads' commands share one): a share of RAM such as 65%, a size such as 16G, or off. A run that reaches it is OOM-killed inside its scope (the largest process there, normally the runaway tool), not node-wide")
	fs.StringVar(&o.runMemoryHigh, "run-memory-high", "off", "Linux: MemoryHigh (reclaim throttling) of each run's scope, as for --run-memory-max; default off (throttling stalls the run instead of ending the runaway)")
	fs.StringVar(&o.runSwapMax, "run-swap-max", supervisor.DefaultRunSwapMax, "Linux: MemorySwapMax of each run's scope: a share of swap such as 25%, a size, 0 (no swap) or off")
	fs.StringVar(&o.guard, "memory-guard", "on", "low-memory watchdog: on (default) or off. When the machine is nearly out of memory and swap it stops the largest process a Pyre run started (never a harness, pyred, tmux or a process outside Pyre's runs), audits it (node.memory_guard_kill) and tells the session")
	fs.StringVar(&o.guardAvail, "memory-guard-available", "5%", "memory guard trigger: available memory below this share of RAM")
	fs.StringVar(&o.guardSwap, "memory-guard-swap", "80%", "memory guard trigger (Linux): and swap used above this share (ignored without swap; macOS uses its critical pressure level instead)")
}

func parseFrac(name, v string) (float64, error) {
	s := strings.TrimSpace(v)
	p, ok := strings.CutSuffix(s, "%")
	f, err := strconv.ParseFloat(p, 64)
	if err != nil || f <= 0 || f >= 100 || (!ok && f >= 1) {
		return 0, fmt.Errorf("--%s %q: want a percentage between 0 and 100, such as 5%%", name, v)
	}
	if ok {
		f /= 100
	}
	return f, nil
}

// applyMemProtection validates the flags and wires them: tmux panes get
// per-run scopes, the daemon the guard and its roots.
func applyMemProtection(ctx context.Context, o memOpts, state string, cfg *daemon.Config, log *slog.Logger) error {
	limits := supervisor.RunScopeLimits{MemoryMax: o.runMemoryMax, MemoryHigh: o.runMemoryHigh, SwapMax: o.runSwapMax}
	if err := limits.Validate(); err != nil {
		return err
	}
	switch strings.ToLower(strings.TrimSpace(o.guard)) {
	case "on", "true", "1", "yes":
		avail, err := parseFrac("memory-guard-available", o.guardAvail)
		if err != nil {
			return err
		}
		swap, err := parseFrac("memory-guard-swap", o.guardSwap)
		if err != nil {
			return err
		}
		cfg.MemoryGuard = &memguard.Config{AvailFrac: avail, SwapFrac: swap}
		cfg.GuardSockets = []string{filepath.Join(state, "tmux", "pyre.sock")}
	case "off", "false", "0", "no":
		log.Info("memory guard off (--memory-guard off)")
	default:
		return fmt.Errorf("--memory-guard %q: want on or off", o.guard)
	}
	if limits.Off() || runtime.GOOS != "linux" {
		return nil
	}
	tmux.SetRunScopeLimits(&limits)
	cfg.RunScopes = &limits
	go func() {
		d := supervisor.DecideRunScope(ctx, limits)
		if d.Use {
			log.Info("per-run memory scopes on", "scope", d.Reason)
		} else {
			log.Info("per-run memory scopes off: runs share the run server's cgroup", "reason", d.Reason)
		}
	}()
	return nil
}
