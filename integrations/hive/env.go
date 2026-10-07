package hive

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jhh3/pyre/integrations/bundled"
	"github.com/jhh3/pyre/internal/contracts"
)

// Default bounds. Every Hive interaction is bounded; none of these run on a
// harness hook path.
const (
	DefaultDetectTimeout = 3 * time.Second
	DefaultQueryTimeout  = 8 * time.Second
	DefaultWriteTimeout  = 20 * time.Second
	DefaultMaxOutput     = 1 << 20 // bytes captured from one Hive subprocess

	// Thresholds mirror `hive doctor`: a tick older than 20 minutes or a vault
	// push older than 26 hours is reported as stale.
	TickStaleAfter = 20 * time.Minute
	SyncStaleAfter = 26 * time.Hour
)

// Env describes where Hive lives on this node and how to invoke it. Build it
// with EnvFromOS (production) or EnvFrom (tests / isolated homes). Paths are
// resolved exactly like Hive's config.py and hive-mem's common.sh so that
// Pyre reads the same files Hive writes.
type Env struct {
	// Vars is the complete environment passed to Hive subprocesses
	// (KEY=VALUE). Path resolution below is derived from it.
	Vars []string

	// Bin is an explicit hive launcher. Empty means discover (PYRE_HIVE_BIN,
	// PATH, ~/.local/bin/hive, newest plugin cache copy).
	Bin string

	DetectTimeout time.Duration
	QueryTimeout  time.Duration
	WriteTimeout  time.Duration
	MaxOutput     int

	// Runner executes subprocesses; nil means the real exec runner.
	Runner Runner
	// Clock is injectable for tests; nil means contracts.SystemClock.
	Clock contracts.Clock
}

// EnvFromOS returns an Env for the current process environment.
func EnvFromOS() Env { return EnvFrom(os.Environ()) }

// EnvFrom returns an Env whose subprocess environment and path resolution
// come from vars.
func EnvFrom(vars []string) Env {
	e := Env{Vars: append([]string(nil), vars...)}
	e.Bin = e.get("PYRE_HIVE_BIN")
	return e
}

func (e Env) get(key string) string {
	prefix := key + "="
	val := ""
	for _, kv := range e.Vars {
		if strings.HasPrefix(kv, prefix) {
			val = kv[len(prefix):] // last one wins, like the C runtime
		}
	}
	return val
}

func (e Env) clock() contracts.Clock {
	if e.Clock != nil {
		return e.Clock
	}
	return contracts.SystemClock{}
}

func (e Env) runner() Runner {
	if e.Runner != nil {
		return e.Runner
	}
	return ExecRunner{}
}

