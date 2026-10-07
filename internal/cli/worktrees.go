package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	c "github.com/jhh3/pyre/internal/contracts"
)

// Session worktrees (W-quicklaunch): `pyre sessions show` lists the
// worktrees of the repositories a session touched; archive and prune offer
// to remove the ones created during the session. The node re-checks every
// rule when removing: never the main checkout, never a worktree with
// uncommitted changes, never a branch that is neither merged nor pushed
// (the folder goes, the branch stays). The default answer is keep.

// SessionWorktrees returns a session's worktrees: a fresh scan on its
// machine when live is set and the machine answers, else the last report.
func (o *Ops) SessionWorktrees(ctx context.Context, sv c.SessionView, live bool) (c.SessionWorktrees, error) {
	if live && sv.Run != nil {
		var sw c.SessionWorktrees
		if _, err := o.NodeAction(ctx, sv.Run.NodeID, c.ActionWorktreeScan, c.WorktreeScanParams{SessionID: sv.Session.SessionID}, &sw, ""); err == nil {
			return sw, nil
		}
	}
	r, err := o.reposAPI()
	if err != nil {
		return c.SessionWorktrees{}, err
	}
	return r.SessionWorktrees(ctx, c.SessionWorktreesRequest{SessionID: sv.Session.SessionID})
}

// worktreeCandidates splits a session's worktrees created during it into
// removable (clean) and kept (uncommitted changes).
func worktreeCandidates(sw c.SessionWorktrees) (removable, dirty []c.Worktree) {
	for _, r := range sw.Repos {
		for _, w := range r.Worktrees {
			if w.Main || !w.CreatedInSession {
				continue
			}
			if w.Dirty {
				dirty = append(dirty, w)
			} else {
				removable = append(removable, w)
			}
		}
	}
	return removable, dirty
}

func wtLine(w c.Worktree) string {
	b := w.Branch
	if b == "" {
		b = "detached " + w.Head
	}
	note := ""
	switch {
	case w.Dirty:
		note = "uncommitted changes"
	case w.Merged:
		note = "merged"
	case w.Pushed:
		note = "pushed"
	case w.Branch != "":
		note = "not merged or pushed: the branch will be kept"
	}
	if note != "" {
		note = "  (" + note + ")"
	}
	return w.Path + "  " + b + note
}

// printSessionWorktrees is the Worktrees section of `pyre sessions show`.
func printSessionWorktrees(w io.Writer, sw c.SessionWorktrees) {
	if len(sw.Repos) == 0 {
		return
	}
	for _, r := range sw.Repos {
		fmt.Fprintf(w, "  worktrees of %s (%s):\n", r.Name, r.Path)
		for _, t := range r.Worktrees {
			tag := "     "
			switch {
			case t.Main:
				tag = "main "
			case t.PyreCreated:
				tag = "pyre "
			case t.CreatedInSession:
				tag = "new  "
			}
			b := t.Branch
			if b == "" {
				b = "detached " + t.Head
			}
			extra := ""
			if t.CreatedInSession {
				var fl []string
				if t.Dirty {
					fl = append(fl, "uncommitted changes")
				}
				if t.Merged {
					fl = append(fl, "merged")
				} else if t.Pushed {
					fl = append(fl, "pushed")
				}
				if len(fl) > 0 {
					extra = " (" + strings.Join(fl, ", ") + ")"
				}
			}
			fmt.Fprintf(w, "    %s %s  %s%s\n", tag, t.Path, b, extra)
		}
	}
	fmt.Fprintln(w, "    (main: the repository's checkout; new: created during this session; pyre: created by pyre new --fresh)")
}

// worktreeMode is how archive/prune treat a session's worktrees.
type worktreeMode struct{ remove, keep bool }

func addWorktreeFlags(cmd *cobra.Command) *worktreeMode {
	m := &worktreeMode{}
	cmd.Flags().BoolVar(&m.remove, "remove-worktrees", false, "also remove the worktrees these sessions created (clean ones only; unmerged, unpushed branches are kept)")
	cmd.Flags().BoolVar(&m.keep, "keep-worktrees", false, "do not offer to remove the sessions' worktrees")
	return m
}

