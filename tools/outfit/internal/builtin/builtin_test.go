package builtin

import (
	"strings"
	"testing"

	"github.com/jhh3/outfit/internal/agentguide"
	"github.com/jhh3/outfit/internal/kit"
)

func TestStripeReferenceHasFocusedCLIAndRESTSurfaces(t *testing.T) {
	pkg := mustPackage(t, "stripe")
	cli := mustMode(t, pkg, "cli")
	rest := mustMode(t, pkg, "rest")
	native := mustCredentialMethod(t, pkg, "native-login")
	if native.Setup == nil || native.Setup.BrowserContinuation == nil || native.Setup.BrowserContinuation.NextStepField != "next_step" {
		t.Fatalf("Stripe native login does not declare its browser continuation: %#v", native.Setup)
	}
	if cli.Recommendation.Rank != 1 || rest.Recommendation.Rank <= cli.Recommendation.Rank {
		t.Fatalf("recommendations: cli=%d rest=%d", cli.Recommendation.Rank, rest.Recommendation.Rank)
	}
	required := map[string]string{
		"account_get": "read", "balance_get": "read", "balance_transactions_list": "read",
		"customers_list": "read", "customers_get": "read", "customers_search": "read", "customers_create": "write", "customers_update": "write", "customers_delete": "destructive",
		"subscriptions_list": "read", "subscriptions_get": "read", "subscriptions_create": "write", "subscriptions_update": "write", "subscriptions_cancel": "destructive",
		"invoices_list": "read", "invoices_get": "read", "invoices_upcoming": "read", "invoices_create": "write", "invoices_finalize": "write", "invoices_void": "destructive",
		"payment_intents_list": "read", "payment_intents_get": "read", "charges_list": "read", "charges_get": "read", "refunds_list": "read", "refunds_get": "read", "refunds_create": "write",
		"events_list": "read", "events_get": "read", "events_resend": "write", "webhook_endpoints_list": "read", "webhook_endpoints_get": "read", "webhook_listen": "read", "event_trigger": "write", "logs_tail": "read",
	}
	actual := map[string]string{}
	for _, operation := range cli.Operations {
		actual[operation.Name] = operation.Class
	}
	for name, class := range required {
		if actual[name] != class {
			t.Errorf("CLI operation %s class = %q, want %q", name, actual[name], class)
		}
	}
	for _, name := range []string{"account_get", "balance_get", "customers_list", "customers_get", "subscriptions_list", "subscriptions_get", "invoices_list", "invoices_get", "payment_intents_list", "payment_intents_get", "charges_list", "charges_get", "refunds_list", "refunds_get", "events_list", "events_get", "webhook_endpoints_list", "webhook_endpoints_get"} {
		operation, ok := findOperation(rest, name)
		if !ok || operation.Class != "read" || operation.HTTP == nil {
			t.Errorf("REST read operation %s is missing or malformed", name)
		}
	}
	links := map[string]bool{}
	for _, resolver := range pkg.Links {
		links[resolver.ID] = true
	}
	for _, id := range []string{"customer", "subscription", "invoice", "payment_intent", "charge", "event", "webhook_endpoint"} {
		if !links[id] {
			t.Errorf("Stripe link resolver %s is missing", id)
		}
	}
}

func TestEveryBlessedSkillFollowsTheCompiledInvocationPolicy(t *testing.T) {
	packages, err := All()
	if err != nil {
		t.Fatal(err)
	}
	for _, loaded := range packages {
		parts := append([]string{}, loaded.Package.Skill.Guidance...)
		for _, workflow := range loaded.Package.Skill.Workflows {
			parts = append(parts, workflow.Steps...)
		}
		if err := agentguide.Audit(strings.Join(parts, "\n")); err != nil {
			t.Errorf("%s skill: %v", loaded.Package.Key, err)
		}
	}
}

