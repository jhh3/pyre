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
// or a request ("Approve the PR when you get a chance.", "Please run aws
// login.", "Let me know which layout you prefer."), or a line says the
// work is waiting on the owner ("I'm waiting on you", "your call", "needs
// your approval", "I asked earlier whether …"). A later step ("then it
// reports the new version for your approval", "I'll ask for your
// approval") or a report label ("Your approval: it adds two screens") is
// not an ask. A
// phrase negated just before it ("Nothing needs you") does not count, and
// neither does one the message quotes or uses to describe a feature
// rather than to wait (owner report 2026-10-19: a release report saying
// 'A session repeating "still waiting on you" no longer brings its flag
// back' raised a waiting item): see ownerWait. A question the message
// reports rather than asks ("the research session asked you a yes/no
// question: should the image move?") is not one either, and a message
// that says "Nothing else needs you" asks only with a direct question
// ("Should I merge it?") or a request (owner report 2026-10-30): see
// reported and settledRe.
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
	// waitRe finds owner-directed waiting/decision phrases. "your input" /
	// "your answer" / "your ok" alone are descriptive too ("tells your input
	// apart", iss_…), and so are "your approval" /
	// "your decision" ("then it reports the new version for your approval",
	// a report bullet "Your approval: it adds two screens",
	// iss_…); they count only after a request or
	// wait word (here, or "needs your" / "waiting on your") or before
	// "needed". Only "your call" asks on its own.
	waitRe = regexp.MustCompile(`(?i)\b(waiting (?:on|for) (?:you|your)\b|awaiting your|pending your|blocked on you|needs? (?:you|your)\b|your call\b|(?:want|wants|wanted|asking for|ask for|request|requesting|like|get|have) your (?:input|answer|answers|ok|okay|decisions?|approval|go-ahead|go ahead|sign-off|choice)\b|your (?:decisions?|approval|go-ahead|go ahead|sign-off|choice|input|answer) (?:is |are )?(?:needed|required)\b|decisions? (?:for|from) you\b|up to you\b|yes or no\b|asked (?:you )?(?:earlier |before |above )?(?:whether|if)\b)`)
	// requestRe is a sentence that asks the owner to act without a "?"
	// ("Approve the PR when you get a chance.", "Please run aws login.",
	// "Let me know which layout you prefer.").
	requestRe = regexp.MustCompile(`(?i)^(?:please\s+)?(approve|confirm|decide|pick|choose|sign off|let me know|tell me|reply|can you|could you|would you|will you)\b|\bplease\s+(?:approve|confirm|decide|pick|choose|review|merge|run|let me know|tell me|reply|answer|log ?in|sign|check|look|send|share|grant|accept|click|open|add|set|give|try|rotate|restart|install|enable)\b|^please\b`)
	// offerRe after "let me know" makes it an offer, not a request ("Let
	// me know if anything needs your approval").
	offerRe = regexp.MustCompile(`(?i)^\s*(?:if|when|whenever|should)\b`)
	// labelRe is a leading report label ("**Next:**", "Your approval:").
	labelRe = regexp.MustCompile(`^\s*(?:[-*+•]|\d+[.)])?\s*(?:\*\*|__)?[\pL\pN' ’-]{1,40}?(?:\*\*|__)?:(?:\*\*|__)?\s+`)
	// futureRe: the writer describes a later step ("I'll ask for your
	// approval once it's green"), not a wait now.
	futureRe = regexp.MustCompile(`(?i)^(?:will|shall|going|gonna|later|\w+'ll|\w+’ll)$`)
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
	settled := settledRe.MatchString(text)
	for _, line := range strings.Split(text, "\n") {
		if len(ownerSentences(unquote(line), settled, false)) > 0 {
			return clean(line), true
		}
	}
	return "", false
}

// DecisionAsk is OwnerQuestion for the Claude Stop hook, which blocks the
// turn and tells the session to file an ask_owner decision: only a question
// to the owner, a request, or a wait on a choice from him counts. A status
// line that says something waits on him without asking for a choice
// ("Stripe login still waiting on you", "the deploy needs you") is a
// blocker he has usually been flagged about already, not a decision
// (iss_…), so it does not count here; it still
// does for OwnerQuestion, which raises a "waiting" item.
func DecisionAsk(msg string) (string, bool) {
	text := tail(prose(msg), TailChars)
	settled := settledRe.MatchString(text)
	for _, line := range strings.Split(text, "\n") {
		if len(ownerSentences(unquote(line), settled, true)) > 0 {
			return clean(line), true
		}
	}
	return "", false
}

// choiceRe: what follows a waiting phrase when the wait is on a choice
// ("waiting on your go-ahead", "needs your 3 decisions", "waiting on you to
// pick a layout").
var choiceRe = regexp.MustCompile(`(?i)^(?:approvals?|approve|decisions?|decide|sign-off|signoff|go-ahead|go|ok|okay|yes|no|input|answers?|call|choice|choose|pick|confirmation|confirm|review|word|opinion|feedback)$`)

