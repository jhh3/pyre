package runfail

import (
	"fmt"
	"hash/fnv"
	"regexp"
	"strconv"
	"strings"
)

// Failure causes (owner report 2026-10-17: 332 separate "Session run failed"
// inbox items in two weeks, 179 on one day, most of them a handful of
// causes). A cause is what a run failed of with the per-session detail
// (IDs, paths, numbers, times) left out, so every run that failed the same
// way has the same cause and the inbox shows one item for them.

// Cause is a failed run's cause.
type Cause struct {
	// Key is stable and safe to store and show: "start.<code>" for a launch
	// that never ran, "run.<code>" for a run that did. A reason Pyre does
	// not recognise gets code "other-<hash of the normalised reason>", so
	// its text is never copied.
	Key string
	// Launch reports that the harness never ran.
	Launch bool
}

// cause is one recognised cause: its code, plain phrase (fits "N sessions
// failed to start: <label>") and what to do about it.
type cause struct {
	code, label, hint string
	match             func(l string) bool
}

func has(subs ...string) func(string) bool {
	return func(l string) bool {
		for _, s := range subs {
			if strings.Contains(l, s) {
				return true
			}
		}
		return false
	}
}

// launchCauses are the recognised launch failures, first match wins
// (rejected logins and "no eligible" are handled before them).
var launchCauses = []cause{
	{"cwd-missing", "working folder missing", "The working folder does not exist on the machine; start again from one that does, or archive.",
		func(l string) bool {
			return (strings.Contains(l, "working directory") || strings.Contains(l, "cwd")) && (strings.Contains(l, "does not exist") || strings.Contains(l, "no such file"))
		}},
	{"cwd-required", "no working folder given", "Start again with a working folder (--cwd).", has("working directory is required")},
	{"resume-unsupported", "can't resume on this account", "There is no native thread to resume on the account it would use; start a new session, or resume on the original account.",
		has("native resume unsupported", "no native thread to resume")},
	{"expired", "the machine did not start it in time", "Nothing ran (the machine was offline or busy); start again.", has("command expired")},
	{"interrupted", "pyred restarted before it started", "Nothing ran; relaunch to retry.", has("launch interrupted")},
	{"bypass", "Claude's bypass-permissions warning not accepted on the machine", "Run claude once on that machine and accept the warning, then retry.",
		has("permissions bypass", "bypass-permissions warning")},
	{"command-too-long", "start command too long", "Shorten the prompt (or pass it in a file) and retry.", has("command too long")},
	{"connection-lost", "lost the harness while starting", "Whether it started is unknown; check the session before retrying.",
		has("outcome unknown", "connection lost")},
	{"account-missing", "account not found", "The account asked for is gone; start with another.", has("account not found")},
}

// CauseOf returns a failed run's cause. exitCode is the run's exit code
// (nil when unknown); a run that ran and failed for a reason Pyre does not
// recognise is grouped by it.
func CauseOf(reason string, exitCode *int) Cause {
	reason = strings.TrimSpace(reason)
	l := strings.ToLower(reason)
	launch := strings.HasPrefix(l, "launch ") || strings.Contains(l, "launch failed") || strings.Contains(l, "no eligible")
	c := Cause{Launch: launch}
	stage := "run."
	if launch {
		stage = "start."
	}
	harness := harnessCode(l)
	switch {
	case rejected(l):
		c.Key = stage + "reauth-" + harness
		return c
	case capacity(l):
		c.Key = stage + "capacity-" + harness
		return c
	case strings.Contains(l, "no eligible"):
		c.Key = stage + "no-account-" + harness
		return c
	}
	if launch {
		for _, k := range launchCauses {
			if k.match(l) {
				c.Key = stage + k.code
				return c
			}
		}
	} else if exitCode != nil {
		c.Key = stage + "exit-" + strconv.Itoa(*exitCode)
		return c
	}
	h := fnv.New32a()
	h.Write([]byte(normalise(l)))
	c.Key = fmt.Sprintf("%sother-%08x", stage, h.Sum32())
	return c
}

