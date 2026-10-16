package claude

// Plugin prompt delivery (capability input.plugin_prompt, evidence
// claude.plugin.prompt_submit). The run's generated plugin carries a hooks
// module (claudeplugin.ModuleSource) that submits queued Pyre messages with
// Claude Code's $.prompt.submit between turns. Nothing is typed into the
// TUI, so it needs no input lease: a human may be attached or typing.
//
// Deliver spools the marked text into <state>/plugin-inbox (atomic), the
// module claims it through `pyred hook claude PyrePluginClaim` (rename to
// .claimed) when no turn runs and reports PyrePluginSubmitted once Claude
// resolved the submit (the prompt entered; its turn started). Unclaimed
// spool entries are withdrawn (rename to .withdrawn) when the attempt ends,
// so a message is never both withdrawn and submitted, and never replayed.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/claudeplugin"
	"github.com/jhh3/pyre/internal/runtime/tmux"
)

// pluginAliveFor is how fresh the module's heartbeat must be.
const pluginAliveFor = 4 * claudeplugin.AliveEvery * time.Second

// PluginAlive reports whether the run's delivery module is loaded and
// running: it rewrote <state>/plugin-alive.json within pluginAliveFor.
func PluginAlive(stateDir string, now time.Time) bool {
	st, err := os.Stat(filepath.Join(stateDir, claudeplugin.AliveFile))
	return err == nil && now.Sub(st.ModTime()) < pluginAliveFor
}

func (a *Adapter) pluginLive(h contracts.RunHandle) bool {
	return !a.o.DisablePluginDelivery && PluginAlive(h.StateDir, a.o.Now())
}

// resetPluginSpool clears a previous launch's module heartbeat and
// withdraws spool entries it never claimed (their attempts end rejected).
func (a *Adapter) resetPluginSpool(stateDir string) {
	_ = os.Remove(filepath.Join(stateDir, claudeplugin.AliveFile))
	dir := filepath.Join(stateDir, claudeplugin.SpoolDir)
	ents, _ := os.ReadDir(dir)
	for _, en := range ents {
		if n := en.Name(); strings.HasSuffix(n, ".msg") {
			p := filepath.Join(dir, n)
			_ = os.Rename(p, strings.TrimSuffix(p, ".msg")+".withdrawn")
		}
	}
}

// pluginReady is InputReady for the plugin path: the module submits only
// between turns, so the engine keeps mail queued while a turn or an
// approval is open instead of spooling it.
func (a *Adapter) pluginReady(h contracts.RunHandle) (bool, string) {
	state, reason, _ := a.derived(h)
	switch state {
	case contracts.RunWaitingInput, contracts.RunWaitingBackground:
		return true, ""
	}
	return false, "plugin delivery waits until the turn ends: " + string(state) + " (" + reason + ")"
}

func (a *Adapter) pluginFailure(h contracts.RunHandle, msgID string) (pluginFailure, bool) {
	e := a.entry(h)
	e.mu.Lock()
	defer e.mu.Unlock()
	a.refresh(h, e)
	f, ok := e.rs.pluginFailed[msgID]
	return f, ok
}

func (a *Adapter) deliverPlugin(ctx context.Context, h contracts.RunHandle, in contracts.DeliveryInput) (contracts.DeliveryResult, error) {
	p, err := a.srv.Pane(ctx, h.TmuxTarget)
	if err != nil {
		if errors.Is(err, tmux.ErrNoSession) || errors.Is(err, tmux.ErrNoServer) {
			return rejected("run is gone (tmux session missing)"), nil
		}
		return contracts.DeliveryResult{}, contracts.Errorf(contracts.ErrUnavailable, "tmux: %v", err)
	}
	if p.Dead {
		return rejected("claude has exited"), nil
	}
	text := renderInput(in)
	name := injectionName(a.o.Now().UnixNano(), in.MessageID) + ".msg"
	path := filepath.Join(h.StateDir, claudeplugin.SpoolDir, name)
	if err := a.journal(h, journalRec{MessageID: in.MessageID, AttemptID: in.AttemptID, State: jAttempting, Mode: "plugin", Ref: name}); err != nil {
		return contracts.DeliveryResult{}, contracts.Errorf(contracts.ErrUnavailable, "journal: %v", err)
	}
	if err := a.crash("plugin_after_journal"); err != nil {
		return contracts.DeliveryResult{}, err
	}
	inj := injection{MessageID: in.MessageID, AttemptID: in.AttemptID, Text: text, At: a.o.Now().UTC()}
	b, _ := json.Marshal(inj)
	err = os.MkdirAll(filepath.Dir(path), 0o700)
	if err == nil {
		err = writeFileAtomic(path, b)
	}
	if err != nil {
		_ = os.Remove(path)
		_ = a.journal(h, journalRec{MessageID: in.MessageID, AttemptID: in.AttemptID, State: jRejected, Detail: "plugin spool write failed: " + err.Error()})
		return rejected("plugin spool write failed (not delivered; safe to retry): " + err.Error()), nil
	}
	_ = a.journal(h, journalRec{MessageID: in.MessageID, AttemptID: in.AttemptID, State: jSubmitted, Mode: "plugin", Ref: name,
		Detail: "queued for the session's pyre-session plugin ($.prompt.submit between turns)"})
	if err := a.crash("plugin_after_spool"); err != nil {
		return contracts.DeliveryResult{}, err
	}
	return a.awaitPlugin(ctx, h, in, path)
}