// choiceFollows reports whether one of the first four words of s names a
// choice the owner makes (choiceRe).
func choiceFollows(s string) bool {
	words := strings.FieldsFunc(s, notWordRune)
	if len(words) > 4 {
		words = words[:4]
	}
	for _, w := range words {
		if choiceRe.MatchString(w) {
			return true
		}
	}
	return false
}

// ownerSentences returns every sentence of line (quotes already removed,
// unquote) that holds an owner-directed ask: the questions, then the
// sentences around waiting phrases (from the previous sentence end, else
// the start of the line, so the subject is kept: "PR 41 merge is waiting
// on your go-ahead"). A question the line reports rather than asks ("the
// research session asked you a yes/no question: should the image move?")
// is not one; when the message says nothing else needs the owner
// (settled), only a question put to him directly ("Should I merge it?",
// "Want me to retry?") is.
//
// strict (DecisionAsk) drops a waiting phrase that states who waits
// (stateRe: "waiting on you", "needs you") unless a choice follows it
// (choiceFollows).
func ownerSentences(line string, settled, strict bool) []string {
	var out []string
	for _, loc := range questionRe.FindAllStringIndex(line, -1) {
		s := line[loc[0]:loc[1]]
		if !cueRe.MatchString(s) || negated(line, loc[0]+cueRe.FindStringIndex(s)[0]) || reported(s, line[loc[1]:]) {
			continue
		}
		if settled && !directRe.MatchString(questionBody(s)) {
			continue
		}
		out = append(out, s)
	}
	out = append(out, requests(line)...)
	for _, loc := range waitRe.FindAllStringIndex(line, -1) {
		if !ownerWait(line, loc) {
			continue
		}
		if strict && stateRe.MatchString(line[loc[0]:loc[1]]) && !choiceFollows(line[loc[1]:]) {
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

// requests returns the sentences of line that ask the owner to act in the
// imperative (requestRe): at the start of a sentence, after any list
// marker, emphasis or report label, or after "please". A "?" sentence is
// left to the question rule.
func requests(line string) []string {
	var out []string
	for _, s := range sentenceRe.FindAllString(line, -1) {
		if strings.HasSuffix(strings.TrimRight(s, " \t*_\"'”’)]"), "?") {
			continue
		}
		body := strings.TrimLeft(emphasisRe.ReplaceAllString(listRe.ReplaceAllString(s, ""), ""), " \t")
		if m := labelRe.FindStringIndex(body); m != nil {
			body = body[m[1]:]
		} else if m := labelRe.FindStringIndex(s); m != nil {
			body = strings.TrimLeft(emphasisRe.ReplaceAllString(s[m[1]:], ""), " \t")
		}
		loc := requestRe.FindStringSubmatchIndex(body)
		if loc == nil || negated(body, loc[0]) {
			continue
		}
		if loc[2] >= 0 && strings.EqualFold(body[loc[2]:loc[3]], "let me know") && offerRe.MatchString(body[loc[3]:]) {
			continue
		}
		out = append(out, strings.TrimSpace(s))
	}
	return out
}

var (
	// settledRe: the message says nothing (else) needs the owner.
	settledRe = regexp.MustCompile(`(?i)\bnothing (?:else |more )?(?:needs|requires) (?:you|your)\b|\bnothing (?:else |more )?(?:is )?(?:waiting|pending) on you\b|\bnothing (?:else |more )?for you to (?:do|decide)\b`)
	// directRe: a question put to the owner directly, at its start.
	directRe = regexp.MustCompile(`(?i)^(?:(?:so|and|ok|okay|also|then),?\s+)?(?:(?:should|shall|can|could|may|do|would|will)\s+(?:i|we)\b|(?:do|would|will|can|could|did|are|have)\s+you\b|(?:want|wanna)\s+me\b|ok(?:ay)?\s+(?:to|if)\b|which\b|go ahead\b|proceed\b|yes or no\b)`)
	// reportVerbRe: a verb that introduces another party's question
	// ("asked you a yes/no question: …", "is asking whether …").
	reportVerbRe = regexp.MustCompile(`(?i)\b(?:asked|asks|asking|wondered|wonders|wondering|wants to know|wanted to know)\b[^:?]*?(?::|\b(?:whether|if)\b)`)
	// otherQuestionRe: a question that belongs to someone else ("its
	// question: …", "the lead's open question — …").
	otherQuestionRe = regexp.MustCompile(`(?i)\b(?:its|their|his|her|[\pL\pN-]+'s|[\pL\pN-]+’s)\s+(?:[\pL-]+\s+)?questions?\b`)
	// attributionRe after a question names who asked it ("— asked by the
	// research session", "(from the billing lead)").
	attributionRe = regexp.MustCompile(`(?i)^[\s*_"'”’)\]]*(?:[(—–-]+\s*(?:asked|from|said|wrote|per)\b|asked by\b)`)
)

// reported reports whether question sentence s, followed by rest of the
// line, relates another party's question rather than asking one: a
// reporting verb whose subject is not the writer opens it ("the research
// session asked you a yes/no question: should the image move?" but not
// "I asked you earlier: should I merge?"), a possessive names whose
// question it is, or an attribution follows it. A sentence that opens as
// a direct question ("Should I keep asking if it's down?") is the writer's.
func reported(s, rest string) bool {
	if attributionRe.MatchString(rest) {
		return true
	}
	if directRe.MatchString(strings.TrimLeft(emphasisRe.ReplaceAllString(listRe.ReplaceAllString(s, ""), ""), " \t")) {
		return false // not questionBody: "The lead asked:" is no label
	}
	if otherQuestionRe.MatchString(s) {
		return true
	}
	for _, loc := range reportVerbRe.FindAllStringIndex(s, -1) {
		words := strings.FieldsFunc(s[:loc[0]], notWordRune)
		i := len(words) - 1
		for i >= 0 && skipRe.MatchString(words[i]) {
			i--
		}
		if i < 0 || !firstPersonRe.MatchString(words[i]) {
			return true
		}
	}
	return false
}

// questionBody is question sentence s without list marker, emphasis or a
// leading report label ("**Next:** should I merge?" gives "should I
// merge?").
func questionBody(s string) string {
	body := strings.TrimLeft(emphasisRe.ReplaceAllString(listRe.ReplaceAllString(s, ""), ""), " \t")
	if m := labelRe.FindStringIndex(body); m != nil {
		body = body[m[1]:]
	}
	return strings.TrimLeft(body, " \t")
}

// sentenceRe is a sentence with its end mark (a "." inside a number or
// URL does not end it).
var sentenceRe = regexp.MustCompile(`[^.!?…;]+(?:[.!?…;]+(?:\S+[.!?…;]+)*|$)`)

// QuestionKey is a normalized form of every sentence in the tail of msg
// that makes it an owner question ("" when there is none): lowercase words
// of letters and digits, single spaces, sentences joined with " | ". Two
// turns that end with the same request ("I'm still waiting on your yes to
// publish the post") give the same key though the rest of the message
// differs; a turn that adds any new question gives another, which lets
// callers tell a repeat from a new question.
func QuestionKey(msg string) string {
	text := tail(prose(msg), TailChars)
	settled := settledRe.MatchString(text)
	var keys []string
	for _, line := range strings.Split(text, "\n") {
		for _, s := range ownerSentences(unquote(line), settled, false) {
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
// whatever softening precedes it ("No rush, should I merge?"); and so do
// a request ("Please approve PR 41.") and a waiting phrase that is not
// negated ("waiting on your go-ahead", but not "nothing needs you").
func MailAsks(msg string) bool {
	text := prose(msg)
	if anyQuestionRe.MatchString(text) {
		return true
	}
	for _, line := range strings.Split(text, "\n") {
		if len(requests(line)) > 0 {
			return true
		}
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
	// requestWaitRe: a request verb before "your …" ("ask for your
	// approval"), which a later step or a condition describes too.
	requestWaitRe = regexp.MustCompile(`(?i)^(?:want|wants|wanted|asking|ask|request|requesting|like|get|have)\b`)
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
	// replyNounRe: what the owner gives in reply to a session ("needs
	// your approval"), unlike a requirement of something else that
	// "needs your password" / "your login" / "your SSO" describes.
	replyNounRe = regexp.MustCompile(`(?i)^(?:approvals?|decisions?|sign-off|signoff|go-ahead|go|ok|okay|yes|input|answers?|call|choice|review|word|confirmation|opinion|feedback|help|attention|eyes|pick)$`)
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
// you", "Final wording needs your approval"), except that something else
// needing "your" something the owner does not give in reply describes a
// requirement ("the drive needs your password").
func ownerWait(line string, loc []int) bool {
	if negated(line, loc[0]) {
		return false
	}
	phrase := line[loc[0]:loc[1]]
	if !stateRe.MatchString(phrase) && !requestWaitRe.MatchString(phrase) {
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
		if conditionRe.MatchString(w) || futureRe.MatchString(w) {
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
	if strings.HasPrefix(strings.ToLower(phrase), "need") && strings.HasSuffix(strings.ToLower(phrase), "your") && !repliesTo(line[loc[1]:]) {
		return false // "the drive needs your password": a requirement, not an ask
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

// repliesTo reports whether one of the first three words of s names
// something the owner gives in reply (replyNounRe): "your approval", "your
// final sign-off", "your 3 decisions".
func repliesTo(s string) bool {
	words := strings.FieldsFunc(s, notWordRune)
	if len(words) > 3 {
		words = words[:3]
	}
	for _, w := range words {
		if replyNounRe.MatchString(w) {
			return true
		}
	}
	return false
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
