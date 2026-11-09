package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/repoindex"
)

// Worktrees removed by session cleanup, added back on resume
// (iss_…). Archive cleanup removes the clean,
// merged-or-pushed worktrees Pyre created for a session; an unarchive and
// resume then found its working directory gone. Cleanup records each
// removal (repository, path, branch, commit) under the session in
// daemon_meta, and a resume whose working directory is one of them adds it
// back first. Only recorded removals are recreated, never another missing
// folder.

const metaRemovedWorktrees = "removed_worktrees/"

func (d *Daemon) removedWorktrees(ctx context.Context, sessionID string) []contracts.WorktreeRemoval {
	v, err := d.st.meta(ctx, metaRemovedWorktrees+sessionID)
	if err != nil || v == "" {
		return nil
	}
	var rms []contracts.WorktreeRemoval
	_ = json.Unmarshal([]byte(v), &rms)
	return rms
}

func (d *Daemon) setRemovedWorktrees(ctx context.Context, sessionID string, rms []contracts.WorktreeRemoval) {
	b, _ := json.Marshal(rms)
	if len(rms) == 0 {
		b = nil
	}
	if err := d.st.setMeta(ctx, metaRemovedWorktrees+sessionID, string(b)); err != nil {
		d.noteErr("record removed worktrees of %s: %v", sessionID, err)
	}
}

// recordRemovedWorktrees keeps the removed worktrees in res that carry
// what Restore needs, under their session (sessionID, else the one their
// marker named).
func (d *Daemon) recordRemovedWorktrees(ctx context.Context, sessionID string, res []contracts.WorktreeRemoval) {
	by := map[string][]contracts.WorktreeRemoval{}
	for _, r := range res {
		sid := sessionID
		if sid == "" {
			sid = r.SessionID
		}
		if r.Outcome != "removed" || sid == "" || r.Repo == "" {
			continue
		}
		by[sid] = append(by[sid], r)
	}
	for sid, add := range by {
		rms := d.removedWorktrees(ctx, sid)
		for _, a := range add {
			kept := rms[:0]
			for _, r := range rms {
				if filepath.Clean(r.Path) != filepath.Clean(a.Path) {
					kept = append(kept, r)
				}
			}
			rms = append(kept, a)
		}
		d.setRemovedWorktrees(ctx, sid, rms)
	}
}

// restoreRemovedWorktrees adds back the worktrees cleanup removed that a
// resume in cwd needs (cwd missing and recorded for the session). Nothing
// happens when cwd exists or no removal matches. A failure says what was
// removed and how to go on.
func (d *Daemon) restoreRemovedWorktrees(ctx context.Context, sessionID, cwd string) error {
	cwd = filepath.Clean(expandHome(cwd))
	if _, err := os.Lstat(cwd); !errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	rms := d.removedWorktrees(ctx, sessionID)
	want := repoindex.RestoreFor(rms, cwd)
	if len(want) == 0 {
		return nil
	}
	restored := map[string]bool{}
	var failed error
	for _, rm := range want {
		if err := repoindex.Restore(ctx, rm, sessionID); err != nil {
			on := "branch " + rm.Branch
			if rm.Branch == "" {
				on = "a fresh branch"
			}
			failed = contracts.Errorf(contracts.ErrInvalid,
				"working directory %q was a worktree Pyre removed when this session was archived, and it could not be recreated: %v. Start a fresh session on %s instead (pyre start --cwd %s --worktree <branch>)",
				cwd, err, on, rm.Repo)
			break
		}
		restored[filepath.Clean(rm.Path)] = true
		d.cfg.Logger.Info("resume: recreated a worktree session cleanup removed", "session", sessionID, "path", rm.Path, "branch", rm.Branch, "head", rm.Head)
	}
	kept := rms[:0]
	for _, r := range rms {
		if !restored[filepath.Clean(r.Path)] {
			kept = append(kept, r)
		}
	}
	d.setRemovedWorktrees(ctx, sessionID, kept)
	return failed
}
