package main

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/winenv"
)

// Harness tools are often installed by interactive-shell setup (user-local
// Homebrew, nvm, ~/.local/bin) that a service or `ssh host cmd` PATH does not
// include. widenToolPath appends, after the existing PATH (which keeps
// precedence), the directories where this user's shells would find tmux,
// claude, codex and node, so the adapters' lookups, version pinning, and run
// environments all agree. Sources, in order: explicit --tool-path /
// PYRE_TOOL_PATH entries, the interactive shell's PATH, common user tool
// directories, and the newest nvm Node bin dir that contains codex/claude
// (which also provides the `node` those npm-installed scripts need).
func widenToolPath(ctx context.Context, explicit []string, log *slog.Logger) {
	home, _ := os.UserHomeDir()
	cur := filepath.SplitList(os.Getenv("PATH"))
	var add []string
	add = append(add, explicit...)
	if v := os.Getenv("PYRE_TOOL_PATH"); v != "" {
		add = append(add, filepath.SplitList(v)...)
	}
	add = append(add, interactiveShellPath(ctx)...)
	if runtime.GOOS == "windows" {
		// The registry PATH as it is now (tools installed after logon), then
		// the well-known per-user/machine tool dirs (W-winport).
		add = append(add, winenv.RegistryPath()...)
		add = append(add, winenv.Default().ToolDirs()...)
	} else if home != "" {
		add = append(add,
			filepath.Join(home, "homebrew", "bin"), "/opt/homebrew/bin", "/usr/local/bin",
			filepath.Join(home, ".local", "bin"), filepath.Join(home, ".bun", "bin"),
			filepath.Join(home, ".npm-global", "bin"))
		add = append(add, nvmToolDirs(home)...)
	}
	seen := map[string]bool{}
	for _, d := range cur {
		seen[d] = true
	}
	out := append([]string(nil), cur...)
	for _, d := range add {
		d = strings.TrimSpace(d)
		if d == "" || seen[d] || !filepath.IsAbs(d) {
			continue
		}
		if st, err := os.Stat(d); err != nil || !st.IsDir() {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	os.Setenv("PATH", strings.Join(out, string(os.PathListSeparator)))
	tools := []string{"tmux", "claude", "codex", "agy", "node"}
	if runtime.GOOS == "windows" {
		tools = []string{"claude", "codex", "node", "npm", "npx", "git"}
		windowsToolEnv(log)
	}
	for _, tool := range tools {
		if p, err := exec.LookPath(tool); err == nil {
			log.Info("tool found", "tool", tool, "path", p)
		} else {
			log.Info("tool not found", "tool", tool)
		}
	}
}

// interactiveShellPath returns the PATH an interactive login shell of the
// user's $SHELL would have (bounded to 3 s; markers isolate it from any
// prompt/integration output). Empty on failure.
func interactiveShellPath(ctx context.Context) []string {
	sh := os.Getenv("SHELL")
	if sh == "" || runtime.GOOS == "windows" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, sh, "-ilc", `printf '__PYRE_PATH__%s__END__' "$PATH"`)
	cmd.Stdin = nil
	out, err := cmd.Output()
	if err != nil && len(out) == 0 {
		return nil
	}
	s := string(out)
	i := strings.LastIndex(s, "__PYRE_PATH__")
	if i < 0 {
		return nil
	}
	s = s[i+len("__PYRE_PATH__"):]
	if j := strings.Index(s, "__END__"); j >= 0 {
		s = s[:j]
	}
	return filepath.SplitList(s)
}

// windowsToolEnv reports Git Bash and the VS Build Tools, and points
// Claude Code at Git Bash (CLAUDE_CODE_GIT_BASH_PATH) when it is installed
// somewhere Claude's own discovery may miss and the variable is unset.
func windowsToolEnv(log *slog.Logger) {
	we := winenv.Default()
	if bash := we.GitBash(); bash != "" {
		log.Info("tool found", "tool", "git-bash", "path", bash)
		if os.Getenv("CLAUDE_CODE_GIT_BASH_PATH") == "" {
			os.Setenv("CLAUDE_CODE_GIT_BASH_PATH", bash)
		}
	} else {
		log.Warn("Git for Windows (bash.exe) not found: Claude Code falls back to PowerShell for its shell and hooks, and the Pyre doorbell monitor is disabled; install Git for Windows (winget install Git.Git)")
	}
	if vw := we.VSWhere(); vw != "" {
		log.Info("tool found", "tool", "vswhere", "path", vw)
	} else {
		log.Info("Visual Studio Build Tools not found: native Node modules (node-gyp) cannot be built in sessions")
	}
}

// nvmToolDirs returns nvm Node bin directories that contain codex or
// claude, newest version first.
func nvmToolDirs(home string) []string {
	matches, _ := filepath.Glob(filepath.Join(home, ".nvm", "versions", "node", "*", "bin"))
	var dirs []string
	for _, d := range matches {
		for _, t := range []string{"codex", "claude"} {
			if _, err := os.Stat(filepath.Join(d, t)); err == nil {
				dirs = append(dirs, d)
				break
			}
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return versionLess(dirs[j], dirs[i]) })
	return dirs
}

// versionLess compares ".../node/vX.Y.Z/bin" paths by numeric version.
func versionLess(a, b string) bool {
	va, vb := nodeVersion(a), nodeVersion(b)
	for i := 0; i < 3; i++ {
		if va[i] != vb[i] {
			return va[i] < vb[i]
		}
	}
	return a < b
}

func nodeVersion(binDir string) [3]int {
	v := strings.TrimPrefix(filepath.Base(filepath.Dir(binDir)), "v")
	var out [3]int
	for i, p := range strings.SplitN(v, ".", 3) {
		n := 0
		for _, c := range p {
			if c < '0' || c > '9' {
				break
			}
			n = n*10 + int(c-'0')
		}
		out[i] = n
	}
	return out
}
