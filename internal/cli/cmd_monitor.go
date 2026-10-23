package cli

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	c "github.com/jhh3/pyre/internal/contracts"
)

func (a *App) addMonitorCommands(root *cobra.Command) {
	mon := &cobra.Command{
		Use:   "monitor",
		Short: "Watch a shell command on a timer; wake a session only when its output changes",
		Long: `A monitor runs a shell command every --every on a machine and sends the
session a Pyre message only when the command's output (stdout+stderr, plus the
exit status when nonzero) differs from the previous check. Nothing runs in the
agent between changes, so a monitor costs no model tokens while it waits.

Use it instead of an agent that re-checks something every few minutes:
  pyre monitor add --every 10m --cmd 'gh pr checks 42 --json state -q ".[].state" | sort | uniq -c'
  pyre monitor add --every 15m --until-change --cmd 'gh run view 123 --json status -q .status'

A monitor is a shell session (harness shell, tagged monitor) on the node. It
keeps running after the session that created it ends, and its screen shows
every check (pyre read <monitor>). Session lists (pyre sessions, the TUI, the
apps, MCP list_sessions) leave monitors out and show them on the session they
wake ("watching N"); pyre monitor ls or pyre sessions --monitors lists them.

Claude Code sessions have a built-in wake (the Monitor tool, or a Bash command
with run_in_background that notifies when it exits); prefer it there.
pyre monitor is for harnesses without one (Codex, agy) and for checks that
must outlive the session.`,
	}

	var ap MonitorAddParams
	add := &cobra.Command{
		Use:   "add --cmd CMD --every DURATION [--session S] [--node N] [--until-change] [--for DURATION]",
		Short: "Start a monitor",
		Long: `Start a monitor. The first check is the baseline (--initial reports it too);
each later check whose output differs wakes the session with the new output.
Adding the same --cmd with the same machine, directory, --every, --for,
--until-change and --initial for the same session again returns the caller's
live monitor instead of starting another ("reused": true in --json).
The command runs with sh -c (PowerShell on Windows) in --cwd (default: this
directory when the monitor runs on this machine) on --node (default: this
session's machine, else the target session's). --until-change stops after the
first reported change; --for stops after a duration (default 7 days).

Guards: at most 6 wakes per hour (a noisier check stops the monitor with a
final notice); while the target session is mid-turn, changes are held and
only the latest is sent once it is idle; the monitor ends when its target is
archived; a check that runs past its interval (at most 10m) is killed with
its whole process tree, and at most 1 MB of its output is kept.`,
		Args: cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			ap.OperationID = a.opID
			if a.callerIsClaude(cmd.Context(), ops) {
				fmt.Fprintln(a.Stderr, "hint: Claude Code can wait without Pyre: its Monitor tool (or Bash run_in_background) wakes you when a check changes or a command exits")
			}
			res, err := ops.MonitorAdd(cmd.Context(), ap)
			if err != nil {
				return err
			}
			return a.emit(res, func(w io.Writer) {
				s := res.Session
				if res.Reused {
					fmt.Fprintf(w, "reused monitor %s %q on %s: it already watches this command and wakes %s when the output changes\n",
						s.Session.SessionID, s.Session.Title, orDash(s.NodeName), monitorTarget(s))
					fmt.Fprintf(w, "checks: pyre read %s | stop: pyre monitor rm %s\n", s.Session.SessionID, s.Session.SessionID)
					return
				}
				fmt.Fprintf(w, "monitor %s %q on %s: every %s; wakes %s when the output changes\n",
					s.Session.SessionID, s.Session.Title, orDash(s.NodeName), ap.Every, monitorTarget(s))
				fmt.Fprintf(w, "checks: pyre read %s | stop: pyre monitor rm %s\n", s.Session.SessionID, s.Session.SessionID)
			})
		}),
	}
	af := add.Flags()
	af.StringVar(&ap.Cmd, "cmd", "", "shell command whose output is watched")
	af.DurationVar(&ap.Every, "every", 0, "check interval (e.g. 5m, 15m; at least 1m)")
	af.StringVar(&ap.Session, "session", "", "session to wake (default: this session)")
	af.StringVar(&ap.Node, "node", "", "machine that runs the command (default: this session's, else the target's)")
	af.StringVar(&ap.Cwd, "cwd", "", "directory the command runs in on that machine")
	af.StringVar(&ap.Title, "title", "", "short label (default: the command)")
	af.DurationVar(&ap.For, "for", 0, "stop after this long (default 7 days)")
	af.BoolVar(&ap.UntilChange, "until-change", false, "stop after the first reported change")
	af.BoolVar(&ap.Initial, "initial", false, "also report the first result")
	mon.AddCommand(add)

	ls := &cobra.Command{
		Use:   "ls",
		Short: "List monitors",
		Args:  cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			list, err := ops.MonitorList(cmd.Context())
			if err != nil {
				return err
			}
			if list == nil {
				list = []c.SessionView{}
			}
			return a.emit(list, func(w io.Writer) {
				var rows [][]string
				for _, s := range list {
					state := "-"
					if s.Run != nil {
						state = string(s.Run.State)
					}
					rows = append(rows, []string{s.Session.SessionID, s.Session.Title, orDash(s.NodeName), state, orDash(monitorTarget(s))})
				}
				table(w, "MONITOR\tTITLE\tNODE\tSTATE\tWAKES", rows)
			})
		}),
	}
	mon.AddCommand(ls)

	rm := &cobra.Command{
		Use:   "rm <monitor>",
		Short: "Stop a monitor and archive it",
		Args:  cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			sv, err := ops.MonitorRemove(cmd.Context(), args[0], a.opID)
			if err != nil {
				return err
			}
			return a.emit(sv, func(w io.Writer) {
				fmt.Fprintf(w, "monitor %s stops and is archived\n", sv.Session.SessionID)
			})
		}),
	}
	mon.AddCommand(rm)

	var to, cmdB64, labelB64 string
	var every, dur time.Duration
	var untilChange, initial bool
	runCmd := &cobra.Command{
		Use:    "run --to S --every D --cmd-b64 B",
		Short:  "The monitor loop (started by pyre monitor add inside the monitor session)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			script, err := base64.RawURLEncoding.DecodeString(cmdB64)
			if err != nil || len(script) == 0 {
				return usagef("--cmd-b64 must be a non-empty base64url command")
			}
			label, _ := base64.RawURLEncoding.DecodeString(labelB64)
			if every < monitorMinEvery {
				return usagef("--every must be at least %s", monitorMinEvery)
			}
			m := &monitorLoop{To: to, Label: string(label), Cmd: string(script), Every: every, For: dur,
				UntilChange: untilChange, Initial: initial, Out: a.Stdout, Now: time.Now, Self: ops.Self.SessionID,
				Sleep: func(ctx context.Context, d time.Duration) error {
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-time.After(d):
						return nil
					}
				},
				Check: func(ctx context.Context, s string) string {
					return runMonitorCheck(ctx, s, min(every, monitorCmdTimeout))
				},
				Send: func(ctx context.Context, text string) error {
					_, err := ops.Send(ctx, SendParams{To: to, Text: text})
					return err
				},
				Target: func(ctx context.Context) monitorTargetState { return monitorTargetOf(ctx, ops.API, to) },
			}
			fmt.Fprintf(a.Stdout, "monitor %q: every %s, wakes %s on change\n$ %s\n", m.Label, every, to, m.Cmd)
			return m.run(cmd.Context())
		}),
	}
	rf := runCmd.Flags()
	rf.StringVar(&to, "to", "", "session to wake")
	rf.DurationVar(&every, "every", 0, "check interval")
	rf.StringVar(&cmdB64, "cmd-b64", "", "command, base64url")
	rf.StringVar(&labelB64, "label-b64", "", "label, base64url")
	rf.DurationVar(&dur, "for", 0, "stop after this long")
	rf.BoolVar(&untilChange, "until-change", false, "stop after the first reported change")
	rf.BoolVar(&initial, "initial", false, "also report the first result")
	mon.AddCommand(runCmd)
	root.AddCommand(mon)
}

// callerIsClaude reports whether the calling session is a Claude one:
// $PYRE_HARNESS (pyred sets it), else (a pyred before it) the session's
// harness from the hub, else outside Pyre CLAUDECODE=1.
func (a *App) callerIsClaude(ctx context.Context, ops *Ops) bool {
	if h := a.callerHarness(); h != "" || ops.Self.SessionID == "" {
		return h == string(c.HarnessClaude)
	}
	sv, err := ops.API.GetSession(ctx, c.GetRequest{ID: ops.Self.SessionID})
	return err == nil && sv.Session.Harness == c.HarnessClaude
}
