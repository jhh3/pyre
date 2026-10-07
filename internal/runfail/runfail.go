// Package runfail turns a failed run's state_reason into one plain
// sentence for the inbox, the session preview and the digest ("Couldn't
// start: X's Codex login on server needs re-auth (401). Fix: ..."), instead
// of a bare "Session run failed" or a raw error chain.
package runfail

import (
	"regexp"
	"strings"
)

// Names resolves IDs in a reason to friendly names (both optional).
type Names struct {
	// Account returns an account's label ("" when unknown).
	Account func(accountID string) string
	// Node is the run's machine name ("" when unknown).
	Node string
}

var acctRe = regexp.MustCompile(`acct_[A-Za-z0-9_-]+`)

// Plain describes a failed run in one sentence. started reports whether the
// harness ever ran (false: the launch itself failed). Empty reason: "".
func Plain(reason string, started bool, n Names) string {
	s, _ := describe(reason, started, n)
	return s
}

// Summary is Plain restricted to fixed phrases (with names filled in): a
// reason Pyre does not recognise is never copied, since free text from a
// harness could carry content. A launch failure then reads "Couldn't start;
// open the session for the reason."; any other unrecognised reason: "".
// For inbox items and notifications.
func Summary(reason string, started bool, n Names) string {
	s, known := describe(reason, started, n)
	switch {
	case known:
		return s
	case strings.HasPrefix(s, "Couldn't start: "):
		return "Couldn't start; open the session for the reason."
	}
	return ""
}

func describe(reason string, started bool, n Names) (string, bool) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return "", false
	}
	l := strings.ToLower(reason)
	launch := !started || strings.Contains(l, "launch failed") || strings.Contains(l, "no eligible")
	prefix := "Failed: "
	if launch {
		prefix = "Couldn't start: "
	}
	harness := "the"
	switch {
	case strings.Contains(l, "openai") || strings.Contains(l, "codex"):
		harness = "Codex"
	case strings.Contains(l, "anthropic") || strings.Contains(l, "claude"):
		harness = "Claude"
	}
	who := ""
	if id := acctRe.FindString(reason); id != "" {
		who = id
		if n.Account != nil {
			if lb := n.Account(id); lb != "" {
				who = lb
			}
		}
	}
	on := ""
	if n.Node != "" {
		on = " on " + n.Node
	}
	login := harness + " login"
	if who != "" {
		login = who + "'s " + login
	} else if harness == "the" {
		login = "the account login"
	}
	switch {
	case rejected(l):
		fix := "pyre accounts login --node " + nodeOr(n.Node)
		if harness == "Claude" {
			fix = "pyre accounts add-token (or log in again" + on + ")"
		}
		return prefix + login + on + " needs re-auth (401). Fix: " + fix, true
	case strings.Contains(l, "no eligible") && (strings.Contains(l, "cooldown") || strings.Contains(l, "limit reached") || strings.Contains(l, "usage limit") ||
		strings.Contains(l, "exhausted") || strings.Contains(l, "window at")):
		return prefix + "no " + harness + " account with capacity right now; it can resume after the reset", true
	case strings.Contains(l, "no eligible"):
		return prefix + "no usable " + harness + " account" + on + " (" + tail(reason) + ")", false
	}
	return prefix + strings.TrimPrefix(strings.TrimPrefix(reason, "launch failed: "), "Launch failed: "), false
}

func rejected(l string) bool {
	return strings.Contains(l, "401") || strings.Contains(l, "unauthorized") || strings.Contains(l, "needs re-auth") ||
		strings.Contains(l, "login rejected") || strings.Contains(l, "login unusable") || strings.Contains(l, "sign in again")
}

func nodeOr(n string) string {
	if n == "" {
		return "<machine>"
	}
	return n
}

// tail is the last segment of an error chain ("a: b: c" -> "c").
func tail(reason string) string {
	parts := strings.Split(reason, ": ")
	return strings.TrimSpace(parts[len(parts)-1])
}
