package daemon

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/autotitle"
	"github.com/jhh3/pyre/internal/contracts"
)

// Automatic session names (W-names), node side. While a run of an
// automatically titled session sits idle at its prompt, its native
// transcript is read incrementally (only appended bytes, off the observe
// path) and the best title found — Claude's own conversation title, Codex's
// thread name, else one made from the first user prompt — is reported to the
// hub as a session.title_suggested event whenever it changes. The hub applies
// it only while the session is still automatic, so a person's rename wins.

// autoTitleRescan is how often an idle run's transcript is re-read for a
// newer native title (Claude writes ai-title records after turns).
const autoTitleRescan = time.Minute

type runTitle struct {
	mu       sync.Mutex
	sc       *autotitle.Scanner
	sent     string
	lastScan time.Time
	busy     bool
}

type runTitleKey struct {
	d   *Daemon
	run string
}

var runTitles sync.Map // runTitleKey -> *runTitle

// maybeAutoTitle schedules a transcript scan for an idle run (at most one
// in flight per run; immediately on entering idle, else every autoTitleRescan).
func (d *Daemon) maybeAutoTitle(ctx context.Context, rec runRecord, enteredIdle bool) {
	// The hub alone decides whether a suggestion applies (only while the
	// session carries title:auto); the node's session copy never sees tags
	// changed after launch, so it suggests for every non-shell run.
	if rec.Handle == nil || rec.Run.Harness == contracts.HarnessShell {
		return
	}
	f, ok := d.adapters[rec.Run.Adapter].(transcriptFiler)
	if !ok {
		return
	}
	v, _ := runTitles.LoadOrStore(runTitleKey{d, rec.Run.RunID}, &runTitle{})
	rt := v.(*runTitle)
	now := d.now()
	rt.mu.Lock()
	if rt.busy || (!enteredIdle && now.Sub(rt.lastScan) < autoTitleRescan) {
		rt.mu.Unlock()
		return
	}
	rt.busy, rt.lastScan = true, now
	rt.mu.Unlock()
	go func() {
		defer func() {
			rt.mu.Lock()
			rt.busy = false
			rt.mu.Unlock()
		}()
		format, paths := f.TranscriptFiles(*rec.Handle)
		if format == "" {
			format = autotitle.FormatClaude
			switch rec.Run.Harness {
			case contracts.HarnessCodex:
				format = autotitle.FormatCodex
			case contracts.HarnessAgy:
				format = autotitle.FormatAgy
			}
		}
		if rt.sc == nil {
			rt.sc = autotitle.NewScanner(format)
		}
		rt.sc.Feed(paths)
		title, source := rt.sc.Best()
		if title == "" || title == rt.sent {
			return
		}
		if err := d.reportTitle(ctx, rec, title, source); err != nil {
			if ctx.Err() == nil {
				d.noteErr("title suggestion for %s: %v", rec.Run.RunID, err)
			}
			return
		}
		rt.sent = title
	}()
}

func (d *Daemon) reportTitle(ctx context.Context, rec runRecord, title, source string) error {
	payload, _ := json.Marshal(autotitle.Suggestion{SessionID: rec.Run.SessionID, RunID: rec.Run.RunID, Title: title, Source: source})
	ev := contracts.Event{Kind: autotitle.EventTitleSuggested, Object: contracts.ObjectRef{Kind: contracts.KindSession, ID: rec.Run.SessionID},
		SessionID: rec.Run.SessionID, ProjectID: rec.Session.ProjectID, PayloadVersion: 1, Payload: payload, OccurredAt: d.now()}
	_, err := d.rep.Enqueue(ctx, contracts.ReportEvent, ev)
	return err
}

// forgetRunTitle drops a finished run's title state.
func (d *Daemon) forgetRunTitle(runID string) { runTitles.Delete(runTitleKey{d, runID}) }
