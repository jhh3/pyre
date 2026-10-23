package notifications

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Notification states recorded in contracts.NotificationStatus.State.
const (
	StatePending    = "pending"
	StateSent       = "sent"
	StateFailed     = "failed"
	StateQuietHours = "suppressed_quiet_hours"
	StateDisabled   = "disabled"
)

var knownKinds = []contracts.AttentionKind{
	contracts.AttnApproval, contracts.AttnQuestion, contracts.AttnFailure, contracts.AttnAccountPool,
	contracts.AttnReviewReady, contracts.AttnMessage, contracts.AttnIndeterminate, contracts.AttnConflict,
	contracts.AttnComment, contracts.AttnUpdate, contracts.AttnNode, contracts.AttnAsk, contracts.AttnWaiting, contracts.AttnGrantFYI,
}

// ValidatePrefs checks preferences, returning an ErrInvalid *contracts.Error.
func ValidatePrefs(p contracts.NotificationPrefs) error {
	if (p.QuietHoursStart == "") != (p.QuietHoursEnd == "") {
		return contracts.Errorf(contracts.ErrInvalid, "quiet_hours_start and quiet_hours_end must be set together")
	}
	if p.QuietHoursStart != "" {
		if _, err := parseClock(p.QuietHoursStart); err != nil {
			return contracts.Errorf(contracts.ErrInvalid, "quiet_hours_start: %v", err)
		}
		if _, err := parseClock(p.QuietHoursEnd); err != nil {
			return contracts.Errorf(contracts.ErrInvalid, "quiet_hours_end: %v", err)
		}
	}
	if p.TZ != "" {
		if _, err := time.LoadLocation(p.TZ); err != nil {
			return contracts.Errorf(contracts.ErrInvalid, "tz %q is not a known IANA time zone", p.TZ)
		}
	}
	switch p.MinSeverity {
	case "", "info", "normal", "urgent":
	default:
		return contracts.Errorf(contracts.ErrInvalid, "min_severity must be info, normal, or urgent")
	}
	for k := range p.Kinds {
		if !slices.Contains(knownKinds, k) {
			return contracts.Errorf(contracts.ErrInvalid, "unknown attention kind %q in kinds", k)
		}
	}
	return nil
}

// parseClock parses "HH:MM" (24h) into minutes after midnight.
func parseClock(s string) (int, error) {
	h, m, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok || len(h) == 0 || len(h) > 2 || len(m) != 2 {
		return 0, fmt.Errorf("%q is not HH:MM", s)
	}
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, fmt.Errorf("%q is not a valid 24-hour time", s)
	}
	return hh*60 + mm, nil
}

// InQuietHours reports whether t falls in the configured quiet window,
// evaluated in prefs.TZ (the hub's local zone when empty). Windows may span
// midnight ("22:00"-"07:00"). Equal start and end means no quiet hours.
func InQuietHours(p contracts.NotificationPrefs, t time.Time) bool {
	if p.QuietHoursStart == "" || p.QuietHoursEnd == "" {
		return false
	}
	start, err1 := parseClock(p.QuietHoursStart)
	end, err2 := parseClock(p.QuietHoursEnd)
	if err1 != nil || err2 != nil || start == end {
		return false
	}
	loc := time.Local
	if p.TZ != "" {
		if l, err := time.LoadLocation(p.TZ); err == nil {
			loc = l
		}
	}
	lt := t.In(loc)
	cur := lt.Hour()*60 + lt.Minute()
	if start < end {
		return cur >= start && cur < end
	}
	return cur >= start || cur < end
}

// SeverityRank orders severities info < normal < urgent (unknown = normal).
func SeverityRank(s string) int {
	switch s {
	case "info":
		return 0
	case "urgent":
		return 2
	}
	return 1
}

// Evaluate applies presentation preferences to an item. It returns "" when
// a push may be sent, otherwise the NotificationStatus state
// (StateDisabled or StateQuietHours) and a detail. Preferences never affect
// the durable item.
func Evaluate(p contracts.NotificationPrefs, it contracts.AttentionItem, now time.Time) (state, detail string) {
	if enabled, ok := p.Kinds[it.Kind]; ok && !enabled {
		return StateDisabled, "push disabled for " + string(it.Kind)
	}
	if enabled := p.Kinds[it.Kind]; it.Kind == contracts.AttnGrantFYI && !enabled {
		return StateDisabled, "informational (no push unless enabled for " + string(it.Kind) + ")"
	}
	if it.ProjectID != nil && slices.Contains(p.MutedProjects, *it.ProjectID) {
		return StateDisabled, "project muted"
	}
	if p.MinSeverity != "" && SeverityRank(it.Severity) < SeverityRank(p.MinSeverity) {
		return StateDisabled, "below minimum severity " + p.MinSeverity
	}
	if InQuietHours(p, now) {
		return StateQuietHours, "quiet hours " + p.QuietHoursStart + "-" + p.QuietHoursEnd
	}
	return "", ""
}

// GenericTitle is the only human text placed in a push payload. It never
// contains message, approval, comment, or transcript content; the app
// fetches details over an authenticated connection.
func GenericTitle(k contracts.AttentionKind) string {
	switch k {
	case contracts.AttnApproval:
		return "Pyre: approval needed"
	case contracts.AttnQuestion:
		return "Pyre: question waiting"
	case contracts.AttnFailure:
		return "Pyre: run failed"
	case contracts.AttnAccountPool:
		return "Pyre: accounts exhausted"
	case contracts.AttnReviewReady:
		return "Pyre: ready for review"
	case contracts.AttnMessage:
		return "Pyre: new message"
	case contracts.AttnIndeterminate:
		return "Pyre: delivery needs review"
	case contracts.AttnConflict:
		return "Pyre: document conflict"
	case contracts.AttnComment:
		return "Pyre: new comment"
	case contracts.AttnUpdate:
		return "Pyre: update problem"
	case contracts.AttnNode:
		return "Pyre: machine needs attention"
	case contracts.AttnAsk:
		return "Pyre: a decision is waiting"
	case contracts.AttnWaiting:
		return "Pyre: a lead is waiting on you"
	case contracts.AttnGrantFYI:
		return "Pyre: a standing go-ahead was used"
	}
	return "Pyre: needs attention"
}

// RepeatWindow is the minimum time between pushes for one group of kind k
// (0: no limit). A session raises a "waiting" item (group per session)
// at each turn that ends with a new question, so it pushes at most once
// per session per hour; the item itself is always raised.
func RepeatWindow(k contracts.AttentionKind) time.Duration {
	if k == contracts.AttnWaiting {
		return time.Hour
	}
	return 0
}
