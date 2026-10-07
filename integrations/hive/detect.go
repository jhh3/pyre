package hive

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Mode is how this node can reach Hive.
type Mode string

const (
	ModeCLI         Mode = "cli"         // hive launcher works: full ranked retrieval and note writes
	ModeIndex       Mode = "index"       // no working CLI, but the local index is readable: degraded read-only search/get
	ModeUnavailable Mode = "unavailable" // neither
)

// Status is the result of Detect: the contract views plus adapter detail.
type Status struct {
	Memory      contracts.MemoryStatus      `json:"memory"`
	Integration contracts.IntegrationStatus `json:"integration"`
	Mode        Mode                        `json:"mode"`
	Bin         string                      `json:"bin,omitempty"`
	PluginRoot  string                      `json:"plugin_root,omitempty"`
	DBPath      string                      `json:"db_path,omitempty"`
	StoreVer    string                      `json:"store_version,omitempty"` // "hive-mem 0.4.5 (schema 2)"
	Machine     string                      `json:"machine,omitempty"`
	Breakers    []string                    `json:"breakers,omitempty"` // tripped per-hook breakers
	Disabled    bool                        `json:"disabled,omitempty"` // HIVE_DISABLE kill switch
	LastTickAt  *time.Time                  `json:"last_tick_at,omitempty"`
	Stale       bool                        `json:"stale"`
	StaleReason string                      `json:"stale_reason,omitempty"`
	CheckedAt   time.Time                   `json:"checked_at"`
}

// Detect inspects this node's Hive installation within env's detect timeout.
// It never returns an error: absence or breakage is reported as
// Memory.Backend "unavailable" (or a degraded mode) with a clear Detail, so
// callers can show it and continue launching sessions.
func Detect(ctx context.Context, env Env) Status {
	ctx, cancel := context.WithTimeout(ctx, env.detectTimeout())
	defer cancel()
	now := env.clock().Now()
	st := Status{CheckedAt: now, Disabled: env.Disabled(), Machine: machineID(env)}
	var notes []string

	// 1. CLI.
	cliOK := false
	st.Bin = env.discoverBin()
	if st.Bin != "" {
		st.PluginRoot = pluginRootFor(env, st.Bin)
		res, err := env.runner().Run(ctx, Cmd{Path: st.Bin, Args: []string{"mem", "version"}, Env: env.childEnv(),
			Timeout: env.detectTimeout(), MaxOutput: 4096})
		switch {
		case err != nil:
			notes = append(notes, "hive CLI did not answer: "+err.Error())
		case res.ExitCode != 0:
			notes = append(notes, fmt.Sprintf("hive CLI failed (exit %d): %s", res.ExitCode, oneLine(res.Stderr)))
		default:
			cliOK = true
			st.StoreVer = oneLine(res.Stdout)
		}
	} else {
		notes = append(notes, "hive CLI not found (PYRE_HIVE_BIN, PATH, ~/.local/bin/hive, ~/.claude/plugins/cache/*/hive)")
	}

	// 2. Index (read-only).
	idxOK := false
	st.DBPath = env.DBPath()
	if fileExists(st.DBPath) {
		stats, err := readIndexStats(ctx, st.DBPath)
		if err != nil {
			notes = append(notes, "hive index unreadable: "+err.Error())
		} else {
			idxOK = true
			st.Memory.PendingItems = stats.PendingJobs
		}
		if t := latestMtime(st.DBPath, st.DBPath+"-wal"); t != nil {
			st.Memory.LastIndexAt = t
		}
	}

	// 3. Hive's own state files.
	sd := env.StateDir()
	if b, err := os.ReadFile(filepath.Join(sd, "sync.push.stamp")); err == nil {
		if f, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64); err == nil && f > 0 {
			t := time.Unix(int64(f), 0).UTC()
			st.Memory.LastSyncAt = &t
		}
	}
	if fi, err := os.Stat(filepath.Join(sd, "tick.log")); err == nil {
		t := fi.ModTime().UTC()
		st.LastTickAt = &t
	}
	st.Breakers = trippedBreakers(sd)

	// 4. Version / revision / dirty for inventory.
	st.Integration = integrationFor(ctx, env, st)

	// 5. Mode and summaries.
	switch {
	case cliOK:
		st.Mode = ModeCLI
	case idxOK:
		st.Mode = ModeIndex
		notes = append(notes, "serving degraded read-only local index search; notes cannot be published")
	default:
		st.Mode = ModeUnavailable
	}
	if st.Mode == ModeUnavailable {
		st.Memory.Backend = "unavailable"
		if st.Bin == "" && !fileExists(st.DBPath) {
			notes = []string{"Hive is not installed on this node (no hive CLI and no memory index); memory search is unavailable here, launches are unaffected"}
		}
	} else {
		st.Memory.Backend = "hive"
		st.Memory.Version = st.Integration.Version
		if st.Memory.Version == "" {
			st.Memory.Version = st.StoreVer
		}
		var stale []string
		if st.LastTickAt == nil {
			stale = append(stale, "no hive tick recorded")
		} else if age := now.Sub(*st.LastTickAt); age > TickStaleAfter {
			stale = append(stale, fmt.Sprintf("last hive tick %s ago", age.Round(time.Minute)))
		}
		if st.Memory.LastSyncAt != nil && now.Sub(*st.Memory.LastSyncAt) > SyncStaleAfter {
			stale = append(stale, fmt.Sprintf("last vault push %s ago", now.Sub(*st.Memory.LastSyncAt).Round(time.Hour)))
		}
		if len(stale) > 0 {
			st.Stale = true
			st.StaleReason = strings.Join(stale, "; ")
			notes = append(notes, "stale: "+st.StaleReason)
		}
	}
	if st.Disabled {
		notes = append(notes, "HIVE_DISABLE is set: Hive hooks (capture) are off")
	}
	if len(st.Breakers) > 0 {
		notes = append(notes, "tripped hive breakers: "+strings.Join(st.Breakers, ", "))
	}
	st.Memory.Detail = strings.Join(notes, "; ")
	st.Integration.Health = health(st)
	return st
}

