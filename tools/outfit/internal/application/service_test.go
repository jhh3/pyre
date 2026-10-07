package application

import (
	"testing"

	"github.com/jhh3/outfit/internal/kit"
	"github.com/jhh3/outfit/internal/skillmanager"
)

func TestSearchMatchesAliasesUseCasesAndFuzzyTerms(t *testing.T) {
	snapshot := Snapshot{Connectors: []ConnectorCard{
		{Key: "stripe", Display: "Stripe", Description: "Payments and subscriptions", Category: "customer-systems"},
		{Key: "google-workspace", Display: "Google Workspace", Description: "Gmail Drive Calendar", Category: "productivity"},
	}}
	for _, query := range []string{"payments", "strp", "customer payments"} {
		result := Search(snapshot, query)
		if len(result.Connectors) != 1 || result.Connectors[0].Key != "stripe" {
			t.Fatalf("query %q returned %#v", query, result.Connectors)
		}
	}
}

func TestCredentialEnvironmentDiscoveryIsDeclarativeAndNeverOverwritesInput(t *testing.T) {
	t.Setenv("ACME_TOKEN", "from-environment")
	method := kit.CredentialMethod{Fields: []kit.CredentialField{{Key: "TOKEN", Environment: []string{"ACME_TOKEN"}}}}
	resolved, sources := resolveCredentialEnvironment(method, map[string]string{})
	if resolved["TOKEN"] != "from-environment" || len(sources) != 1 || sources[0] != "ACME_TOKEN" {
		t.Fatalf("resolved = %#v sources = %#v", resolved, sources)
	}
	resolved, sources = resolveCredentialEnvironment(method, map[string]string{"TOKEN": "explicit"})
	if resolved["TOKEN"] != "explicit" || len(sources) != 0 {
		t.Fatalf("explicit input was overwritten: %#v %#v", resolved, sources)
	}
}

func TestSearchIndexesSkillProvenance(t *testing.T) {
	snapshot := Snapshot{Skills: []SkillCard{{Receipt: skillmanager.Receipt{ID: "gws-gmail", Connection: "work", Connector: "google-workspace", Source: "googleworkspace/cli", Path: "skills/gws-gmail"}}}}
	result := Search(snapshot, "googleworkspace gmail")
	if len(result.Skills) != 1 {
		t.Fatalf("skill provenance was not searchable: %#v", result.Skills)
	}
}
