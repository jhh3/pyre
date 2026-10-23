package builtin

import (
	"testing"

	"github.com/jhh3/outfit/internal/kit"
)

func TestStripeReadsOfferLiveModeAndWritesDoNot(t *testing.T) {
	pkg := mustPackage(t, "stripe")
	for _, mode := range pkg.Modes {
		for _, operation := range mode.Operations {
			property, _ := kit.EffectiveInput(operation)["properties"].(map[string]any)["live"].(map[string]any)
			wantLive := operation.Class == "read" && operation.Interaction != "stream"
			if (property != nil) != wantLive {
				t.Errorf("%s/%s (%s): live argument present = %v, want %v", mode.ID, operation.Name, operation.Class, property != nil, wantLive)
				continue
			}
			if !wantLive {
				continue
			}
			if property["type"] != "boolean" || property["description"] == "" {
				t.Errorf("%s/%s: live argument = %#v", mode.ID, operation.Name, property)
			}
			if mode.ID == "cli" {
				found := false
				for _, optional := range operation.CLI.OptionalArgs {
					found = found || (optional.Argument == "live" && optional.Flag == "--live" && optional.Boolean)
				}
				if !found {
					t.Errorf("cli/%s does not pass --live to the Stripe CLI", operation.Name)
				}
			}
		}
	}
	method := mustCredentialMethod(t, pkg, "managed-restricted-key")
	var live kit.CredentialField
	for _, field := range method.Fields {
		if field.Key == "STRIPE_LIVE_API_KEY" {
			live = field
		}
	}
	if !live.Secret || !live.Optional || live.Pattern != "(rk|sk)_live_[A-Za-z0-9_]+" || len(live.Environment) != 1 {
		t.Fatalf("live key field = %#v", live)
	}
	for _, modeID := range []string{"cli", "rest"} {
		bindings := kit.CredentialBindingsFor(mustMode(t, pkg, modeID), method)
		if len(bindings) != 2 || bindings[0].UnlessArgument != "live" || bindings[1].WhenArgument != "live" || bindings[1].Value[len(bindings[1].Value)-len("{{secret.STRIPE_LIVE_API_KEY}}"):] != "{{secret.STRIPE_LIVE_API_KEY}}" {
			t.Fatalf("%s bindings = %#v", modeID, bindings)
		}
	}
	cli := mustMode(t, pkg, "cli")
	if cli.Executor.ErrorField != "error" || cli.Executor.ErrorStatus["resource_missing"] != 404 {
		t.Fatalf("cli error mapping = %#v", cli.Executor)
	}
}