func durOr(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

func (e Env) detectTimeout() time.Duration { return durOr(e.DetectTimeout, DefaultDetectTimeout) }
func (e Env) queryTimeout() time.Duration  { return durOr(e.QueryTimeout, DefaultQueryTimeout) }
func (e Env) writeTimeout() time.Duration  { return durOr(e.WriteTimeout, DefaultWriteTimeout) }
func (e Env) maxOutput() int {
	if e.MaxOutput > 0 {
		return e.MaxOutput
	}
	return DefaultMaxOutput
}

// Home is $HOME as seen by Hive.
func (e Env) Home() string {
	if h := e.get("HOME"); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return h
}

// DataDir is Hive's data dir (HIVE_DATA_DIR, default ~/.local/share/hive).
func (e Env) DataDir() string {
	if v := e.get("HIVE_DATA_DIR"); v != "" {
		return v
	}
	return filepath.Join(e.Home(), ".local", "share", "hive")
}

// StateDir is Hive's state dir (HIVE_STATE_DIR, default ~/.local/state/hive):
// stamps, tick.log, breakers.
func (e Env) StateDir() string {
	if v := e.get("HIVE_STATE_DIR"); v != "" {
		return v
	}
	return filepath.Join(e.Home(), ".local", "state", "hive")
}

// StreamsVault is the streams vault / ws root (HIVE_STREAMS_VAULT, default
// ~/streams).
func (e Env) StreamsVault() string {
	if v := e.get("HIVE_STREAMS_VAULT"); v != "" {
		return v
	}
	return filepath.Join(e.Home(), "streams")
}

// Hive's vendored memory store (Hive's names, not Pyre's: Pyre must resolve
// the same directory and honour the same variables Hive does).
const (
	storeDirName      = "jhh3-mem"            // data directory under XDG_DATA_HOME
	storeHomeVar      = "JHH3_MEM_HOME"       // explicit store home
	storeNoCaptureVar = "JHH3_MEM_NO_CAPTURE" // self-capture guard
)

// StoreHome is the vendored store home (storeHomeVar, else
// $XDG_DATA_HOME/<storeDirName>, else ~/.local/share/<storeDirName>).
func (e Env) StoreHome() string {
	if v := e.get(storeHomeVar); v != "" {
		return strings.TrimRight(v, "/")
	}
	if x := e.get("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, storeDirName)
	}
	return filepath.Join(e.Home(), ".local", "share", storeDirName)
}

// DBPath resolves the active index exactly like hive/db.py: HIVE_DB, else
// index.db once cut over, else the shared hive-mem memory.db, else the
// (absent) index.db path.
func (e Env) DBPath() string {
	if v := e.get("HIVE_DB"); v != "" {
		return v
	}
	idx := filepath.Join(e.DataDir(), "index.db")
	if fileExists(idx) {
		return idx
	}
	mem := filepath.Join(e.StoreHome(), "memory.db")
	if fileExists(mem) {
		return mem
	}
	return idx
}

// Disabled reports Hive's kill switch (HIVE_DISABLE). It disables Hive's
// hooks (capture); the CLI still answers queries.
func (e Env) Disabled() bool {
	v := e.get("HIVE_DISABLE")
	return v != "" && v != "0"
}

// childEnv is the subprocess environment: Vars plus Hive's self-capture
// guards so a Pyre-initiated call never records a memory about itself.
func (e Env) childEnv() []string {
	out := append([]string(nil), e.Vars...)
	return append(out, "HIVE_NO_CAPTURE=1", storeNoCaptureVar+"=1")
}

// discoverBin finds the hive launcher: Env.Bin, the bundled Hive, PATH,
// ~/.local/bin, then the newest Claude plugin cache. Returns "" when absent.
func (e Env) discoverBin() string {
	if e.Bin != "" {
		if isExecutable(e.Bin) {
			return e.Bin
		}
		return ""
	}
	// A Hive bundled with this Pyre install (install.sh --with-tools) wins.
	if p := bundled.Path("hive"); p != "" {
		return p
	}
	for _, dir := range filepath.SplitList(e.get("PATH")) {
		if dir == "" {
			continue
		}
		if p := filepath.Join(dir, "hive"); isExecutable(p) {
			return p
		}
	}
	if p := filepath.Join(e.Home(), ".local", "bin", "hive"); isExecutable(p) {
		return p
	}
	if root := e.newestPluginRoot(); root != "" {
		if p := filepath.Join(root, "bin", "hive"); isExecutable(p) {
			return p
		}
	}
	return ""
}

// newestPluginRoot mirrors the ~/.local/bin/hive shim: the highest-versioned
// ~/.claude/plugins/cache/*/hive/<version> directory.
func (e Env) newestPluginRoot() string {
	matches, _ := filepath.Glob(filepath.Join(e.Home(), ".claude", "plugins", "cache", "*", "hive", "*", "bin", "hive"))
	if len(matches) == 0 {
		return ""
	}
	roots := make([]string, 0, len(matches))
	for _, m := range matches {
		roots = append(roots, filepath.Dir(filepath.Dir(m)))
	}
	sort.Slice(roots, func(i, j int) bool {
		return versionLess(filepath.Base(roots[i]), filepath.Base(roots[j]))
	})
	return roots[len(roots)-1]
}

// versionLess compares dotted versions numerically segment by segment
// (like `sort -V` for the shapes Hive uses).
func versionLess(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		ai, aerr := strconv.Atoi(as[i])
		bi, berr := strconv.Atoi(bs[i])
		if aerr == nil && berr == nil {
			if ai != bi {
				return ai < bi
			}
			continue
		}
		if as[i] != bs[i] {
			return as[i] < bs[i]
		}
	}
	return len(as) < len(bs)
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func isExecutable(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir() && st.Mode()&0o111 != 0
}
