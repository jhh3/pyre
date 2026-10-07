package updates

import (
	"strings"
	"testing"
)

// TestSourceRefusesEmbeddedCredentials: an npm spec goes on npm's command
// line and every source is recorded, so credentials in a URL are refused
// (and not echoed in the error).
func TestSourceRefusesEmbeddedCredentials(t *testing.T) {
	const secret = "SYNTHETIC-registry-token"
	for _, src := range []string{
		"https://user:" + secret + "@example.invalid/codex.tgz",
		"npm:https://user:" + secret + "@example.invalid/codex.tgz",
		"npm:git+https://" + secret + "@example.invalid/x.git",
	} {
		_, _, err := sourceKind(src)
		if err == nil {
			t.Fatalf("%s accepted", strings.ReplaceAll(src, secret, "***"))
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error echoes the credential: %v", err)
		}
	}
	for _, src := range []string{"npm:@openai/codex@0.50.0", "npm:https://example.invalid/codex.tgz", "https://example.invalid/codex.tgz?sig=abc"} {
		if _, _, err := sourceKind(src); err != nil {
			t.Fatalf("%s refused: %v", src, err)
		}
	}
}
