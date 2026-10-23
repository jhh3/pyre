package sentinel

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// pacific is where the owner reads times. Loaded on first use: pyred runs
// for every Claude hook and must not read the tz database at init.
var pacific = sync.OnceValue(func() *time.Location {
	if l, err := time.LoadLocation("America/Los_Angeles"); err == nil {
		return l
	}
	return time.FixedZone("PT", -8*3600)
})

// clockPT formats t as "12:35 AM PT", or "Oct 18, 9:12 AM PT" when t is
// not on now's Pacific date.
func clockPT(t, now time.Time) string {
	t, now = t.In(pacific()), now.In(pacific())
	ty, tm, td := t.Date()
	ny, nm, nd := now.Date()
	if ty == ny && tm == nm && td == nd {
		return t.Format("3:04 PM") + " PT"
	}
	if ty == ny {
		return t.Format("Jan 2, 3:04 PM") + " PT"
	}
	return t.Format("Jan 2 2006, 3:04 PM") + " PT"
}

// span formats a duration as "6 min", "2 h", "2 h 5 min".
func span(d time.Duration) string {
	m := int(d.Round(time.Minute) / time.Minute)
	if m < 60 {
		return strconv.Itoa(max(m, 1)) + " min"
	}
	h, m := m/60, m%60
	if m == 0 {
		return strconv.Itoa(h) + " h"
	}
	return fmt.Sprintf("%d h %d min", h, m)
}

// describe summarises an observation for a message: "Hub unreachable on
// LAN and Tailscale; ssh works, user manager not running."
func describe(o *Observation, c Cond) string {
	if o == nil {
		return ""
	}
	var parts []string
	if len(o.Hub) > 0 {
		var failed []string
		for _, h := range o.Hub {
			if !h.OK {
				failed = append(failed, h.Label)
			}
		}
		switch {
		case len(failed) == len(o.Hub):
			parts = append(parts, "Hub unreachable on "+joinAnd(failed))
		case len(failed) == 0:
			parts = append(parts, "Hub up")
		}
	}
	switch o.SSH {
	case SSHOK:
		s := "ssh works"
		switch o.Manager {
		case ManagerAbsent:
			s += ", user manager not running"
			if o.ManagerRaw != "" {
				s += " (is-system-running: " + o.ManagerRaw + ")"
			}
		case ManagerTimeout:
			s += ", user manager not answering (systemctl timed out)"
		}
		parts = append(parts, s)
	case SSHHung:
		parts = append(parts, "ssh connects but the probe hung (systemd or D-Bus not answering?)")
	case SSHUnreachable:
		parts = append(parts, "ssh fails ("+o.SSHDetail+")")
	case SSHAuth:
		parts = append(parts, "ssh refused this sentinel ("+o.SSHDetail+")")
	case SSHError:
		parts = append(parts, "ssh error ("+o.SSHDetail+")")
	}
	if len(o.UnitsDown) > 0 {
		parts = append(parts, "not active: "+strings.Join(o.UnitsDown, ", "))
	}
	if len(o.TCPDown) > 0 {
		parts = append(parts, "port closed: "+strings.Join(o.TCPDown, ", "))
	}
	var pub []string
	for _, p := range o.Public {
		if !p.OK {
			pub = append(pub, p.URL+" ("+p.Detail+")")
		}
	}
	if len(pub) > 0 {
		parts = append(parts, "public URL down: "+strings.Join(pub, ", "))
	}
	if c.SentinelMissing {
		parts = append(parts, "its sentinel is not reporting")
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "; ") + "."
}

func sentence(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, " ")
}

// alertText is the first (or escalation) alert for an incident.
func alertText(target string, level Level, since time.Time, c Cond, o *Observation, recovery string, now time.Time) string {
	var head string
	switch level {
	case LevelDown:
		head = fmt.Sprintf("Pyre: %s is DOWN since %s (confirmed by %s).", target, clockPT(since, now), strings.Join(c.DownBy, ", "))
	case LevelUnconfirmed:
		who := "no other sentinel is reporting"
		if len(c.Missing) > 0 {
			who = strings.Join(c.Missing, ", ") + " not reporting"
		}
		head = fmt.Sprintf("Pyre: %s may be DOWN since %s (unconfirmed: only %s can check; %s).", target, clockPT(since, now), strings.Join(c.DownBy, ", "), who)
	default:
		by := c.voters()
		seen := ""
		if len(by) > 0 {
			seen = " (seen by " + strings.Join(by, ", ") + ")"
		}
		head = fmt.Sprintf("Pyre: %s is DEGRADED since %s%s.", target, clockPT(since, now), seen)
	}
	return sentence(head, describe(o, c), recovery)
}

