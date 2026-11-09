package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
)

// RecentCwds is implemented by adapters that can tell where a run's
// conversation worked lately (Claude records the cwd on every transcript
// line), most recent first.
type recentCwder interface {
	RecentCwds(prev contracts.Run) []string
}

// isDir reports whether p is an existing directory.
func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// resumeCwdFallback moves a resume whose recorded working directory is gone
// to one that exists, so a session that removed its launch worktree (after
// merging, say) and went on working elsewhere can still be resumed (server,
// 2026-11-07: a proactive account move could not bring such a session back
// and it stayed down). In order: the latest directory the conversation
// worked in that still exists (adapter), the main checkout of the git
// repository the old directory belonged to, the node user's home. It sets
// sc.Cwd and returns a note for the session ("" when the cwd exists or the
// launch is not a plain resume).
func (d *Daemon) resumeCwdFallback(ctx context.Context, adapter contracts.RuntimeAdapter, sc *contracts.StartRunCommand) string {
	if sc.Checkout != nil || sc.Cwd == "" {
		return ""
	}
	old := filepath.Clean(expandHome(sc.Cwd))
	if !filepath.IsAbs(old) || isDir(old) {
		return ""
	}
	cwd, why := "", ""
	if rc, ok := adapter.(recentCwder); ok && sc.PreviousRun != nil {
		for _, c := range rc.RecentCwds(*sc.PreviousRun) {
			if c = filepath.Clean(c); filepath.IsAbs(c) && isDir(c) {
				cwd, why = c, "the directory the conversation last worked in"
				break
			}
		}
	}
	if cwd == "" {
		if m := mainCheckoutNear(ctx, old); m != "" {
			cwd, why = m, "the main checkout of its repository"
		}
	}
	if cwd == "" {
		h, err := os.UserHomeDir()
		if err != nil || !isDir(h) {
			return ""
		}
		cwd, why = h, "your home directory"
	}
	d.cfg.Logger.Info("resume: working directory is gone; resuming elsewhere", "session", sc.Session.SessionID, "old", old, "cwd", cwd, "why", why)
	sc.Cwd = cwd
	return fmt.Sprintf("[Pyre] This session was resumed in %s (%s) because its previous working directory %s no longer exists. Use absolute paths if you work elsewhere. Nothing else changed: continue what you were doing, or end your turn if you were waiting for something.", cwd, why, old)
}

// mainCheckoutNear returns the main checkout of the git repository holding
// p's nearest existing ancestor ("" when that is not inside a repository).
func mainCheckoutNear(ctx context.Context, p string) string {
	dir := filepath.Dir(p)
	for !isDir(dir) {
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
	common, err := git(ctx, dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || common == "" {
		return ""
	}
	common = filepath.Clean(common)
	if filepath.Base(common) != ".git" {
		return "" // a bare repository has no checkout to run in
	}
	main := filepath.Dir(common)
	if !isDir(main) || strings.TrimSpace(main) == "" {
		return ""
	}
	return main
}
