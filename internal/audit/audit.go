// Package audit renders the owner-facing audit log (W-audit): plain-English
// summaries of contracts.AuditEntry records, shared by the hub, the mock
// backend, the CLI and the TUI so every surface says the same thing.
package audit

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jhh3/pyre/internal/contracts"
)

// ActionSessionLaunchFailed: a run never started (Reason: the run's
// "launch failed: <reason>"). Kept here until contracts/audit.go carries it.
const ActionSessionLaunchFailed = "session.launch_failed"

// KindOf returns the audit kind of an action ("account.x" -> account,
// "session.x" -> session, anything else -> fleet).
func KindOf(action string) contracts.AuditKind {
	switch {
	case strings.HasPrefix(action, "account."):
		return contracts.AuditAccount
	case strings.HasPrefix(action, "session."):
		return contracts.AuditSession
	}
	return contracts.AuditFleet
}

// ParseKind accepts "account", "session" or "fleet" (plural and case
// tolerant).
func ParseKind(s string) (contracts.AuditKind, error) {
	k := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), "s")
	switch contracts.AuditKind(k) {
	case contracts.AuditAccount, contracts.AuditSession, contracts.AuditFleet:
		return contracts.AuditKind(k), nil
	}
	return "", fmt.Errorf("unknown audit kind %q (account, session or fleet)", s)
}

// windowName is a usage window's short display name.
func windowName(n string) string {
	switch n {
	case "five_hour":
		return "5h"
	case "seven_day":
		return "weekly"
	case "seven_day_opus":
		return "weekly opus"
	case "seven_day_sonnet":
		return "weekly sonnet"
	}
	return strings.ReplaceAll(n, "_", " ")
}

// UsageNote is the dominant usage window of an observation, e.g.
// "weekly 96%" ("" when nothing is known).
func UsageNote(o *contracts.AccountObservation) string {
	if o == nil || len(o.Windows) == 0 {
		return ""
	}
	ws := append([]contracts.UsageWindow(nil), o.Windows...)
	sort.SliceStable(ws, func(i, j int) bool { return ws[i].UsedPercent > ws[j].UsedPercent })
	return windowName(ws[0].Name) + " " + strconv.Itoa(int(ws[0].UsedPercent+0.5)) + "%"
}

// ShortReason condenses a fallback/limit reason to a few words.
func ShortReason(reason string) string {
	r := strings.ToLower(reason)
	switch {
	case strings.HasPrefix(r, "usage_limit"), strings.Contains(r, "usage limit"), strings.Contains(r, "limit reached"):
		return "limit reached"
	case strings.HasPrefix(r, "rate_limited"), strings.Contains(r, "rate limit"):
		return "rate limited"
	case strings.HasPrefix(r, "auth_invalid"), strings.Contains(r, "login"):
		return "login failed"
	}
	return clip(reason, 80)
}

// clip shortens s to n runes with an ellipsis.
func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

func quote(s string) string {
	if s == "" {
		return "a session"
	}
	return "'" + clip(s, 60) + "'"
}

func orID(label, id string) string {
	if label != "" {
		return label
	}
	if id != "" {
		return id
	}
	return "an account"
}

func withNote(label, note string) string {
	if note == "" {
		return label
	}
	return label + " (" + note + ")"
}

// By is the actor suffix (" — by owner", " — by session 'x'"); empty for
// Pyre's own actions.
func By(a contracts.AuditActor) string {
	switch a.Kind {
	case "owner":
		// Client names often read "owner (laptop)": show "by owner (laptop)".
		n := strings.TrimSpace(strings.TrimPrefix(a.Name, "owner"))
		if strings.HasPrefix(n, "(") && strings.HasSuffix(n, ")") {
			n = strings.TrimSpace(n[1 : len(n)-1])
		}
		if n != "" {
			return " — by owner (" + n + ")"
		}
		return " — by owner"
	case "session":
		if a.Name != "" {
			return " — by session " + quote(a.Name)
		}
		return " — by session " + a.ID
	case "node":
		if a.Name != "" {
			return " — by " + a.Name
		}
	}
	return ""
}

func because(r string) string {
	if strings.TrimSpace(r) == "" {
		return ""
	}
	return " — " + clip(r, 100)
}