func reminderText(target string, level Level, since time.Time, c Cond, o *Observation, now time.Time) string {
	head := fmt.Sprintf("Pyre reminder: %s is still %s (since %s, %s).", target, level, clockPT(since, now), span(now.Sub(since)))
	return sentence(head, describe(o, c))
}

func recoveredText(inc *Incident, now time.Time) string {
	head := fmt.Sprintf("Pyre: %s recovered at %s (%s since %s, %s).", inc.Target, clockPT(now, now), inc.Level, clockPT(inc.Since, now), span(now.Sub(inc.Since)))
	rec := ""
	if n := len(inc.Recovery); n > 0 {
		rec = fmt.Sprintf("Auto-recovery: %d ssh login(s), last at %s.", n, clockPT(inc.Recovery[n-1].At, now))
	}
	return sentence(head, rec, "Incident "+inc.ID+".")
}

func manualHelpText(inc *Incident, c Cond, o *Observation, now time.Time) string {
	last := inc.Recovery[len(inc.Recovery)-1]
	res := ""
	if last.Result != "" {
		res = ": " + last.Result
	}
	head := fmt.Sprintf("Pyre: %s is still DOWN after %d auto-recovery attempts (last at %s%s). Needs manual help.", inc.Target, len(inc.Recovery), clockPT(last.At, now), res)
	return sentence(head, describe(o, c))
}

func restartText(target string, started, previous time.Time, muted int, now time.Time) string {
	prev := ""
	if !previous.IsZero() {
		prev = " (previous start " + clockPT(previous, now) + ")"
	}
	more := ""
	if muted > 0 {
		more = fmt.Sprintf(" It also restarted %d more time(s) in the past hour without a message.", muted)
	}
	return fmt.Sprintf("Pyre: %s's systemd user manager restarted at %s%s. Everything it ran (pyre-hub, pyred, tmux sessions, tunnels) restarted or stopped with it; check sessions and hand-started services.%s", target, clockPT(started, now), prev, more)
}

// flapText reports an incident that came back soon after "recovered".
func flapText(target string, level Level, since time.Time, inc *Incident, c Cond, o *Observation, now time.Time) string {
	head := fmt.Sprintf("Pyre: %s is %s again since %s (it had recovered; incident %s, flapping). The next \"recovered\" waits for 30 min stable.", target, level, clockPT(since, now), inc.ID)
	return sentence(head, describe(o, c))
}

// recoveryLine is the "Auto-recovery: ..." sentence of a DOWN alert.
func recoveryLine(t Target, o *Observation, attempt *RecoveryAttempt, n, maxN int, now time.Time) string {
	if attempt != nil {
		s := fmt.Sprintf("Auto-recovery: ssh login (attempt %d of %d) at %s", n, maxN, clockPT(attempt.At, now))
		if attempt.Result != "" && attempt.Result != "ok" {
			s += ": " + attempt.Result
		}
		return s + "."
	}
	switch {
	case t.NoRecovery:
		return "Auto-recovery: off for this host."
	case o == nil:
		return ""
	case o.SSH == SSHUnreachable:
		return "Auto-recovery: none possible (ssh fails, so the host is off or off the network)."
	case o.SSH == SSHHung || o.Manager == ManagerTimeout:
		return "Auto-recovery: none (ssh connects but systemd is not answering; needs a look)."
	case o.SSH == SSHOK && o.Manager != ManagerAbsent:
		return "Auto-recovery: none (the user manager is running; check the hub service itself)."
	}
	return ""
}

// parseUnixStamp parses systemctl --timestamp=unix output ("@1792505198").
func parseUnixStamp(s string) time.Time {
	s = strings.TrimPrefix(strings.TrimSpace(s), "@")
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}
	}
	return time.Unix(n, 0)
}