func harnessCode(l string) string {
	switch {
	case isAgy(l):
		return "agy"
	case strings.Contains(l, "openai") || strings.Contains(l, "codex"):
		return "codex"
	case strings.Contains(l, "anthropic") || strings.Contains(l, "claude"):
		return "claude"
	}
	return "any"
}

func harnessName(code string) string {
	switch code {
	case "codex":
		return "Codex"
	case "claude":
		return "Claude"
	case "agy":
		return "agy"
	}
	return "the"
}

// CauseLaunch reports whether a cause key is a launch that never ran.
func CauseLaunch(key string) bool { return strings.HasPrefix(key, "start.") }

func causeCode(key string) string {
	_, code, _ := strings.Cut(key, ".")
	return code
}

// CauseLabel is a cause's plain phrase ("working folder missing", "exit
// code 1"), or "" for a cause Pyre does not recognise.
func CauseLabel(key string) string {
	code := causeCode(key)
	switch {
	case strings.HasPrefix(code, "reauth-"):
		h := harnessName(strings.TrimPrefix(code, "reauth-"))
		if h == "the" {
			return "account login needs re-auth"
		}
		return h + " login needs re-auth"
	case strings.HasPrefix(code, "capacity-"):
		return "no " + harnessName(strings.TrimPrefix(code, "capacity-")) + " account with capacity"
	case strings.HasPrefix(code, "no-account-"):
		return "no usable " + harnessName(strings.TrimPrefix(code, "no-account-")) + " account"
	case strings.HasPrefix(code, "exit-"):
		return "exit code " + strings.TrimPrefix(code, "exit-")
	}
	for _, k := range launchCauses {
		if k.code == code {
			return k.label
		}
	}
	return ""
}

// CauseHint says what to do about a cause, in fixed words.
func CauseHint(key string) string {
	code := causeCode(key)
	switch {
	case strings.HasPrefix(code, "reauth-"):
		if code == "reauth-claude" {
			return "Fix: pyre accounts add-token (or log in again on the machine), then retry."
		}
		return "Fix: pyre accounts login --node <machine>, then retry."
	case strings.HasPrefix(code, "capacity-"):
		return "Resume or retry after the reset."
	case strings.HasPrefix(code, "no-account-"):
		return "Check pyre accounts for the machine, then retry."
	case strings.HasPrefix(code, "exit-"):
		return "Open a session for what happened; resume or archive."
	}
	for _, k := range launchCauses {
		if k.code == code {
			return k.hint
		}
	}
	if CauseLaunch(key) {
		return "Open a session for the reason."
	}
	return "Open a session for what happened."
}

var (
	quotedRe = regexp.MustCompile(`"[^"]*"|'[^']*'|` + "`[^`]*`")
	idRe     = regexp.MustCompile(`\b[a-z]{2,8}_[0-9a-z_-]{4,}\b`)
	pathRe   = regexp.MustCompile(`(?:[a-z]:\\|~?/)[^\s:;,()]*`)
	hexRe    = regexp.MustCompile(`\b[0-9a-f]{8,}\b|\b[0-9a-f]{8}-[0-9a-f-]{27}\b`)
	numRe    = regexp.MustCompile(`[0-9]+`)
	spaceRe  = regexp.MustCompile(`\s+`)
)

// normalise drops per-run detail from a lowercased reason: quoted text,
// IDs, paths, hex digests, numbers (and so times), extra spaces.
func normalise(l string) string {
	l = quotedRe.ReplaceAllString(l, `""`)
	l = idRe.ReplaceAllString(l, "<id>")
	l = pathRe.ReplaceAllString(l, "<path>")
	l = hexRe.ReplaceAllString(l, "<hex>")
	l = numRe.ReplaceAllString(l, "#")
	return strings.TrimSpace(spaceRe.ReplaceAllString(l, " "))
}
