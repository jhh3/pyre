package kit

import (
	"strings"
	"testing"
)

func validPackage() Package {
	return Package{
		APIVersion: APIVersion, Key: "acme", Display: "Acme", Version: "1.0.0",
		CredentialMethods: []CredentialMethod{{
			ID: "managed-token", Label: "Managed token", Description: "Store an Acme token in outfit", Recommended: true, Custody: "outfit",
			Fields:   []CredentialField{{Key: "ACME_TOKEN", Label: "Token", Secret: true}},
			Bindings: []CredentialBinding{{Target: "http-header", Name: "Authorization", Value: "Bearer {{secret.ACME_TOKEN}}"}},
		}},
		Modes: []Mode{{
			ID: "api", Label: "API", Description: "Fixed API", Recommendation: Recommendation{Rank: 1, Reason: "Complete fixed-origin API"},
			Executor:                Executor{Kind: "http", BaseURL: "https://api.example.com/"},
			CredentialMethods:       []string{"managed-token"},
			DefaultCredentialMethod: "managed-token",
			Operations:              []Operation{{Name: "get_widget", Description: "Get widget", Class: "read", HTTP: &HTTPOperation{Method: "GET", Path: "/widgets/{id}"}}},
			Verify:                  "get_widget",
		}},
		Links: []LinkResolver{{
			ID: "widget", ResourceType: "widget",
			Patterns:     []LinkPattern{{Hosts: []string{"app.example.com"}, Path: "/widgets/:id", Segments: map[string]SegmentRule{"id": {Kind: "integer"}}}},
			Lookup:       LinkLookup{Mode: "api", Operation: "get_widget", Arguments: map[string]string{"id": "{id}"}},
			Presentation: LinkPresentation{CanonicalID: "id", Title: "name"},
		}},
		Skill: Skill{Summary: "Use Acme widgets"},
	}
}

func TestValidateComposeAndExecutorHeaders(t *testing.T) {
	pkg := validPackage()
	mode := &pkg.Modes[0]
	mode.Operations[0].HTTP.Path = "/widgets/:id"
	mode.Operations[0] = NormalizeOperation(mode.Operations[0])
	mode.Operations = append(mode.Operations,
		NormalizeOperation(Operation{Name: "delete_widget", Description: "Delete", Class: "destructive", HTTP: &HTTPOperation{Method: "DELETE", Path: "/widgets/:id"}}),
		NormalizeOperation(Operation{Name: "snapshot", Description: "Snapshot", Class: "read", Compose: []ComposeStep{{As: "widget", Operation: "get_widget"}}}),
	)
	mode.Executor.Headers = map[string]string{"User-Agent": "AcmeBot (https://example.com, 1)"}
	if issues := Validate(pkg); len(issues) == 0 || !strings.Contains(strings.Join(issues, "\n"), "requires id, which the composite does not declare") {
		t.Fatalf("composite without its step's required argument was accepted: %#v", issues)
	}
	mode.Operations[2] = NormalizeOperation(Operation{Name: "snapshot", Description: "Snapshot", Class: "read", Input: map[string]any{"properties": map[string]any{"id": map[string]any{"type": "string", "description": "Widget.", "examples": []any{"1"}}}}, Compose: []ComposeStep{{As: "widget", Operation: "get_widget"}}})
	if issues := Validate(pkg); len(issues) != 0 {
		t.Fatalf("valid composite rejected: %#v", issues)
	}
	mode.Operations[2].Compose = append(mode.Operations[2].Compose, ComposeStep{As: "gone", Operation: "delete_widget"})
	mode.Executor.Headers["Authorization"] = "Bearer static"
	joined := strings.Join(Validate(pkg), "\n")
	if !strings.Contains(joined, "must be a non-composite read") || !strings.Contains(joined, "is reserved") {
		t.Fatalf("unsafe composite or header accepted: %s", joined)
	}
}

func TestValidateRejectsSecretInProcessArgv(t *testing.T) {
	pkg := validPackage()
	pkg.Modes[0].Executor = Executor{Kind: "cli", Binary: "example"}
	pkg.Modes[0].Operations[0].HTTP = nil
	pkg.Modes[0].Operations[0].CLI = &CLIOperation{Args: []string{"--token", "{{secret.ACME_TOKEN}}"}}
	if issues := Validate(pkg); !strings.Contains(strings.Join(issues, "\n"), "secrets may not enter process argv") {
		t.Fatalf("missing argv finding: %#v", issues)
	}
}

