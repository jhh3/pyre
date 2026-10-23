// Package harnessargs validates the extra harness arguments a session may
// launch with (pyre start --harness-arg / `-- ARGS`, MCP start_session
// harness_args; contracts.StartSessionRequest.Args).
//
// Extra arguments are an allowlist per harness, never "anything at all":
// Pyre controls the session identity (--session-id, --resume), permissions
// (--dangerously-skip-permissions, --permission-mode), the model (--model,
// --effort) and the print/background modes itself, and an unknown flag could
// break supervision. Accepted arguments are rendered in one canonical form,
// "--flag=value" per value, so a variadic Claude option (--add-dir, --channels,
// --mcp-config, ...) can never swallow the prompt the adapter appends after
// the extra arguments, and a run's arguments read the same on every launch.
//
// The CLI checks early for a clear usage error; the node checks again
// (authoritative) before anything starts.
package harnessargs

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
)

// Bounds on one launch's extra arguments.
const (
	MaxArgs  = 32
	MaxBytes = 16 << 10
)

// arity of an allowed flag.
type arity int

const (
	one      arity = iota // exactly one value; the flag may repeat if the harness accumulates it
	variadic              // one or more values ("<dirs...>"): following non-flag tokens
)

type flagSpec struct {
	arity  arity
	repeat bool // may be given more than once
	path   bool // an absolute path on the node (Claude resolves no ~)
}

// claudeFlags are the Claude Code options a session may add (checked against
// `claude --help` and the 2.1.299 option table; --channels and
// --dangerously-load-development-channels are hidden options).
var claudeFlags = map[string]flagSpec{
	"--append-system-prompt":      {arity: one},
	"--append-system-prompt-file": {arity: one, path: true},
	"--system-prompt-file":        {arity: one, path: true},
	// plugin:NAME@MARKETPLACE or server:NAME; development channels show a
	// confirmation dialog at startup that the adapter accepts for the run.
	"--dangerously-load-development-channels": {arity: variadic, repeat: true},
	"--channels":   {arity: variadic, repeat: true},
	"--plugin-dir": {arity: one, repeat: true, path: true},
	"--add-dir":    {arity: variadic, repeat: true, path: true},
	"--mcp-config": {arity: variadic, repeat: true},
	// A file merged into the run's own settings (Claude takes one
	// --settings); the node refuses keys Pyre controls (SettingsKeysRefused).
	"--settings": {arity: one, path: true},
	"--agents":   {arity: one},
	"--agent":    {arity: one},
}

// pyreOwned explains why a known flag is refused.
var pyreOwned = map[contracts.Harness]map[string]string{
	contracts.HarnessClaude: {
		"--resume": "session identity", "-r": "session identity", "--continue": "session identity", "-c": "session identity",
		"--session-id": "session identity", "--fork-session": "session identity", "--from-pr": "session identity",
		"--dangerously-skip-permissions": "permissions (use --permissions bypass)", "--allow-dangerously-skip-permissions": "permissions (use --permissions bypass)",
		"--permission-mode": "permissions (use --permissions)", "--permission-prompt-tool": "permissions",
		"--model": "the model (use --model)", "--effort": "the effort (use --effort)",
		"-p": "the run mode (headless runs: pyre run)", "--print": "the run mode (headless runs: pyre run)",
		"--bg": "the run mode", "--background": "the run mode", "--tmux": "the run mode", "-w": "the run mode", "--worktree": "the run mode (use --worktree)",
		"--strict-mcp-config": "Pyre's own MCP server", "--bare": "Pyre's hooks", "--safe-mode": "Pyre's hooks",
	},
}

// allowed returns the flag table of harness h; nil means no extra arguments.
func allowed(h contracts.Harness) map[string]flagSpec {
	if h == contracts.HarnessClaude {
		return claudeFlags
	}
	return nil
}

// Passthrough reports harnesses whose extra arguments are their command line
// itself (shell runs `sh ARGS`; the mock runtime ignores them). They are not
// filtered.
func Passthrough(h contracts.Harness) bool {
	return h == contracts.HarnessShell || h == contracts.HarnessMock
}

