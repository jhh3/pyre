package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/sysstats"
	"github.com/jhh3/pyre/internal/terminal"
)

func (a *App) addSessionCommands(root *cobra.Command) {
	var f SessionFilter
	var standalone, endedOnly, all bool
	lsFlags := func(cmd *cobra.Command) {
		fl := cmd.Flags()
		fl.StringVar(&f.Node, "node", "", "only sessions whose current run is on this node")
		fl.StringVar(&f.Project, "project", "", "only sessions in this project")
		fl.BoolVar(&standalone, "standalone", false, "only sessions without a project")
		fl.StringVar(&f.Harness, "harness", "", "claude, codex or shell")
		fl.StringVar(&f.Query, "query", "", "text search")
		fl.StringSliceVar(&f.States, "state", nil, "run states (running, waiting_input, waiting_approval, exited, unknown, ...)")
		fl.BoolVar(&f.IncludeArchived, "archived", false, "include archived sessions")
		fl.BoolVar(&endedOnly, "ended", false, "only ended sessions (exited, failed or without a run; not archived)")
		fl.BoolVar(&all, "all", false, "every session, archived ones included (same as --archived)")
	}
	ls := func(cmd *cobra.Command, args []string) error {
		ops, err := a.connect()
		if err != nil {
			return err
		}
		if standalone {
			if f.Project != "" {
				return usagef("--standalone and --project are mutually exclusive")
			}
			f.Project = "none"
		}
		if endedOnly && all {
			return usagef("--ended and --all are mutually exclusive")
		}
		if all {
			f.IncludeArchived = true
		}
		page, err := ops.ListSessions(cmd.Context(), f)
		if err != nil {
			return err
		}
		if endedOnly {
			kept := page.Items[:0]
			for _, s := range page.Items {
				if sessionEnded(s) && !s.Session.Archived {
					kept = append(kept, s)
				}
			}
			page.Items = kept
		}
		return a.emit(page, func(w io.Writer) { printSessions(w, page.Items) })
	}
	sessions := &cobra.Command{
		Use:     "sessions [ls|show|rename|archive|unarchive|prune]",
		Aliases: []string{"session"},
		Short:   "List sessions across the fleet",
		Args:    cobra.NoArgs,
		RunE:    a.run(ls),
	}
	lsFlags(sessions)
	sls := &cobra.Command{Use: "ls", Short: "List sessions", Args: cobra.NoArgs, RunE: a.run(ls)}
	lsFlags(sls)
	sessions.AddCommand(sls)
	sessions.AddCommand(&cobra.Command{
		Use:   "show <session>",
		Short: "Show a session and its current run (ID, exact title, or self)",
		Args:  cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			sv, err := ops.ResolveSession(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if err := a.emit(sv, func(w io.Writer) { printSession(w, sv) }); err != nil {
				return err
			}
			a.showWorktrees(cmd.Context(), ops, sv) // W-quicklaunch
			return nil
		}),
	})
	sessions.AddCommand(a.renameCommand())   // W-names
	sessions.AddCommand(a.iconCommand())     // W-archive2
	sessions.AddCommand(a.reparentCommand()) // W-reparent
	var force, whenDone bool
	var archiveWT, pruneWT *worktreeMode // W-quicklaunch
	archive := &cobra.Command{
		Use:   "archive [session]...",
		Short: "Archive sessions (hidden from listings; live ones when their turn ends)",
		Long: `Archive hides sessions from pyre sessions, the TUI and the apps.
With no session, archives the calling session ($PYRE_SESSION_ID).
A live session is archived when done (its current turn ends, Pyre stops it
gracefully, then archives it) when it is the calling session, a session the
caller started, or --when-done is given; otherwise stop it first
(pyre stop <session>). --force archives it at once, leaving it running out
of sight (archiving revokes its Pyre agent tokens). pyre sessions unarchive
restores one (and cancels a pending archive); pyre sessions ls --all lists
archived sessions too. --json prints the sessions as an array.`,
		Args: cobra.ArbitraryArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			if a.opID != "" && len(args) > 1 {
				return usagef("--operation-id applies to a single session")
			}
			ops, err := a.connect()
			if err != nil {
				return err
			}
			if len(args) == 0 {
				if ops.Self.SessionID == "" {
					return usagef("a session is required outside a Pyre session (see `pyre sessions`)")
				}
				args = []string{"self"}
			}
			done := []c.SessionView{}
			var firstErr error
			for _, ref := range args {
				sv, err := ops.Archive(cmd.Context(), ArchiveParams{Session: ref, OperationID: a.opID, Force: force, WhenDone: whenDone})
				if err != nil {
					a.warnf("pyre: archive %s: %v", ref, err)
					if firstErr == nil {
						firstErr = err
					}
					continue
				}
				done = append(done, sv)
			}
			if err := a.emit(done, func(w io.Writer) {
				for _, sv := range done {
					if !sv.Session.Archived {
						fmt.Fprintf(w, "%s %q %s\n", sv.Session.SessionID, sv.Session.Title, archiveResult(sv, sv.Session.SessionID == ops.Self.SessionID).Note)
						continue
					}
					fmt.Fprintf(w, "archived %s %q\n", sv.Session.SessionID, sv.Session.Title)
				}
			}); err != nil {
				return err
			}
			var now []c.SessionView
			for _, sv := range done {
				if sv.Session.Archived {
					now = append(now, sv)
				}
			}
			a.afterArchive(cmd.Context(), ops, now, archiveWT) // W-quicklaunch
			return firstErr
		}),
	}
	archiveWT = addWorktreeFlags(archive)
	archive.Flags().BoolVar(&force, "force", false, "archive even if the session is still live (it keeps running, but its Pyre agent tokens are revoked)")
	archive.Flags().BoolVar(&whenDone, "when-done", false, "archive a live session when its current turn ends (Pyre stops it gracefully first)")
	sessions.AddCommand(archive)
	sessions.AddCommand(&cobra.Command{
		Use:   "unarchive <session>",
		Short: "Restore an archived session to listings (ID or exact title)",
		Args:  cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			sv, err := ops.Unarchive(cmd.Context(), ArchiveParams{Session: args[0], OperationID: a.opID})
			if err != nil {
				return err
			}
			return a.emit(sv, func(w io.Writer) { fmt.Fprintf(w, "restored %s %q\n", sv.Session.SessionID, sv.Session.Title) })
		}),
	})
	var pp PruneParams
	prune := &cobra.Command{
		Use:   "prune [--older-than 24h] [--node N] [--dry-run]",
		Short: "Archive ended sessions (exited, failed or without a run)",
		Long: `Archive every ended session: its run exited or failed, or it never got one.
Live sessions, including offline and stale ones whose run may still be
alive, are never touched. --older-than keeps sessions that ended more
recently; --node limits to one machine; --dry-run only lists them.
pyre sessions unarchive restores one.`,
		Args: cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			if pp.OlderThan < 0 {
				return usagef("--older-than must not be negative")
			}
			ops, err := a.connect()
			if err != nil {
				return err
			}
			res, err := ops.Prune(cmd.Context(), pp, time.Now())
			if eerr := a.emit(res, func(w io.Writer) {
				verb := "archived"
				if res.DryRun {
					verb = "would archive"
				}
				for _, sv := range res.Archived {
					state := "no run"
					if sv.Run != nil {
						state = string(sv.Run.State)
					}
					fmt.Fprintf(w, "%s %s %q (%s on %s, ended %s)\n", verb, sv.Session.SessionID, sv.Session.Title, state, orDash(sv.NodeName), ago(endedAt(sv)))
				}
				fmt.Fprintf(w, "%s %d ended session(s)\n", verb, len(res.Archived))
			}); eerr != nil {
				return eerr
			}
			if !res.DryRun {
				a.afterArchive(cmd.Context(), ops, res.Archived, pruneWT) // W-quicklaunch
			}
			return err
		}),
	}
	pruneWT = addWorktreeFlags(prune)
	prune.Flags().DurationVar(&pp.OlderThan, "older-than", 0, "only sessions that ended at least this long ago (e.g. 24h; default: any)")
	prune.Flags().StringVar(&pp.Node, "node", "", "only sessions whose last run was on this node")
	prune.Flags().BoolVar(&pp.DryRun, "dry-run", false, "list what would be archived; change nothing")
	sessions.AddCommand(prune)
	root.AddCommand(sessions)
	rootLs := &cobra.Command{Use: "ls", Short: "List sessions (same as `pyre sessions`)", Args: cobra.NoArgs, RunE: a.run(ls)}
	lsFlags(rootLs)
	root.AddCommand(rootLs)

	var sp StartParams
	start := &cobra.Command{
		Use:   "start --node NODE --harness claude|codex|shell [--cwd DIR | --project P --repo R [--worktree B]] [--prompt TEXT]",
		Short: "Start a session on a machine (no project required)",
		Long: `Start a new session and its first run. The launch is asynchronous: the
response means the session is recorded and the launch command queued; watch
the run state (pyre sessions show) for the launch outcome.

The account policy defaults to an eligible configured subscription for the
harness's provider; --account pins one, --ambient uses the node's existing
login. Crossing providers never happens implicitly. A node with no Pyre
accounts for the provider uses your own login there (pyred --default-account).

--permissions bypass runs the harness without approval prompts or sandbox
(the node's pyred --permissions is the default); --permissions ask keeps them.

Run from an agent session, the new session reports back to it automatically:
a Pyre message when it finishes a turn (with its last message), needs input,
or exits. --no-report turns that off.

Projects and checkouts (all optional):
  --project P --repo R            run in P's recorded checkout of R on the node
                                  (shared: other sessions may work there too)
  --project P --repo R --worktree B
                                  new isolated git worktree on branch B, created
                                  from P's recorded checkout of R on the node
  --cwd D                         pick one of several recorded checkouts, or use
                                  an unrecorded directory
Record checkouts with ` + "`pyre project update P --checkout node:/abs/path:R:shared`" + `.
When a shared checkout is already in use, the other sessions are listed so you
can tell them what you will change.`,
		Args: cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			sp.OperationID = a.opID
			if sp.Repo != "" && sp.Project == "" && sp.Cwd == "" { // W-quicklaunch
				return usagef("--repo names a project repository and needs --project; to start in a repository by name use `pyre new %s`", sp.Repo)
			}
			if sp.Prompt, err = a.readArg(sp.Prompt); err != nil {
				return err
			}
			res, err := ops.StartSession(cmd.Context(), sp)
			if err != nil {
				return err
			}
			a.warnSharedCheckout(cmd.Context(), ops, res, sp.Worktree != "")
			return a.emit(res, func(w io.Writer) {
				s := res.Session
				fmt.Fprintf(w, "started session %s", s.Session.SessionID)
				if s.Session.Title != "" {
					fmt.Fprintf(w, " %q", s.Session.Title)
				}
				fmt.Fprintf(w, " (%s)", s.Session.Harness)
				if s.Run != nil {
					fmt.Fprintf(w, " on %s, run %s: %s", orDash(s.NodeName), s.Run.RunID, s.Run.State)
					if s.Run.AccountID != "" {
						fmt.Fprintf(w, ", account %s", s.Run.AccountID)
					}
				}
				fmt.Fprintln(w)
				if res.CommandID != "" {
					fmt.Fprintf(w, "launch command %s queued; the run state shows the outcome\n", res.CommandID)
				}
				fmt.Fprintf(w, "next: pyre read %s --tail 40 | pyre attach %s | pyre send %s \"...\"\n", s.Session.SessionID, s.Session.SessionID, s.Session.SessionID)
			})
		}),
	}
	sf := start.Flags()
	sf.StringVar(&sp.Node, "node", "", "node ID or name (default: this session's node, or the only connected node)")
	sf.StringVar(&sp.Harness, "harness", "", "claude, codex or shell")
	sf.StringVar(&sp.Cwd, "cwd", "", "absolute working directory on the node")
	sf.StringVar(&sp.Project, "project", "", "optional project ID or name")
	sf.StringVar(&sp.Worktree, "worktree", "", "create/use an isolated git worktree on this branch")
	sf.StringVar(&sp.Repo, "repo", "", "project repo ID: run in the project's recorded checkout of it on the node (shared), or the --worktree source")
	sf.StringVar(&sp.Account, "account", "", "explicit account: ID, label or rota name, or a unique prefix (fails if ineligible)")
	sf.BoolVar(&sp.Ambient, "ambient", false, "use the node's existing default login (no Pyre account assignment)")
	sf.StringVar(&sp.Permissions, "permissions", "", "ask (harness approval prompts) or bypass (Claude --dangerously-skip-permissions, Codex no approvals and no sandbox); default: the node's pyred --permissions")
	sf.StringVar(&sp.Prompt, "prompt", "", "initial prompt (- reads stdin)")
	sf.StringVar(&sp.Title, "title", "", "session title")
	sf.BoolVar(&sp.IncludeContext, "context", false, "include the bounded project brief at launch")
	sf.StringSliceVar(&sp.Tags, "tag", nil, "tags")
	var noReport bool
	sf.BoolVar(&noReport, "no-report", false, "do not report back to the starting session (turn finished, needs input, exited)")
	start.PreRun = func(cmd *cobra.Command, args []string) {
		if noReport {
			f := false
			sp.ReportToParent = &f
		}
	}
	root.AddCommand(start)

	var readOnly, takeover, printOnly, relay bool
	attach := &cobra.Command{
		Use:   "attach <session>",
		Short: "Attach to a session's real terminal (native tmux), or view it read-only",
		Long: `Attach to the session's current run through the mechanism the node
advertises. Native tmux attach runs the descriptor's command with this
terminal and holds an input lease while attached (other clients see it; raw
writes from others queue). Structured peer messages continue while attached.
Detach with the tmux prefix (usually Ctrl-b d); the session keeps running.

A node with no ssh route (no ssh alias, e.g. a Windows cloud VM) is attached
through the hub's terminal relay instead, when this client talks to the hub
directly: the same input lease, this terminal in raw mode, its size
followed. Detach with Ctrl-b d (Ctrl-b Ctrl-b sends one Ctrl-b). --relay
uses the relay even when ssh would work.`,
		Args: cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			return a.attach(cmd.Context(), args[0], attachOpts{readOnly: readOnly, takeover: takeover, printOnly: printOnly, relay: relay})
		}),
	}
	attach.Flags().BoolVar(&readOnly, "read-only", false, "view without taking input")
	attach.Flags().BoolVar(&takeover, "takeover", false, "take over an input lease held by another client (recorded)")
	attach.Flags().BoolVar(&printOnly, "print", false, "print the attach descriptor instead of attaching")
	attach.Flags().BoolVar(&relay, "relay", false, "attach through the hub's terminal relay instead of ssh")
	root.AddCommand(attach)

	var rp ReadParams
	var follow, screen bool
	read := &cobra.Command{
		Use:   "read <session> [--tail N] [--follow]",
		Short: "Read a session's bounded, redacted transcript",
		Args:  cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			rp.Session = args[0]
			if rp.Tail == 0 && rp.Cursor == "" {
				rp.Tail = 40
			}
			return a.read(cmd.Context(), ops, rp, follow, screen)
		}),
	}
	read.Flags().IntVar(&rp.Tail, "tail", 0, "latest N entries (default 40)")
	read.Flags().StringVar(&rp.Cursor, "cursor", "", "continue after a previous next_cursor")
	read.Flags().StringVar(&rp.Run, "run", "", "a specific run (default current)")
	read.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing new entries")
	read.Flags().BoolVar(&screen, "screen", false, "also print the current terminal snapshot")
	root.AddCommand(read)

	for _, which := range []string{"interrupt", "stop"} {
		which := which
		var reason string
		short := "Interrupt the current agent turn"
		if which == "stop" {
			short = "Stop the session's current run (the session remains resumable)"
		}
		cc := &cobra.Command{
			Use:   which + " <session>",
			Short: short,
			Args:  cobra.ExactArgs(1),
			RunE: a.run(func(cmd *cobra.Command, args []string) error {
				ops, err := a.connect()
				if err != nil {
					return err
				}
				p := SessionCmdParams{Session: args[0], Reason: reason, OperationID: a.opID}
				var rc c.CommandReceipt
				if which == "stop" {
					rc, err = ops.Stop(cmd.Context(), p)
				} else {
					rc, err = ops.Interrupt(cmd.Context(), p)
				}
				if err != nil {
					return err
				}
				if err := a.emit(rc, func(w io.Writer) { printReceipt(w, which, rc) }); err != nil {
					return err
				}
				return outcomeStatus(rc.Outcome, rc.Detail)
			}),
		}
		cc.Flags().StringVar(&reason, "reason", "", "reason recorded with the command")
		root.AddCommand(cc)
	}

	var rsp ResumeParams
	resume := &cobra.Command{
		Use:   "resume <session> [--allow-handoff]",
		Short: "Start a new run that continues a session (native resume, or explicit handoff)",
		Args:  cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			rsp.Session, rsp.OperationID = args[0], a.opID
			res, err := ops.Resume(cmd.Context(), rsp)
			if err != nil {
				if c.IsCode(err, c.ErrUnsupported) && !rsp.AllowHandoff {
					a.warnf("native resume is unsupported here; --allow-handoff starts a reconstructed (not identical) continuation")
				}
				return err
			}
			return a.emit(res, func(w io.Writer) { printResume(w, res) })
		}),
	}
	resume.Flags().StringVar(&rsp.Node, "node", "", "node for the new run (default: the previous run's)")
	resume.Flags().StringVar(&rsp.Account, "account", "", "explicit account: ID, label or rota name, or a unique prefix")
	resume.Flags().BoolVar(&rsp.Ambient, "ambient", false, "use the node's existing login")
	resume.Flags().BoolVar(&rsp.AllowHandoff, "allow-handoff", false, "permit a reconstructed continuation when native resume is unsupported")
	root.AddCommand(resume)

	var ap AssociateParams
	associate := &cobra.Command{
		Use:   "associate <session> --project P | --none",
		Short: "Associate a session with a project after the fact, or detach it",
		Args:  cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			ap.Session, ap.OperationID = args[0], a.opID
			sv, err := ops.Associate(cmd.Context(), ap)
			if err != nil {
				return err
			}
			return a.emit(sv, func(w io.Writer) {
				fmt.Fprintf(w, "session %s project: %s\n", sv.Session.SessionID, strPtr(sv.Session.ProjectID))
			})
		}),
	}
	associate.Flags().StringVar(&ap.Project, "project", "", "project ID or name")
	associate.Flags().BoolVar(&ap.None, "none", false, "detach from any project (standalone)")
	root.AddCommand(associate)
}