// Summarize renders e as one plain-English line without time or machine.
// Display fields (titles, labels, names) must already be resolved.
func Summarize(e contracts.AuditEntry) string {
	acct := orID(e.AccountLabel, e.AccountID)
	title := quote(e.SessionTitle)
	switch e.Action {
	case contracts.AuditAccountChosen:
		return fmt.Sprintf("chose %s for %s", withNote(acct, e.ToNote), title) + because(e.Reason)
	case contracts.AuditAccountSwitched:
		return fmt.Sprintf("switched %s from %s to %s", title, withNote(orID(e.From, e.FromAccountID), e.FromNote),
			withNote(orID(e.To, e.ToAccountID), e.ToNote)) + " — " + ShortReason(e.Reason)
	case contracts.AuditAccountWaiting:
		if e.To == "switching paused" {
			return fmt.Sprintf("%s hit a limit on %s again soon after a switch; automatic account switching paused (needs you)", title,
				withNote(orID(e.From, e.FromAccountID), e.FromNote)) + because(e.Reason)
		}
		s := fmt.Sprintf("%s hit a limit on %s; no other account is eligible", title, withNote(orID(e.From, e.FromAccountID), e.FromNote))
		if e.To != "" {
			s += " (" + e.To + ")"
		}
		return s + because(e.Reason)
	case contracts.AuditAccountReauthNeeded:
		return acct + " needs re-auth" + because(e.Reason)
	case contracts.AuditAccountReauthed:
		return acct + " login is valid again"
	case contracts.AuditAccountCooldown:
		return withNote(acct, e.ToNote) + " reached its limit" + because(e.Reason)
	case contracts.AuditAccountCooldownEnded:
		return withNote(acct, e.ToNote) + " is usable again"
	case contracts.AuditAccountTokenAdded:
		return "token added for " + acct + By(e.Actor)
	case contracts.AuditAccountTokenReplaced:
		return "token replaced for " + acct + By(e.Actor)
	case contracts.AuditAccountTokenRemoved:
		return "token removed from " + acct + By(e.Actor)
	case contracts.AuditAccountTokenSynced:
		return acct + " token synced to this machine"
	case contracts.AuditAccountLoginAdded:
		s := acct + " login added on this machine"
		if e.Reason != "" {
			s += " (" + clip(e.Reason, 60) + ")"
		}
		return s
	case contracts.AuditAccountAdded:
		s := "account " + acct + " added"
		if e.Reason != "" {
			s += " (" + e.Reason + ")"
		}
		return s + By(e.Actor)
	case contracts.AuditAccountFolded:
		return fmt.Sprintf("folded account %s into %s", orID(e.From, e.FromAccountID), orID(e.To, e.ToAccountID)) + because(e.Reason)
	case contracts.AuditAccountRenamed:
		return fmt.Sprintf("renamed account %s to %s", e.From, e.To) + By(e.Actor)
	case contracts.AuditAccountDisabled:
		return "disabled account " + acct + By(e.Actor)
	case contracts.AuditAccountEnabled:
		return "enabled account " + acct + By(e.Actor)

	case contracts.AuditSessionStarted:
		s := "started " + title
		if e.To != "" {
			s += " (" + e.To + ")"
		}
		return s + By(e.Actor)
	case ActionSessionLaunchFailed:
		return "could not start " + title + because(e.Reason)
	case contracts.AuditSessionStopped:
		return "stopped " + title + By(e.Actor) + because(e.Reason)
	case contracts.AuditSessionInterrupted:
		return "interrupted " + title + By(e.Actor) + because(e.Reason)
	case contracts.AuditSessionResumed:
		s := "resumed " + title
		if e.To != "" {
			s += " (" + e.To + ")"
		}
		return s + By(e.Actor)
	case contracts.AuditSessionArchived:
		return "archived " + title + By(e.Actor) + because(e.Reason)
	case contracts.AuditSessionArchiveScheduled:
		return title + " will be archived when its turn ends" + By(e.Actor)
	case contracts.AuditSessionArchiveCanceled:
		return "canceled the scheduled archive of " + title + By(e.Actor)
	case contracts.AuditSessionUnarchived:
		return "unarchived " + title + By(e.Actor)
	case contracts.AuditSessionIconSet:
		if e.To == "" {
			return "cleared the icon of " + title + By(e.Actor)
		}
		return "set the icon of " + title + " to " + e.To + By(e.Actor)
	case contracts.AuditSessionRenamed:
		return fmt.Sprintf("renamed %s to %s", quote(e.From), quote(e.To)) + By(e.Actor)
	case contracts.AuditSessionAutoTitled:
		if e.From == "" {
			return "named a session " + quote(e.To) + " automatically"
		}
		return fmt.Sprintf("named %s %s automatically", quote(e.From), quote(e.To))
	case contracts.AuditSessionLeaseTakenOver:
		s := "took over typing in " + title
		if e.From != "" {
			s += " from " + e.From
		}
		return s + By(e.Actor)
	case contracts.AuditSessionReparented: // W-reparent; Reason: from "A" to "B"
		if e.Reason != "" {
			return "moved " + title + " " + e.Reason + By(e.Actor)
		}
		return fmt.Sprintf("moved %s from %s to %s", title, orDash(e.From), orDash(e.To)) + By(e.Actor)
	case contracts.AuditSessionReportedToParent:
		s := title + " reported to its parent"
		if e.To != "" {
			s += " " + quote(e.To)
		}
		return s

	case contracts.AuditNodeEnrolled:
		return "machine enrolled" + By(e.Actor)
	case contracts.AuditNodeRevoked:
		return "machine revoked" + By(e.Actor)
	case contracts.AuditNodeConnected:
		if e.To != "" {
			return "connected (pyred " + e.To + ")"
		}
		return "connected"
	case contracts.AuditNodeDisconnected:
		return "disconnected"
	case contracts.AuditNodeUpgraded:
		return fmt.Sprintf("pyred upgraded %s → %s", orDash(e.From), orDash(e.To))
	case contracts.AuditNodeUpdateFailed:
		return fmt.Sprintf("update to %s did not complete", orDash(e.To)) + because(e.Reason)
	case contracts.AuditHubUpgraded:
		return fmt.Sprintf("hub upgraded %s → %s", orDash(e.From), orDash(e.To))
	case contracts.AuditNodeTrustAccepted:
		s := "answered the folder-trust prompt for " + title
		if e.Reason != "" {
			s += " (" + clip(e.Reason, 60) + ", owner-trusted)"
		}
		return s
	case contracts.AuditNodeExitConfirmed:
		return "confirmed Claude's exit dialog while stopping " + title + " (background tasks stopped)"
	case contracts.AuditNodeCapacityRetry:
		return "continued " + title + " after the model was at capacity" + because(e.Reason)
	}
	s := e.Action
	if e.SessionTitle != "" {
		s += " " + title
	}
	return s + because(e.Reason)
}

