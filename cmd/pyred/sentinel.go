package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/jhh3/pyre/internal/sentinel"
)

// cmdSentinel is the hub-independent watchdog (internal/sentinel). It runs
// as its own service (pyre-sentinel.service / dev.jhh3.pyre.sentinel), so
// it keeps watching when this machine's pyred is the thing that broke.
func cmdSentinel(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: pyred sentinel run|status|test-alert [--config FILE]")
	}
	sub, args := args[0], args[1:]
	fs := flag.NewFlagSet("sentinel "+sub, flag.ContinueOnError)
	cfgPath := fs.String("config", sentinel.DefaultConfigPath(), "sentinel config (0600 JSON)")
	once := fs.Bool("once", false, "run: one round, then exit")
	dry := fs.Bool("dry-run", false, "run: probe and evaluate, verify the Slack bot token (auth.test), print what would be sent; no alerts, recovery or state writes; exits 1 if Slack cannot deliver")
	asJSON := fs.Bool("json", false, "status, run --once/--dry-run: print the state as JSON")
	text := fs.String("text", "Pyre sentinel test — ignore", "test-alert: message text")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := sentinel.LoadConfig(*cfgPath)
	if err != nil {
		return err
	}
	notifier := &sentinel.SlackNotifier{Cfg: cfg.Slack}
	switch sub {
	case "run":
		// PYRE_SENTINEL_SSH replaces the ssh executable (a wrapper adding -F
		// for isolated tests).
		prober := &sentinel.SSHProber{Self: cfg.Self, Net: cfg.Network, SSHBin: os.Getenv("PYRE_SENTINEL_SSH"), Identity: cfg.SSHIdentity}
		e := &sentinel.Engine{Cfg: cfg, Prober: prober, Recoverer: prober, Notifier: notifier, DryRun: *dry}
		logger := log.New(os.Stderr, "", log.LstdFlags)
		e.Logf = logger.Printf
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if *once || *dry {
			if err := e.Load(); err != nil {
				return err
			}
			if err := e.Tick(ctx); err != nil {
				return err
			}
			// Delivery: verify the bot token without sending (auth.test and
			// scopes), so an install never runs with a sentinel that cannot
			// notify the owner.
			nerr := notifier.Check(ctx)
			if nerr != nil {
				e.State().NotifyErr = nerr.Error()
			}
			if *asJSON {
				b, _ := json.MarshalIndent(e.State(), "", "  ")
				fmt.Println(string(b))
			} else {
				printSentinelState(e.State())
			}
			if nerr != nil {
				return fmt.Errorf("slack delivery check failed: %w", nerr)
			}
			return nil
		}
		logger.Printf("sentinel %s: watching %d targets every %s (state %s)", cfg.Self, len(cfg.Targets)-1, cfg.Timing.Interval.D(), e.StatePath())
		return e.Run(ctx)
	case "status":
		e := &sentinel.Engine{Cfg: cfg}
		st, err := sentinel.ReadState(e.StatePath())
		if err != nil {
			return err
		}
		if st == nil {
			return fmt.Errorf("no state at %s (is the sentinel running?)", e.StatePath())
		}
		if *asJSON {
			b, _ := json.MarshalIndent(st, "", "  ")
			fmt.Println(string(b))
			return nil
		}
		printSentinelState(st)
		return nil
	case "test-alert":
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := notifier.Send(ctx, "test-alert", *text); err != nil {
			return err
		}
		fmt.Println("sent")
		return nil
	}
	return fmt.Errorf("unknown sentinel command %q (run|status|test-alert)", sub)
}