func health(st Status) string {
	switch {
	case st.Mode == ModeUnavailable:
		return "unavailable"
	case st.Mode == ModeIndex || st.Stale || st.Disabled || len(st.Breakers) > 0:
		return "degraded"
	default:
		return "ok"
	}
}

func oneLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return truncBytes(s, 200)
}

func latestMtime(paths ...string) *time.Time {
	var best time.Time
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil && fi.ModTime().After(best) {
			best = fi.ModTime()
		}
	}
	if best.IsZero() {
		return nil
	}
	t := best.UTC()
	return &t
}

// trippedBreakers mirrors `hive doctor`: breaker.<name> files whose count is
// >= 3 (".notified" companions ignored).
func trippedBreakers(stateDir string) []string {
	matches, _ := filepath.Glob(filepath.Join(stateDir, "breaker.*"))
	var out []string
	for _, m := range matches {
		if strings.HasSuffix(m, ".notified") {
			continue
		}
		b, err := os.ReadFile(m)
		if err != nil {
			continue
		}
		if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && n >= 3 {
			out = append(out, strings.TrimPrefix(filepath.Base(m), "breaker."))
		}
	}
	sort.Strings(out)
	return out
}

func machineID(env Env) string {
	if v := env.get("HIVE_MACHINE"); v != "" {
		return v
	}
	if b, err := os.ReadFile(filepath.Join(env.DataDir(), "machine-id")); err == nil {
		return strings.TrimSpace(string(b))
	}
	return ""
}

// pluginRootFor finds the plugin directory behind a launcher: the bin's own
// plugin root when it is plugins/hive/bin/hive, else (the ~/.local/bin shim)
// the newest plugin cache copy the shim would exec.
func pluginRootFor(env Env, bin string) string {
	if real, err := filepath.EvalSymlinks(bin); err == nil {
		root := filepath.Dir(filepath.Dir(real))
		if fileExists(filepath.Join(root, ".claude-plugin", "plugin.json")) {
			return root
		}
	}
	return env.newestPluginRoot()
}

func integrationFor(ctx context.Context, env Env, st Status) contracts.IntegrationStatus {
	is := contracts.IntegrationStatus{Present: st.Bin != "" || fileExists(st.DBPath)}
	if st.PluginRoot == "" {
		is.Path = st.Bin
		return is
	}
	is.Path = st.PluginRoot
	var pj struct {
		Version string `json:"version"`
	}
	if b, err := os.ReadFile(filepath.Join(st.PluginRoot, ".claude-plugin", "plugin.json")); err == nil {
		_ = json.Unmarshal(b, &pj)
		is.Version = pj.Version
	}
	is.Revision = installedRevision(env, st.PluginRoot)
	// A plugin-cache copy is not a checkout; a dev checkout (HIVE_DEV) is —
	// <repo>/plugins/hive. Only that exact layout counts, so an enclosing
	// dotfiles repo is never mistaken for the Hive checkout.
	if top := gitTop(ctx, st.PluginRoot); top != "" && sameDir(top, filepath.Dir(filepath.Dir(st.PluginRoot))) {
		if rev := gitOut(ctx, top, "rev-parse", "HEAD"); rev != "" && is.Revision == "" {
			is.Revision = rev
		}
		is.Dirty = gitOut(ctx, top, "status", "--porcelain") != ""
	}
	return is
}

// installedRevision reads Claude Code's installed_plugins.json entry for
// hive@<marketplace> (gitCommitSha), preferring the matching installPath.
func installedRevision(env Env, root string) string {
	b, err := os.ReadFile(filepath.Join(env.Home(), ".claude", "plugins", "installed_plugins.json"))
	if err != nil {
		return ""
	}
	var doc struct {
		Plugins map[string]json.RawMessage `json:"plugins"`
	}
	if json.Unmarshal(b, &doc) != nil {
		return ""
	}
	type inst struct {
		InstallPath  string `json:"installPath"`
		GitCommitSha string `json:"gitCommitSha"`
	}
	fallback := ""
	for k, raw := range doc.Plugins {
		if !strings.HasPrefix(k, "hive@") {
			continue
		}
		var list []inst
		if json.Unmarshal(raw, &list) != nil {
			var one inst
			if json.Unmarshal(raw, &one) != nil {
				continue
			}
			list = []inst{one}
		}
		for _, in := range list {
			if filepath.Clean(in.InstallPath) == filepath.Clean(root) {
				return in.GitCommitSha
			}
			if fallback == "" {
				fallback = in.GitCommitSha
			}
		}
	}
	return fallback
}

func sameDir(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	if err1 != nil || err2 != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ra == rb
}

func gitTop(ctx context.Context, dir string) string {
	if _, err := exec.LookPath("git"); err != nil {
		return ""
	}
	return gitOut(ctx, dir, "rev-parse", "--show-toplevel")
}

func gitOut(ctx context.Context, dir string, args ...string) string {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
