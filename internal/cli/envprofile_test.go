package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/configuration"
)

// The CLI mirror of env.profile.get must carry every field the node returns.
func TestEnvProfileResultMirrorsNode(t *testing.T) {
	node := configuration.ProfileGetResult{Scope: "global", Name: "default", Present: true,
		Profile: &configuration.Profile{Name: "default"}, ProfileDigest: "sha256:aa",
		Pending: true, Desired: &configuration.Profile{Name: "default"}, DesiredDigest: "sha256:bb",
		AppliedGeneration: "env_synthetic"}
	b, _ := json.Marshal(node)
	var res envProfileResult
	if err := json.Unmarshal(b, &res); err != nil {
		t.Fatal(err)
	}
	back, _ := json.Marshal(res)
	var a, z map[string]any
	_ = json.Unmarshal(b, &a)
	_ = json.Unmarshal(back, &z)
	for k := range a {
		if _, ok := z[k]; !ok {
			t.Fatalf("CLI result drops field %q", k)
		}
	}
	var out bytes.Buffer
	printEnvProfile(&out, res)
	for _, want := range []string{"active profile default (global), digest sha256:aa", "applied generation env_synthetic", "pending desired profile, digest sha256:bb"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
}
