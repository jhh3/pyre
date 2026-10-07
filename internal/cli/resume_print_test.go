package cli

import (
	"bytes"
	"strings"
	"testing"

	c "github.com/jhh3/pyre/internal/contracts"
)

// `pyre resume` returns once the new run is queued: the output must not
// claim the session resumed before the launch outcome is known, and names
// the node (registry name from the hub), like `pyre start`.
func TestResumeOutputDoesNotClaimSuccess(t *testing.T) {
	res := c.StartSessionResponse{CommandID: "cmd_synth-r", Session: c.SessionView{
		Session:  c.Session{SessionID: "ses_synth-r"},
		NodeName: "wp-synth-b",
		Run:      &c.Run{RunID: "run_synth-r2", State: c.RunStarting, ContinuationKind: c.ContinuationNativeResume, PreviousRunID: "run_synth-r1"},
	}}
	var b bytes.Buffer
	printResume(&b, res)
	out := b.String()
	if strings.Contains(out, "resumed") {
		t.Fatalf("claims success before the launch outcome:\n%s", out)
	}
	for _, want := range []string{"resume of session ses_synth-r queued", "run_synth-r2", "on wp-synth-b", "starting", "cmd_synth-r", "the run state shows the outcome"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}
