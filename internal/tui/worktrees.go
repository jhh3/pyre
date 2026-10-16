package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/contracts"
)

// Session worktrees in the TUI (W-quicklaunch): the i overlay lists the
// worktrees of the repositories a session touched (as its machine last
// reported them); archiving a session (X) offers to remove the ones it
// created. The machine re-checks every rule when removing: never the main
// checkout, never anything uncommitted, never deleting a branch that is
// neither merged nor pushed. Enter, n and esc keep them (the default).

func wtKey(sid string) string { return "wt:" + sid }

// loadWorktrees fetches the session's last reported worktrees.
func (m *Model) loadWorktrees(sid string) tea.Cmd {
	r, ok := m.api.(contracts.ReposAPI)
	if !ok {
		return nil
	}
	return m.ensure(wtKey(sid), 30*time.Second, func(ctx context.Context) (any, error) {
		return r.SessionWorktrees(ctx, contracts.SessionWorktreesRequest{SessionID: sid})
	})
}

// worktreeInfoLines renders the Worktrees block of the info overlay.
func (m *Model) worktreeInfoLines(s contracts.SessionView, w int) []string {
	sw, st := get[contracts.SessionWorktrees](m, wtKey(s.Session.SessionID))
	if st == nil || !st.ok() || len(sw.Repos) == 0 {
		return nil
	}
	lw := 11
	out := []string{""}
	first := true
	for _, r := range sw.Repos {
		for _, t := range r.Worktrees {
			label := ""
			if first {
				label, first = "Worktrees", false
			}
			tag, note := "     ", ""
			switch {
			case t.Main:
				tag = m.th.dim.Render("main ")
			case t.PyreCreated:
				tag = m.th.tone(toneGood, "fresh")
			case t.CreatedInSession:
				tag = m.th.tone(toneGood, "new  ")
			}
			if t.CreatedInSession {
				switch {
				case t.Dirty:
					note = m.th.tone(toneWarn, " uncommitted")
				case t.Merged:
					note = m.th.dim.Render(" merged")
				case t.Pushed:
					note = m.th.dim.Render(" pushed")
				}
			}
			b := t.Branch
			if b == "" {
				b = "detached"
			}
			val := tag + " " + trunc(tildePath(t.Path), max(10, w-lw-30)) + "  " + m.th.dim.Render(b) + note
			out = append(out, m.kvl(label, val, lw))
		}
	}
	out = append(out, m.kvl("", m.th.dim.Render("new: created during this session · X offers to remove them"), lw))
	return out
}

// wtCleanup is the archive-time offer.
type wtCleanup struct {
	sv        contracts.SessionView
	removable []contracts.Worktree
	dirty     []contracts.Worktree
}

func (o *wtCleanup) hints() []hint { return []hint{{"y", "remove"}, {"n enter esc", "keep"}} }

func (o *wtCleanup) update(m *Model, k tea.KeyPressMsg) tea.Cmd {
	switch k.String() {
	case "y", "Y":
		m.overlay = nil
		return m.removeWorktrees(o.sv, o.removable)
	case "n", "N", "esc", "q", "enter":
		m.overlay = nil
		return m.setToast("kept the session's worktrees", toneMuted)
	}
	return nil
}

func wtRow(t contracts.Worktree) string {
	b := t.Branch
	if b == "" {
		b = "detached"
	}
	return tildePath(t.Path) + "  " + b
}

func (o *wtCleanup) view(m *Model, w, h int) []string {
	lines := []string{fmt.Sprintf("%q created %s:", trunc(o.sv.Session.Title, 40), plural(len(o.removable)+len(o.dirty), "worktree", "worktrees")), ""}
	for _, t := range o.removable {
		note := ""
		if !t.Merged && !t.Pushed && t.Branch != "" {
			note = m.th.dim.Render("  branch kept (not merged or pushed)")
		}
		lines = append(lines, "  "+trunc(wtRow(t), 60)+note)
	}
	for _, t := range o.dirty {
		lines = append(lines, "  "+m.th.dim.Render(trunc(wtRow(t), 60))+m.th.tone(toneWarn, "  kept: uncommitted changes"))
	}
	lines = append(lines, "", m.th.dim.Render("The main checkout and anything uncommitted always stay."),
		"", m.th.key.Render("y")+" remove the clean ones   "+m.th.key.Render("n")+" keep them (default)")
	return m.box("Remove this session's worktrees?", lines, 84, w, h)
}