// ---- attach ----

type attachOpts struct {
	readOnly, takeover, printOnly bool
	relay                         bool // --relay: the hub's terminal relay, never ssh
}

func (a *App) attach(ctx context.Context, ref string, o attachOpts) error {
	ops, err := a.connect()
	if err != nil {
		return err
	}
	sv, err := ops.ResolveSession(ctx, ref)
	if err != nil {
		return err
	}
	req := c.AttachRequest{Mutation: mut(a.opID), SessionID: sv.Session.SessionID, ClientNodeID: ops.Self.NodeID, ReadOnly: o.readOnly,
		Preferred: []c.AttachMechanism{c.AttachLocalTmux, c.AttachSSHTmux, c.AttachWebTerminal, c.AttachReadOnly}}
	td := a.relayDialer(ops)
	if o.relay {
		if td == nil {
			return c.Errorf(c.ErrUnsupported, "--relay needs a direct hub connection (--hub, PYRE_HUB or the client config); the local daemon socket has no terminal relay")
		}
		return a.attachRelay(ctx, ops, td, sv, req, o)
	}
	d, err := ops.API.AttachSession(ctx, req)
	if err != nil {
		if td != nil && terminal.NoSSHRoute(err) {
			// No ssh to that node (a Windows cloud VM has no ssh server):
			// its terminal is still reachable through the hub.
			a.warnf("pyre: no ssh route to the session's node; attaching through the hub's terminal relay")
			return a.attachRelay(ctx, ops, td, sv, req, o)
		}
		return err
	}
	if printOnly := o.printOnly || a.jsonOut; printOnly {
		return a.emit(d, func(w io.Writer) { printDescriptor(w, d) })
	}
	switch d.Mechanism {
	case c.AttachLocalTmux, c.AttachSSHTmux:
		if td != nil && !runnableHere(d) {
			a.warnf("pyre: %q is not on this machine; attaching through the hub's terminal relay", d.Command[0])
			return a.attachRelay(ctx, ops, td, sv, req, o)
		}
		return a.attachNative(ctx, ops, d, o.takeover)
	case c.AttachWebTerminal:
		if td != nil {
			return a.attachRelay(ctx, ops, td, sv, req, o)
		}
		fmt.Fprintf(a.Stdout, "this run is reachable through the authenticated web terminal: %s\n", d.URL)
		fmt.Fprintln(a.Stdout, "open it from the Android app or a Pyre web client (its one-time ticket is presented by the client, never in the URL)")
		if d.Notes != "" {
			fmt.Fprintln(a.Stdout, d.Notes)
		}
		return nil
	default:
		if d.Notes != "" {
			a.warnf("%s", d.Notes)
		}
		a.warnf("read-only view of run %s (pyre read %s --follow for live output)", d.RunID, sv.Session.SessionID)
		return a.read(ctx, ops, ReadParams{Session: sv.Session.SessionID, Run: d.RunID, Tail: 40}, false, true)
	}
}

