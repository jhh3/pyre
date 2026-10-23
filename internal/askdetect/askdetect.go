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
// or a line says the work is waiting on the owner ("I'm waiting on you",
// "your call", "needs your approval", "I asked earlier whether …"). A
// phrase negated just before it ("Nothing needs you") does not count, and
// neither does one the message quotes or uses to describe a feature
// rather than to wait (owner report 2026-10-19: a release report saying
// 'A session repeating "still waiting on you" no longer brings its flag
// back' raised a waiting item): see ownerWait.
package askdetect

import (
	"regexp"
	"strings"
	"unicode"
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
		if len(ownerSentences(unquote(line))) > 0 {
			return clean(line), true
		}
	}
	return "", false
}

// ownerSentences returns every sentence of line (quotes already removed,
// unquote) that holds an owner-directed ask: the questions, then the
// sentences around waiting phrases (from the previous sentence end, else
// the start of the line, so the subject is kept: "PR 41 merge is waiting
// on your go-ahead").
func ownerSentences(line string) []string {
	var out []string
	for _, loc := range questionRe.FindAllStringIndex(line, -1) {
		if s := line[loc[0]:loc[1]]; cueRe.MatchString(s) && !negated(line, loc[0]+cueRe.FindStringIndex(s)[0]) {
			out = append(out, s)
		}
	}
	for _, loc := range waitRe.FindAllStringIndex(line, -1) {
		if !ownerWait(line, loc) {
			continue
		}
		start, end := 0, len(line)
		if i := strings.LastIndexAny(line[:loc[0]], ".!?…;"); i >= 0 {
			_, size := utf8.DecodeRuneInString(line[i:])
			start = i + size
		}
		if i := strings.IndexAny(line[loc[1]:], ".!?…;"); i >= 0 {
			end = loc[1] + i
		}
		out = append(out, line[start:end])
	}
	return out
}

// QuestionKey is a normalized form of every sentence in the tail of msg
// that makes it an owner question ("" when there is none): lowercase words
// of letters and digits, single spaces, sentences joined with " | ". Two
// turns that end with the same request ("I'm still waiting on your yes to
// publish the post") give the same key though the rest of the message
// differs; a turn that adds any new question gives another, which lets
// callers tell a repeat from a new question.
func QuestionKey(msg string) string {
	text := tail(prose(msg), TailChars)
	var keys []string
	for _, line := range strings.Split(text, "\n") {
		for _, s := range ownerSentences(unquote(line)) {
			if k := strings.Join(strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !isWordRune(r) }), " "); k != "" {
				keys = append(keys, k)
			}
		}
	}
	return strings.Join(keys, " | ")
}

// anyQuestionRe is a sentence-ending "?" anywhere in prose.
var anyQuestionRe = regexp.MustCompile(`\?+(?:[*_"'”’)\]]*)(?:\s|$)`)

// MailAsks reports whether msg, mail to the owner, asks him anything:
// stricter than OwnerQuestion in the owner's favour, since a miss here
// files the mail away unseen. The whole prose is read (not just the
// tail); any sentence ending in "?" counts, with or without a cue word and
// whatever softening precedes it ("No rush, should I merge?"); and so does
// a waiting phrase that is not negated ("waiting on your go-ahead", but not
// "nothing needs you").
func MailAsks(msg string) bool {
	text := prose(msg)
	if anyQuestionRe.MatchString(text) {
		return true
	}
	for _, line := range strings.Split(text, "\n") {
		for _, loc := range waitRe.FindAllStringIndex(line, -1) {
			if !negated(line, loc[0]) {
				return true
			}
		}
	}
	return false
}

// The session itself waiting, as opposed to a waiting phrase used to name
// or describe something (a feature, another session's state, a
// condition). Release notes and status reports in this project describe
// the "waiting on you" and "needs you" features in those words.

// adverbs may sit between a subject and the phrase ("I'm still waiting").
const adverbs = `still|now|currently|also|just|already`

var (
	// stateRe: the phrases that state who waits ("waiting on you", "needs
	// your approval"), which descriptions use too; the others ("your
	// call", "up to you", "yes or no") are asks however they are phrased.
	stateRe = regexp.MustCompile(`(?i)^(?:waiting|awaiting|pending|blocked|needs?)\b`)
	// firstPersonRe: the writer is the one waiting ("I'm still waiting on
	// your yes", "we need your sign-off").
	firstPersonRe = regexp.MustCompile(`(?i)^(?:i|i'm|i’m|im|i've|i’ve|i'd|i’d|we|we're|we’re|we've|we’ve)$`)
	// determinerRe before a phrase makes it a noun modifier ("the waiting
	// on you flag", "its needs you state").
	determinerRe = regexp.MustCompile(`(?i)^(?:the|a|an|its|their|every|each|any|his|her|whose)$`)
	// featureNounRe after a bare phrase ("waiting on you items", "needs
	// you badge") names a thing, not a wait.
	featureNounRe = regexp.MustCompile(`(?i)^(?:items?|flags?|states?|badges?|threads?|kinds?|detectors?|detection|notifications?|alerts?|labels?|status|rows?|chips?|filters?|tabs?|lists?|inbox|views?|phrases?|texts?|titles?|cards?|columns?|signals?|checks?|reasons?|counts?|tests?|cases?|logic|feature|heuristics?|sentences?|entries|entry|groups?|keys?)$`)
	// genericNounRe: sessions as a kind ("Sessions waiting on you get a
	// badge", "when a lead needs your answer") are a description; the
	// writer's own work ("Open items awaiting your decision:", "One item
	// needs your decision") is not.
	genericNounRe = regexp.MustCompile(`(?i)^(?:sessions|leads|agents|children|coordinators)$`)
	genericPronRe = regexp.MustCompile(`(?i)^(?:anything|whatever|whoever|whichever|anyone)$`)
	singularRe    = regexp.MustCompile(`(?i)^(?:session|lead|agent|child|coordinator)$`)
	indefiniteRe  = regexp.MustCompile(`(?i)^(?:a|an|any|every|each)$`)
	adverbRe      = regexp.MustCompile(`(?i)^(?:` + adverbs + `)$`)
	// skipRe: words between a subject and the phrase ("The prototype is
	// still waiting on you", "a session that is now waiting on you",
	// "I have been waiting on you").
	skipRe = regexp.MustCompile(`(?i)^(?:` + adverbs + `|all|and|that|which|who|is|are|was|were|be|been|being|am|have|has|had|'s|’s|'re|’re|'m|’m|'ve|’ve)$`)
	// conditionRe opens a condition or a description of when something
	// happens ("when it needs your answer", "if a session is waiting on
	// you", "ping you when I need your input").
	conditionRe = regexp.MustCompile(`(?i)^(?:when|whenever|if|while)$`)
)

