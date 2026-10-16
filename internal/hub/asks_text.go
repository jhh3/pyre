package hub

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/notifications"
)

// ---- deadlines ----

// askDefaultQuiet is used when the owner has not set quiet hours.
var askDefaultQuiet = contracts.NotificationPrefs{QuietHoursStart: "22:00", QuietHoursEnd: "08:00", TZ: "America/Los_Angeles"}

// askQuietPrefs returns the owner's quiet hours (askDefaultQuiet when unset
// or when no attention store is configured).
func (h *Hub) askQuietPrefs(ctx context.Context) contracts.NotificationPrefs {
	src, ok := h.cfg.Attention.(interface {
		Prefs(ctx context.Context) (contracts.NotificationPrefs, error)
	})
	if !ok {
		return askDefaultQuiet
	}
	p, err := src.Prefs(ctx)
	if err != nil || p.QuietHoursStart == "" || p.QuietHoursEnd == "" {
		return askDefaultQuiet
	}
	return p
}

// askDeadline computes an ask's deadline and rule: deadlineSeconds or the
// default hour; a deadline inside quiet hours moves to the end of quiet
// hours ("morning") unless the ask is urgent.
func askDeadline(now time.Time, deadlineSeconds int, urgent bool, quiet contracts.NotificationPrefs) (time.Time, string) {
	d, rule := contracts.AskDefaultDeadline, "1h"
	if deadlineSeconds > 0 {
		d, rule = time.Duration(deadlineSeconds)*time.Second, "custom"
	}
	at := now.Add(d)
	if urgent {
		return at, "urgent"
	}
	if notifications.InQuietHours(quiet, at) {
		if end, ok := quietEnd(quiet, at); ok {
			return end, "morning"
		}
	}
	return at, rule
}

// quietEnd returns the first end-of-quiet-hours instant after t.
func quietEnd(p contracts.NotificationPrefs, t time.Time) (time.Time, bool) {
	hh, mm, ok := parseHHMM(p.QuietHoursEnd)
	if !ok {
		return t, false
	}
	loc := time.Local
	if p.TZ != "" {
		if l, err := time.LoadLocation(p.TZ); err == nil {
			loc = l
		}
	}
	lt := t.In(loc)
	for i := 0; i < 2; i++ {
		day := lt.AddDate(0, 0, i)
		end := time.Date(day.Year(), day.Month(), day.Day(), hh, mm, 0, 0, loc)
		if end.After(lt) {
			return end.UTC(), true
		}
	}
	return t, false
}

func parseHHMM(s string) (int, int, bool) {
	h, m, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok {
		return 0, 0, false
	}
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, 0, false
	}
	return hh, mm, true
}

// pacific is the zone asks are shown in (the owner's); a fixed UTC-8 zone
// stands in when the system has no tz database.
var pacific = func() *time.Location {
	if l, err := time.LoadLocation("America/Los_Angeles"); err == nil {
		return l
	}
	return time.FixedZone("PT", -8*3600)
}()

// askClock renders a time in Pacific time ("Tue Oct 13 9:00 AM PT").
func askClock(t time.Time) string {
	return t.In(pacific).Format("Mon Jan 2 3:04 PM") + " PT"
}

// ---- rendering ----

func askOptionsText(opts []contracts.AskOption) string {
	parts := make([]string, 0, len(opts))
	for _, o := range opts {
		parts = append(parts, strings.TrimSpace(o.Label+" "+o.Detail))
	}
	return strings.Join(parts, "\n")
}

func askOptionLabel(a contracts.Ask, id string) string {
	if o, ok := a.Option(id); ok {
		return o.ID + " — " + o.Label
	}
	return id
}

// renderAskBody is the readable ask: title, context, numbered options,
// default and deadline.
func renderAskBody(a contracts.Ask) string {
	var b strings.Builder
	b.WriteString(a.Title)
	b.WriteString("\n\n")
	b.WriteString(a.Context)
	b.WriteString("\n\nOptions:\n")
	for i, o := range a.Options {
		fmt.Fprintf(&b, "  %d. [%s] %s", i+1, o.ID, o.Label)
		if o.Detail != "" {
			b.WriteString(" — " + o.Detail)
		}
		b.WriteString("\n")
	}
	switch {
	case a.DefaultOption != "" && a.DeadlineAt != nil:
		fmt.Fprintf(&b, "Default: %s, applied %s if there is no answer.\n", askOptionLabel(a, a.DefaultOption), askClock(*a.DeadlineAt))
	case a.DefaultOption != "":
		fmt.Fprintf(&b, "Default: %s.\n", askOptionLabel(a, a.DefaultOption))
	case a.DeadlineAt != nil:
		fmt.Fprintf(&b, "No default: it expires %s if there is no answer (the asker then uses its judgement).\n", askClock(*a.DeadlineAt))
	}
	if a.Urgent {
		b.WriteString("Urgent.\n")
	}
	if a.AskID != "" {
		fmt.Fprintf(&b, "Topic: %s · ask %s", a.TopicKey, a.AskID)
	} else {
		fmt.Fprintf(&b, "Topic: %s", a.TopicKey)
	}
	return b.String()
}

