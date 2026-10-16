// Package askdetect decides, deterministically and without a model call,
// whether an agent's final message of a turn leaves an open question or
// decision for the owner in its chat (owner request 2026-10-14: leads wrote
// their decisions only at the end of chat turns, which the owner does not
// read). The Claude Stop hook uses it to make a top-level session file an
// ask instead; the node/hub side uses it to raise a "waiting" attention item
// for any harness.
//
// The heuristic looks at the tail of the message with code blocks, inline
// code and blockquotes removed, and reports a hit when a sentence is an
// owner-directed question (ends with "?" and names the reader or a choice)
// or a line says the work is waiting on the owner ("waiting on you", "your
// call", "needs your approval", "I asked earlier whether …"). A phrase
// negated just before it ("Nothing needs you") does not count.
package askdetect

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// TailChars is how much of the end of the message is examined: owner
// questions sit at the end of a turn, and an earlier question in a long
// report has usually been answered within it.
const TailChars = 1200

// maxQuestion bounds the returned question line.
const maxQuestion = 200

var (
	// questionRe finds sentences ending in "?" (a "?" inside a URL, followed
	// by a non-space, is not a sentence end).
	questionRe = regexp.MustCompile(`[^.!?\n]*\?+(?:[*_"'”’)\]]*)(?:\s|$)`)
	// cueRe marks a question as addressed to the reader or asking for a
	// choice, unlike a rhetorical "Why did it fail?" in a status report.
	cueRe = regexp.MustCompile(`(?i)\b(you|your|should|shall|want me|may i|may we|can i|could i|ok to|okay to|approve|which|whether|prefer|go ahead|proceed|confirm|decide|yes or no)\b`)
	// waitRe finds owner-directed waiting/decision phrases.
	waitRe = regexp.MustCompile(`(?i)\b(waiting (?:on|for) (?:you|your)\b|awaiting your|pending your|blocked on you|needs? (?:you|your)\b|your (?:call|decision|decisions|approval|go-ahead|go ahead|sign-off|input|answer|answers|choice|ok|okay)\b|decisions? (?:for|from) you\b|up to you\b|yes or no\b|asked (?:you )?(?:earlier |before |above )?(?:whether|if)\b)`)
	// negRe: a negation among the few words before a phrase cancels it.
	negRe = regexp.MustCompile(`(?i)\b(nothing|no|nobody|none|not|never|neither|nor|without|don't|doesn't|didn't|won't|isn't|aren't|no longer|dont|doesnt)\b`)
	// fenceRe opens or closes a fenced code block.
	fenceRe      = regexp.MustCompile("^\\s*(```|~~~)")
	inlineCodeRe = regexp.MustCompile("`[^`\n]*`")
	quoteRe      = regexp.MustCompile(`^\s*>`)
	// listRe strips a leading list marker or heading from a returned line.
	listRe     = regexp.MustCompile(`^\s*(?:[-*+•]|\d+[.)]|#+)\s+`)
	emphasisRe = regexp.MustCompile(`\*\*|__`)
)

// OwnerQuestion reports whether msg (an agent's final message of a turn)
// leaves an open question or decision for the owner, and returns the first
// such line (markdown list markers and emphasis removed, at most 200
// characters) for use as a title.
func OwnerQuestion(msg string) (string, bool) {
	text := tail(prose(msg), TailChars)
	for _, line := range strings.Split(text, "\n") {
		if ownerLine(line) {
			return clean(line), true
		}
	}
	return "", false
}

// ownerLine reports whether one prose line holds an owner-directed ask.
func ownerLine(line string) bool {
	for _, loc := range questionRe.FindAllStringIndex(line, -1) {
		if s := line[loc[0]:loc[1]]; cueRe.MatchString(s) && !negated(line, loc[0]+cueRe.FindStringIndex(s)[0]) {
			return true
		}
	}
	for _, loc := range waitRe.FindAllStringIndex(line, -1) {
		if !negated(line, loc[0]) {
			return true
		}
	}
	return false
}

// negated reports a negation among the three words before position i of
// line, within the same sentence ("Nothing needs you", "I don't need your
// input", "There is nothing else waiting on you").
func negated(line string, i int) bool {
	before := line[:i]
	if j := strings.LastIndexAny(before, ".!?;:"); j >= 0 {
		before = before[j+1:]
	}
	words := strings.Fields(before)
	if len(words) > 3 {
		words = words[len(words)-3:]
	}
	return negRe.MatchString(strings.Join(words, " "))
}

// prose drops fenced code blocks, blockquotes (quoted user messages, cited
// text) and inline code, which hold questions that are not the agent's.
func prose(msg string) string {
	var b strings.Builder
	inFence := false
	for _, line := range strings.Split(msg, "\n") {
		if fenceRe.MatchString(line) {
			inFence = !inFence
			continue
		}
		if inFence || quoteRe.MatchString(line) {
			continue
		}
		b.WriteString(inlineCodeRe.ReplaceAllString(line, "code"))
		b.WriteByte('\n')
	}
	return b.String()
}

// tail returns the last n characters of s, starting at a line boundary when
// one is near so the first line is not a fragment.
func tail(s string, n int) string {
	s = strings.TrimRight(s, "\n ")
	if len(s) <= n {
		return s
	}
	cut := len(s) - n
	for cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut++
	}
	if j := strings.IndexByte(s[cut:], '\n'); j >= 0 && j < 200 {
		cut += j + 1
	}
	return s[cut:]
}

func clean(line string) string {
	s := strings.TrimSpace(emphasisRe.ReplaceAllString(listRe.ReplaceAllString(line, ""), ""))
	if len(s) > maxQuestion {
		cut := maxQuestion
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = strings.TrimSpace(s[:cut]) + "…"
	}
	return s
}