// ownerWait reports whether the waiting phrase at loc in line says the
// writer waits on the owner. A negated phrase never does ("Nothing needs
// you"). A phrase that states who waits ("waiting on you", "needs your
// approval", stateRe) does not when it sits in a condition ("when I need
// your input", "if anything needs your approval"), modifies a noun ("the
// waiting on you flag", "waiting on you items") or has a generic or gerund
// subject ("Sessions waiting on you get a badge", "a session repeating
// still waiting on you"). It counts when the writer is its subject ("I'm
// still waiting on you", "we need your sign-off"), when it opens the
// sentence ("Waiting on your go-ahead.", "Still waiting on you:") and
// when its subject is anything else ("The prototype is still waiting on
// you", "Final wording needs your approval").
func ownerWait(line string, loc []int) bool {
	if negated(line, loc[0]) {
		return false
	}
	phrase := line[loc[0]:loc[1]]
	if !stateRe.MatchString(phrase) {
		return true
	}
	before := line[:loc[0]]
	if j := strings.LastIndexAny(before, ".!?;:…"); j >= 0 {
		_, size := utf8.DecodeRuneInString(before[j:])
		before = before[j+size:]
	}
	words := strings.FieldsFunc(before, notWordRune)
	clause := words // since the last comma
	if j := strings.LastIndex(before, ","); j >= 0 {
		clause = strings.FieldsFunc(before[j+1:], notWordRune)
	}
	for _, w := range clause {
		if conditionRe.MatchString(w) {
			return false
		}
	}
	n := len(words)
	i := n - 1
	for i >= 0 && skipRe.MatchString(words[i]) {
		i--
	}
	if i >= 0 && firstPersonRe.MatchString(words[i]) {
		return true // "I'm still waiting on you", "we need your …"
	}
	if strings.HasSuffix(strings.ToLower(phrase), "you") {
		if after := strings.FieldsFunc(line[loc[1]:], notWordRune); len(after) > 0 && featureNounRe.MatchString(after[0]) &&
			!strings.ContainsAny(firstNonSpace(line[loc[1]:]), ":;,.!?") {
			return false
		}
	}
	if n == 0 {
		return true // the phrase opens the sentence
	}
	if determinerRe.MatchString(words[n-1]) {
		return false
	}
	if strings.HasPrefix(strings.ToLower(phrase), "waiting") {
		g := n - 1
		for g > 0 && adverbRe.MatchString(words[g]) {
			g--
		}
		if h := strings.ToLower(words[g]); len(h) > 4 && strings.HasSuffix(h, "ing") && !strings.HasSuffix(h, "thing") && h != "being" {
			return false // a gerund before it: "repeating (still) waiting on you"
		}
	}
	if i < 0 {
		return true // "Still waiting on you:"
	}
	head := words[i]
	switch {
	case genericPronRe.MatchString(head), genericNounRe.MatchString(head):
		return false
	case singularRe.MatchString(head) && i > 0 && indefiniteRe.MatchString(words[i-1]):
		return false
	}
	return true
}

func notWordRune(r rune) bool {
	return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '\'' && r != '’' && r != '-'
}

// firstNonSpace is the first non-space character of s ("" when none).
func firstNonSpace(s string) string {
	s = strings.TrimLeft(s, " \t*_")
	if s == "" {
		return ""
	}
	_, size := utf8.DecodeRuneInString(s)
	return s[:size]
}

// unquote replaces every quoted span of line ("…", “…”, ‘…’, and '…'
// outside a word) with the word "quote": a phrase the message quotes is
// not the writer asking ('the "still waiting on you" repeat', 'the
// prompt "Should I overwrite?"'). An unclosed quote is left as it is.
func unquote(line string) string {
	closers := map[rune]rune{'"': '"', '“': '”', '‘': '’', '\'': '\''}
	var b strings.Builder
	rs := []rune(line)
	for i := 0; i < len(rs); i++ {
		closer, ok := closers[rs[i]]
		if ok && i > 0 && isWordRune(rs[i-1]) {
			ok = false // an apostrophe ("don't") or inches (`a 12" screen`)
		}
		if ok {
			end := -1
			for j := i + 1; j < len(rs); j++ {
				if rs[j] == closer && (j+1 == len(rs) || !isWordRune(rs[j+1])) {
					end = j
					break
				}
			}
			if end > i+1 {
				b.WriteString("quote")
				i = end
				continue
			}
		}
		b.WriteRune(rs[i])
	}
	return b.String()
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

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