func TestGWSReferenceUsesOneModeAndCapabilityScopedSkills(t *testing.T) {
	pkg := mustPackage(t, "google-workspace")
	if len(pkg.Modes) != 1 || pkg.Modes[0].ID != "cli" {
		t.Fatalf("GWS modes = %#v", pkg.Modes)
	}
	mode := pkg.Modes[0]
	if mode.DefaultCredentialMethod != "native-login" || len(mode.CredentialMethods) != 5 {
		t.Fatalf("GWS credential recommendation = %#v", mode)
	}
	oauth := mustCredentialMethod(t, pkg, "outfit-oauth")
	if oauth.Recommended || oauth.OAuth == nil || oauth.OAuth.AuthorizationURL != "https://accounts.google.com/o/oauth2/v2/auth" || oauth.OAuth.TokenURL != "https://oauth2.googleapis.com/token" {
		t.Fatalf("GWS managed OAuth = %#v", oauth)
	}
	if native := mustCredentialMethod(t, pkg, "native-login"); !native.Recommended {
		t.Fatalf("GWS native login is not recommended: %#v", native)
	}
	for _, methodID := range []string{"outfit-oauth", "managed-credentials-file", "managed-access-token", "managed-service-account", "native-login"} {
		method := mustCredentialMethod(t, pkg, methodID)
		bindings := append([]kit.CredentialBinding{}, method.Bindings...)
		bindings = append(bindings, mode.CredentialBindings[methodID]...)
		for _, inherited := range []string{"GOOGLE_WORKSPACE_CLI_TOKEN", "GOOGLE_WORKSPACE_CLI_CREDENTIALS_FILE", "GOOGLE_APPLICATION_CREDENTIALS", "GOOGLE_WORKSPACE_CLI_CLIENT_ID", "GOOGLE_WORKSPACE_CLI_CLIENT_SECRET"} {
			if !bindingDefinesOrClears(bindings, inherited) {
				t.Errorf("%s does not isolate inherited %s", methodID, inherited)
			}
		}
	}
	paths := map[string][]string{}
	for _, source := range pkg.SkillSources {
		for _, path := range source.Paths {
			paths[path.Path] = path.Capabilities
		}
	}
	if capabilities := paths["skills/gws-shared"]; len(capabilities) != 0 {
		t.Fatalf("gws-shared unexpectedly gated by %v", capabilities)
	}
	for path, capability := range map[string]string{
		"skills/gws-gmail": "gmail", "skills/gws-drive": "drive", "skills/gws-calendar": "calendar", "skills/gws-admin-reports": "admin",
	} {
		if values := paths[path]; len(values) != 1 || values[0] != capability {
			t.Errorf("%s capabilities = %v, want [%s]", path, values, capability)
		}
	}
}

func TestBlessedHostedMCPImportsAndEnvironmentDiscoveryAreDeclared(t *testing.T) {
	for _, connector := range []string{"linear", "notion", "sentry", "slack"} {
		pkg := mustPackage(t, connector)
		mode := mustMode(t, pkg, "hosted_mcp")
		method := mustCredentialMethod(t, pkg, "adopted-host-oauth")
		if mode.Executor.Kind != "mcp" || mode.Executor.Endpoint == "" || method.OAuth == nil || mode.Recommendation.Rank == 1 {
			t.Errorf("%s host import contract = mode %#v method %#v", connector, mode, method)
		}
		var scopesOptional bool
		for _, field := range method.Fields {
			if field.Key == "OAUTH_GRANTED_SCOPES" {
				scopesOptional = field.Generated && field.Optional
			}
		}
		if !scopesOptional {
			t.Errorf("%s requires diagnostic OAuth scope metadata that hosts may omit", connector)
		}
	}
	for connector, methodID := range map[string]string{"github": "managed-token", "intercom": "managed-token", "stripe": "managed-restricted-key"} {
		method := mustCredentialMethod(t, mustPackage(t, connector), methodID)
		found := false
		for _, field := range method.Fields {
			found = found || len(field.Environment) > 0
		}
		if !found {
			t.Errorf("%s %s has no declared environment discovery", connector, methodID)
		}
	}
}