func printSentinelState(st *sentinel.State) {
	age := time.Since(st.UpdatedAt).Round(time.Second)
	fmt.Printf("sentinel  %s (rank %d), updated %s ago\n", st.Sentinel, st.Rank, age)
	net := "ok"
	if !st.NetworkOK {
		net = "FAILED"
	}
	fmt.Printf("network   %s (%s)\n", net, st.Network)
	if st.NotifyErr != "" {
		fmt.Printf("slack     CANNOT DELIVER: %s\n", st.NotifyErr)
	} else {
		fmt.Println("slack     ok (bot token verified or last DM delivered)")
	}
	fmt.Printf("leader    %s; live: %s\n", st.Leader, strings.Join(st.Live, ", "))
	for _, name := range sortedNames(st.Votes) {
		v := st.Votes[name]
		line := fmt.Sprintf("target    %-14s vote %-8s", name, v.Status)
		if tm := st.Timers[name]; tm != nil {
			line += " eval " + tm.Level.String()
		}
		if w := st.Maintenance[name]; w.Active(time.Now()) {
			line += fmt.Sprintf(" [maintenance until %s: %s]", w.Until.Local().Format(time.Kitchen), w.Reason)
		}
		if len(v.Reasons) > 0 {
			line += " — " + strings.Join(v.Reasons, "; ")
		}
		fmt.Println(line)
	}
	for _, name := range sortedNames(st.Incidents) {
		i := st.Incidents[name]
		fmt.Printf("incident  %s %s since %s, alerted %s, reminders %d, recovery attempts %d\n",
			i.ID, i.Level, i.Since.Local().Format(time.Kitchen), i.AlertedLevel, i.RemindersSent, len(i.Recovery))
	}
	for _, key := range sortedNames(st.Links) {
		i := st.Links[key]
		fmt.Printf("link      %s since %s, alerted %v: %s\n", i.ID, i.Since.Local().Format(time.Kitchen), !i.FirstAlertAt.IsZero(), i.Detail)
	}
	for _, c := range st.SelfPublic {
		state := "ok"
		if !c.OK {
			state = "FAILING (" + c.Detail + ")"
		}
		fmt.Printf("self      public %s %s\n", c.URL, state)
	}
}

func sortedNames[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// cmdMaintenance records a maintenance window on this machine
// (~/.local/state/pyre/maintenance.json), or on another sentinel target
// over the sentinel's ssh (`pyred maintenance server 15m`); sentinels watching
// it read the file over ssh and stay silent until it ends.
func cmdMaintenance(args []string) error {
	fs := flag.NewFlagSet("maintenance", flag.ContinueOnError)
	dur := fs.Duration("for", 0, "start a window of this length (e.g. 30m)")
	reason := fs.String("reason", "", "why (shown in sentinel status)")
	clear := fs.Bool("clear", false, "end the current window")
	path := fs.String("file", sentinel.DefaultMaintenancePath(), "maintenance file")
	cfgPath := fs.String("config", sentinel.DefaultConfigPath(), "sentinel config, for another node's ssh aliases")
	// Positionals may come before or after flags: [NODE] [DURATION|clear].
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() == 0 {
			break
		}
		pos, args = append(pos, fs.Arg(0)), fs.Args()[1:]
	}
	node := ""
	for _, a := range pos {
		if a == "clear" {
			*clear = true
		} else if d, err := time.ParseDuration(a); err == nil && d > 0 {
			*dur = d
		} else if node == "" {
			node = a
		} else {
			return errors.New("usage: pyred maintenance [NODE] [DURATION | clear] [--reason X]")
		}
	}
	if node != "" {
		cfg, err := sentinel.LoadConfig(*cfgPath)
		if err != nil {
			return fmt.Errorf("maintenance for %s needs the sentinel config for its ssh aliases (or run pyred maintenance on that machine): %w", node, err)
		}
		if node != cfg.Self {
			if !*clear && *dur <= 0 {
				return errors.New("usage: pyred maintenance NODE DURATION|clear [--reason X]")
			}
			d := *dur
			if *clear {
				d = 0
			}
			if *reason == "" {
				*reason = "pyred maintenance from " + cfg.Self
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			w, err := sentinel.SetRemoteMaintenance(ctx, cfg, node, time.Now(), d, *reason, os.Getenv("PYRE_SENTINEL_SSH"))
			if err != nil {
				return err
			}
			if w == nil {
				fmt.Printf("maintenance window on %s cleared\n", node)
			} else {
				fmt.Printf("maintenance on %s until %s\n", node, w.Until.Local().Format("Jan 2 3:04 PM MST"))
			}
			return nil
		}
	}
	switch {
	case *clear:
		if err := sentinel.ClearMaintenance(*path); err != nil {
			return err
		}
		fmt.Println("maintenance window cleared")
		return nil
	case *dur > 0:
		w, err := sentinel.WriteMaintenance(*path, time.Now(), *dur, *reason)
		if err != nil {
			return err
		}
		fmt.Printf("maintenance until %s (%s)\n", w.Until.Local().Format("Jan 2 3:04 PM MST"), *path)
		return nil
	}
	w, err := sentinel.ReadMaintenance(*path)
	if err != nil {
		return err
	}
	if !w.Active(time.Now()) {
		fmt.Println("no maintenance window")
		return nil
	}
	fmt.Printf("maintenance until %s: %s\n", w.Until.Local().Format("Jan 2 3:04 PM MST"), w.Reason)
	return nil
}