// runnableHere reports whether a native descriptor's command exists on
// this machine.
func runnableHere(d c.AttachDescriptor) bool {
	if len(d.Command) == 0 {
		return true // attachNative reports the malformed descriptor
	}
	_, err := exec.LookPath(d.Command[0])
	return err == nil
}

func (a *App) attachNative(ctx context.Context, ops *Ops, d c.AttachDescriptor, takeover bool) error {
	if len(d.Command) == 0 {
		return c.Errorf(c.ErrInternal, "attach descriptor for run %s has no command", d.RunID)
	}
	if _, err := exec.LookPath(d.Command[0]); err != nil {
		return c.Errorf(c.ErrUnsupported, "attach needs %q on this machine: %v", d.Command[0], err)
	}
	var lease *c.InputLease
	if !d.ReadOnly {
		l, err := ops.API.AcquireLease(ctx, c.LeaseRequest{Mutation: mut(""), RunID: d.RunID, TTLSeconds: 120, Takeover: takeover})
		if err != nil {
			if c.IsCode(err, c.ErrConflict) && !takeover {
				a.warnf("another client holds this run's input lease; attach --read-only to watch, or --takeover to take over (recorded)")
			}
			return err
		}
		lease = &l
		if l.TakeoverOf != "" {
			a.warnf("took over input lease %s", l.TakeoverOf)
		}
	}
	if d.Notes != "" {
		a.warnf("%s", d.Notes)
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	if lease != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t := time.NewTicker(40 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
					rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
					if _, err := ops.API.AcquireLease(rctx, c.LeaseRequest{Mutation: mut(""), RunID: d.RunID, TTLSeconds: 120}); err != nil {
						a.warnf("pyre: could not renew input lease: %v", err)
					}
					cancel()
				}
			}
		}()
	}
	runErr := a.Exec(d.Command)
	close(stop)
	wg.Wait()
	if lease != nil {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		if _, err := ops.API.ReleaseLease(rctx, c.LeaseReleaseRequest{Mutation: mut(""), LeaseID: lease.LeaseID}); err != nil {
			a.warnf("pyre: could not release input lease %s (it expires on its own): %v", lease.LeaseID, err)
		}
		cancel()
	}
	if runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			return withStatus(ExitError, fmt.Errorf("attach command exited with status %d", ee.ExitCode()))
		}
		return withStatus(ExitError, fmt.Errorf("attach command: %w", runErr))
	}
	return nil
}