func TestEveryBlessedModeHasFromScratchCredentialGuidance(t *testing.T) {
	packages, err := All()
	if err != nil {
		t.Fatal(err)
	}
	for _, loaded := range packages {
		pkg := loaded.Package
		for _, mode := range pkg.Modes {
			if mode.Recommendation.Rank < 1 || mode.Recommendation.Reason == "" {
				t.Errorf("%s/%s has no actionable recommendation", pkg.Key, mode.ID)
			}
			if mode.DefaultCredentialMethod == "" {
				t.Errorf("%s/%s has no default credential method", pkg.Key, mode.ID)
			}
			for _, methodID := range mode.CredentialMethods {
				method := mustCredentialMethod(t, pkg, methodID)
				if method.Description == "" {
					t.Errorf("%s/%s/%s has no setup description", pkg.Key, mode.ID, methodID)
				}
				for _, field := range method.Fields {
					if !field.Generated && !field.Optional && field.Description == "" && len(field.Choices) == 0 {
						t.Errorf("%s/%s/%s required field %s has no instructions", pkg.Key, mode.ID, methodID, field.Key)
					}
				}
			}
		}
	}
}

func TestEveryBlessedOperationHasAnErgonomicMachineSchema(t *testing.T) {
	packages, err := All()
	if err != nil {
		t.Fatal(err)
	}
	operations := 0
	for _, loaded := range packages {
		for _, mode := range loaded.Package.Modes {
			for _, operation := range mode.Operations {
				operations++
				if issues := kit.AuditOperationSchema(operation); len(issues) > 0 {
					t.Errorf("%s/%s/%s: %v", loaded.Package.Key, mode.ID, operation.Name, issues)
				}
				properties, _ := operation.Input["properties"].(map[string]any)
				for name, raw := range properties {
					property, _ := raw.(map[string]any)
					if property["description"] == "" || property["examples"] == nil && property["default"] == nil && property["enum"] == nil {
						t.Errorf("%s/%s/%s argument %s is not self-teaching: %#v", loaded.Package.Key, mode.ID, operation.Name, name, property)
					}
					for _, alias := range kit.ArgumentAliases(properties)[name] {
						matches := kit.MatchArgumentNames(properties, alias)
						if len(matches) != 1 || matches[0] != name {
							t.Errorf("%s/%s/%s alias %s resolves to %v", loaded.Package.Key, mode.ID, operation.Name, alias, matches)
						}
					}
				}
			}
		}
	}
	if operations < 100 {
		t.Fatalf("operation audit unexpectedly small: %d", operations)
	}
}

func TestProviderErrorEnvelopesAreDeclared(t *testing.T) {
	for _, operation := range mustMode(t, mustPackage(t, "slack"), "rest").Operations {
		if operation.HTTP == nil || operation.HTTP.SuccessField != "ok" || operation.HTTP.ErrorField != "error" {
			t.Errorf("Slack %s does not reject HTTP-200 error envelopes", operation.Name)
		}
	}
	for _, operation := range mustMode(t, mustPackage(t, "linear"), "graphql").Operations {
		if operation.HTTP == nil || operation.HTTP.ErrorField != "errors" {
			t.Errorf("Linear %s does not reject GraphQL errors", operation.Name)
		}
	}
}

func TestBlessedHostedMCPLinksUseTheSelectedExecutor(t *testing.T) {
	expected := map[string]map[string]string{
		"linear": {"graphql": "issue", "hosted_mcp": "get_issue"},
		"notion": {"rest": "page", "hosted_mcp": "notion-fetch"},
		"sentry": {"rest": "issue", "hosted_mcp": "get_sentry_resource"},
		"slack":  {"rest": "thread", "hosted_mcp": "slack_read_thread"},
	}
	for connector, modes := range expected {
		pkg := mustPackage(t, connector)
		if len(pkg.Links) == 0 {
			t.Fatalf("%s has no automatic link resolver", connector)
		}
		actual := map[string]string{}
		for _, lookup := range pkg.Links[0].LookupOptions() {
			actual[lookup.Mode] = lookup.Operation
		}
		for mode, operation := range modes {
			if actual[mode] != operation {
				t.Errorf("%s %s link lookup = %q, want %q", connector, mode, actual[mode], operation)
			}
		}
	}
}