// askOutcome is a one-line outcome of a decided ask.
func askOutcome(a contracts.Ask) string {
	ans := a.Answer
	if ans == nil {
		ans = &contracts.AskAnswer{}
	}
	text := ""
	if ans.Text != "" {
		text = " (" + oneLine(ans.Text, 300) + ")"
	}
	switch a.State {
	case contracts.AskAnswered:
		if ans.OptionID == "" {
			return "owner answered: " + oneLine(ans.Text, 1000)
		}
		return "owner chose " + askOptionLabel(a, ans.OptionID) + text
	case contracts.AskDefaulted:
		return fmt.Sprintf("no answer by %s; default %s applied", askClock(ans.At), askOptionLabel(a, ans.OptionID))
	case contracts.AskExpired:
		return fmt.Sprintf("no answer by %s and no default — use your judgement, prefer the reversible path", askClock(ans.At))
	case contracts.AskWithdrawn:
		return "withdrawn" + text
	case contracts.AskSuperseded:
		return "superseded by " + a.SupersededBy + text
	}
	return string(a.State)
}

func askTag(a contracts.Ask) string {
	switch a.State {
	case contracts.AskExpired:
		return "[pyre ask expired]"
	case contracts.AskWithdrawn, contracts.AskSuperseded:
		return "[pyre ask withdrawn]"
	}
	return "[pyre ask decided]"
}

// renderDecisions is the one message an asking session gets for its
// decided asks. It uses only stored fields, so a resend is identical.
func renderDecisions(asks []contracts.Ask) string {
	if len(asks) == 1 {
		a := asks[0]
		return fmt.Sprintf("%s %s: %s\n(ask %s, topic %s)", askTag(a), a.Title, askOutcome(a), a.AskID, a.TopicKey)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[pyre ask decided] %d of your asks were decided:\n", len(asks))
	for _, a := range asks {
		fmt.Fprintf(&b, "- %s: %s (ask %s, topic %s)\n", a.Title, askOutcome(a), a.AskID, a.TopicKey)
	}
	return strings.TrimRight(b.String(), "\n")
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// ---- similarity ----

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// askNormText is the text compared for near-duplicates: title and option
// labels, lowercased, punctuation folded to single spaces.
func askNormText(title string, opts []contracts.AskOption) string {
	parts := []string{title}
	for _, o := range opts {
		parts = append(parts, o.Label)
	}
	return normWords(strings.Join(parts, " "))
}

func normWords(s string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !isWordRune(r) }), " ")
}

var askStopwords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "that": true, "this": true, "from": true, "are": true, "was": true,
	"should": true, "would": true, "could": true, "will": true, "can": true, "not": true, "now": true, "you": true, "your": true,
	"our": true, "what": true, "which": true, "when": true, "into": true, "have": true, "has": true, "but": true, "all": true,
}

// askMatchAny builds an FTS5 query matching any significant word of s
// (each quoted, so no input is FTS syntax).
func askMatchAny(s string) string {
	seen := map[string]bool{}
	var terms []string
	for _, w := range strings.Fields(normWords(s)) {
		if len([]rune(w)) < 3 || askStopwords[w] || seen[w] {
			continue
		}
		seen[w] = true
		terms = append(terms, `"`+w+`"`)
		if len(terms) == 32 {
			break
		}
	}
	return strings.Join(terms, " OR ")
}

func trigrams(s string) map[string]bool {
	r := []rune(" " + s + " ")
	out := make(map[string]bool, len(r))
	for i := 0; i+3 <= len(r); i++ {
		out[string(r[i:i+3])] = true
	}
	return out
}

func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for k := range a {
		if b[k] {
			inter++
		}
	}
	return float64(inter) / float64(len(a)+len(b)-inter)
}
