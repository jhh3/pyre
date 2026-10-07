package application

import (
	"testing"

	"github.com/jhh3/outfit/internal/hostmcp"
	"github.com/jhh3/outfit/internal/kit"
	"github.com/jhh3/outfit/internal/linkcontext"
	"github.com/jhh3/outfit/internal/skillmanager"
	"github.com/jhh3/outfit/internal/state"
)

func TestBuildWorkbenchRanksProviderStateAndRecipes(t *testing.T) {
	card := ConnectorCard{
		Key: "workspace", Display: "Workspace", RecommendedMode: "cli", DetectedCredentialMethod: "native",
		Modes:   []kit.Mode{{ID: "cli", Label: "CLI", CredentialMethods: []string{"oauth", "native"}, DefaultCredentialMethod: "oauth", Recommendation: kit.Recommendation{Rank: 0}}},
		Methods: []kit.CredentialMethod{{ID: "oauth", Label: "Managed OAuth"}, {ID: "native", Label: "Existing login"}},
	}
	workbench := BuildWorkbench(Snapshot{Connectors: []ConnectorCard{card}})
	provider := workbench.Providers[0]
	if provider.State != ProviderDetected || provider.Recipes[0].MethodID != "native" || !provider.Recipes[0].Detected {
		t.Fatalf("provider = %#v", provider)
	}

	workbench = BuildWorkbench(Snapshot{
		Connectors:  []ConnectorCard{card},
		Connections: []state.Connection{{Name: "workspace", ConnectorKey: "workspace", Status: "active"}},
	})
	if workbench.Providers[0].State != ProviderConnected {
		t.Fatalf("connected provider state = %q", workbench.Providers[0].State)
	}
}

func TestBuildWorkbenchKeepsImportsCredentialsToolsAndSkillsUnderProvider(t *testing.T) {
	card := ConnectorCard{
		Key: "github", Display: "GitHub",
		Modes:   []kit.Mode{{ID: "cli", Executor: kit.Executor{Tool: "gh"}, CredentialMethods: []string{"native"}}},
		Methods: []kit.CredentialMethod{{ID: "native", Label: "Existing gh login"}},
	}
	snapshot := Snapshot{
		Connectors:  []ConnectorCard{card},
		Connections: []state.Connection{{Name: "github", ConnectorKey: "github", Status: "active"}},
		Credentials: []state.CredentialRecord{{ID: "cred", ConnectorKey: "github"}},
		Tools:       []ToolCard{{ID: "gh"}},
		Skills:      []SkillCard{{Receipt: skillmanager.Receipt{ID: "repos", Connector: "github"}}},
		Imports:     []hostmcp.Candidate{{ID: "import", ConnectorKey: "github", Transferable: true}},
	}
	provider := BuildWorkbench(snapshot).Providers[0]
	if len(provider.Connections) != 1 || len(provider.Credentials) != 1 || len(provider.Tools) != 1 || len(provider.Skills) != 1 || len(provider.Imports) != 1 {
		t.Fatalf("provider composition = %#v", provider)
	}
}

func TestBuildWorkbenchKeepsPerAccountLinkEnrichmentState(t *testing.T) {
	card := ConnectorCard{Key: "notion", Display: "Notion", Links: []kit.LinkResolver{{ID: "page", ResourceType: "notion.page"}}}
	snapshot := Snapshot{
		Connectors:  []ConnectorCard{card},
		Connections: []state.Connection{{Name: "notion", ConnectorKey: "notion", Status: "active"}},
		LinkModes:   map[string]linkcontext.Mode{"notion": linkcontext.ModeHeavy},
	}
	provider := BuildWorkbench(snapshot).Providers[0]
	if provider.LinkModes["notion"] != linkcontext.ModeHeavy || len(provider.Connector.Links) != 1 {
		t.Fatalf("link enrichment state was not nested under the account: %#v", provider)
	}
}

func TestUnavailableHostImportDoesNotDisplaceRecommendedSetup(t *testing.T) {
	card := ConnectorCard{
		Key: "linear", Display: "Linear", RecommendedMode: "api",
		Modes:   []kit.Mode{{ID: "api", Label: "API", CredentialMethods: []string{"token"}, DefaultCredentialMethod: "token", Recommendation: kit.Recommendation{Rank: 0}}},
		Methods: []kit.CredentialMethod{{ID: "token", Label: "API token"}},
	}
	provider := BuildWorkbench(Snapshot{
		Connectors: []ConnectorCard{card},
		Imports:    []hostmcp.Candidate{{ID: "stale", ConnectorKey: "linear", Transferable: false, Reason: "reconnect required"}},
	}).Providers[0]
	if provider.Recipes[0].ModeID != "api" || !provider.Recipes[0].Recommended {
		t.Fatalf("unavailable import displaced recommended setup: %#v", provider.Recipes)
	}
}

func TestHostedOAuthRecipeIsExplicitlyBlockedUntilAHostImportExists(t *testing.T) {
	card := ConnectorCard{
		Key: "linear", Display: "Linear", RecommendedMode: "graphql",
		Modes: []kit.Mode{
			{ID: "graphql", Label: "GraphQL", CredentialMethods: []string{"key"}, DefaultCredentialMethod: "key", Recommendation: kit.Recommendation{Rank: 1}},
			{ID: "hosted_mcp", Label: "Hosted MCP", CredentialMethods: []string{"adopted-host-oauth"}, DefaultCredentialMethod: "adopted-host-oauth", Recommendation: kit.Recommendation{Rank: 2}},
		},
		Methods: []kit.CredentialMethod{
			{ID: "key", Label: "API key", Fields: []kit.CredentialField{{Key: "TOKEN", Label: "Token"}}},
			{ID: "adopted-host-oauth", Label: "Imported OAuth", Fields: []kit.CredentialField{{Key: "OAUTH_ACCESS_TOKEN", Generated: true, Secret: true}}},
		},
	}
	provider := BuildWorkbench(Snapshot{Connectors: []ConnectorCard{card}}).Providers[0]
	if provider.Recipes[0].ModeID != "graphql" || provider.Recipes[1].BlockedReason == "" {
		t.Fatalf("hosted OAuth recipe was not honestly blocked: %#v", provider.Recipes)
	}
}

func TestSearchMatchesOperationAndPreservesProviderContext(t *testing.T) {
	card := ConnectorCard{
		Key: "github", Display: "GitHub",
		Modes: []kit.Mode{{ID: "cli", Executor: kit.Executor{Tool: "gh"}, Operations: []kit.Operation{{Name: "pull_request.review", Title: "Review pull request"}}}},
	}
	snapshot := Snapshot{
		Connectors:  []ConnectorCard{card},
		Connections: []state.Connection{{Name: "work", ConnectorKey: "github", Status: "active"}},
		Credentials: []state.CredentialRecord{{ID: "cred", ConnectorKey: "github"}},
		Tools:       []ToolCard{{ID: "gh"}},
		Skills:      []SkillCard{{Receipt: skillmanager.Receipt{ID: "repos", Connector: "github"}}},
	}
	filtered := Search(snapshot, "pull request")
	if len(filtered.Connectors) != 1 || len(filtered.Connections) != 1 || len(filtered.Credentials) != 1 || len(filtered.Tools) != 1 || len(filtered.Skills) != 1 {
		t.Fatalf("search lost provider context: %#v", filtered)
	}
}
