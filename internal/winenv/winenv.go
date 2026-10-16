// Package winenv locates the developer tools a Windows Pyre node needs —
// Claude Code, Codex, Node/npm (nvm-windows, the official MSI, winget,
// scoop, volta), Git for Windows (Git Bash, which Claude Code uses for its
// shell and hooks) and the Visual Studio Build Tools that native Node module
// builds need. The logic is OS-neutral and takes its environment through
// Env, so it is unit-tested on any platform; only RegistryPath reads the
// live Windows registry.
package winenv

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// Env is the environment winenv inspects.
type Env struct {
	Getenv   func(string) string
	Stat     func(string) (os.FileInfo, error)
	LookPath func(string) (string, error)
	Glob     func(string) ([]string, error)
}

// Default is the live process environment.
func Default() Env {
	return Env{Getenv: os.Getenv, Stat: os.Stat, LookPath: lookPath, Glob: filepath.Glob}
}

func (e Env) exists(p string) bool {
	if p == "" {
		return false
	}
	_, err := e.Stat(p)
	return err == nil
}

func (e Env) join(envVar string, elem ...string) string {
	base := e.Getenv(envVar)
	if base == "" {
		return ""
	}
	return filepath.Join(append([]string{base}, elem...)...)
}

// ToolDirs returns the per-user and machine directories where Windows
// installs put claude, codex, node, npm, npx, git and bash, in preference
// order. A service started at logon has the PATH of that moment; these (and
// RegistryPath) cover tools installed later and installers that never touch
// PATH.
func (e Env) ToolDirs() []string {
	cands := []string{
		e.join("USERPROFILE", ".local", "bin"),                 // Claude Code native installer
		e.join("APPDATA", "npm"),                               // npm -g (claude/codex .cmd shims)
		e.Getenv("NVM_SYMLINK"),                                // nvm-windows active Node
		e.join("ProgramFiles", "nodejs"),                       // official MSI / winget OpenJS.NodeJS
		e.join("LOCALAPPDATA", "Volta", "bin"),                 // volta
		e.join("LOCALAPPDATA", "pnpm"),                         // pnpm global bin
		e.join("USERPROFILE", ".bun", "bin"),                   // bun
		e.join("LOCALAPPDATA", "Microsoft", "WinGet", "Links"), // winget portable apps
		e.join("USERPROFILE", "scoop", "shims"),                // scoop
		e.join("ProgramFiles", "Git", "cmd"),                   // Git for Windows (git.exe)
		e.join("LOCALAPPDATA", "Programs", "Git", "cmd"),       // per-user Git for Windows
		e.join("LOCALAPPDATA", "Programs", "Pyre", "current", "bin"),
	}
	var out []string
	seen := map[string]bool{}
	for _, d := range cands {
		if d == "" || seen[strings.ToLower(d)] {
			continue
		}
		seen[strings.ToLower(d)] = true
		if st, err := e.Stat(d); err == nil && st.IsDir() {
			out = append(out, d)
		}
	}
	return out
}

// GitBash returns Git for Windows' bash.exe: $CLAUDE_CODE_GIT_BASH_PATH
// when it exists, else the standard install locations, else derived from a
// git.exe on PATH (<root>\cmd\git.exe -> <root>\bin\bash.exe). "" when none
// is found. It never returns WSL's System32\bash.exe.
func (e Env) GitBash() string {
	if p := e.Getenv("CLAUDE_CODE_GIT_BASH_PATH"); e.exists(p) {
		return p
	}
	for _, p := range []string{
		e.join("ProgramFiles", "Git", "bin", "bash.exe"),
		e.join("ProgramW6432", "Git", "bin", "bash.exe"),
		e.join("ProgramFiles(x86)", "Git", "bin", "bash.exe"),
		e.join("LOCALAPPDATA", "Programs", "Git", "bin", "bash.exe"),
		e.join("USERPROFILE", "scoop", "apps", "git", "current", "bin", "bash.exe"),
	} {
		if e.exists(p) {
			return p
		}
	}
	if e.LookPath != nil {
		if g, err := e.LookPath("git"); err == nil {
			root := filepath.Dir(filepath.Dir(g)) // …\Git\cmd\git.exe or …\Git\mingw64\bin\git.exe
			for _, p := range []string{filepath.Join(root, "bin", "bash.exe"), filepath.Join(filepath.Dir(root), "bin", "bash.exe")} {
				if e.exists(p) {
					return p
				}
			}
		}
	}
	return ""
}

