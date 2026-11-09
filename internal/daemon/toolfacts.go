package daemon

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Tool facts (pyre start --needs): which common command-line tools this
// machine has. Presence only, from PATH lookups; nothing is ever run, so no
// credential is touched. Sampled at startup (the hello's resources) and at
// most every toolFactsTTL after that, with the heartbeat.

const toolFactsTTL = 10 * time.Minute

var toolFactsCache struct {
	mu    sync.Mutex
	at    time.Time
	facts map[string]bool
}

// toolFacts reports contracts.ProbedTools: name -> found.
func toolFacts() map[string]bool {
	toolFactsCache.mu.Lock()
	defer toolFactsCache.mu.Unlock()
	if toolFactsCache.facts != nil && time.Since(toolFactsCache.at) < toolFactsTTL {
		return toolFactsCache.facts
	}
	out := make(map[string]bool, len(contracts.ProbedTools))
	for _, t := range contracts.ProbedTools {
		out[t] = hasTool(t)
	}
	toolFactsCache.facts, toolFactsCache.at = out, time.Now()
	return out
}

// userBinDirs are folders a login shell usually adds to PATH that a
// service manager's PATH (pyred's) may lack.
func userBinDirs() []string {
	home, _ := os.UserHomeDir()
	dirs := []string{"/usr/local/bin", "/opt/homebrew/bin", "/snap/bin", "/usr/local/go/bin"}
	if home != "" {
		for _, d := range []string{".local/bin", "bin", "go/bin", ".cargo/bin", ".bun/bin", ".npm-global/bin",
			"google-cloud-sdk/bin", ".docker/bin", ".volta/bin"} {
			dirs = append(dirs, filepath.Join(home, d))
		}
	}
	return dirs
}

// hasTool reports whether name is an executable on pyred's PATH or in a
// usual user bin folder.
func hasTool(name string) bool {
	if name == "" || strings.ContainsAny(name, `/\`) {
		return false
	}
	if _, err := exec.LookPath(name); err == nil {
		return true
	}
	if runtime.GOOS == "windows" {
		return false
	}
	for _, d := range userBinDirs() {
		if st, err := os.Stat(filepath.Join(d, name)); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return true
		}
	}
	return false
}

// actionNeedsCheck answers placement's --needs: tools by PATH lookup,
// paths (absolute, or ~/... under this user's home) by stat.
func (d *Daemon) actionNeedsCheck(_ context.Context, raw json.RawMessage) (any, error) {
	var p contracts.NeedsCheckParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, contracts.Errorf(contracts.ErrInvalid, "needs.check params: %v", err)
		}
	}
	if len(p.Tools)+len(p.Paths) > 64 {
		return nil, contracts.Errorf(contracts.ErrInvalid, "needs.check: at most 64 names")
	}
	res := contracts.NeedsCheckResult{Tools: map[string]bool{}, Paths: map[string]bool{}}
	facts := toolFacts()
	for _, t := range p.Tools {
		if v, ok := facts[t]; ok {
			res.Tools[t] = v
		} else {
			res.Tools[t] = hasTool(t)
		}
	}
	for _, path := range p.Paths {
		full := expandHome(path)
		_, err := os.Stat(full)
		res.Paths[path] = filepath.IsAbs(full) && err == nil
	}
	return res, nil
}
