package main

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/jhh3/pyre/integrations/hive"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/daemon"
)

// Pyre-side Hive capture of Codex rollouts (Hive 0.2.3 has no Codex capture
// hook and its sweep reads only ~/.codex/sessions; see integrations/hive
// capture.go). The node hands a Codex run's rollout to `hive mem capture`
// when the run ends (holding the run's profile so a rota check-in waits for
// the capture) and at idle boundaries of live runs.

// captureIdleInterval is how often live Codex runs are checked for an idle
// boundary; captureQuiet is how long a rollout must be unchanged first.
const (
	captureIdleInterval = time.Minute
	captureQuiet        = 20 * time.Second
)

// codexHome is the CODEX_HOME a run used: its bound profile, else the
// daemon's own CODEX_HOME or ~/.codex.
func codexHome(profileDir string) string {
	if profileDir != "" {
		return profileDir
	}
	if v := os.Getenv("CODEX_HOME"); v != "" {
		return v
	}
	if h := userHome(); h != "" {
		return filepath.Join(h, ".codex")
	}
	return ""
}

// onRunEnded queues the ended run's Codex rollout for capture. It runs on the
// daemon's run-end path, so it only globs for the file and enqueues.
func (in *integrations) onRunEnded(ev daemon.RunEnded) {
	if in.capturer == nil || ev.Run.Harness != contracts.HarnessCodex || ev.Run.NativeRef == nil {
		return
	}
	home := codexHome(ev.ProfileDir)
	path, err := hive.CodexRollout(home, ev.Run.NativeRef.ID)
	if err != nil {
		in.log.Info("hive capture: no rollout for ended codex run", "run", ev.Run.RunID, "err", err)
		return
	}
	var done func()
	if in.acct != nil && ev.ProfileDir != "" {
		done = in.acct.HoldProfile(ev.ProfileDir) // rota check-in waits for the capture
	}
	in.capturer.Enqueue(hive.CaptureRequest{Transcript: path, SessionID: ev.Run.NativeRef.ID, Cwd: ev.Run.Cwd,
		Reason: "pyre-run-end", Env: []string{"CODEX_HOME=" + home}, Done: done})
}

// captureIdleLoop enqueues the rollouts of live, idle Codex runs that changed
// and have been quiet for captureQuiet (the capturer skips unchanged files).
func (in *integrations) captureIdleLoop(ctx context.Context, d *daemon.Daemon) {
	t := time.NewTicker(captureIdleInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if !in.hive.Present(ctx) {
			continue
		}
		runs, err := d.ActiveRuns(ctx)
		if err != nil {
			continue
		}
		for _, r := range runs {
			if r.Harness != contracts.HarnessCodex || r.NativeID == "" || r.State != contracts.RunWaitingInput {
				continue
			}
			home := codexHome(r.ProfileDir)
			path, err := hive.CodexRollout(home, r.NativeID)
			if err != nil {
				continue
			}
			if fi, err := os.Stat(path); err != nil || time.Since(fi.ModTime()) < captureQuiet {
				continue
			}
			in.capturer.Enqueue(hive.CaptureRequest{Transcript: path, SessionID: r.NativeID, Cwd: r.Cwd,
				Reason: "pyre-idle", Env: []string{"CODEX_HOME=" + home}})
		}
	}
}