func TestEveryBlessedConnectorHasHeavyLinkEvidence(t *testing.T) {
	packages, err := All()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, loaded := range packages {
		pkg := loaded.Package
		if len(pkg.Links) == 0 {
			t.Errorf("%s has no automatic link resolver", pkg.Key)
			continue
		}
		seen[pkg.Key] = true
		for _, resolver := range pkg.Links {
			for _, lookup := range resolver.LookupOptions() {
				mode := mustMode(t, pkg, lookup.Mode)
				if mode.Executor.Kind != "mcp" {
					operation, ok := findOperation(mode, lookup.Operation)
					if !ok || operation.Class != "read" {
						t.Errorf("%s/%s %s is not a readable primary lookup", pkg.Key, resolver.ID, lookup.Operation)
					}
				}
			}
		}
	}
	for _, key := range []string{"github", "google-workspace", "intercom", "linear", "notion", "sentry", "slack", "stripe"} {
		if !seen[key] {
			t.Errorf("blessed connector %s is missing heavy link support", key)
		}
	}

	if lookup := lookupFor(t, mustPackage(t, "notion"), "page", "rest"); len(lookup.HeavyReads) != 1 || lookup.HeavyReads[0].Recurse == nil {
		t.Fatalf("Notion REST heavy lookup must recursively fetch page blocks: %#v", lookup.HeavyReads)
	}
	if lookup := lookupFor(t, mustPackage(t, "sentry"), "issue", "rest"); len(lookup.HeavyReads) != 1 || lookup.HeavyReads[0].Operation != "issue_events" {
		t.Fatalf("Sentry REST heavy lookup must fetch recent events: %#v", lookup.HeavyReads)
	}
	if lookup := lookupFor(t, mustPackage(t, "github"), "pull_request", "cli"); len(lookup.HeavyReads) != 3 {
		t.Fatalf("GitHub pull request heavy lookup must fetch files and discussion: %#v", lookup.HeavyReads)
	}
}

func lookupFor(t *testing.T, pkg kit.Package, resolverID, mode string) kit.LinkLookup {
	t.Helper()
	for _, resolver := range pkg.Links {
		if resolver.ID != resolverID {
			continue
		}
		for _, lookup := range resolver.LookupOptions() {
			if lookup.Mode == mode {
				return lookup
			}
		}
	}
	t.Fatalf("%s link %s has no %s lookup", pkg.Key, resolverID, mode)
	return kit.LinkLookup{}
}

func mustCredentialMethod(t *testing.T, pkg kit.Package, id string) kit.CredentialMethod {
	t.Helper()
	for _, method := range pkg.CredentialMethods {
		if method.ID == id {
			return method
		}
	}
	t.Fatalf("credential method %s is missing", id)
	return kit.CredentialMethod{}
}

func bindingDefinesOrClears(bindings []kit.CredentialBinding, name string) bool {
	for _, binding := range bindings {
		if binding.Name == name && (binding.Target == "unset-env" || binding.Target == "process-env" || binding.Target == "temporary-file") {
			return true
		}
	}
	return false
}

func mustPackage(t *testing.T, key string) kit.Package {
	t.Helper()
	packages, err := All()
	if err != nil {
		t.Fatal(err)
	}
	for _, loaded := range packages {
		if loaded.Package.Key == key {
			return loaded.Package
		}
	}
	t.Fatalf("package %s is missing", key)
	return kit.Package{}
}

func mustMode(t *testing.T, pkg kit.Package, id string) kit.Mode {
	t.Helper()
	for _, mode := range pkg.Modes {
		if mode.ID == id {
			return mode
		}
	}
	t.Fatalf("mode %s is missing", id)
	return kit.Mode{}
}

func findOperation(mode kit.Mode, name string) (kit.Operation, bool) {
	for _, operation := range mode.Operations {
		if operation.Name == name {
			return operation, true
		}
	}
	return kit.Operation{}, false
}
