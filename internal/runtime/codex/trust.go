package codex

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/tmux"
)

// Codex's folder-trust screen (0.160.1):
//
//	Folder access
//	/path/to/cwd
//	Trust this folder? Codex can read, edit, and run files here, ...
//	› 1. Trust and continue
//	  2. Back to Agent Command Center
//	enter continue · esc back
type trustScreen int

const (
	trustNone     trustScreen = iota // not the trust prompt
	trustSelected                    // prompt, "Trust and continue" selected
	trustOther                       // prompt, another option selected
)

// classifyTrustScreen reads a captured TUI screen.
func classifyTrustScreen(screen string) trustScreen {
	if !isCodexTrustScreen(screen) {
		return trustNone
	}
	for _, line := range strings.Split(screen, "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "›") || strings.HasPrefix(l, ">") || strings.HasPrefix(l, "❯") {
			if strings.Contains(strings.ToLower(l), "trust and continue") {
				return trustSelected
			}
			return trustOther
		}
	}
	return trustOther
}

// isCodexTrustScreen reports whether a captured TUI screen is Codex's
// folder-trust prompt.
func isCodexTrustScreen(screen string) bool {
	s := strings.ToLower(screen)
	return strings.Contains(s, "trust and continue") ||
		(strings.Contains(s, "trust this folder") && strings.Contains(s, "continue"))
}

// codexConversationScreen: the TUI is past start-up dialogs.
func codexConversationScreen(screen string) bool {
	return strings.Contains(screen, "Ask Codex") || strings.Contains(screen, "OpenAI Codex")
}

var (
	trustWatchWindow  = 30 * time.Second
	trustPollInterval = 400 * time.Millisecond
)

// watchTrust answers Codex's "Trust this folder?" in a run's TUI pane with
// "Trust and continue", only when the owner trusts the run's cwd
// (--trust-dir). A profile-home run normally never sees the prompt
// (ensureTrust); an ambient run (the owner's own CODEX_HOME, which Pyre
// never edits itself) does, and older Codex versions may. Codex then saves
// the answer exactly as if the owner had pressed it. Bounded: one watcher
// per run, at most trustWatchWindow, stops at the conversation screen.
func (a *Adapter) watchTrust(rr *runRef) {
	if a.o.TrustWorkspace == nil || !a.o.TrustWorkspace(rr.meta.Cwd) {
		return
	}
	if !rr.trustWatch.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer rr.trustWatch.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), trustWatchWindow)
		defer cancel()
		t := time.NewTicker(trustPollInterval)
		defer t.Stop()
		ups, enters := 0, 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			screen, err := a.srv.CapturePane(ctx, rr.meta.RunID, tmux.CaptureOptions{})
			if err != nil {
				if errors.Is(err, tmux.ErrNoSession) || errors.Is(err, tmux.ErrNoServer) {
					return
				}
				continue
			}
			switch classifyTrustScreen(screen) {
			case trustSelected:
				if enters < 3 {
					enters++
					if a.srv.SendKeys(ctx, rr.meta.RunID, tmux.KeyEnter) == nil && enters == 1 && a.o.OnAutomation != nil {
						go a.o.OnAutomation(contracts.NodeAutomation{RunID: rr.meta.RunID, SessionID: rr.meta.SessionID,
							Harness: contracts.HarnessCodex, Action: contracts.AutomationTrustAccepted, Detail: rr.meta.Cwd})
					}
				}
			case trustOther:
				if ups < 3 {
					ups++
					_ = a.srv.SendKeys(ctx, rr.meta.RunID, tmux.KeyUp)
				}
			default:
				if codexConversationScreen(screen) {
					return
				}
			}
		}
	}()
}
