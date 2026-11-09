package daemon

import (
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

// The handoff prompt's commands must be ones the CLI accepts: rc129 told
// agents `pyre session read --run` (no such flag), `pyre note get` and
// `pyre note create --kind handoff`.
func TestHandoffPromptUsesRealCommands(t *testing.T) {
	sc := contracts.StartRunCommand{Session: contracts.Session{SessionID: "ses_synth-a", Lineage: contracts.Lineage{HandoffNoteID: "note_synth-h"}}}
	p := handoffPrompt(sc, contracts.Run{RunID: "run_synth-1", NodeID: "node_synth-n"})
	for _, want := range []string{"pyre note show note_synth-h", "pyre read ses_synth-a --run run_synth-1 --tail 80", "--handoff --body"} {
		if !strings.Contains(p, want) {
			t.Fatalf("missing %q in:\n%s", want, p)
		}
	}
	for _, bad := range []string{"pyre session read", "pyre note get", "note create"} {
		if strings.Contains(p, bad) {
			t.Fatalf("unsupported command %q in:\n%s", bad, p)
		}
	}
}