// Allowed lists the flags harness h accepts (sorted), for help and errors.
func Allowed(h contracts.Harness) []string {
	var out []string
	for f := range allowed(h) {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// Normalize validates extra arguments for harness h and returns them in
// canonical form ("--flag=value" per value). Shell and mock arguments are
// returned unchanged. Errors are contracts.ErrInvalid with the reason.
func Normalize(h contracts.Harness, args []string) ([]string, error) {
	if len(args) == 0 {
		return nil, nil
	}
	if Passthrough(h) {
		return append([]string(nil), args...), nil
	}
	if len(args) > MaxArgs {
		return nil, contracts.Errorf(contracts.ErrInvalid, "too many harness arguments (%d, at most %d)", len(args), MaxArgs)
	}
	size := 0
	for _, a := range args {
		size += len(a)
		if strings.ContainsRune(a, 0) {
			return nil, contracts.Errorf(contracts.ErrInvalid, "harness argument contains a NUL byte")
		}
	}
	if size > MaxBytes {
		return nil, contracts.Errorf(contracts.ErrInvalid, "harness arguments are too long (%d bytes, at most %d)", size, MaxBytes)
	}
	table := allowed(h)
	if table == nil {
		return nil, contracts.Errorf(contracts.ErrInvalid, "%s takes no extra harness arguments", h)
	}
	var out []string
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		tok := args[i]
		if !strings.HasPrefix(tok, "-") || tok == "-" {
			return nil, contracts.Errorf(contracts.ErrInvalid, "harness argument %q is not a flag (prompts go in --prompt)", clip(tok))
		}
		name, val, hasVal := strings.Cut(tok, "=")
		spec, ok := table[name]
		if !ok {
			if why := pyreOwned[h][name]; why != "" || strings.HasPrefix(name, "--pyre-") {
				if why == "" {
					why = "Pyre's launch"
				}
				return nil, contracts.Errorf(contracts.ErrInvalid, "harness argument %s is controlled by Pyre (%s)", name, why)
			}
			return nil, contracts.Errorf(contracts.ErrInvalid, "harness argument %s is not allowed for %s (allowed: %s)", name, h, strings.Join(Allowed(h), ", "))
		}
		if seen[name] && !spec.repeat {
			return nil, contracts.Errorf(contracts.ErrInvalid, "harness argument %s given twice", name)
		}
		seen[name] = true
		var vals []string
		switch {
		case hasVal:
			vals = []string{val}
		case spec.arity == one:
			if i+1 >= len(args) {
				return nil, contracts.Errorf(contracts.ErrInvalid, "harness argument %s needs a value", name)
			}
			i++
			vals = []string{args[i]}
		default: // variadic: the following tokens up to the next flag
			for i+1 < len(args) && !isFlag(args[i+1]) {
				i++
				vals = append(vals, args[i])
			}
			if len(vals) == 0 {
				return nil, contracts.Errorf(contracts.ErrInvalid, "harness argument %s needs a value", name)
			}
		}
		if len(vals) > 1 && !spec.repeat {
			return nil, contracts.Errorf(contracts.ErrInvalid, "harness argument %s takes one value", name)
		}
		for _, v := range vals {
			if strings.TrimSpace(v) == "" {
				return nil, contracts.Errorf(contracts.ErrInvalid, "harness argument %s has an empty value", name)
			}
			if spec.path && !isAbs(v) {
				return nil, contracts.Errorf(contracts.ErrInvalid, "harness argument %s needs an absolute path on the machine (got %q)", name, clip(v))
			}
			out = append(out, name+"="+v)
		}
	}
	return out, nil
}

// Has reports whether canonical args contain flag name.
func Has(args []string, name string) bool {
	for _, a := range args {
		if n, _, _ := strings.Cut(a, "="); n == name {
			return true
		}
	}
	return false
}

// SettingsKeysRefused are top-level settings a session's --settings file
// may not set: they would override the account binding (env, API key
// helpers), the run's permission mode or Pyre's hooks.
var SettingsKeysRefused = []string{"env", "apiKeyHelper", "awsAuthRefresh", "awsCredentialExport", "otelHeadersHelper",
	"disableAllHooks", "forceLoginMethod", "forceLoginOrgUUID"}

// isAbs accepts Unix and Windows absolute paths (the node's platform).
func isAbs(p string) bool {
	return strings.HasPrefix(p, "/") || len(p) > 2 && p[1] == ':' && (p[2] == '\\' || p[2] == '/') || strings.HasPrefix(p, `\\`)
}

func isFlag(s string) bool { return len(s) > 1 && strings.HasPrefix(s, "-") }

func clip(s string) string {
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}

// String renders canonical args for display (one shell-like line).
func String(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		if strings.ContainsAny(a, " \t\n'\"\\$`") {
			q[i] = fmt.Sprintf("%q", a)
		} else {
			q[i] = a
		}
	}
	return strings.Join(q, " ")
}
