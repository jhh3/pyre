package invoke

import (
	"context"
	"strings"
	"testing"

	"github.com/jhh3/outfit/internal/kit"
	"github.com/jhh3/outfit/internal/runtime"
)

func TestRunReportsProviderErrorStatusAndCode(t *testing.T) {
	operation := kit.NormalizeOperation(kit.Operation{Name: "subscriptions_get", Class: "read", CLI: &kit.CLIOperation{Args: []string{"subscriptions", "retrieve", "{{arg.subscription_id}}"}}})
	envelope, err := Run(context.Background(), Metadata{Connection: "stripe", Connector: "stripe", Mode: "cli"}, operation, map[string]string{"subscription_id": "sub_missing"}, Options{}, func(context.Context, map[string]string) (runtime.Result, error) {
		return runtime.Result{Status: 404, ContentType: "application/json", Data: map[string]any{"error": map[string]any{"code": "resource_missing"}}}, &runtime.ProviderError{Status: 404, Code: "resource_missing", Message: "No such subscription: 'sub_missing'"}
	})
	if err == nil || envelope.OK || envelope.Meta.Status != 404 || envelope.Error == nil || envelope.Error.ProviderCode != "resource_missing" || envelope.Error.Code != "operation_failed" {
		t.Fatalf("envelope=%#v err=%v", envelope, err)
	}
	if envelope.Error.Message != "upstream returned 404 Not Found: No such subscription: 'sub_missing' (resource_missing)" {
		t.Fatalf("message = %q", envelope.Error.Message)
	}
	encoded, _ := Marshal(envelope)
	for _, want := range []string{`"ok": false`, `"status": 404`, `"provider_code": "resource_missing"`} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("encoded envelope lacks %s: %s", want, encoded)
		}
	}
}
