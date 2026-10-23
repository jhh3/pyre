// Package ownermail decides which messages to the owner need him (owner
// report 2026-10-19: "why are there a bunch of useless messages in my
// inbox" — 144 unacked messages, most of them status reports, digests and
// "ignore my previous message" corrections that asked nothing of him).
//
// A message to the owner is an update when it asks nothing of him: the hub
// stores it already acknowledged (contracts.AckRefUpdate), so it raises no
// inbox item and never piles up as unacked mail, while his mailbox, its
// session's and project's threads and search keep it. It needs him (and
// stays in the inbox until he reads, answers or acknowledges it) when:
//   - its sender asked for an acknowledgement (request_ack);
//   - it replies to a message from him (the answer he is waiting for);
//   - it asks him anything or needs him (NeedsOwner): a question anywhere
//     in it, a waiting phrase ("waiting on your go-ahead"), or a request,
//     urgency or incident word ("please", "need your", "prod is down",
//     "security", "unless you"); a "no reply needed" label does not
//     outrank these, and softening words never cancel a question;
//   - it is mail Pyre itself sends in queue mode (approvals, failure
//     notices), unless it says otherwise with mode info.
//
// Mode info ("store and show only") mail from anyone is an update under
// the same tests. The owner's notes to himself and ask carriers (thread = an ask
// ID, closed with their ask) are left alone. The rule is pure so the hub
// and the mock backend apply the same one.
package ownermail

import (
	"regexp"

	"github.com/jhh3/pyre/internal/askdetect"
	"github.com/jhh3/pyre/internal/contracts"
)

// Update reports whether env, a message about to be stored, is an update
// for the owner (see the package comment). repliesToOwner reports whether
// it answers the owner (it replies to his message, or he wrote to the
// session since it last wrote to him); it is called only when needed.
func Update(env contracts.MessageEnvelope, repliesToOwner func() bool) bool {
	if env.Recipient.Kind != contracts.AddrOwner || contracts.ValidID(env.ThreadID, contracts.PrefixAsk) {
		return false
	}
	sender := env.Sender
	if sender.OnBehalfOf != nil {
		sender = *sender.OnBehalfOf
	}
	if sender.Kind == contracts.PrincipalOwner || env.RequestAck {
		return false // a note to himself; asked to be read
	}
	if env.Mode != contracts.ModeInfo && sender.Kind != contracts.PrincipalSession {
		return false // Pyre's own mail in queue mode is for him
	}
	if NeedsOwner(env.Body.Text) {
		return false
	}
	return repliesToOwner == nil || !repliesToOwner()
}

// needsOwnerRe finds requests, urgency and incidents: mail that needs the
// owner though it asks no question ("URGENT: prod is down", "Please
// approve the deploy", "I need the Stripe key", "… unless you object").
// It errs towards the inbox: a false hit costs one inbox item, a miss a
// silently filed incident.
var needsOwnerRe = regexp.MustCompile(`(?i)\b(urgent|asap|immediately|emergency|critical|sev[- ]?[0-3]|p[01]|outage|incident|data loss|security|leak(?:ed)?|breach|compromised|roll ?back|revert|blocked|action (?:required|needed)|please|pls|let me know|lmk|can you|could you|would you|need(?:s|ed)? (?:a|an|the|your|you)|unless you|review|approve|sign[- ]?off|(?:prod|production|site|hub|api|service|deploy)\b[^.\n]{0,24}\b(?:down|broken|failing|failed|on fire))\b`)

// notUrgentRe: "nothing urgent", "nothing needs you", "no action needed"
// say the opposite of a request; they are removed before needsOwnerRe
// looks (they never cancel a question: MailAsks reads the text as
// written).
var notUrgentRe = regexp.MustCompile(`(?i)\b(?:(?:nothing|not|non)[- ]urgent|nothing (?:else )?(?:needs|needed from) you|no action (?:needed|required))\b`)

// NeedsOwner reports whether text asks the owner anything or needs him:
// any question or owner-directed waiting phrase anywhere in it
// (askdetect.MailAsks, softening words do not cancel a question), or a
// request, urgency or incident word (needsOwnerRe). A "no reply needed"
// label does not outrank either.
func NeedsOwner(text string) bool {
	return askdetect.MailAsks(text) || needsOwnerRe.MatchString(notUrgentRe.ReplaceAllString(text, ""))
}

// Question reports whether a session's message to the owner asks him
// something; such mail belongs in an ask (the sender is told so in its
// delivery status).
func Question(env contracts.MessageEnvelope) bool {
	if env.Recipient.Kind != contracts.AddrOwner || env.Sender.Kind != contracts.PrincipalSession || contracts.ValidID(env.ThreadID, contracts.PrefixAsk) {
		return false
	}
	return askdetect.MailAsks(env.Body.Text)
}