// ---- read ----

func (a *App) read(ctx context.Context, ops *Ops, p ReadParams, follow, screen bool) error {
	resp, err := ops.ReadSession(ctx, p)
	if err != nil {
		return err
	}
	if !follow {
		return a.emit(resp, func(w io.Writer) { printTranscript(w, resp, screen) })
	}
	_ = a.emitLine(resp, func(w io.Writer) { printTranscript(w, resp, screen) })
	cursor := resp.NextCursor
	for {
		t := time.NewTimer(a.PollInterval)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
		next, err := ops.ReadSession(ctx, ReadParams{Session: p.Session, Run: p.Run, Cursor: cursor})
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if c.IsCode(err, c.ErrUnavailable) {
				a.warnf("pyre: %v (retrying)", err)
				continue
			}
			return err
		}
		if next.NextCursor != "" {
			cursor = next.NextCursor
		}
		if len(next.Entries) == 0 && !next.Gap {
			continue
		}
		_ = a.emitLine(next, func(w io.Writer) { printTranscript(w, next, false) })
	}
}

// ---- formatting ----

func runStateText(sv c.SessionView) string {
	if sv.Run == nil {
		return "no run"
	}
	s := string(sv.Run.State)
	if sv.Freshness.Stale || sv.Run.Freshness.Stale {
		s += "? (stale " + ago(sv.Run.Freshness.ObservedAt) + ")"
	} else if a := sv.Run.Activity; a != nil && a.Text != "" && sv.Run.State == c.RunRunning {
		s += ": " + a.Text // "doing now" (v1.9)
	}
	return s
}