// awaitPlugin waits for the module's report. An entry the module never
// claimed is withdrawn (not delivered, retried later); a claimed one with
// no report yet stays indeterminate until its evidence arrives as a receipt.
func (a *Adapter) awaitPlugin(ctx context.Context, h contracts.RunHandle, in contracts.DeliveryInput, path string) (contracts.DeliveryResult, error) {
	end := time.Now().Add(a.o.DeliverTimeout)
	claimed := strings.TrimSuffix(path, ".msg") + ".claimed"
	var other time.Time // when non-plugin evidence (prompt hook, transcript) was first seen
	for {
		if ev, ok := a.evidenceFor(h, in.MessageID); ok {
			// Claude's own UserPromptSubmit hook usually reports before the
			// module's report: prefer the module's evidence for a moment.
			if ev.Kind != contracts.EvidenceClaudePluginPrompt && (other.IsZero() || time.Since(other) < 3*time.Second) && ctx.Err() == nil {
				if other.IsZero() {
					other = time.Now()
				}
				time.Sleep(100 * time.Millisecond)
				continue
			}
			_ = a.journal(h, journalRec{MessageID: in.MessageID, AttemptID: in.AttemptID, State: jAccepted, Evidence: string(ev.Kind), Ref: ev.Ref})
			return accepted(ev, "submitted by the pyre-session plugin between turns ($.prompt.submit)"), nil
		}
		if f, ok := a.pluginFailure(h, in.MessageID); ok {
			if f.Event == claudeplugin.EvPluginDropped {
				_ = a.journal(h, journalRec{MessageID: in.MessageID, AttemptID: in.AttemptID, State: jRejected, Detail: "plugin prompt dropped: " + f.Reason})
				return rejected("a Claude prompt hook dropped the plugin's prompt (not entered; safe to retry): " + f.Reason), nil
			}
			_ = a.journal(h, journalRec{MessageID: in.MessageID, AttemptID: in.AttemptID, State: jIndeterminate, Detail: "plugin submit failed: " + f.Reason})
			return indeterminate("the plugin's $.prompt.submit failed after claiming the message (" + f.Reason + "); evidence found later resolves it (never auto-replayed)"), nil
		}
		if time.Now().After(end) || ctx.Err() != nil {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if os.Rename(path, strings.TrimSuffix(path, ".msg")+".withdrawn") == nil {
		_ = a.journal(h, journalRec{MessageID: in.MessageID, AttemptID: in.AttemptID, State: jRejected, Detail: "plugin did not take it in time; withdrawn"})
		return queued("the session's plugin did not take the message before the attempt ended (a turn was running); withdrawn unsent, delivered between turns later"), nil
	}
	if _, err := os.Stat(claimed); err == nil {
		// Claimed: Claude has it or is about to (a prompt submitted while a
		// turn runs waits for it to end). Its report arrives as a receipt.
		_ = a.journal(h, journalRec{MessageID: in.MessageID, AttemptID: in.AttemptID, State: jIndeterminate, Detail: "claimed by the plugin; submit not yet confirmed"})
		return indeterminate("claimed by the pyre-session plugin; Claude has not yet confirmed the prompt entered (it waits for the running turn); acceptance follows as a receipt (never auto-replayed)"), nil
	}
	_ = a.journal(h, journalRec{MessageID: in.MessageID, AttemptID: in.AttemptID, State: jIndeterminate, Detail: "plugin spool entry vanished"})
	return indeterminate("plugin spool entry is neither queued nor claimed; resolve explicitly (never auto-replayed)"), nil
}

// reconcilePlugin settles an earlier plugin attempt with no evidence:
// an unclaimed entry is withdrawn (rejected, safe to retry); a dropped one
// is rejected; a claimed one stays indeterminate.
func (a *Adapter) reconcilePlugin(h contracts.RunHandle, messageID string, last journalRec) contracts.DeliveryResult {
	dir := filepath.Join(h.StateDir, claudeplugin.SpoolDir)
	if f, ok := a.pluginFailure(h, messageID); ok && f.Event == claudeplugin.EvPluginDropped {
		_ = a.journal(h, journalRec{MessageID: messageID, AttemptID: last.AttemptID, State: jRejected, Detail: "plugin prompt dropped: " + f.Reason})
		return rejected("a Claude prompt hook dropped the plugin's prompt (not entered; safe to retry)")
	}
	ents, _ := os.ReadDir(dir)
	for _, en := range ents {
		n := en.Name()
		if !strings.Contains(n, "-"+messageID+".") {
			continue
		}
		p := filepath.Join(dir, n)
		switch {
		case strings.HasSuffix(n, ".msg"):
			if os.Rename(p, strings.TrimSuffix(p, ".msg")+".withdrawn") == nil {
				_ = a.journal(h, journalRec{MessageID: messageID, AttemptID: last.AttemptID, State: jRejected, Detail: "plugin spool entry withdrawn by reconcile"})
				return rejected("plugin delivery was still queued; withdrawn; safe to retry")
			}
		case strings.HasSuffix(n, ".claimed"):
			return indeterminate("claimed by the pyre-session plugin with no confirmation that the prompt entered; resolve explicitly (never auto-replayed)")
		}
	}
	return rejected("plugin delivery was withdrawn before the module claimed it (not delivered); safe to retry")
}