// afterArchive offers to remove the worktrees the archived sessions
// created. It asks on a terminal (default keep), removes them with
// --remove-worktrees, and otherwise only says what remains.
func (a *App) afterArchive(ctx context.Context, ops *Ops, done []c.SessionView, mode *worktreeMode) {
	if mode == nil || mode.keep || len(done) == 0 {
		return
	}
	if mode.remove && mode.keep {
		return
	}
	type plan struct {
		sv        c.SessionView
		removable []c.Worktree
		dirty     []c.Worktree
	}
	var plans []plan
	total := 0
	for _, sv := range done {
		sw, err := ops.SessionWorktrees(ctx, sv, true)
		if err != nil {
			continue
		}
		rm, dirty := worktreeCandidates(sw)
		if len(rm)+len(dirty) == 0 {
			continue
		}
		plans = append(plans, plan{sv, rm, dirty})
		total += len(rm)
	}
	if len(plans) == 0 {
		return
	}
	for _, p := range plans {
		a.warnf("worktrees created by %s %q:", p.sv.Session.SessionID, p.sv.Session.Title)
		for _, w := range p.removable {
			a.warnf("  %s", wtLine(w))
		}
		for _, w := range p.dirty {
			a.warnf("  %s  — kept", wtLine(w))
		}
	}
	if total == 0 {
		a.warnf("nothing to remove: every worktree above has uncommitted changes")
		return
	}
	switch {
	case mode.remove:
	case a.IsTerminal() && !a.jsonOut:
		fmt.Fprintf(a.Stderr, "remove the %d clean worktree(s) above? the main checkout and anything uncommitted stay [y/N] ", total)
		line, _ := bufio.NewReader(a.Stdin).ReadString('\n')
		if ans := strings.ToLower(strings.TrimSpace(line)); ans != "y" && ans != "yes" {
			a.warnf("kept the worktrees")
			return
		}
	default:
		a.warnf("kept; --remove-worktrees removes the clean ones")
		return
	}
	for _, p := range plans {
		if len(p.removable) == 0 || p.sv.Run == nil {
			continue
		}
		paths := make([]string, len(p.removable))
		for i, w := range p.removable {
			paths[i] = w.Path
		}
		var res c.WorktreeRemoveResult
		if _, err := ops.NodeAction(ctx, p.sv.Run.NodeID, c.ActionWorktreeRemove, c.WorktreeRemoveParams{SessionID: p.sv.Session.SessionID, Paths: paths}, &res, ""); err != nil {
			a.warnf("could not remove worktrees of %s on %s: %v (nothing removed there)", p.sv.Session.SessionID, orDash(p.sv.NodeName), err)
			continue
		}
		for _, r := range res.Results {
			a.warnf("  %s", removalLine(r))
		}
	}
}

func removalLine(r c.WorktreeRemoval) string {
	name := filepath.Base(r.Path)
	switch r.Outcome {
	case "removed":
		switch r.BranchOutcome {
		case "deleted":
			return "removed " + name + " and its branch " + r.Branch
		case "kept_unmerged":
			return "removed " + name + "; kept branch " + r.Branch + " (not merged or pushed)"
		}
		return "removed " + name
	case "kept_dirty":
		return "kept " + name + ": uncommitted changes"
	case "kept_main":
		return "kept " + name + ": main checkout"
	case "kept_not_session":
		return "kept " + name + ": " + r.Detail
	case "missing":
		return name + ": already gone"
	}
	return "failed " + name + ": " + r.Detail
}

// showWorktrees prints the session's worktree section (sessions show).
func (a *App) showWorktrees(ctx context.Context, ops *Ops, sv c.SessionView) {
	if a.jsonOut {
		return
	}
	sw, err := ops.SessionWorktrees(ctx, sv, false)
	if err != nil {
		return
	}
	printSessionWorktrees(a.Stdout, sw)
}