func printSessions(w io.Writer, ss []c.SessionView) {
	sort.SliceStable(ss, func(i, j int) bool { return ss[i].Session.CreatedAt.After(ss[j].Session.CreatedAt) })
	var rows [][]string
	for _, s := range ss {
		node, updated := orDash(s.NodeName), "-"
		if s.Run != nil {
			if node == "-" {
				node = s.Run.NodeID
			}
			updated = ago(s.Run.Freshness.ObservedAt)
		}
		attn := ""
		if s.Attention > 0 {
			attn = fmt.Sprintf("%d", s.Attention)
		}
		rows = append(rows, []string{s.Session.SessionID, clip(orDash(s.Session.Title), 32), string(s.Session.Harness), node, runStateText(s), updated, strPtr(s.Session.ProjectID), attn})
	}
	table(w, "ID\tTITLE\tHARNESS\tNODE\tSTATE\tUPDATED\tPROJECT\tATTN", rows)
}

func printSession(w io.Writer, sv c.SessionView) {
	s := sv.Session
	fmt.Fprintf(w, "%s  %s\n", s.SessionID, orDash(s.Title))
	fmt.Fprintf(w, "  harness:   %s   project: %s   created %s by %s %s\n", s.Harness, strPtr(s.ProjectID), ago(s.CreatedAt), s.CreatedBy.Kind, s.CreatedBy.ID)
	if s.Lineage.ParentSessionID != "" || s.Lineage.ForkedFrom != "" || s.Lineage.HandoffNoteID != "" {
		fmt.Fprintf(w, "  lineage:   parent %s, forked from %s, handoff %s\n", orDash(s.Lineage.ParentSessionID), orDash(s.Lineage.ForkedFrom), orDash(s.Lineage.HandoffNoteID))
	}
	if s.Lineage.ReparentedAt != nil { // W-reparent
		fmt.Fprintf(w, "  reparented: from %s at %s\n", orDash(s.Lineage.ReparentedFrom), s.Lineage.ReparentedAt.Format(time.RFC3339))
	}
	if sv.Attention > 0 {
		fmt.Fprintf(w, "  attention: %d open item(s) (pyre attention --session %s)\n", sv.Attention, s.SessionID)
	}
	r := sv.Run
	if r == nil {
		fmt.Fprintln(w, "  run:       none")
		return
	}
	fmt.Fprintf(w, "  run:       %s on %s (%s), adapter %s\n", r.RunID, orDash(sv.NodeName), r.NodeID, orDash(r.Adapter))
	state := string(r.State)
	if r.StateReason != "" {
		state += " (" + r.StateReason + ")"
	}
	fmt.Fprintf(w, "  state:     %s, observed %s via %s", state, ago(r.Freshness.ObservedAt), orDash(r.Freshness.Source))
	if r.Freshness.Stale {
		fmt.Fprintf(w, " STALE: %s (not a failure; the observer lost contact)", orDash(r.Freshness.Reason))
	}
	fmt.Fprintln(w)
	if a := r.Activity; a != nil && a.Text != "" && r.State == c.RunRunning && !r.Freshness.Stale {
		fmt.Fprintf(w, "  doing:     %s (started %s)\n", a.Text, ago(a.Since))
	}
	if u := r.Usage; u != nil {
		fmt.Fprintf(w, "  usage:     %s (sampled %s)\n", sysstats.Detail(*u), ago(u.ObservedAt))
	}
	fmt.Fprintf(w, "  cwd:       %s\n", orDash(r.Cwd))
	if r.Checkout != nil {
		fmt.Fprintf(w, "  checkout:  %s %s branch %s repo %s\n", r.Checkout.Mode, r.Checkout.Path, orDash(r.Checkout.Branch), orDash(r.Checkout.RepoID))
	}
	fmt.Fprintf(w, "  runtime:   %s %s\n", r.Runtime.Harness, orDash(r.Runtime.Version))
	if r.AccountID != "" {
		fmt.Fprintf(w, "  account:   %s (assignment %s)\n", r.AccountID, orDash(r.AssignmentID))
	}
	if r.ContinuationKind != "" {
		fmt.Fprintf(w, "  continues: %s from run %s\n", r.ContinuationKind, orDash(r.PreviousRunID))
	}
	if r.Lease != nil && r.Lease.Active(time.Now()) {
		fmt.Fprintf(w, "  input:     leased by %s %s until %s\n", r.Lease.Holder.Kind, r.Lease.Holder.ID, stamp(r.Lease.ExpiresAt))
	}
	if len(r.PendingApprovalIDs) > 0 {
		fmt.Fprintf(w, "  approvals: %s (pyre approve <id>)\n", strings.Join(r.PendingApprovalIDs, ", "))
	}
	for _, sa := range r.Subagents {
		fmt.Fprintf(w, "  subagent:  %s %s %s %s\n", sa.ID, orDash(sa.Kind), sa.State, clip(sa.Summary, 60))
	}
	if p := r.AdapterData["pyre.permissions"]; p != "" {
		desc := p
		if p == "bypass" {
			desc += " (no approval prompts, no sandbox)"
		}
		fmt.Fprintf(w, "  perms:     %s\n", desc)
	}
	if len(r.Capabilities) > 0 {
		fmt.Fprintf(w, "  caps:      %s\n", strings.Join(r.Capabilities, ", "))
	}
}

