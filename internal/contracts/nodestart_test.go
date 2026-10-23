package contracts

import (
	"strings"
	"testing"
)

// A node credential starts a headless pyre run of claude or codex on
// itself; any other harness is refused.
func TestCheckNodeStartHarnesses(t *testing.T) {
	p := Principal{Kind: PrincipalNode, ID: "node_synth"}
	for _, h := range []Harness{HarnessClaude, HarnessCodex, HarnessShell, HarnessAgy} {
		req := StartSessionRequest{NodeID: "node_synth", Harness: h, Args: []string{ArgPyreExec + "=" + strings.Repeat("ab", 32)}}
		err := CheckNodeStart(p, &req)
		if want := h == HarnessClaude || h == HarnessCodex; (err == nil) != want {
			t.Errorf("%s: %v", h, err)
		}
	}
}
