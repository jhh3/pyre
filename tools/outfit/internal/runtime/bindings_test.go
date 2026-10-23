package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jhh3/outfit/internal/kit"
)

func liveCredentials() CredentialContext {
	return CredentialContext{
		Method: kit.CredentialMethod{ID: "managed-restricted-key", Custody: "outfit", Fields: []kit.CredentialField{
			{Key: "STRIPE_API_KEY", Secret: true},
			{Key: "STRIPE_LIVE_API_KEY", Secret: true, Optional: true, Environment: []string{"STRIPE_LIVE_API_KEY"}},
		}},
		Bindings: []kit.CredentialBinding{
			{Target: "process-env", Name: "STRIPE_API_KEY", Value: "{{secret.STRIPE_API_KEY}}", UnlessArgument: "live"},
			{Target: "process-env", Name: "STRIPE_API_KEY", Value: "{{secret.STRIPE_LIVE_API_KEY}}", WhenArgument: "live"},
		},
	}
}

func TestForArgumentsSelectsOneKeyAndNeverFallsBack(t *testing.T) {
	secrets := map[string]string{"STRIPE_API_KEY": "rk_test_SYNTHETIC", "STRIPE_LIVE_API_KEY": "rk_live_SYNTHETIC"}
	for _, tc := range []struct {
		arguments map[string]string
		want      string
	}{
		{nil, "{{secret.STRIPE_API_KEY}}"},
		{map[string]string{"live": "false"}, "{{secret.STRIPE_API_KEY}}"},
		{map[string]string{"live": "true"}, "{{secret.STRIPE_LIVE_API_KEY}}"},
	} {
		selected, err := liveCredentials().ForArguments(tc.arguments, secrets)
		if err != nil || len(selected.Bindings) != 1 || selected.Bindings[0].Value != tc.want {
			t.Fatalf("arguments %v selected %#v, %v", tc.arguments, selected.Bindings, err)
		}
	}
	if _, err := liveCredentials().ForArguments(map[string]string{"live": "yes please"}, secrets); err == nil {
		t.Fatal("a non-boolean condition was accepted")
	}
	_, err := liveCredentials().ForArguments(map[string]string{"live": "true"}, map[string]string{"STRIPE_API_KEY": "rk_test_SYNTHETIC"})
	if err == nil || !strings.Contains(err.Error(), "--live needs this connection's STRIPE_LIVE_API_KEY credential") || !strings.Contains(err.Error(), "STRIPE_LIVE_API_KEY set in its environment") {
		t.Fatalf("missing live key = %v", err)
	}
	// A plan has no secrets and only filters.
	if selected, err := liveCredentials().ForArguments(map[string]string{"live": "true"}, nil); err != nil || len(selected.Bindings) != 1 {
		t.Fatalf("plan selection = %#v, %v", selected.Bindings, err)
	}
}

func TestCLIErrorObjectBecomesProviderError(t *testing.T) {
	mode := kit.Mode{Executor: kit.Executor{Kind: "cli", Binary: "printf", ErrorField: "error", ErrorStatus: map[string]int{"resource_missing": 404, "invalid_request_error": 400}}}
	run := func(output string) (Result, error) {
		operation := kit.Operation{CLI: &kit.CLIOperation{Args: []string{"%s", output}}}
		return (Executor{}).Run(context.Background(), mode, operation, nil, nil)
	}
	result, err := run(`{"error":{"code":"resource_missing","message":"No such subscription: 'sub_missing'","type":"invalid_request_error"}}`)
	var failure *ProviderError
	if !errors.As(err, &failure) || failure.Status != 404 || failure.Code != "resource_missing" || result.Status != 404 {
		t.Fatalf("resource_missing = %#v status %d, %v", failure, result.Status, err)
	}
	// Without a code the type picks the status.
	if _, err := run(`{"error":{"message":"Invalid limit","type":"invalid_request_error"}}`); !errors.As(err, &failure) || failure.Status != 400 || err.Error() != "upstream returned 400 Bad Request: Invalid limit (invalid_request_error)" {
		t.Fatalf("typed error = %v", err)
	}
	// An unmapped error is still a failure, without an invented status.
	if result, err := run(`{"error":{"message":"Something odd","type":"novel_error"}}`); !errors.As(err, &failure) || result.Status != 0 || err.Error() != "upstream reported an error: Something odd (novel_error)" {
		t.Fatalf("unmapped error = %v status %d", err, result.Status)
	}
	if result, err := run(`{"id":"sub_SYNTHETIC1","error":null}`); err != nil || result.Status != 200 {
		t.Fatalf("success with a null error field = %v status %d", err, result.Status)
	}
}