func printTranscript(w io.Writer, resp c.ReadSessionResponse, screen bool) {
	if resp.Gap {
		fmt.Fprintln(w, "[gap: earlier entries were lost or expired]")
	}
	for _, e := range resp.Entries {
		text := e.Text
		if e.Truncated {
			text += " [truncated]"
		}
		fmt.Fprintf(w, "%s %-9s %s\n", e.At.Local().Format("15:04:05"), e.Kind, strings.TrimRight(text, "\n"))
	}
	if resp.Freshness.Stale {
		fmt.Fprintf(w, "[stale: observed %s via %s]\n", ago(resp.Freshness.ObservedAt), resp.Freshness.Source)
	}
	if screen && resp.Screen != "" {
		fmt.Fprintln(w, "---- screen ----")
		fmt.Fprintln(w, strings.TrimRight(resp.Screen, "\n"))
	}
}

func printReceipt(w io.Writer, what string, rc c.CommandReceipt) {
	acc := "accepted"
	if !rc.Accepted {
		acc = "NOT accepted"
	}
	fmt.Fprintf(w, "%s %s (command %s): outcome %s", what, acc, rc.CommandID, rc.Outcome)
	switch rc.Outcome {
	case c.OutcomePending:
		fmt.Fprint(w, " (queued for the node; effect not yet observed)")
	case c.OutcomeDelivered:
		fmt.Fprint(w, " (node has the command; effect not yet observed)")
	case c.OutcomeIndeterminate:
		fmt.Fprint(w, " (UNKNOWN whether it took effect; check the run state)")
	}
	if rc.Detail != "" {
		fmt.Fprintf(w, ": %s", rc.Detail)
	}
	fmt.Fprintln(w)
}

