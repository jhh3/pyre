package kit

import (
	"strings"
	"testing"
)

func TestValidateConditionalBindingsAndCLIErrorFields(t *testing.T) {
	pkg := validPackage()
	method := &pkg.CredentialMethods[0]
	method.Fields = append(method.Fields, CredentialField{Key: "ACME_LIVE_TOKEN", Label: "Live token", Secret: true, Optional: true})
	method.Bindings = []CredentialBinding{
		{Target: "http-header", Name: "Authorization", Value: "Bearer {{secret.ACME_TOKEN}}", UnlessArgument: "live"},
		{Target: "http-header", Name: "Authorization", Value: "Bearer {{secret.ACME_LIVE_TOKEN}}", WhenArgument: "live"},
	}
	if issues := Validate(pkg); len(issues) != 0 {
		t.Fatalf("conditional bindings rejected: %v", issues)
	}
	if got := BindingConditions(method.Bindings); len(got) != 1 || got[0] != "live" {
		t.Fatalf("binding conditions = %v", got)
	}
	method.Bindings[1].UnlessArgument = "live"
	method.Bindings[0].WhenArgument = "Live-Mode"
	joined := strings.Join(Validate(pkg), "\n")
	if !strings.Contains(joined, "not both") || !strings.Contains(joined, "must be a lowercase argument name") {
		t.Fatalf("malformed binding conditions accepted: %s", joined)
	}

	pkg = validPackage()
	pkg.Modes[0].Executor.ErrorField = "error"
	if joined := strings.Join(Validate(pkg), "\n"); !strings.Contains(joined, "supported only for cli modes") {
		t.Fatalf("HTTP executor errorField accepted: %s", joined)
	}
	pkg.Modes[0].Executor = Executor{Kind: "cli", Binary: "acme", ErrorStatus: map[string]int{"resource_missing": 200}}
	pkg.Modes[0].Operations[0].HTTP = nil
	pkg.Modes[0].Operations[0].CLI = &CLIOperation{Args: []string{"get"}}
	pkg.CredentialMethods[0].Bindings = []CredentialBinding{{Target: "process-env", Name: "ACME_TOKEN", Value: "{{secret.ACME_TOKEN}}"}}
	joined = strings.Join(Validate(pkg), "\n")
	if !strings.Contains(joined, "needs an errorField") || !strings.Contains(joined, "must map to a 4xx or 5xx status") {
		t.Fatalf("malformed CLI error mapping accepted: %s", joined)
	}
	pkg.Modes[0].Executor.ErrorField = "error"
	pkg.Modes[0].Executor.ErrorStatus["resource_missing"] = 404
	if issues := Validate(pkg); len(issues) != 0 {
		t.Fatalf("valid CLI error mapping rejected: %v", issues)
	}
}
