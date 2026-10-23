package accounts

import (
	"slices"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

// The binding's unsets and the env-file refusals share one list
// (contracts.ClaudeLoginEnv): the Foundry switch is removed too.
func TestUnsetForSharesLoginList(t *testing.T) {
	u := UnsetFor(contracts.HarnessClaude)
	for _, k := range append(append([]string{}, contracts.ClaudeLoginEnv...), contracts.CodexLoginEnv...) {
		if !slices.Contains(u, k) {
			t.Errorf("claude launch keeps %s", k)
		}
	}
	if slices.Contains(unsetAmbient[contracts.HarnessClaude], "CLAUDE_SECURESTORAGE_CONFIG_DIR") || !slices.Contains(unsetAmbient[contracts.HarnessClaude], "CLAUDE_CODE_USE_FOUNDRY") {
		t.Fatalf("ambient unset %v", unsetAmbient[contracts.HarnessClaude])
	}
}
