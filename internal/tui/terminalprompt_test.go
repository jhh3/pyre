package tui

import (
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

// The hub's "answer in its terminal" item (an expired remote approval whose
// native prompt is still up) points at the session: no answer overlay.
func TestTerminalPromptItemIsNotAnswerable(t *testing.T) {
	term := contracts.AttentionItem{Kind: contracts.AttnApproval, GroupKey: "terminal:apr_synth", Object: contracts.ObjectRef{Kind: contracts.KindSession, ID: "ses_synth"}}
	if isAnswerable(term) {
		t.Fatal("terminal-prompt item offered as answerable")
	}
	live := contracts.AttentionItem{Kind: contracts.AttnApproval, Object: contracts.ObjectRef{Kind: contracts.KindApproval, ID: "apr_synth"}}
	if !isAnswerable(live) {
		t.Fatal("pending approval item not answerable")
	}
}