func printDescriptor(w io.Writer, d c.AttachDescriptor) {
	fmt.Fprintf(w, "run %s on %s: %s", d.RunID, d.NodeID, d.Mechanism)
	if d.ReadOnly {
		fmt.Fprint(w, " (read-only)")
	}
	fmt.Fprintln(w)
	if len(d.Command) > 0 {
		fmt.Fprintf(w, "command: %s\n", strings.Join(d.Command, " "))
	}
	if d.URL != "" {
		fmt.Fprintf(w, "url: %s\n", d.URL)
	}
	if d.Lease != nil {
		fmt.Fprintf(w, "input lease: %s %s until %s\n", d.Lease.Holder.Kind, d.Lease.Holder.ID, stamp(d.Lease.ExpiresAt))
	}
	if d.Notes != "" {
		fmt.Fprintln(w, d.Notes)
	}
}

// printResume reports a queued resume. The response comes back once the new
// run is recorded and its launch queued; whether it actually resumed is
// the run state's to say, so this never claims success.
func printResume(w io.Writer, res c.StartSessionResponse) {
	s := res.Session
	fmt.Fprintf(w, "resume of session %s queued", s.Session.SessionID)
	if s.Run != nil {
		kind := string(s.Run.ContinuationKind)
		fmt.Fprintf(w, ": run %s on %s: %s (%s", s.Run.RunID, orDash(s.NodeName), s.Run.State, orDash(kind))
		if s.Run.ContinuationKind == c.ContinuationHandoff {
			fmt.Fprint(w, ": reconstructed context, not the same native thread")
		}
		fmt.Fprintf(w, "), previous run %s", orDash(s.Run.PreviousRunID))
	}
	fmt.Fprintln(w)
	if res.CommandID != "" {
		fmt.Fprintf(w, "launch command %s queued; the run state shows the outcome (pyre sessions show %s)\n", res.CommandID, s.Session.SessionID)
	}
}
