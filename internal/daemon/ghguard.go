package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/ghguard"
	"github.com/jhh3/pyre/internal/osutil"
)

// ghGuard is the node side of the GitHub API budget guard
// (internal/ghguard): the per-session limiter the shim asks, and the
// latest `gh api rate_limit` sample.
type ghGuard struct {
	once    sync.Once
	shimDir string
	lim     *ghguard.Limiter

	mu       sync.Mutex
	sample   contracts.GitHubBudget // ObservedAt zero: none yet
	tried    time.Time
	sampling bool
	// sampleGH runs `gh api rate_limit` (tests replace it).
	sampleGH func(ctx context.Context) ([]byte, error)
}

const (
	ghSampleEvery = time.Minute
	ghWarnEvery   = 5 * time.Minute
	ghTopSessions = 5
)

func (g *ghGuard) limiter() *ghguard.Limiter {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.lim == nil {
		g.lim = ghguard.NewLimiter()
	}
	return g.lim
}

// ghShimDir creates (once per pyred) the directory holding the `gh` shim:
// a link to the pyre CLI, which runs ghguard.Main when started as gh. ""
// when there is no pyre CLI or the link cannot be made.
func (d *Daemon) ghShimDir() string {
	d.gh.once.Do(func() {
		if d.cfg.PyreCLI == "" || d.cfg.StateDir == "" {
			return
		}
		dir := filepath.Join(d.cfg.StateDir, "ghshim")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return
		}
		link := filepath.Join(dir, osutil.ExeName("gh"))
		_ = os.Remove(link)
		var err error
		if runtime.GOOS == "windows" {
			// No symlinks without developer mode: a hard link (same volume)
			// or a copy of pyre.exe, renewed at each pyred start.
			if err = os.Link(d.cfg.PyreCLI, link); err != nil {
				err = copyFile(d.cfg.PyreCLI, link)
			}
		} else {
			err = os.Symlink(d.cfg.PyreCLI, link)
		}
		if err != nil {
			d.cfg.Logger.Warn("gh shim not installed", "dir", dir, "err", err)
			return
		}
		d.gh.shimDir = dir
	})
	return d.gh.shimDir
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o700)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// ghBudget is the node's GitHub budget for status reports: the latest
// sample plus the top callers. It starts a new sample in the background
// when the last is a minute old. Nil when gh was never sampled or called.
func (d *Daemon) ghBudget() *contracts.GitHubBudget {
	d.ghRefresh()
	now := d.now()
	top, total := d.gh.limiter().Top(now, ghTopSessions)
	d.gh.mu.Lock()
	b := d.gh.sample
	d.gh.mu.Unlock()
	if b.ObservedAt.IsZero() && total == 0 && len(top) == 0 {
		return nil
	}
	b.CallsLastHour, b.TopSessions = total, top
	return &b
}

// ghRefresh starts one `gh api rate_limit` (free: it does not count
// against the limit) unless one ran in the last minute or is running.
func (d *Daemon) ghRefresh() {
	g := &d.gh
	g.mu.Lock()
	now := d.now()
	if g.sampling || (!g.tried.IsZero() && now.Sub(g.tried) < ghSampleEvery) {
		g.mu.Unlock()
		return
	}
	g.sampling, g.tried = true, now
	run := g.sampleGH
	g.mu.Unlock()
	if run == nil {
		run = ghRateLimit
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		out, err := run(ctx)
		var b contracts.GitHubBudget
		if err == nil {
			b, err = ghguard.RateLimit(out, d.now())
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		g.sampling = false
		if err == nil {
			g.sample = b
		}
	}()
}

// ghRateLimit runs pyred's own gh (its PATH has no shim).
func ghRateLimit(ctx context.Context) ([]byte, error) {
	gh, err := exec.LookPath("gh")
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, gh, "api", "rate_limit")
	cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1")
	return cmd.Output()
}

// serveGHCall answers the gh shim (ghguard.PathCall): take one call from
// the session's budget, and add a low-budget warning now and then.
func (d *Daemon) serveGHCall(w http.ResponseWriter, r *http.Request) {
	var req ghguard.CallRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&req); err != nil {
		writeLocalErr(w, contracts.Errorf(contracts.ErrInvalid, "gh call: %v", err))
		return
	}
	sid := "owner"
	if p, ok := contracts.PrincipalFrom(r.Context()); ok && p.Kind == contracts.PrincipalSession {
		sid = p.ID
	}
	writeLocalJSON(w, http.StatusOK, d.ghCall(sid, req))
}

func (d *Daemon) ghCall(sid string, req ghguard.CallRequest) ghguard.CallResponse {
	now := d.now()
	lim := d.gh.limiter()
	dec := lim.Take(sid, req.RunID, now)
	var resp ghguard.CallResponse
	switch {
	case dec.Refused:
		resp.Refuse = true
		resp.Message = ghguard.RefuseMessage(sid, dec, int(lim.PerMinute))
		d.cfg.Logger.Warn("gh call refused", "session", sid, "run", req.RunID, "command", req.Command, "last_minute", dec.LastMinute)
	case dec.Wait > 0:
		resp.WaitMS = dec.Wait.Milliseconds()
		resp.Message = ghguard.WaitMessage(sid, dec, int(lim.PerMinute))
	}
	d.ghRefresh()
	d.gh.mu.Lock()
	b := d.gh.sample
	d.gh.mu.Unlock()
	if b.Low() && lim.Warn(sid, now, ghWarnEvery) {
		resp.Warn = ghguard.WarnMessage(b)
	}
	return resp
}
