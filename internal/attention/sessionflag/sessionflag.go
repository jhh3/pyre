// Package sessionflag computes a session's attention flag (why it wants the
// owner) from its attention items and current run. It is pure so the hub,
// the mock backend and clients agree on the same mapping.
//
// Blocking reasons ("needs you") outrank informational ones ("message"): a
// session with only an unread message for the owner is not blocked on them.
package sessionflag

import (
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
)

type kindInfo struct {
	level, reason string
	rank          int
	// pending kinds are outstanding requests: they keep flagging while
	// merely read (seen) and clear only when resolved.
	pending bool
}

var kinds = map[contracts.AttentionKind]kindInfo{
	contracts.AttnApproval:      {contracts.FlagNeedsYou, "approval", 100, true},
	contracts.AttnQuestion:      {contracts.FlagNeedsYou, "question", 95, true},
	contracts.AttnFlag:          {contracts.FlagNeedsYou, "asked", 90, false},
	contracts.AttnFailure:       {contracts.FlagNeedsYou, "failed", 80, false},
	contracts.AttnAccountPool:   {contracts.FlagNeedsYou, "reauth", 75, false},
	contracts.AttnIndeterminate: {contracts.FlagNeedsYou, "delivery", 70, true},
	contracts.AttnConflict:      {contracts.FlagNeedsYou, "conflict", 60, false},
	contracts.AttnMessage:       {contracts.FlagMessage, "message", 30, false},
	contracts.AttnComment:       {contracts.FlagMessage, "comment", 20, false},
	contracts.AttnReviewReady:   {contracts.FlagMessage, "review", 20, false},
}

func info(k contracts.AttentionKind) kindInfo {
	if ki, ok := kinds[k]; ok {
		return ki
	}
	return kindInfo{contracts.FlagMessage, string(k), 10, false}
}

// Level returns the flag level and reason an item of kind k contributes.
func Level(k contracts.AttentionKind) (level, reason string) {
	ki := info(k)
	return ki.level, ki.reason
}

// Flags reports whether an item in its current state flags its session:
// pending requests while open or read, everything else only while open.
// Handled, withdrawn and snoozed items never flag.
func Flags(it contracts.AttentionItem) bool {
	switch it.State {
	case contracts.AttnOpen:
		return true
	case contracts.AttnRead:
		return info(it.Kind).pending
	}
	return false
}

// Compute returns the flag for one session's items (callers pass that
// session's items in any state), or nil when nothing flags. Text is the
// top item's title as stored; callers resolve IDs in it.
func Compute(items []contracts.AttentionItem) *contracts.SessionFlag {
	var f *contracts.SessionFlag
	best := -1
	for _, it := range items {
		if !Flags(it) {
			continue
		}
		ki := info(it.Kind)
		if f == nil {
			f = &contracts.SessionFlag{}
		}
		f.Count++
		if ki.level == contracts.FlagMessage {
			f.Messages++
		}
		if ki.rank > best || (ki.rank == best && it.UpdatedAt.After(f.Since)) {
			best = ki.rank
			f.Level, f.Reason, f.Text, f.AttentionID, f.Kind, f.Since = ki.level, ki.reason, it.Title, it.AttentionID, string(it.Kind), it.UpdatedAt
		}
	}
	return f
}

// BlockedText is the flag text for a run waiting on a prompt that has no
// answerable approval item (the harness fell back to its own dialog).
const BlockedText = "Waiting on a prompt in its terminal"

// WithRun adds what the run itself says: a run waiting on approval with no
// blocking item is blocked on the owner. A nil run leaves f unchanged.
func WithRun(f *contracts.SessionFlag, r *contracts.Run) *contracts.SessionFlag {
	if r == nil || r.State != contracts.RunWaitingApproval {
		return f
	}
	if f != nil && f.Level == contracts.FlagNeedsYou {
		return f
	}
	out := contracts.SessionFlag{Level: contracts.FlagNeedsYou, Reason: "blocked", Text: BlockedText, Since: r.Freshness.ObservedAt}
	if f != nil {
		out.Count, out.Messages = f.Count, f.Messages
	}
	out.Count++
	return &out
}

// ResolveTitle replaces a session ID inside a hub-provided title with the
// session's title, at render time so renames apply.
func ResolveTitle(title, sessionID, sessionTitle string) string {
	sessionTitle = strings.TrimSpace(sessionTitle)
	if sessionID == "" || sessionTitle == "" || sessionTitle == sessionID {
		return title
	}
	if r := []rune(sessionTitle); len(r) > 48 {
		sessionTitle = strings.TrimSpace(string(r[:47])) + "…"
	}
	return strings.ReplaceAll(title, sessionID, sessionTitle)
}
