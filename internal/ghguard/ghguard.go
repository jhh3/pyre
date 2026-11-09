// Package ghguard keeps one session tree from exhausting the GitHub API
// budget that every agent on every machine shares (one gh login per
// machine, the same account everywhere).
//
// pyred puts a `gh` link to the pyre binary first on each run's PATH
// (daemon ghguard.go). Started under that name, pyre runs Main: it raises
// the poll interval of watch commands, asks pyred for a token from the
// session's budget (Limiter) over the local socket, then execs the real gh
// found later on PATH. pyred counts the calls per session and samples
// `gh api rate_limit` for `pyre nodes` and get_context.
package ghguard

import (
	"path/filepath"
	"strings"
	"time"
)

const (
	// PathCall is the local-socket endpoint the shim asks before each call.
	PathCall = "/v1/local/gh/call"
	// EnvShimDir names the shim's directory, so the shim skips it when it
	// looks for the real gh.
	EnvShimDir = "PYRE_GH_SHIM_DIR"
	// EnvGuard set to "off" makes the shim a plain pass-through.
	EnvGuard = "PYRE_GH_GUARD"
	// WatchInterval is the poll interval (seconds) given to `gh run watch`
	// and `gh pr checks --watch` when the caller set none (gh polls every
	// 3 s and 10 s by default).
	WatchInterval = "60"
)

// CallRequest is the shim's question to pyred. The session comes from the
// caller's bearer (PYRE_SESSION_TOKEN).
type CallRequest struct {
	RunID   string `json:"run_id,omitempty"`
	Command string `json:"command,omitempty"` // e.g. "api", "pr view"
}

// CallResponse is pyred's answer: wait WaitMS before calling, or refuse.
// Message explains a wait or refusal; Warn is a low-budget warning to show
// the session (at most every few minutes).
type CallResponse struct {
	WaitMS  int64  `json:"wait_ms,omitempty"`
	Refuse  bool   `json:"refuse,omitempty"`
	Message string `json:"message,omitempty"`
	Warn    string `json:"warn,omitempty"`
}

// IsShimName reports whether argv0 names the gh shim.
func IsShimName(argv0 string) bool {
	b := strings.ToLower(filepath.Base(argv0))
	return strings.TrimSuffix(b, ".exe") == "gh"
}

// words returns the first n positional arguments (the command words),
// stopping at "--".
func words(args []string, n int) []string {
	var out []string
	for _, a := range args {
		if a == "--" || len(out) == n {
			break
		}
		if !strings.HasPrefix(a, "-") {
			out = append(out, a)
		}
	}
	return out
}

// Command is the one- or two-word gh command args run, e.g. "pr view".
func Command(args []string) string {
	w := words(args, 2)
	if len(w) == 2 && w[0] == "api" {
		w = w[:1]
	}
	return strings.Join(w, " ")
}

// hasFlag reports whether args (before "--") set any of the flags, as
// "--flag", "--flag=v" or, for a short flag, "-iV".
func hasFlag(args []string, flags ...string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		for _, f := range flags {
			if a == f || strings.HasPrefix(a, f+"=") || (len(f) == 2 && strings.HasPrefix(a, f) && !strings.HasPrefix(a, "--")) {
				return true
			}
		}
	}
	return false
}

// RewriteArgs gives `gh run watch` and `gh pr checks --watch` a 60 s poll
// interval unless the caller chose one. Other commands are unchanged.
func RewriteArgs(args []string) []string {
	w := words(args, 2)
	if len(w) < 2 {
		return args
	}
	watch := (w[0] == "run" && w[1] == "watch") || (w[0] == "pr" && w[1] == "checks" && hasFlag(args, "--watch"))
	if !watch || hasFlag(args, "-i", "--interval") {
		return args
	}
	out := make([]string, 0, len(args)+2)
	for i, a := range args {
		if a == "--" {
			out = append(out, "--interval", WatchInterval)
			return append(out, args[i:]...)
		}
		out = append(out, a)
	}
	return append(out, "--interval", WatchInterval)
}

// Exempt reports a call that never spends API budget: help, version,
// local configuration, and `gh api rate_limit` (free by GitHub's rules).
func Exempt(args []string) bool {
	if len(args) == 0 || hasFlag(args, "-h", "--help", "--version") {
		return true
	}
	w := words(args, 2)
	if len(w) == 0 {
		return true
	}
	switch w[0] {
	case "help", "version", "completion", "config", "alias", "environment", "reference":
		return true
	case "api":
		return len(w) == 2 && strings.Trim(w[1], "/") == "rate_limit"
	}
	return false
}

// Default limiter settings: about 30 calls a minute per session with a
// burst of 60, delaying a call up to 10 s before refusing it.
const (
	DefaultPerMinute = 30
	DefaultBurst     = 60
	DefaultMaxWait   = 10 * time.Second
)