// NativeCodex returns the native codex.exe behind an npm shim (codex.cmd or
// codex.ps1 in the npm global bin), or "" when there is none. Running the
// native binary avoids a cmd.exe + node wrapper chain around the long-lived
// app-server and cmd.exe's argument quoting rules.
func (e Env) NativeCodex(shim string) string {
	ext := strings.ToLower(filepath.Ext(shim))
	if ext != ".cmd" && ext != ".ps1" && ext != "" {
		return ""
	}
	pkg := filepath.Join(filepath.Dir(shim), "node_modules", "@openai", "codex")
	var found []string
	for _, pat := range []string{
		filepath.Join(pkg, "node_modules", "@openai", "codex-win32-*", "vendor", "*", "bin", "codex.exe"),
		filepath.Join(pkg, "node_modules", "@openai", "codex-win32-*", "vendor", "*", "codex", "codex.exe"),
		filepath.Join(pkg, "vendor", "*-pc-windows-msvc", "bin", "codex.exe"),
		filepath.Join(pkg, "vendor", "*-pc-windows-msvc", "codex", "codex.exe"),
	} {
		m, _ := e.Glob(pat)
		found = append(found, m...)
	}
	if len(found) == 0 {
		return ""
	}
	sort.Strings(found)
	// Prefer the binary for this machine's architecture.
	arch := "x86_64"
	if strings.EqualFold(e.Getenv("PROCESSOR_ARCHITECTURE"), "ARM64") {
		arch = "aarch64"
	}
	for _, p := range found {
		if strings.Contains(p, arch) {
			return p
		}
	}
	return found[0]
}

// NativeClaude returns the native claude.exe behind an npm shim (claude.cmd
// or claude.ps1 in the npm global bin), or "" when there is none. cmd.exe
// cuts a command line at its first newline, so Claude launched through
// claude.cmd lost every argument after the multi-line
// --append-system-prompt (the agent guide and the initial prompt; first GCP
// Windows node).
func (e Env) NativeClaude(shim string) string {
	ext := strings.ToLower(filepath.Ext(shim))
	if ext != ".cmd" && ext != ".ps1" && ext != "" {
		return ""
	}
	pkg := filepath.Join(filepath.Dir(shim), "node_modules", "@anthropic-ai", "claude-code")
	for _, pat := range []string{
		filepath.Join(pkg, "bin", "claude.exe"),
		filepath.Join(pkg, "node_modules", "@anthropic-ai", "claude-code-win32-*", "claude.exe"),
		filepath.Join(pkg, "vendor", "*", "claude.exe"),
	} {
		if m, _ := e.Glob(pat); len(m) > 0 {
			return m[0]
		}
	}
	return ""
}

// VSWhere returns the Visual Studio Installer's vswhere.exe (present when
// Visual Studio or the Build Tools are installed; node-gyp uses it to find
// the C++ toolchain for native modules), or "".
func (e Env) VSWhere() string {
	p := e.join("ProgramFiles(x86)", "Microsoft Visual Studio", "Installer", "vswhere.exe")
	if e.exists(p) {
		return p
	}
	return ""
}

// PosixPath converts a Windows path to the forward-slash form that Git Bash,
// cmd.exe and the Win32 API all accept (C:\Users\me -> C:/Users/me), for
// commands that a POSIX shell parses.
func PosixPath(p string) string { return strings.ReplaceAll(p, `\`, "/") }

// ConfigDir is Pyre's per-user config directory on Windows,
// %APPDATA%\Pyre (client.json, keys.toml, release-allowed-signers); "" on
// other platforms or when APPDATA is unset.
func ConfigDir(getenv func(string) string) string {
	if runtime.GOOS != "windows" {
		return ""
	}
	if d := getenv("APPDATA"); d != "" {
		return filepath.Join(d, "Pyre")
	}
	return ""
}

// StateDir is the node/client state directory on Windows,
// %LOCALAPPDATA%\Pyre (machine-local, not roamed); "" on other platforms or
// when LOCALAPPDATA is unset.
func StateDir(getenv func(string) string) string {
	if runtime.GOOS != "windows" {
		return ""
	}
	if d := getenv("LOCALAPPDATA"); d != "" {
		return filepath.Join(d, "Pyre")
	}
	return ""
}

// systemVars are the variables Windows programs need to start and to find
// the user's profile (Winsock needs SystemRoot; Node, Claude Code and Codex
// need USERPROFILE/APPDATA/LOCALAPPDATA, TEMP and PATHEXT).
var systemVars = []string{"SystemRoot", "SystemDrive", "windir", "ComSpec", "PATHEXT", "TEMP", "TMP",
	"USERPROFILE", "APPDATA", "LOCALAPPDATA", "HOMEDRIVE", "HOMEPATH", "USERNAME", "USERDOMAIN",
	"ProgramData", "ProgramFiles", "ProgramFiles(x86)", "ProgramW6432", "CommonProgramFiles",
	"NUMBER_OF_PROCESSORS", "PROCESSOR_ARCHITECTURE", "OS", "CLAUDE_CODE_GIT_BASH_PATH"}

// SystemEnv returns KEY=VALUE for the Windows system variables present in
// the current environment, to add to a deliberately minimal child
// environment; nil on other platforms.
func SystemEnv() []string {
	if runtime.GOOS != "windows" {
		return nil
	}
	var out []string
	for _, k := range systemVars {
		if v, ok := os.LookupEnv(k); ok {
			out = append(out, k+"="+v)
		}
	}
	return out
}