// offerWorktreeCleanup scans the archived session's worktrees on its
// machine (falling back to the last report) and, if it created any, asks.
func (m *Model) offerWorktreeCleanup(s contracts.SessionView) tea.Cmd {
	r, ok := m.api.(contracts.ReposAPI)
	if !ok || s.Run == nil {
		return nil
	}
	api, sid, nid := m.api, s.Session.SessionID, s.Run.NodeID
	return m.do(func(ctx context.Context) (any, error) {
		params, _ := json.Marshal(contracts.WorktreeScanParams{SessionID: sid})
		res, err := api.InvokeNodeAction(ctx, contracts.NodeActionRequest{Mutation: contracts.Mutation{OperationID: contracts.NewID(contracts.PrefixOperation)},
			NodeID: nid, Action: contracts.ActionWorktreeScan, Params: params, TimeoutSeconds: 10})
		if err == nil && res.Error == nil && res.Outcome == contracts.OutcomeSucceeded {
			var sw contracts.SessionWorktrees
			if json.Unmarshal(res.Result, &sw) == nil {
				return sw, nil
			}
		}
		return r.SessionWorktrees(ctx, contracts.SessionWorktreesRequest{SessionID: sid})
	}, func(m *Model, v any, err error) tea.Cmd {
		if err != nil {
			return nil
		}
		sw := v.(contracts.SessionWorktrees)
		o := &wtCleanup{sv: s}
		for _, rw := range sw.Repos {
			for _, t := range rw.Worktrees {
				if t.Main || !t.CreatedInSession {
					continue
				}
				if t.Dirty {
					o.dirty = append(o.dirty, t)
				} else {
					o.removable = append(o.removable, t)
				}
			}
		}
		if len(o.removable) == 0 {
			if len(o.dirty) > 0 {
				return m.setToast(plural(len(o.dirty), "worktree", "worktrees")+" of this session kept: uncommitted changes", toneMuted)
			}
			return nil
		}
		// Never a modal that opens by itself: it would appear seconds
		// after the archive was confirmed, over whatever is selected by
		// then. W opens it, bound to this session.
		m.wtOffer = o
		return m.setToast(fmt.Sprintf("archived · W reviews / removes the %s %q created", plural(len(o.removable), "clean worktree", "clean worktrees"), trunc(s.Session.Title, 32)), toneMuted)
	})
}

// openWorktreeOffer is W: the last archive's worktree offer, if any.
func (m *Model) openWorktreeOffer() tea.Cmd {
	if m.wtOffer == nil {
		return m.setToast("no worktree cleanup pending (archiving a session that created worktrees offers one)", toneMuted)
	}
	m.overlay, m.wtOffer, m.toast = m.wtOffer, nil, nil
	return nil
}

func (m *Model) removeWorktrees(s contracts.SessionView, ws []contracts.Worktree) tea.Cmd {
	if s.Run == nil || len(ws) == 0 {
		return nil
	}
	paths := make([]string, len(ws))
	for i, t := range ws {
		paths[i] = t.Path
	}
	api, sid, nid := m.api, s.Session.SessionID, s.Run.NodeID
	op := m.opID("wtrm:" + sid)
	return m.do(func(ctx context.Context) (any, error) {
		params, _ := json.Marshal(contracts.WorktreeRemoveParams{SessionID: sid, Paths: paths})
		res, err := api.InvokeNodeAction(ctx, contracts.NodeActionRequest{Mutation: contracts.Mutation{OperationID: op},
			NodeID: nid, Action: contracts.ActionWorktreeRemove, Params: params, TimeoutSeconds: 30})
		if err != nil {
			return nil, err
		}
		if res.Error != nil {
			return nil, res.Error
		}
		var out contracts.WorktreeRemoveResult
		err = json.Unmarshal(res.Result, &out)
		return out, err
	}, func(m *Model, v any, err error) tea.Cmd {
		if err != nil {
			return m.errToast("worktrees not removed", err)
		}
		res := v.(contracts.WorktreeRemoveResult)
		removed, keptBranch, kept := 0, 0, 0
		for _, r := range res.Results {
			switch r.Outcome {
			case "removed":
				removed++
				if r.BranchOutcome == "kept_unmerged" {
					keptBranch++
				}
			case "missing":
			default:
				kept++
			}
		}
		msg := "removed " + plural(removed, "worktree", "worktrees")
		if keptBranch > 0 {
			msg += "; kept " + plural(keptBranch, "unmerged branch", "unmerged branches")
		}
		if kept > 0 {
			msg += "; kept " + itoa(kept) + " (changed since)"
		}
		delete(m.loads, wtKey(sid))
		return m.setToast(msg, toneGood)
	})
}