func TestValidateAcceptsCoherentPackage(t *testing.T) {
	if issues := Validate(validPackage()); len(issues) != 0 {
		t.Fatalf("unexpected issues: %v", issues)
	}
}

func TestValidateRejectsWriteLinkLookup(t *testing.T) {
	pkg := validPackage()
	pkg.Modes[0].Operations[0].Class = "write"
	issues := Validate(pkg)
	if len(issues) == 0 {
		t.Fatal("expected write link lookup to be rejected")
	}
}

func TestValidateRejectsWriteOrUnboundedHeavyLinkReads(t *testing.T) {
	pkg := validPackage()
	pkg.Modes[0].Operations = append(pkg.Modes[0].Operations, Operation{Name: "delete_widget", Description: "Delete widget", Class: "destructive", HTTP: &HTTPOperation{Method: "DELETE", Path: "/widgets/{id}"}})
	pkg.Links[0].Lookup.HeavyReads = []LinkHeavyRead{{Operation: "delete_widget", Arguments: map[string]string{"id": "id"}, Recurse: &LinkRecursion{ItemsPath: "$.items", IDPath: "$.id", Argument: "id", MaxCalls: 101}}}
	issues := strings.Join(Validate(pkg), "\n")
	if !strings.Contains(issues, "heavy read must cite a read operation") || !strings.Contains(issues, "exceeds depth or call bounds") {
		t.Fatalf("unsafe heavy read was accepted: %s", issues)
	}
}

func TestValidateAcceptsModeSpecificDynamicMCPLinkLookup(t *testing.T) {
	pkg := validPackage()
	pkg.CredentialMethods = append(pkg.CredentialMethods, CredentialMethod{
		ID: "host-oauth", Label: "Hosted OAuth", Description: "Use a hosted OAuth grant", Custody: "oauth",
		Fields:   []CredentialField{{Key: "OAUTH_ACCESS_TOKEN", Label: "Access token", Secret: true, Generated: true}},
		Bindings: []CredentialBinding{{Target: "mcp-header", Name: "Authorization", Value: "Bearer {{secret.OAUTH_ACCESS_TOKEN}}"}},
	})
	pkg.Modes = append(pkg.Modes, Mode{
		ID: "hosted_mcp", Label: "Hosted MCP", Description: "Discovered tools", Recommendation: Recommendation{Rank: 2, Reason: "Use an existing hosted login"},
		Executor: Executor{Kind: "mcp", Endpoint: "https://mcp.example.com/mcp"}, CredentialMethods: []string{"host-oauth"}, DefaultCredentialMethod: "host-oauth",
	})
	pkg.Links[0].Lookup = LinkLookup{}
	pkg.Links[0].Lookups = []LinkLookup{
		{Mode: "api", Operation: "get_widget", Arguments: map[string]string{"id": "id"}},
		{Mode: "hosted_mcp", Operation: "fetch_widget", Arguments: map[string]string{"id": "$url"}},
	}
	if issues := Validate(pkg); len(issues) != 0 {
		t.Fatalf("mode-specific MCP link was rejected: %v", issues)
	}
}

func TestValidateRejectsMultipleRecommendedModes(t *testing.T) {
	pkg := validPackage()
	second := pkg.Modes[0]
	second.ID = "other"
	second.Recommendation.Rank = 1
	pkg.Modes[0].Recommendation.Rank = 1
	pkg.Modes = append(pkg.Modes, second)
	if issues := Validate(pkg); len(issues) == 0 {
		t.Fatal("expected multiple recommended modes to be rejected")
	}
}

func TestValidateRejectsAmbiguousBooleanCLIArgument(t *testing.T) {
	pkg := validPackage()
	pkg.Modes[0].Executor = Executor{Kind: "cli", Binary: "acme"}
	pkg.Modes[0].Operations[0].HTTP = nil
	pkg.Modes[0].Operations[0].CLI = &CLIOperation{Args: []string{"list"}, OptionalArgs: []CLIOptionalArg{{Argument: "json", Boolean: true}}}
	issues := Validate(pkg)
	if !strings.Contains(strings.Join(issues, "\n"), "boolean CLI args need a flag") {
		t.Fatalf("missing boolean flag issue: %#v", issues)
	}
}