func orDash(s string) string {
	if s == "" {
		return "?"
	}
	return s
}

// Where is the entry's machine for display ("" when none).
func Where(e contracts.AuditEntry) string {
	if e.NodeName != "" {
		return e.NodeName
	}
	if e.NodeID != "" {
		return strings.TrimPrefix(e.NodeID, "node_")
	}
	if KindOf(e.Action) == contracts.AuditFleet && strings.HasPrefix(e.Action, "hub.") {
		return "hub"
	}
	return ""
}

// When formats At for a list: "15:04" today, "Mon 15:04" within a week,
// else "Jan 2 15:04" (local time).
func When(at, now time.Time) string {
	at, now = at.Local(), now.Local()
	y1, m1, d1 := at.Date()
	y2, m2, d2 := now.Date()
	switch {
	case y1 == y2 && m1 == m2 && d1 == d2:
		return at.Format("15:04")
	case now.Sub(at) < 6*24*time.Hour && now.After(at):
		return at.Format("Mon 15:04")
	}
	return at.Format("Jan 2 15:04")
}

// Line is "14:05 server · <summary>".
func Line(e contracts.AuditEntry, now time.Time) string {
	s := When(e.At, now)
	if w := Where(e); w != "" {
		s += " " + w
	}
	sum := e.Summary
	if sum == "" {
		sum = Summarize(e)
	}
	return s + " · " + sum
}

// ParseSince accepts a duration such as 24h, 90m or 7d (back from now),
// or an RFC 3339 time. "" means no bound.
func ParseSince(s string, now time.Time) (*time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	if strings.HasSuffix(s, "d") {
		if n, err := strconv.Atoi(strings.TrimSuffix(s, "d")); err == nil && n >= 0 {
			t := now.Add(-time.Duration(n) * 24 * time.Hour)
			return &t, nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil && d >= 0 {
		t := now.Add(-d)
		return &t, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return &t, nil
	}
	return nil, fmt.Errorf("--since %q: want a duration (24h, 90m, 7d) or an RFC 3339 time", s)
}