func TestValidateRejectsMethodBindingIncompatibleWithSelectedExecutor(t *testing.T) {
	pkg := validPackage()
	pkg.CredentialMethods[0].Bindings = []CredentialBinding{{Target: "process-env", Name: "ACME_TOKEN", Value: "{{secret.ACME_TOKEN}}"}}
	issues := Validate(pkg)
	if !strings.Contains(strings.Join(issues, "\n"), "incompatible with http") {
		t.Fatalf("inherited incompatible binding was accepted: %#v", issues)
	}
}

func TestValidateCredentialEnvironmentSources(t *testing.T) {
	pkg := validPackage()
	pkg.CredentialMethods[0].Fields[0].Environment = []string{"ACME_TOKEN", "ACME_API_KEY"}
	if issues := Validate(pkg); len(issues) != 0 {
		t.Fatalf("valid environment discovery was rejected: %v", issues)
	}
	pkg.CredentialMethods[0].Fields[0].Environment = []string{"AcmeToken", "AcmeToken"}
	issues := strings.Join(Validate(pkg), "\n")
	if !strings.Contains(issues, "UPPER_SNAKE_CASE") || !strings.Contains(issues, "duplicate environment") {
		t.Fatalf("invalid environment discovery was accepted: %s", issues)
	}
}

func TestValidateBrowserContinuationContract(t *testing.T) {
	pkg := validPackage()
	pkg.CredentialMethods[0].Setup = &CommandTemplate{
		Program: "acme",
		Args:    []string{"login"},
		BrowserContinuation: &BrowserContinuation{
			URLField: "browser_url", VerificationCodeField: "verification_code", NextStepField: "next_step", TimeoutSeconds: 300,
		},
	}
	if issues := Validate(pkg); len(issues) != 0 {
		t.Fatalf("valid browser continuation was rejected: %v", issues)
	}
	pkg.CredentialMethods[0].Setup.BrowserContinuation.URLField = "Browser URL"
	pkg.CredentialMethods[0].Setup.BrowserContinuation.TimeoutSeconds = 901
	pkg.CredentialMethods[0].Verify = &CommandTemplate{Program: "acme", BrowserContinuation: &BrowserContinuation{URLField: "url", NextStepField: "next"}}
	issues := strings.Join(Validate(pkg), "\n")
	for _, want := range []string{"lowercase urlField", "between 0 and 900", "only for setup commands"} {
		if !strings.Contains(issues, want) {
			t.Fatalf("missing %q in validation issues: %s", want, issues)
		}
	}
}

func TestValidateArgumentDrivenHTTPStaysHonest(t *testing.T) {
	pkg := validPackage()
	mode := &pkg.Modes[0]
	mode.Operations = append(mode.Operations,
		NormalizeOperation(Operation{Name: "api_get", Description: "Any GET", Class: "read", HTTP: &HTTPOperation{Method: "GET", Path: "/", PathArgument: "path"}}),
		NormalizeOperation(Operation{Name: "api_call", Description: "Any call", Class: "destructive", HTTP: &HTTPOperation{Path: "/", MethodArgument: "method", Methods: []string{"POST", "DELETE"}, PathArgument: "path", BodyArgument: "body", ArgumentHeaders: []ArgumentHeader{{Name: "X-Audit-Log-Reason", Argument: "reason", Encoding: "url"}}}}),
	)
	if issues := Validate(pkg); len(issues) != 0 {
		t.Fatalf("valid raw-path operations rejected: %#v", issues)
	}
	mode.Operations = append(mode.Operations,
		NormalizeOperation(Operation{Name: "sneaky_read", Description: "Read that posts", Class: "read", HTTP: &HTTPOperation{Path: "/", MethodArgument: "method", Methods: []string{"GET", "POST"}, PathArgument: "path"}}),
		NormalizeOperation(Operation{Name: "header_swap", Description: "Credential header", Class: "write", HTTP: &HTTPOperation{Method: "POST", Path: "/x", ArgumentHeaders: []ArgumentHeader{{Name: "Authorization", Argument: "token"}}}}),
	)
	joined := strings.Join(Validate(pkg), "\n")
	if !strings.Contains(joined, "must use GET") || !strings.Contains(joined, "Authorization needs a token name that is not reserved") {
		t.Fatalf("dishonest argument-driven HTTP accepted: %s", joined)
	}
}
